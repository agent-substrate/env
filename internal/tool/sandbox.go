package tool

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Sandbox confines filesystem paths a tool touches to a single root directory.
type Sandbox struct {
	root string
}

// NewSandbox roots a sandbox at dir, which must exist and be a directory.
func NewSandbox(dir string) (*Sandbox, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve root %q: %w", dir, err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("stat root %q: %w", abs, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("root %q is not a directory", abs)
	}
	return &Sandbox{root: abs}, nil
}

// Root returns the absolute sandbox root.
func (s *Sandbox) Root() string { return s.root }

// Resolve turns a model-supplied path into an absolute path inside the sandbox.
func (s *Sandbox) Resolve(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return s.root, nil
	}
	if strings.HasPrefix(p, "~") {
		return "", fmt.Errorf("path %q: ~ is not expanded; use a path relative to the workspace root", p)
	}
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(s.root, abs)
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}

	if !s.contains(abs) {
		return "", s.escapeErr(p)
	}
	probe := abs
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			suffix := strings.TrimPrefix(abs, probe)
			if !s.contains(filepath.Clean(resolved + suffix)) {
				return "", s.escapeErr(p)
			}
			return abs, nil
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return abs, nil
		}
		probe = parent
	}
}

// Rel renders an absolute path relative to the root.
func (s *Sandbox) Rel(abs string) string {
	rel, err := filepath.Rel(s.root, abs)
	if err != nil {
		return abs
	}
	if rel == "." {
		return "."
	}
	return rel
}

func (s *Sandbox) contains(abs string) bool {
	if abs == s.root {
		return true
	}
	return strings.HasPrefix(abs, s.root+string(filepath.Separator))
}

func (s *Sandbox) escapeErr(p string) error {
	return fmt.Errorf("path %q is outside the workspace root %s", p, s.root)
}
