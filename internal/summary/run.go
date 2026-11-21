package summary

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
)

const (
	// concurrency is how many summary requests run at once.
	concurrency = 4
	// requestTimeout bounds one summary request.
	requestTimeout = 2 * time.Minute
	// maxFailures aborts a run once this many requests failed (bad key,
	// rate limit, model without access) instead of failing every file.
	maxFailures = 5
	// saveEvery files, the index is saved so a cancelled run keeps progress.
	saveEvery = 20
)

// Progress reports a run's state.
type Progress struct {
	Phase       string // "files", "dirs", "project", "done"
	Done, Total int
	Failed      int
}

// Result summarizes a finished run.
type Result struct {
	Files, Dirs int // summaries generated this run
	Failed      int
	Path        string // PROJECT_SUMMARY.md
}

const fileSystem = "You write summaries of source files for a code index that a coding agent reads to find its way around a repository. Be accurate and specific; never invent behaviour that isn't in the code."

const fileInstr = `Summarize this file in 2-4 sentences of plain text (no headings, no lists, no preamble):
what it is responsible for, its main types and functions by name, and how it connects to the rest of the project when the imports or calls make that clear.`

const dirInstr = `Below are summaries of the files and subdirectories in the directory %q. In 2-4 sentences of plain text (no headings, no preamble), describe what this directory is responsible for and name its main parts.`

const projectInstr = `You are writing the overview of the project %q for a coding agent that is about to work on it. Below are summaries of its top-level directories and files%s.

Write 150-250 words in markdown:
1. One or two sentences on what the project is and does.
2. Architecture: the main components (by directory) and how control and data flow between them.
3. Where to start reading for common tasks.
No headings above level 3, no preamble, nothing that the summaries don't support.`

// Run summarizes what Scan found changed, then rebuilds the affected
// directory summaries and the project overview, and writes the index and
// PROJECT_SUMMARY.md. model is recorded for display only. progress calls
// are serialized and must not block.
func (p *Plan) Run(ctx context.Context, provider llm.Provider, model string, progress func(Progress)) (*Result, error) {
	if progress == nil {
		progress = func(Progress) {}
	}
	ix := p.index
	ix.Model = model
	res := &Result{}

	// Drop files that no longer exist.
	live := map[string]candidate{}
	for _, f := range p.files {
		live[f.rel] = f
	}
	for rel := range ix.Files {
		if _, ok := live[rel]; !ok {
			delete(ix.Files, rel)
		}
	}

	// ── files ────────────────────────────────────────────────────────────
	var todo []candidate
	for _, f := range p.files {
		if e, ok := ix.Files[f.rel]; !ok || e.Hash != f.hash {
			todo = append(todo, f)
		}
	}
	var mu sync.Mutex
	failed, done := 0, 0
	var firstErr error
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan candidate)
	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range jobs {
				sum, err := summarizeFile(runCtx, provider, p.Root, f)
				mu.Lock()
				done++
				if err != nil {
					if runCtx.Err() == nil {
						failed++
						if firstErr == nil {
							firstErr = fmt.Errorf("%s: %w", f.rel, err)
						}
						if failed >= maxFailures {
							cancel()
						}
					}
				} else {
					ix.Files[f.rel] = FileEntry{Hash: f.hash, Summary: sum}
					res.Files++
					if res.Files%saveEvery == 0 {
						_ = ix.save()
					}
				}
				// Called under the lock: callbacks are serialized.
				progress(Progress{Phase: "files", Done: done, Total: len(todo), Failed: failed})
				mu.Unlock()
			}
		}()
	}
