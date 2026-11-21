package mention

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func workspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"main.go":             "package main\n\nfunc main() {}\n",
		"internal/a/a.go":     "line1\nline2\nline3\nline4\n",
		"dir with space/b.md": "hello\n",
		"bin.dat":             "\x00\x01\x02",
	}
	for p, c := range files {
		full := filepath.Join(dir, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(c), 0o644)
	}
	return dir
}

func TestExpand(t *testing.T) {
	dir := workspace(t)
	text := `look at @main.go, @internal/a/a.go:2-3 and @"dir with space/b.md". Mail me@example.com or ping @alice; also @internal/a/ @missing/x.go @bin.dat`
	content, atts := Expand(text, dir)

	var labels []string
	for _, a := range atts {
		labels = append(labels, a.Label())
	}
	want := []string{
		"main.go (3 lines)",
		"internal/a/a.go:2-3 (2 lines)",
		"dir with space/b.md (1 line)",
		"internal/a (1 entry)",
		"@missing/x.go: no such file",
		"@bin.dat: binary file, not attached",
	}
	if !reflect.DeepEqual(labels, want) {
		t.Fatalf("labels:\n%q\nwant\n%q", labels, want)
	}
	for _, s := range []string{
		"\n\n<file path=\"main.go\">\npackage main\n\nfunc main() {}\n</file>",
		"<file path=\"internal/a/a.go\" lines=\"2-3\">\nline2\nline3\n</file>",
		"<file path=\"dir with space/b.md\">\nhello\n</file>",
		"<directory path=\"internal/a\">\na.go\n</directory>",
	} {
		if !strings.Contains(content, s) {
			t.Errorf("content lacks %q:\n%s", s, content)
		}
	}
	if !strings.HasPrefix(content, text+"\n\n") {
		t.Fatal("typed text must come first, unchanged")
	}

	typed, paths := Split(content)
	if typed != text || !reflect.DeepEqual(paths, []string{"main.go", "internal/a/a.go", "dir with space/b.md", "internal/a"}) {
		t.Fatalf("Split = %q %q", typed, paths)
	}
	if typed, paths := Split("no attachments"); typed != "no attachments" || paths != nil {
		t.Fatal("Split of plain text")
	}
}

func TestExpandLegacyReadAndLimits(t *testing.T) {
	dir := workspace(t)
	content, atts := Expand("summarize /read(main.go) and @main.go", dir)
	if len(atts) != 1 || strings.Count(content, "<file ") != 1 {
		t.Fatalf("legacy /read + duplicate mention: %d attachments\n%s", len(atts), content)
	}

	big := strings.Repeat("0123456789abcdef\n", MaxFileBytes/17+100)
	os.WriteFile(filepath.Join(dir, "big.txt"), []byte(big), 0o644)
	content, atts = Expand("@big.txt", dir)
	if !atts[0].Truncated || len(content) > MaxFileBytes+300 || !strings.Contains(content, "use read_file for the rest") {
		t.Fatalf("big file: truncated=%v len=%d", atts[0].Truncated, len(content))
	}

	for i := 0; i < 5; i++ {
		os.WriteFile(filepath.Join(dir, "big"+string(rune('a'+i))+".txt"), []byte(big), 0o644)
	}
	_, atts = Expand("@biga.txt @bigb.txt @bigc.txt @bigd.txt @bige.txt", dir)
	if atts[2].Err != nil || atts[3].Err == nil || atts[4].Err == nil {
		t.Fatalf("total limit: %v / %v", atts[2].Err, atts[3].Err)
	}

	if _, atts := Expand("@main.go:9-12", dir); atts[0].Err == nil {
		t.Fatal("range past the end must fail")
	}
}

func TestComplete(t *testing.T) {
	ix := NewIndex([]string{"main.go", "internal/agent/agent.go", "internal/agent/context.go", "internal/tui/model.go", "README.md"})
	cases := map[string][]string{
		"":          {"main.go", "README.md", "internal/"},
		"int":       {"internal/", "internal/tui/", "internal/agent/"},
		"internal/": {"internal/tui/", "internal/agent/", "internal/tui/model.go"},
		"agent.go":  {"internal/agent/agent.go", "internal/agent/context.go"}, // then fuzzy
		"ctx":       {"internal/agent/context.go"},
		"READ":      {"README.md"},
	}
	for q, want := range cases {
		got := ix.Complete(q, 3)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Complete(%q) = %q, want %q", q, got, want)
		}
	}
	for text, want := range map[string]string{"see @int": "int", "@": "", "a @b c": "\x00", "me@x": "\x00", `@"a b`: "\x00"} {
		q, ok := Partial(text)
		if (want == "\x00") == ok || ok && q != want {
			t.Errorf("Partial(%q) = %q, %v", text, q, ok)
		}
	}
}
