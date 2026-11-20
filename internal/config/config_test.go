package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolvePersonaWithFile(t *testing.T) {
	// Create a temp directory for test files
	tmpDir, err := os.MkdirTemp("", "aig_config_test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Test file content
	promptContent := "This is a custom prompt file content."
	filePath := filepath.Join(tmpDir, "test_prompt.md")
	if err := os.WriteFile(filePath, []byte(promptContent), 0o600); err != nil {
		t.Fatalf("failed to write test prompt file: %v", err)
	}

	// 1. Resolve existing file path
	fc := &FileConfig{}
	resolved, ok := ResolvePersona(filePath, fc)
	if !ok {
		t.Errorf("expected ResolvePersona to succeed for file path %s", filePath)
	}
	if resolved != promptContent {
		t.Errorf("expected resolved prompt to be %q, got %q", promptContent, resolved)
	}

	// 2. Resolve non-existing file path
	nonExisting := filepath.Join(tmpDir, "non_existent.md")
	resolved, ok = ResolvePersona(nonExisting, fc)
	if ok {
		t.Errorf("expected ResolvePersona to fail for non-existing path %s, but got prompt: %q", nonExisting, resolved)
	}

	// 3. Fallback to standard persona when file doesn't exist
	resolved, ok = ResolvePersona("coder", fc)
	if !ok {
		t.Errorf("expected fallback to default coder persona to succeed")
	}
	if resolved == "" {
		t.Errorf("expected non-empty fallback prompt for coder")
	}
}

// isolateHome points HOME (and thus Path()) at a temp dir so tests never
// touch the real ~/.config/aig.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("GEMINI_API_KEY", "mock-gemini-key")
	t.Setenv("DEEPSEEK_API_KEY", "")
	return home
}

func TestLoadIgnoresLocalAigMdUnlessPassed(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(LocalPromptFile, []byte("Local prompt"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Empty persona → aig.md must NOT be picked up implicitly.
	cfg, err := Load("", "", "")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.Persona != "coder" {
		t.Errorf("expected default persona 'coder', got %q", cfg.Persona)
	}

	// Explicitly passed (after user consent) → used.
	cfg, err = Load("", "", LocalPromptFile)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.SystemPrompt != "Local prompt" {
		t.Errorf("expected local prompt, got %q", cfg.SystemPrompt)
	}
}

func TestPromptTrustStore(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(LocalPromptFile, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}

	if IsPromptTrusted(LocalPromptFile) {
		t.Fatal("new file must not be trusted")
	}
	if err := TrustPrompt(LocalPromptFile); err != nil {
		t.Fatal(err)
	}
	if !IsPromptTrusted(LocalPromptFile) {
		t.Fatal("approved file should be trusted")
	}

	// Changing the content invalidates the approval.
	if err := os.WriteFile(LocalPromptFile, []byte("v2 — injected"), 0o600); err != nil {
		t.Fatal(err)
	}
	if IsPromptTrusted(LocalPromptFile) {
		t.Fatal("modified file must not stay trusted")
	}
}

func TestDefaultConfigDoesNotCopyPersonas(t *testing.T) {
	home := isolateHome(t)
	if err := WriteDefaultConfig(Path()); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(Path(), home) {
		t.Fatalf("Path() escaped the test HOME: %s", Path())
	}
	data, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "personas") {
		t.Errorf("default config should not embed built-in personas:\n%s", data)
	}

	// Built-ins are still available, and user personas override them.
	custom := `{"personas": {"coder": "my coder", "mine": "custom"}}`
	if err := os.WriteFile(Path(), []byte(custom), 0o600); err != nil {
		t.Fatal(err)
	}
	fc, err := LoadFileConfigE()
	if err != nil {
		t.Fatal(err)
	}
	if fc.Personas["coder"] != "my coder" || fc.Personas["mine"] != "custom" {
		t.Errorf("user personas not applied: %v", fc.Personas["coder"])
	}
	if fc.Personas["sysadmin"] == "" {
		t.Error("built-in sysadmin persona missing after merge")
	}
}

func TestLoadReportsInvalidJSON(t *testing.T) {
	isolateHome(t)
	if err := os.MkdirAll(filepath.Dir(Path()), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load("", "", ""); err == nil {
		t.Fatal("expected an error for malformed config JSON")
	}
}
