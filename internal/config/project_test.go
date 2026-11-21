package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectConfigKeepsCheckOrder(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".aig"), 0o755)
	path := filepath.Join(root, ".aig", "config.json")
	os.WriteFile(path, []byte(`{"checks": {"vet": "go vet ./...", "build": "go build ./...", "test": "go test ./..."}, "checks_timeout_seconds": 90}`), 0o644)

	sub := filepath.Join(root, "internal", "x")
	os.MkdirAll(sub, 0o755)
	found := FindProjectConfig(sub, root)
	if found != path {
		t.Fatalf("FindProjectConfig = %q, want %q", found, path)
	}
	pc, err := LoadProjectConfig(found)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range pc.Checks {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "vet,build,test" {
		t.Errorf("order = %v", names)
	}
	if pc.Root != root || pc.ChecksTimeoutSeconds != 90 {
		t.Errorf("root=%q timeout=%d", pc.Root, pc.ChecksTimeoutSeconds)
	}

	out, _ := json.Marshal(pc.Checks)
	if string(out) != `{"vet":"go vet ./...","build":"go build ./...","test":"go test ./..."}` {
		t.Errorf("marshal = %s", out)
	}
}

func TestProjectConfigRejectsBadChecks(t *testing.T) {
	for _, tc := range []string{
		`{"checks": ["go test"]}`,
		`{"checks": {"test": 1}}`,
		`{"checks": {"test": ""}}`,
		`{"checks": {"test": "a", "test": "b"}}`,
	} {
		var pc ProjectConfig
		if err := json.Unmarshal([]byte(tc), &pc); err == nil {
			t.Errorf("%s: expected error", tc)
		}
	}
}

func TestFindProjectConfigNone(t *testing.T) {
	if p := FindProjectConfig(t.TempDir(), ""); p != "" {
		t.Errorf("got %q", p)
	}
}
