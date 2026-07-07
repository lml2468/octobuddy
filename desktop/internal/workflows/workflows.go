// Package workflows manages a bot's own workflow scripts. Each workflow is a
// single.js file (an `export const meta = {…}` header plus a body using
// agent/parallel/pipeline) living under the bot's CLAUDE_CONFIG_DIR
// (~/.octobuddy/<id>/.claude/workflows), so the agent's Workflow tool resolves
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

	"github.com/lml2468/octobuddy/core/safepath"
	"github.com/lml2468/octobuddy/desktop/internal/agentdir"
)

// botDir is ~/.octobuddy/<botID>/<agent-config-dir>/workflows — inside the agent
// config dir (Claude: .claude) so the agent CLI loads it as user-scope on
// launch. The config-dir segment is the configured driver's, via agentdir.
func botDir(botID string) (string, error) {
	if !safepath.ValidSlug(botID) {
		return "", fmt.Errorf("invalid bot id %q — letters, digits, . _ - only", botID)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".octobuddy", botID, agentdir.Name(botID), "workflows"), nil
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

// metaRe finds the `export const meta = { … }` block, scoping descRe's
// search so a stray `description:` in comments or downstream code can't
// shadow the canonical one. Uses a balanced-brace count via a helper
// (not a single regex) because workflows' meta blocks may contain nested
// objects/arrays — a `(.*?)\}` non-greedy match stopped at the first
// inner `}` and truncated the capture before description: was reached
// for any non-trivial meta.
//
// descRe accepts EITHER single or double quotes, with the matching
// closing quote — the character class `[^"']+` excluded BOTH quote chars
// and truncated descriptions like `"It's a workflow"` at the apostrophe.
var (
	metaRe = regexp.MustCompile(`(?s)export\s+const\s+meta\s*=\s*\{`)
	descRe = regexp.MustCompile(`description\s*:\s*(?:"([^"]+)"|'([^']+)')`)
)

// extractMetaBlock returns the body inside the meta = { … } braces with
// brace-counting (not greedy regex), so nested {...}/[…] survive intact.
// Returns "" if no balanced match is found.
func extractMetaBlock(b []byte) string {
	loc := metaRe.FindIndex(b)
	if loc == nil {
		return ""
	}
	depth := 1
	for i := loc[1]; i < len(b); i++ {
		switch b[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return string(b[loc[1]:i])
			}
		}
	}
	return ""
}

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
	if block := extractMetaBlock(b); block != "" {
		if m := descRe.FindStringSubmatch(block); m != nil {
			// Either group 1 (double-quoted) or group 2 (single-quoted) matched.
			val := m[1]
			if val == "" {
				val = m[2]
			}
			return strings.TrimSpace(val)
		}
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

// ensureBotWorkflowsDir creates ~/.octobuddy/<botID>/<agent-config-dir>/workflows
// via the dirfd-walk SafeMkdirAll so every intermediate component is symlink-
// refused. replaces the prior `os.MkdirAll(root, …)`
// in writeIn that followed any symlinked intermediate component.
func ensureBotWorkflowsDir(botID string) error {
	if !safepath.ValidSlug(botID) {
		return fmt.Errorf("invalid bot id %q", botID)
	}
	home, _ := os.UserHomeDir()
	return safepath.SafeMkdirAll(home, ".octobuddy/"+botID+"/"+agentdir.Name(botID)+"/workflows", 0o755)
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

// ---- Per-bot workflows (~/.octobuddy/<id>/.claude/workflows) ----

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
