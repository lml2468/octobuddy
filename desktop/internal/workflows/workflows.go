// Package workflows manages a bot's own workflow scripts. Each workflow is a
// single .js file (an `export const meta = {…}` header plus a body using
// agent()/parallel()/pipeline()) living under the bot's CLAUDE_CONFIG_DIR
// (~/.xclaw/<id>/.claude/workflows), so the agent's Workflow tool resolves
// them by name on every spawn — no per-turn sandbox linking. There is no
// shared marketplace anymore: every bot owns its own workflows, period.
//
// All file ops go through safepath; this file has no Lstat / EvalSymlinks /
// O_NOFOLLOW concerns of its own.
package workflows

import (
	"errors"
	"fmt"
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

// workflowRel returns "<name>.js" after slug-validating name. The result
// is passed straight to safepath — no per-call symlink concerns here.
func workflowRel(name string) (string, error) {
	if !safepath.ValidSlug(name) {
		return "", fmt.Errorf("invalid workflow name %q — letters, digits, . _ - only", name)
	}
	return name + ".js", nil
}

func translateSymlink(verb, name string, err error) error {
	if errors.Is(err, safepath.ErrSymlink) {
		return fmt.Errorf("refusing to %s symlink: %q", verb, name)
	}
	return err
}

// Info summarizes a per-bot workflow for the list view.
type Info struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

var descRe = regexp.MustCompile(`description\s*:\s*["']([^"']+)["']`)

// listIn returns every workflow (*.js) directly under root.
func listIn(root string) ([]Info, error) {
	entries, err := safepath.SafeReadDir(root, "")
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
	rel, err := workflowRel(name)
	if err != nil {
		return ""
	}
	b, err := safepath.SafeRead(root, rel, 1<<20) // 1 MiB cap; workflow headers are tiny
	if err != nil {
		return ""
	}
	if m := descRe.FindSubmatch(b); m != nil {
		return strings.TrimSpace(string(m[1]))
	}
	return ""
}

func readIn(root, name string) (string, error) {
	rel, err := workflowRel(name)
	if err != nil {
		return "", err
	}
	b, err := safepath.SafeRead(root, rel, 0)
	if err != nil {
		return "", translateSymlink("read", name, err)
	}
	return string(b), nil
}

func writeIn(root, name, content string) error {
	rel, err := workflowRel(name)
	if err != nil {
		return err
	}
	if err := safepath.SafeWrite(root, rel, []byte(content), 0o600); err != nil {
		return translateSymlink("write through", name, err)
	}
	return nil
}

// ensureBotWorkflowsDir creates ~/.xclaw/<botID>/.claude/workflows via the
// dirfd-walk SafeMkdirAll so every intermediate component is symlink-
// refused. Round 19 Sec #4: replaces the prior `os.MkdirAll(root, …)`
// in writeIn that followed any symlinked intermediate component.
func ensureBotWorkflowsDir(botID string) error {
	if !safepath.ValidSlug(botID) {
		return fmt.Errorf("invalid bot id %q", botID)
	}
	home, _ := os.UserHomeDir()
	return safepath.SafeMkdirAll(home, ".xclaw/"+botID+"/.claude/workflows", 0o755)
}

func createIn(root, name string) error {
	rel, err := workflowRel(name)
	if err != nil {
		return err
	}
	if safepath.SafeExists(root, rel) {
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
	if err := ensureBotWorkflowsDir(botID); err != nil {
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
	if err := ensureBotWorkflowsDir(botID); err != nil {
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
	rel, err := workflowRel(name)
	if err != nil {
		return err
	}
	if err := safepath.SafeRemove(root, rel); err != nil {
		return translateSymlink("delete", name, err)
	}
	return nil
}
