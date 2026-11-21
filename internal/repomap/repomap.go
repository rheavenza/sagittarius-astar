// Package repomap builds a compact map of a repository: the symbols and
// signatures defined in each source file, ranked by how often they are
// referenced elsewhere, and rendered to fit a token budget.
//
// Go files are parsed with go/parser. Python, JS/TS, Rust and shell use
// line-based patterns, which is cruder but needs no extra tools. Parsed files
// are cached by path, size and mtime, so repeated maps only re-read changed
// files.
package repomap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/clitorhea/sagittarius-astar.git/internal/workspace"
)

const (
	// maxFiles bounds how many source files one map parses.
	maxFiles = 20_000
	// maxFileBytes: larger files (generated code, bundles) are skipped.
	maxFileBytes = 512 * 1024
	// maxSigLen caps one rendered signature.
	maxSigLen = 200
)

// Symbol is one definition in a file.
type Symbol struct {
	Kind string // func, method, type, const, var, class, …
	// Names are the identifiers it defines (several for a const block); they
	// are what references are counted against.
	Names    []string
	Sig      string // one-line signature
	Line     int
	Exported bool
}

// File is one parsed source file.
type File struct {
	Path    string // slash-separated, relative to the map root
	Lang    string
	Symbols []Symbol
	// idents counts identifier occurrences in the file (definitions included).
	idents map[string]int
}

// Map is a parsed repository.
type Map struct {
	Root      string
	Files     []*File
	Truncated bool // more source files existed than were parsed
}

// Options tunes Render.
type Options struct {
	// MaxTokens is the output budget (≈4 bytes per token). Default 4096.
	MaxTokens int
	// Focus boosts files whose path contains an entry and symbols whose name
	// equals one, e.g. files and functions the user mentioned.
	Focus []string
	// LineNumbers prefixes each symbol with its line number.
	LineNumbers bool
}

// Build parses the source files under root (respecting .gitignore).
func Build(ctx context.Context, root string) (*Map, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("repo_map: %s is not a directory", root)
	}
	all, _, err := workspace.ListFiles(abs, 200_000)
	if err != nil {
		return nil, err
	}
	m := &Map{Root: abs}
	var paths []string
	for _, rel := range all {
		if langOf(rel) == "" || isVendored(rel) {
			continue
		}
		if len(paths) >= maxFiles {
			m.Truncated = true
			break
		}
		paths = append(paths, rel)
	}

	files := make([]*File, len(paths))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < runtime.NumCPU(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				files[i] = loadFile(abs, paths[i])
			}
		}()
	}
feed:
	for i := range paths {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, f := range files {
		if f != nil {
			m.Files = append(m.Files, f)
		}
	}
	return m, nil
}

// ── cache ────────────────────────────────────────────────────────────────────

type cacheKey struct {
	path  string
	size  int64
	mtime time.Time
}

var (
	cacheMu sync.Mutex
	cache   = map[string]cacheEntry{}
)

type cacheEntry struct {
	key  cacheKey
	file File // Path is relative to the root it was first parsed under
}

func loadFile(root, rel string) *File {
	full := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Stat(full)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxFileBytes {
		return nil
	}
	key := cacheKey{full, info.Size(), info.ModTime()}
	cacheMu.Lock()
	e, ok := cache[full]
	cacheMu.Unlock()
	if ok && e.key == key {
		f := e.file
		f.Path = rel
		return &f
	}
	data, err := os.ReadFile(full)
	if err != nil || looksBinary(data) {
		return nil
	}
	f := parseFile(rel, data)
	if isGenerated(data) {
		// Its references still count; its own symbols are noise.
		f.Symbols = nil
	}
	cacheMu.Lock()
	cache[full] = cacheEntry{key: key, file: *f}
	cacheMu.Unlock()
	return f
}

func parseFile(rel string, data []byte) *File {
	f := &File{Path: rel, Lang: langOf(rel)}
	if f.Lang == "go" {
		if syms, idents, ok := parseGo(rel, data); ok {
			f.Symbols, f.idents = syms, idents
			return f
		}
		// Syntax error: fall back to the line patterns below.
	}
	f.Symbols = parseLines(f.Lang, string(data))
	f.idents = countIdents(string(data))
	return f
}

// reGenerated is the standard marker for generated files
// (https://go.dev/s/generatedcode, also used by protoc, sqlc, …).
var reGenerated = regexp.MustCompile(`(?m)^(//|#) Code generated .* DO NOT EDIT\.?$`)

