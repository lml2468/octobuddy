// Package workflows manages the install-wide workflow catalog at
// ~/.xclaw/workflows. Each workflow is a single .js script (an `export const
// meta = {…}` header plus a body using agent()/parallel()/pipeline()); the
// daemon links a bot's selected workflows into its session sandbox's
// .claude/workflows/ so the agent's Workflow tool resolves them by name. This
// package backs the desktop "Manage Workflows" panel.
package workflows

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Dir is ~/.xclaw/workflows (the global catalog the daemon reads).
func Dir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".xclaw", "workflows")
}

var slugRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func validSlug(s string) bool { return s != "" && s != "." && s != ".." && slugRe.MatchString(s) }

// path resolves and validates a workflow's .js file.
func path(name string) (string, error) {
	if !validSlug(name) {
		return "", fmt.Errorf("invalid workflow name %q — letters, digits, . _ - only", name)
	}
	return filepath.Join(Dir(), name+".js"), nil
}

// Info summarizes a workflow for the list view.
type Info struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

var descRe = regexp.MustCompile(`description\s*:\s*["']([^"']+)["']`)

// List returns every workflow (*.js) in the catalog, with the description pulled
// from the script's meta block.
func List() ([]Info, error) {
	entries, err := os.ReadDir(Dir())
	if err != nil {
		if os.IsNotExist(err) {
			return []Info{}, nil
		}
		return nil, err
	}
	out := []Info{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || strings.HasPrefix(n, ".") || !strings.HasSuffix(n, ".js") {
			continue
		}
		name := strings.TrimSuffix(n, ".js")
		out = append(out, Info{Name: name, Description: descriptionOf(name)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func descriptionOf(name string) string {
	p, err := path(name)
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	if m := descRe.FindSubmatch(b); m != nil {
		return strings.TrimSpace(string(m[1]))
	}
	return ""
}

// Read returns a workflow's script source.
func Read(name string) (string, error) {
	p, err := path(name)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Write creates or overwrites a workflow's script.
func Write(name, content string) error {
	p, err := path(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), 0o644)
}

// Create scaffolds a new workflow with a starter script.
func Create(name string) error {
	p, err := path(name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(p); err == nil {
		return fmt.Errorf("workflow %q already exists", name)
	}
	tmpl := fmt.Sprintf(`export const meta = {
  name: %q,
  description: 'One line on what this workflow does and when to run it.',
  phases: [{ title: 'Run' }],
}

phase('Run')
// const out = await agent('do something', { schema: { type: 'object' } })
return { ok: true }
`, name)
	return Write(name, tmpl)
}

// Delete removes a workflow script.
func Delete(name string) error {
	p, err := path(name)
	if err != nil {
		return err
	}
	return os.Remove(p)
}
