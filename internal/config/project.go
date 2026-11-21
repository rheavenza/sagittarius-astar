package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ProjectConfigFile is the per-project config, looked up in the working
// directory and then the git root. Like instruction files, a repository can
// ship it, so it is only used after the user approves its content.
//
//	{
//	  "checks": {
//	    "build": "go build ./...",
//	    "test":  "go test ./..."
//	  },
//	  "checks_timeout_seconds": 600
//	}
var ProjectConfigFile = filepath.Join(".aig", "config.json")

// Check is one named project check (a shell command). Checks run from the
// project root, in the order they appear in the file.
type Check struct {
	Name    string
	Command string
}

// ProjectConfig is the content of .aig/config.json.
type ProjectConfig struct {
	// Path is the file it was loaded from; Root is the project root (the
	// directory containing .aig/), where checks run.
	Path string `json:"-"`
	Root string `json:"-"`

	Checks Checks `json:"checks,omitempty"`
	// ChecksTimeoutSeconds bounds each check (default 300, max 1800).
	ChecksTimeoutSeconds int `json:"checks_timeout_seconds,omitempty"`
}

// Checks keeps the declaration order of the "checks" JSON object, so
// "build" can run before "test".
type Checks []Check

// UnmarshalJSON decodes {"name": "command", …} preserving key order.
func (c *Checks) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf(`"checks" must be an object of name → command`)
	}
	var out Checks
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		name := tok.(string) // object keys are always strings
		var cmd string
		if err := dec.Decode(&cmd); err != nil {
			return fmt.Errorf("check %q: command must be a string", name)
		}
		name, cmd = strings.TrimSpace(name), strings.TrimSpace(cmd)
		if name == "" || cmd == "" {
			return fmt.Errorf("check %q: name and command must not be empty", name)
		}
		if seen[name] {
			return fmt.Errorf("check %q is defined twice", name)
		}
		seen[name] = true
		out = append(out, Check{Name: name, Command: cmd})
	}
	*c = out
	return nil
}

// MarshalJSON writes the checks back as an ordered object.
func (c Checks) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, ch := range c {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(ch.Name)
		v, _ := json.Marshal(ch.Command)
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// FindProjectConfig returns the path of the nearest .aig/config.json: in cwd
// first, then the git root. "" if there is none.
func FindProjectConfig(cwd, gitRoot string) string {
	for _, dir := range []string{cwd, gitRoot} {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, ProjectConfigFile)
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			if abs, err := filepath.Abs(p); err == nil {
				return abs
			}
			return p
		}
	}
	return ""
}

// LoadProjectConfig parses a project config file.
func LoadProjectConfig(path string) (*ProjectConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pc := &ProjectConfig{}
	if err := json.Unmarshal(data, pc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	pc.Path = abs
	pc.Root = filepath.Dir(filepath.Dir(abs)) // <root>/.aig/config.json
	return pc, nil
}