// isGenerated reports a generated-file marker in the first 4 KB.
func isGenerated(data []byte) bool {
	if len(data) > 4096 {
		data = data[:4096]
	}
	return reGenerated.Match(data)
}

var reIdent = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

func countIdents(src string) map[string]int {
	out := map[string]int{}
	for _, id := range reIdent.FindAllString(src, -1) {
		if len(id) > 1 {
			out[id]++
		}
	}
	return out
}

func looksBinary(data []byte) bool {
	n := len(data)
	if n > 8000 {
		n = 8000
	}
	for _, b := range data[:n] {
		if b == 0 {
			return true
		}
	}
	return false
}

// ── languages ────────────────────────────────────────────────────────────────

var langByExt = map[string]string{
	".go":   "go",
	".py":   "python",
	".js":   "js",
	".jsx":  "js",
	".mjs":  "js",
	".cjs":  "js",
	".ts":   "ts",
	".tsx":  "ts",
	".mts":  "ts",
	".rs":   "rust",
	".sh":   "shell",
	".bash": "shell",
}

func langOf(path string) string {
	if strings.HasSuffix(path, ".min.js") || strings.HasSuffix(path, ".d.ts") {
		return ""
	}
	return langByExt[strings.ToLower(filepath.Ext(path))]
}

// isVendored skips dependency and build dirs even when they are tracked.
func isVendored(rel string) bool {
	for _, part := range strings.Split(rel, "/") {
		if workspace.IsIgnoredDir(part) {
			return true
		}
	}
	return false
}

// isTest reports test files, which rank lower: they reference a lot but
// rarely explain the design.
func isTest(path string) bool {
	base := filepath.Base(path)
	return strings.HasSuffix(base, "_test.go") ||
		strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py") ||
		strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") ||
		strings.Contains("/"+path, "/tests/") || strings.Contains("/"+path, "/__tests__/")
}

// ── ranking & rendering ──────────────────────────────────────────────────────

// genericName reports short lowercase names (db, row, json, status) whose
// textual matches are mostly unrelated uses of the same word.
func genericName(name string) bool {
	if len(name) < 5 {
		return true
	}
	for _, r := range name {
		if r < 'a' || r > 'z' {
			return false // camelCase, snake_case, digits: specific enough
		}
	}
	return len(name) < 8
}

type ranked struct {
	file  *File
	sym   *Symbol
	score float64
}

// goPackage returns the package name recorded by parseGo ("" otherwise).
func (f *File) goPackage() string {
	if f.Lang != "go" {
		return ""
	}
	for k := range f.idents {
		if name, ok := strings.CutPrefix(k, "package "); ok {
			return name
		}
	}
	return ""
}

func dirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return "."
}

