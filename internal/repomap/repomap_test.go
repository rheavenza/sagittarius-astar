package repomap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func build(t *testing.T, root string) *Map {
	t.Helper()
	m, err := Build(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestGoSignatures(t *testing.T) {
	root := t.TempDir()
	write(t, root, "go.mod", "module x\n")
	write(t, root, "store/store.go", `package store

import "context"

// Store keeps things.
type Store struct {
	// Name is documented.
	Name string `+"`json:\"name\"`"+`
	items map[string]int
}

type Getter interface {
	Get(ctx context.Context, key string) (int, error)
}

const (
	ModeA Mode = iota
	ModeB
	modeHidden
)

type Mode int

func New(name string) *Store { return &Store{Name: name} }

func (s *Store) Get(ctx context.Context, key string) (int, error) {
	return s.items[key], nil
}

func helper[T any](v T) T { return v }
`)
	out := build(t, root).Render(Options{MaxTokens: 2000, LineNumbers: true})
	for _, want := range []string{
		"store/store.go:",
		"type Store struct { Name string; items map[string]int }",
		"type Getter interface { Get(ctx context.Context, key string) (int, error) }",
		"const ModeA, ModeB Mode",
		"func New(name string) *Store",
		"func (s *Store) Get(ctx context.Context, key string) (int, error)",
		"func helper[T any](v T) T",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "modeHidden") || strings.Contains(out, "documented") || strings.Contains(out, "json:") {
		t.Errorf("unexported consts, comments or tags leaked:\n%s", out)
	}
	if !strings.Contains(out, "   24  func New") {
		t.Errorf("line numbers missing:\n%s", out)
	}
}

func TestGoRankingUsesPackageScope(t *testing.T) {
	root := t.TempDir()
	write(t, root, "go.mod", "module x\n")
	// core.Important is used from two other packages; core.rare only once
	// in its own package. util has a method named like a common word.
	write(t, root, "core/core.go", "package core\n\nfunc Important() {}\n\nfunc rare() {}\n\nfunc use() { rare() }\n")
	write(t, root, "a/a.go", "package a\n\nimport \"x/core\"\n\nfunc A() { core.Important(); core.Important() }\n")
	write(t, root, "b/b.go", "package b\n\nimport \"x/core\"\n\nfunc B() { core.Important() }\n\n// rare rare rare: words in comments are not references.\nfunc Important() {}\n")
	m := build(t, root)
	ranks := m.rank(nil)
	score := map[string]float64{}
	for _, r := range ranks {
		score[r.file.Path+":"+r.sym.Names[0]] = r.score
	}
	if ranks[0].file.Path != "core/core.go" || ranks[0].sym.Names[0] != "Important" {
		t.Errorf("core.Important should rank first: %v", score)
	}
	// 1 base + 3 cross-package refs + 0.5 exported.
	if got := score["core/core.go:Important"]; got != 4.5 {
		t.Errorf("core.Important score = %v, want 4.5", got)
	}
	// b.Important is never referenced as b.Important: core.Important calls
	// must not count for it (1 base + 0.5 exported).
	if got := score["b/b.go:Important"]; got != 1.5 {
		t.Errorf("b.Important score = %v, want 1.5 (no borrowed references)", got)
	}
	// rare: one call in its own file counts a quarter.
	if got := score["core/core.go:rare"]; got != 1.25 {
		t.Errorf("rare score = %v, want 1.25", got)
	}
}

func TestBudgetFocusAndOmittedNote(t *testing.T) {
	root := t.TempDir()
	write(t, root, "go.mod", "module x\n")
	var src strings.Builder
	src.WriteString("package big\n")
	for i := 0; i < 200; i++ {
		src.WriteString("func F" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + itoa(i) + "() {}\n")
	}
	write(t, root, "big/big.go", src.String())
	write(t, root, "small/target.go", "package small\n\nfunc Needle() {}\n")
	m := build(t, root)

	out := m.Render(Options{MaxTokens: 100})
	if len(out) > 100*4+200 {
		t.Errorf("budget exceeded: %d bytes", len(out))
	}
	if !strings.Contains(out, "lower-ranked symbols omitted") {
		t.Errorf("no omitted note:\n%s", out)
	}
	for _, focus := range []string{"small/", "Needle"} {
		if got := m.Render(Options{MaxTokens: 60, Focus: []string{focus}}); !strings.Contains(got, "func Needle()") {
			t.Errorf("focus %q not boosted:\n%s", focus, got)
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

func TestLinePatternLanguages(t *testing.T) {
	root := t.TempDir()
	write(t, root, "app/models.py", "class UserAccount(Base):\n    def full_name(self) -> str:\n        return ''\n\nasync def fetch_users(db):\n    pass\n")
	write(t, root, "web/api.ts", "export interface ApiClient {\n}\nexport type UserId = string;\nexport async function loadUser(id: UserId): Promise<User> {\n  if (x) {\n  }\n  const save = async () => {};\n}\nconst formatName = (u) => u.name;\nexport class Repo {\n  async findAll(limit: number): Promise<User[]> {\n  }\n}\n")
	write(t, root, "src/lib.rs", "pub struct Parser {\n}\nimpl Parser {\n    pub fn parse_all(&self) -> Result<()> {\n    }\n}\npub trait Visitor {}\n")
	write(t, root, "scripts/run.sh", "build_all() {\n  :\n}\n")
	write(t, root, "web/bundle.min.js", "function minified(){}\n")
	write(t, root, "node_modules/dep/index.js", "export function vendored() {}\n")
	write(t, root, "gen/gen.go", "// Code generated by tool. DO NOT EDIT.\n\npackage gen\n\nfunc Generated() {}\n")

	out := build(t, root).Render(Options{MaxTokens: 4000})
	for _, want := range []string{
		"class UserAccount(Base)", "  def full_name(self) -> str", "async def fetch_users(db)",
		"export interface ApiClient", "export type UserId = string;", "export async function loadUser(id: UserId): Promise<User>",
		"const formatName = (u)", "export class Repo", "  async findAll(limit: number): Promise<User[]>",
		"pub struct Parser", "impl Parser", "  pub fn parse_all(&self) -> Result<()>", "pub trait Visitor",
		"build_all()",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, bad := range []string{"if (x)", "const save", "minified", "vendored", "Generated"} {
		if strings.Contains(out, bad) {
			t.Errorf("should not contain %q:\n%s", bad, out)
		}
	}
}

func TestCacheReparsesChangedFiles(t *testing.T) {
	root := t.TempDir()
	write(t, root, "x.py", "def first_version():\n    pass\n")
	if out := build(t, root).Render(Options{}); !strings.Contains(out, "first_version") {
		t.Fatalf("first build:\n%s", out)
	}
	write(t, root, "x.py", "def second_version_longer():\n    pass\n")
	// Make sure mtime moves even on coarse-grained filesystems.
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(filepath.Join(root, "x.py"), future, future)
	out := build(t, root).Render(Options{})
	if !strings.Contains(out, "second_version_longer") || strings.Contains(out, "first_version") {
		t.Errorf("stale cache:\n%s", out)
	}
}

func TestBuildRejectsFile(t *testing.T) {
	root := t.TempDir()
	write(t, root, "f.go", "package f\n")
	if _, err := Build(context.Background(), filepath.Join(root, "f.go")); err == nil {
		t.Error("expected error for a file")
	}
}