feed:
	for _, f := range todo {
		select {
		case jobs <- f:
		case <-runCtx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	res.Failed = failed
	_ = ix.save()
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if failed >= maxFailures {
		return res, fmt.Errorf("stopped after %d failed requests; first error: %w", failed, firstErr)
	}

	// ── directories, deepest first ───────────────────────────────────────
	dirs := map[string]bool{}
	for rel := range ix.Files {
		for d := path.Dir(rel); d != "."; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	for d := range ix.Dirs {
		if !dirs[d] {
			delete(ix.Dirs, d)
		}
	}
	byDepth := map[int][]string{}
	maxDepth := 0
	for d := range dirs {
		n := strings.Count(d, "/") + 1
		byDepth[n] = append(byDepth[n], d)
		maxDepth = max(maxDepth, n)
	}
	totalDirs := len(dirs)
	doneDirs := 0
	for depth := maxDepth; depth >= 1; depth-- {
		level := byDepth[depth]
		sort.Strings(level)
		errs := parallel(ctx, level, func(ctx context.Context, d string) error {
			// Other goroutines of this level write ix.Dirs: read under the lock.
			mu.Lock()
			hash, children := dirInput(ix, d)
			cached, ok := ix.Dirs[d]
			mu.Unlock()
			if ok && cached.Hash == hash {
				return nil
			}
			var sum string
			if len(children) == 1 {
				// A single child says it all; no request needed.
				sum = children[0][strings.Index(children[0], ": ")+2:]
			} else {
				out, err := complete(ctx, provider, fileSystem, fmt.Sprintf(dirInstr, d)+"\n\n"+strings.Join(children, "\n"))
				if err != nil {
					return fmt.Errorf("%s/: %w", d, err)
				}
				sum = out
				mu.Lock()
				res.Dirs++
				mu.Unlock()
			}
			mu.Lock()
			ix.Dirs[d] = FileEntry{Hash: hash, Summary: sum}
			mu.Unlock()
			return nil
		}, func() {
			mu.Lock()
			doneDirs++
			progress(Progress{Phase: "dirs", Done: doneDirs, Total: totalDirs, Failed: failed})
			mu.Unlock()
		})
		if ctx.Err() != nil {
			_ = ix.save()
			return res, ctx.Err()
		}
		if len(errs) > 0 {
			res.Failed += len(errs)
			_ = ix.save()
			return res, fmt.Errorf("directory summaries failed: %w", errs[0])
		}
	}

	// ── project ──────────────────────────────────────────────────────────
	progress(Progress{Phase: "project", Done: 0, Total: 1})
	hash, children := dirInput(ix, ".")
	readme := readmeExcerpt(p.Root)
	hash = hashOf("project", hash, readme)
	if ix.Project.Hash != hash || ix.Project.Summary == "" {
		extra := ""
		input := strings.Join(children, "\n")
		if readme != "" {
			extra = " and an excerpt of its README"
			input += "\n\nREADME excerpt:\n" + readme
		}
		out, err := complete(ctx, provider, fileSystem, fmt.Sprintf(projectInstr, filepath.Base(p.Root), extra)+"\n\n"+input)
		if err != nil {
			_ = ix.save()
			return res, fmt.Errorf("project summary: %w", err)
		}
		ix.Project = FileEntry{Hash: hash, Summary: out}
	}

	ix.Updated = time.Now()
	if err := ix.save(); err != nil {
		return res, err
	}
	if err := os.WriteFile(MarkdownPath(p.Root), []byte(ix.Markdown()), 0o600); err != nil {
		return res, err
	}
	res.Path = MarkdownPath(p.Root)
	progress(Progress{Phase: "done", Done: 1, Total: 1, Failed: res.Failed})
	return res, nil
}

// dirInput returns the summaries of dir's direct children as "- name: summary"
// lines, and a hash over them (the directory's cache key).
func dirInput(ix *Index, dir string) (string, []string) {
	var lines []string
	for _, f := range ix.filesIn(dir) {
		lines = append(lines, fmt.Sprintf("- %s: %s", path.Base(f), oneLine(ix.Files[f].Summary)))
	}
	for _, d := range ix.subdirsOf(dir) {
		lines = append(lines, fmt.Sprintf("- %s/: %s", path.Base(d), oneLine(ix.Dirs[d].Summary)))
	}
	return hashOf("dir", dir, strings.Join(lines, "\n")), lines
}

func readmeExcerpt(root string) string {
	for _, name := range []string{"README.md", "README", "readme.md", "README.rst", "README.txt"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err == nil {
			s := string(data)
			if len(s) > 4000 {
				s = s[:4000] + "\n…"
			}
			return strings.ToValidUTF8(s, "")
		}
	}
	return ""
}

func summarizeFile(ctx context.Context, provider llm.Provider, root string, f candidate) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.rel)))
	if err != nil {
		return "", err
	}
	content := string(data)
	note := ""
	if len(content) > maxSentBytes {
		content = strings.ToValidUTF8(content[:maxSentBytes], "")
		note = fmt.Sprintf("\n(The file is %d bytes; only the first %d are shown.)", len(data), maxSentBytes)
	}
	return complete(ctx, provider, fileSystem, fmt.Sprintf("%s\n\nFile: %s%s\n```\n%s\n```", fileInstr, f.rel, note, content))
}

// parallel runs fn over items with bounded concurrency, calling tick after
// each; it returns the errors.
func parallel(ctx context.Context, items []string, fn func(context.Context, string) error, tick func()) []error {
	var mu sync.Mutex
	var errs []error
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, it := range items {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(it string) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := fn(ctx, it); err != nil && ctx.Err() == nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
			tick()
		}(it)
	}
	wg.Wait()
	return errs
}

// complete sends one request without tools and returns the answer text
// (reasoning dropped).
func complete(ctx context.Context, provider llm.Provider, system, user string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	ch := make(chan string, 64)
	errc := make(chan error, 1)
	go func() {
		_, _, err := provider.StreamChat(ctx, []llm.Message{
			{Role: llm.RoleSystem, Content: system},
			{Role: llm.RoleUser, Content: user},
		}, nil, ch)
		errc <- err
	}()
	var split llm.ThinkSplitter
	for tok := range ch {
		if strings.HasPrefix(tok, "\x00") {
			continue // native reasoning
		}
		split.Write(tok)
	}
	err := <-errc
	split.Flush()
	if err != nil {
		return "", err
	}
	out := strings.TrimSpace(split.Content())
	if out == "" {
		return "", fmt.Errorf("empty response")
	}
	return out, nil
}
