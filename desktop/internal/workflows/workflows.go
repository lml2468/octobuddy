// Package workflows manages two layers of workflow scripts. The global catalog
// at ~/.xclaw/workflows is a READ-ONLY marketplace (each workflow a single .js
// script: an `export const meta = {…}` header plus a body using
// agent()/parallel()/pipeline()). A bot uses a workflow only after it is
// INSTALLED into the bot's own dir ~/.xclaw/<id>/workflows — the install is a
// symlink to the catalog file — and a bot may also author its own real workflow
// scripts there. The daemon links ONLY ~/.xclaw/<id>/workflows into the session
// sandbox's .claude/workflows/, so a marketplace workflow reaches the agent
// solely via the per-bot symlink. Backs the desktop Workflows windows.
package workflows

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/lml2468/xclaw/desktop/internal/safepath"
)

// Dir is ~/.xclaw/workflows (the global read-only marketplace catalog).
func Dir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".xclaw", "workflows")
}

// botDir is ~/.xclaw/<botID>/workflows — the bot's own workflows dir, the single
// source the daemon links into the session sandbox.
func botDir(botID string) (string, error) {
	if !safepath.ValidSlug(botID) {
		return "", fmt.Errorf("invalid bot id %q — letters, digits, . _ - only", botID)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".xclaw", botID, "workflows"), nil
}

// pathIn resolves and validates a workflow's .js file inside a given root. The
// name is a single slug (no separators); the path is symlink-checked so a
// symlinked component can't redirect a write outside root.
func pathIn(root, name string) (string, error) {
	if !safepath.ValidSlug(name) {
		return "", fmt.Errorf("invalid workflow name %q — letters, digits, . _ - only", name)
	}
	full := filepath.Join(root, name+".js")
	// dirOnly: the parent chain is checked; the .js itself may be a symlink we
	// intentionally created (an installed workflow), so don't reject that.
	if err := safepath.AssertNoSymlinkEscape(root, full, true); err != nil {
		return "", err
	}
	return full, nil
}

// Info summarizes a workflow for the list view. Installed marks a per-bot entry
// that is a symlink into the marketplace catalog (vs. a real per-bot script).
type Info struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Installed   bool   `json:"installed"`
}

var descRe = regexp.MustCompile(`description\s*:\s*["']([^"']+)["']`)

// listIn returns every workflow (*.js) directly under root, including symlinks.
func listIn(root string) ([]Info, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return []Info{}, nil
		}
		return nil, err
	}
	out := []Info{}
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, ".") || !strings.HasSuffix(n, ".js") {
			continue
		}
		// Skip directories (only files / symlinks-to-files are workflows).
		if e.IsDir() {
			continue
		}
		name := strings.TrimSuffix(n, ".js")
		out = append(out, Info{
			Name:        name,
			Description: descriptionIn(root, name),
			Installed:   e.Type()&os.ModeSymlink != 0,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func descriptionIn(root, name string) string {
	p, err := pathIn(root, name)
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(p) // follows a symlinked install
	if err != nil {
		return ""
	}
	if m := descRe.FindSubmatch(b); m != nil {
		return strings.TrimSpace(string(m[1]))
	}
	return ""
}

func readIn(root, name string) (string, error) {
	p, err := pathIn(root, name)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func writeIn(root, name, content string) error {
	p, err := pathIn(root, name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), 0o644)
}

func createIn(root, name string) error {
	p, err := pathIn(root, name)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(p); err == nil {
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
	return writeIn(root, name, tmpl)
}

// isInstalled reports whether <root>/<name>.js is a symlink (an installed catalog
// workflow) rather than a real per-bot script.
func isInstalled(root, name string) (bool, error) {
	p, err := pathIn(root, name)
	if err != nil {
		return false, err
	}
	info, err := os.Lstat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return info.Mode()&os.ModeSymlink != 0, nil
}

// ---- Global marketplace catalog (~/.xclaw/workflows) ----

// List returns every workflow in the global catalog.
func List() ([]Info, error) { return listIn(Dir()) }

// Read returns a catalog workflow's script source.
func Read(name string) (string, error) { return readIn(Dir(), name) }

// Write creates or overwrites a catalog workflow's script.
func Write(name, content string) error { return writeIn(Dir(), name, content) }

// Create scaffolds a new catalog workflow with a starter script.
func Create(name string) error { return createIn(Dir(), name) }

// Delete removes a catalog workflow script.
func Delete(name string) error {
	p, err := pathIn(Dir(), name)
	if err != nil {
		return err
	}
	return os.Remove(p)
}

// ---- Per-bot workflows (~/.xclaw/<id>/workflows) ----

// BotList returns the bot's own + installed workflows (Installed flags symlinks).
func BotList(botID string) ([]Info, error) {
	root, err := botDir(botID)
	if err != nil {
		return nil, err
	}
	return listIn(root)
}

// BotRead reads one of the bot's workflow scripts (own or installed).
func BotRead(botID, name string) (string, error) {
	root, err := botDir(botID)
	if err != nil {
		return "", err
	}
	return readIn(root, name)
}

// BotWrite writes one of the bot's OWN workflow scripts. Refuses to write into an
// installed (symlinked) workflow — edit it in the catalog instead.
func BotWrite(botID, name, content string) error {
	root, err := botDir(botID)
	if err != nil {
		return err
	}
	if linked, err := isInstalled(root, name); err != nil {
		return err
	} else if linked {
		return fmt.Errorf("workflow %q is installed from the catalog (read-only); edit it in the marketplace", name)
	}
	return writeIn(root, name, content)
}

// BotCreate scaffolds a new per-bot OWN workflow script.
func BotCreate(botID, name string) error {
	root, err := botDir(botID)
	if err != nil {
		return err
	}
	return createIn(root, name)
}

// BotDelete removes one of the bot's OWN workflow scripts. Use Uninstall for an
// installed (symlinked) catalog workflow.
func BotDelete(botID, name string) error {
	root, err := botDir(botID)
	if err != nil {
		return err
	}
	if linked, err := isInstalled(root, name); err != nil {
		return err
	} else if linked {
		return fmt.Errorf("workflow %q is installed from the catalog — uninstall it instead", name)
	}
	p, err := pathIn(root, name)
	if err != nil {
		return err
	}
	return os.Remove(p)
}

// Install symlinks a catalog workflow into the bot's dir. Idempotent; refuses to
// overwrite a real (own) script of the same name.
func Install(botID, name string) error {
	src, err := pathIn(Dir(), name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("workflow %q not found in catalog", name)
	}
	root, err := botDir(botID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(root, name+".js")
	if info, err := os.Lstat(dst); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			if cur, _ := os.Readlink(dst); cur == src {
				return nil // already installed
			}
			_ = os.Remove(dst)
		} else {
			return fmt.Errorf("a per-bot workflow named %q already exists", name)
		}
	}
	return os.Symlink(src, dst)
}

// Uninstall removes an installed (symlinked) catalog workflow from the bot's dir.
// It only ever removes a symlink, so a real per-bot script is never deleted.
func Uninstall(botID, name string) error {
	root, err := botDir(botID)
	if err != nil {
		return err
	}
	p, err := pathIn(root, name)
	if err != nil {
		return err
	}
	info, err := os.Lstat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("workflow %q is a per-bot script, not an installed workflow — delete it instead", name)
	}
	return os.Remove(p)
}
