// Package workflows manages a bot's own workflow scripts. Each workflow is a
// single .js file (an `export const meta = {…}` header plus a body using
// agent()/parallel()/pipeline()) living under the bot's CLAUDE_CONFIG_DIR
// (~/.xclaw/<id>/.claude/workflows), so the agent's Workflow tool resolves
// them by name on every spawn — no per-turn sandbox linking. There is no
// shared marketplace anymore: every bot owns its own workflows, period.
package workflows

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/lml2468/xclaw/desktop/internal/safepath"
)

// botDir is ~/.xclaw/<botID>/.claude/workflows — inside CLAUDE_CONFIG_DIR so
// the claude CLI loads it as user-scope on launch.
func botDir(botID string) (string, error) {
	if !safepath.ValidSlug(botID) {
		return "", fmt.Errorf("invalid bot id %q — letters, digits, . _ - only", botID)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".xclaw", botID, ".claude", "workflows"), nil
}

// pathIn resolves and validates a workflow's .js file inside a given root. The
// name is a single slug (no separators); the parent chain is symlink-checked so
// an intermediate symlink can't redirect a write outside root.
func pathIn(root, name string) (string, error) {
	if !safepath.ValidSlug(name) {
		return "", fmt.Errorf("invalid workflow name %q — letters, digits, . _ - only", name)
	}
	full := filepath.Join(root, name+".js")
	if err := safepath.AssertNoSymlinkEscape(root, full, true); err != nil {
		return "", err
	}
	return full, nil
}

// Info summarizes a per-bot workflow for the list view.
type Info struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

var descRe = regexp.MustCompile(`description\s*:\s*["']([^"']+)["']`)

// listIn returns every workflow (*.js) directly under root.
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
		if strings.HasPrefix(n, ".") || !strings.HasSuffix(n, ".js") || e.IsDir() {
			continue
		}
		// Round 15 Arch #7 / Sec H3 mirror: refuse symlinks in the workflow
		// listing. A workflow.js → /etc/passwd link would otherwise have
		// its target surfaced via descriptionIn + BotRead. Matches the
		// round-14 skills discipline.
		if e.Type()&os.ModeSymlink != 0 {
			continue
		}
		name := strings.TrimSuffix(n, ".js")
		out = append(out, Info{
			Name:        name,
			Description: descriptionIn(root, name),
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
	f, err := safepath.OpenNoFollow(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, err := io.ReadAll(f)
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
	// Round 16 H2: race-free symlink refusal via O_NOFOLLOW (round 15 used
	// Lstat-before-Read which races vs an agent rename).
	f, err := safepath.OpenNoFollow(p)
	if err != nil {
		if errors.Is(err, safepath.ErrSymlinkLeaf) {
			return "", fmt.Errorf("refusing to read symlink: %q", name)
		}
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
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
	// Round 17 Go #4: AtomicWriteNoFollow combines the round-16 symlink
	// refusal with atomicfile's temp+fsync+rename, so a crash mid-write
	// can't leave a half-written workflow script behind.
	if err := safepath.AtomicWriteNoFollow(p, []byte(content), 0o600); err != nil {
		if errors.Is(err, safepath.ErrSymlinkLeaf) {
			return fmt.Errorf("refusing to write through symlink: %q", name)
		}
		return err
	}
	return nil
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

// ---- Per-bot workflows (~/.xclaw/<id>/.claude/workflows) ----

// BotList returns the bot's workflow scripts.
func BotList(botID string) ([]Info, error) {
	root, err := botDir(botID)
	if err != nil {
		return nil, err
	}
	return listIn(root)
}

// BotRead reads one of the bot's workflow scripts.
func BotRead(botID, name string) (string, error) {
	root, err := botDir(botID)
	if err != nil {
		return "", err
	}
	return readIn(root, name)
}

// BotWrite writes one of the bot's workflow scripts.
func BotWrite(botID, name, content string) error {
	root, err := botDir(botID)
	if err != nil {
		return err
	}
	return writeIn(root, name, content)
}

// BotCreate scaffolds a new per-bot workflow script.
func BotCreate(botID, name string) error {
	root, err := botDir(botID)
	if err != nil {
		return err
	}
	return createIn(root, name)
}

// BotDelete removes one of the bot's workflow scripts.
func BotDelete(botID, name string) error {
	root, err := botDir(botID)
	if err != nil {
		return err
	}
	p, err := pathIn(root, name)
	if err != nil {
		return err
	}
	return os.Remove(p)
}
