package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// postEditCheckTimeout bounds a single post-edit check.
const postEditCheckTimeout = 60 * time.Second

// maxCheckOutput caps check output returned to the model.
const maxCheckOutput = 4 * 1024

// PostEditCheck runs a fast syntax/compile check for a file that was just
// written and returns a short report for the tool result ("" if no check
// applies). Errors are fed back to the model so it can fix them right away.
//
// custom maps a file extension (".ts") to a shell command with {file} and
// {dir} placeholders; the value "off" disables checks for that extension.
func PostEditCheck(ctx context.Context, path string, custom map[string]string) string {
	ext := strings.ToLower(filepath.Ext(path))
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}

	if cmdline, ok := custom[ext]; ok {
		if strings.EqualFold(strings.TrimSpace(cmdline), "off") {
			return ""
		}
		cmdline = strings.NewReplacer("{file}", shellQuote(abs), "{dir}", shellQuote(filepath.Dir(abs))).Replace(cmdline)
		return runCheck(ctx, filepath.Dir(abs), cmdline, "bash", "-c", cmdline)
	}

	switch ext {
	case ".go":
		if _, err := exec.LookPath("go"); err == nil {
			if modRoot := findUp(filepath.Dir(abs), "go.mod"); modRoot != "" {
				rel, err := filepath.Rel(modRoot, filepath.Dir(abs))
				if err == nil {
					pkg := "./" + filepath.ToSlash(rel)
					return runCheck(ctx, modRoot, "go vet "+pkg, "go", "vet", pkg)
				}
			}
		}
		if _, err := exec.LookPath("gofmt"); err == nil {
			return runCheck(ctx, filepath.Dir(abs), "gofmt -e", "gofmt", "-e", "-l", abs)
		}
	case ".py":
		if _, err := exec.LookPath("python3"); err == nil {
			return runCheck(ctx, filepath.Dir(abs), "python3 -m py_compile", "python3", "-m", "py_compile", abs)
		}
	case ".sh", ".bash":
		if _, err := exec.LookPath("bash"); err == nil {
			return runCheck(ctx, filepath.Dir(abs), "bash -n", "bash", "-n", abs)
		}
	case ".js", ".mjs", ".cjs":
		if _, err := exec.LookPath("node"); err == nil {
			return runCheck(ctx, filepath.Dir(abs), "node --check", "node", "--check", abs)
		}
	case ".json":
		data, err := os.ReadFile(abs)
		if err != nil {
			return ""
		}
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			return "Post-edit check (JSON) FAILED: " + err.Error()
		}
		return "Post-edit check (JSON): OK"
	}
	return ""
}

func runCheck(ctx context.Context, dir, label, name string, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, postEditCheckTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if len(text) > maxCheckOutput {
		text = TruncateUTF8(text, maxCheckOutput) + "\n…(truncated)"
	}
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf("Post-edit check (%s) timed out after %s", label, postEditCheckTimeout)
	}
	if err != nil {
		return fmt.Sprintf("Post-edit check (%s) FAILED:\n%s", label, text)
	}
	if label == "gofmt -e" && text != "" {
		// gofmt -l prints the file name when it isn't formatted.
		return "Post-edit check (gofmt): OK, but the file is not gofmt-formatted"
	}
	return fmt.Sprintf("Post-edit check (%s): OK", label)
}

// findUp returns the nearest ancestor of dir (inclusive) containing name.
func findUp(dir, name string) string {
	for {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