// rank scores every symbol by how often it is referenced: fully from other
// files, a quarter from its own file. A reference shared by several
// definitions of the same name is split between them. Types and exported
// names get a small bonus, test files are discounted, focus matches boosted.
//
// Go uses scoped keys: a package-level name counts as "pkg.Name" outside its
// package and as a bare name inside it; methods count ".Name" selectors.
func (m *Map) rank(focus []string) []ranked {
	type goPkg struct{ dir, name string }
	pkgOf := map[*File]goPkg{}
	for _, f := range m.Files {
		if n := f.goPackage(); n != "" {
			pkgOf[f] = goPkg{dirOf(f.Path), n}
		}
	}

	// Totals per identifier key, over all files and per Go package, so
	// each symbol is scored in O(1).
	global := map[string]int{}
	fileFreq := map[string]int{} // non-Go: how many files mention a name
	byPkg := map[goPkg]map[string]int{}
	for _, f := range m.Files {
		if _, ok := pkgOf[f]; !ok {
			for k := range f.idents {
				fileFreq[k]++
			}
		}
		var pk map[string]int
		if p, ok := pkgOf[f]; ok {
			if pk = byPkg[p]; pk == nil {
				pk = map[string]int{}
				byPkg[p] = pk
			}
		}
		for k, n := range f.idents {
			global[k] += n
			if pk != nil {
				pk[k] += n
			}
		}
	}
	refs := func(f *File, s *Symbol, name string) (other, same float64) {
		fp, isGo := pkgOf[f]
		var o, sm int
		switch {
		case !isGo:
			// Without scopes, raw counts are dominated by common words;
			// count referencing files instead.
			sm = min(f.idents[name]-1, 10) // minus the definition
			o = fileFreq[name] - 1
		case s.Kind == "method":
			key := "." + name
			sm = f.idents[key]
			if s.Exported {
				o = global[key] - sm
			} else {
				o = byPkg[fp][key] - sm
			}
		default:
			sm = f.idents[name] - 1
			o = byPkg[fp][name] - f.idents[name]
			if s.Exported {
				q := fp.name + "." + name
				o += global[q] - byPkg[fp][q]
			}
		}
		return float64(o), float64(max(sm, 0))
	}

	// How many symbols define each name, to split shared references.
	defs := map[string]int{}
	for _, f := range m.Files {
		for _, s := range f.Symbols {
			for _, n := range s.Names {
				key := n
				if s.Kind == "method" {
					key = "." + n
				} else if p, ok := pkgOf[f]; ok {
					key = p.dir + "." + n
				}
				defs[key]++
			}
		}
	}

	var out []ranked
	for _, f := range m.Files {
		fileFocus := false
		for _, q := range focus {
			if q = strings.TrimPrefix(filepath.ToSlash(q), "./"); q != "" && strings.Contains(f.Path, q) {
				fileFocus = true
			}
		}
		for i := range f.Symbols {
			s := &f.Symbols[i]
			score := 1.0
			for _, name := range s.Names {
				other, same := refs(f, s, name)
				key := name
				if s.Kind == "method" {
					key = "." + name
				} else if p, ok := pkgOf[f]; ok {
					key = p.dir + "." + name
				}
				share := float64(max(defs[key], 1))
				score += (other + 0.25*same) / share
				for _, q := range focus {
					if q == name {
						score = score*10 + 100
					}
				}
			}
			if _, isGo := pkgOf[f]; !isGo {
				if genericName(s.Names[0]) {
					score *= 0.2
				}
				if strings.HasPrefix(s.Sig, "  ") { // nested (method)
					score *= 0.5
				}
			}
			if s.Exported {
				score += 0.5
			}
			switch s.Kind {
			case "type", "class", "interface", "struct", "trait":
				score += 2
			}
			if isTest(f.Path) {
				score *= 0.1
			}
			if fileFocus {
				score = score*5 + 50
			}
			out = append(out, ranked{f, s, score})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].file.Path < out[j].file.Path
	})
	return out
}

// Render picks the highest-ranked symbols that fit the budget and prints
// them grouped by file (files in path order, symbols in line order).
func (m *Map) Render(o Options) string {
	if o.MaxTokens <= 0 {
		o.MaxTokens = 4096
	}
	budget := o.MaxTokens * 4
	all := m.rank(o.Focus)

	chosen := map[*File][]*Symbol{}
	used, nSyms := 0, 0
	for _, r := range all {
		cost := len(r.sym.Sig) + 8
		if _, ok := chosen[r.file]; !ok {
			cost += len(r.file.Path) + 2
		}
		if used+cost > budget {
			continue // a shorter one may still fit
		}
		used += cost
		chosen[r.file] = append(chosen[r.file], r.sym)
		nSyms++
	}
	if nSyms == 0 {
		return ""
	}

	files := make([]*File, 0, len(chosen))
	for f := range chosen {
		files = append(files, f)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	var b strings.Builder
	for _, f := range files {
		syms := chosen[f]
		sort.Slice(syms, func(i, j int) bool { return syms[i].Line < syms[j].Line })
		b.WriteString(f.Path + ":\n")
		for _, s := range syms {
			if o.LineNumbers {
				fmt.Fprintf(&b, "%5d  %s\n", s.Line, s.Sig)
			} else {
				b.WriteString("  " + s.Sig + "\n")
			}
		}
	}
	if omitted := len(all) - nSyms; omitted > 0 || m.Truncated {
		fmt.Fprintf(&b, "(%d lower-ranked symbols omitted", omitted)
		if m.Truncated {
			fmt.Fprintf(&b, "; only the first %d source files were parsed", maxFiles)
		}
		b.WriteString(")\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// Stats summarises the map for logs.
func (m *Map) Stats() (files, symbols int) {
	for _, f := range m.Files {
		symbols += len(f.Symbols)
	}
	return len(m.Files), symbols
}

func clip(sig string) string {
	sig = strings.Join(strings.Fields(sig), " ")
	if len(sig) <= maxSigLen {
		return sig
	}
	cut := maxSigLen
	for cut > 0 && sig[cut]&0xC0 == 0x80 {
		cut--
	}
	return sig[:cut] + "…"
}
