// Package skills manages a bot's own Claude skill bundles. Each skill is a
// directory with a SKILL.md plus supporting files; they live under the bot's
// CLAUDE_CONFIG_DIR (~/.octobuddy/<id>/.claude/skills), so the agent's claude CLI
// auto-discovers them as user-scope assets every spawn — no per-turn sandbox
// linking. There is no shared marketplace anymore: every bot owns its own
// skills, period.
//
// Backs the desktop Skills window: list/create/edit/delete per-bot skills with
// slug + path-traversal validation. All file ops go through safepath, which
// owns the symlink-refusal + structural-containment invariants — this file
// has no Lstat / EvalSymlinks / O_NOFOLLOW concerns of its own.
package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lml2468/octobuddy/core/safepath"
	"github.com/lml2468/octobuddy/desktop/internal/agentdir"
)

// botDir is ~/.octobuddy/<botID>/<agent-config-dir>/skills — the bot's skills
// dir, sitting inside the agent config dir (Claude: .claude) so the agent CLI
// loads it as user-scope on launch. The config-dir segment is the configured
// driver's, resolved via agentdir (defaults to .claude).
func botDir(botID string) (string, error) {
	if !safepath.ValidSlug(botID) {
		return "", fmt.Errorf("invalid bot id %q — letters, digits, . _ - only", botID)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".octobuddy", botID, agentdir.Name(botID), "skills"), nil
}

// bundleRoot composes <root>/<name> as the validated per-bundle root, so the
// user-supplied `rel` flows DIRECTLY into safepath (without sanitization)
// and ResolveLexical correctly rejects absolute /.. paths instead of
// silently re-rooting them under the bundle.
func bundleRoot(root, name string) (string, error) {
	if !safepath.ValidSlug(name) {
		return "", fmt.Errorf("invalid skill name %q — letters, digits, . _ - only", name)
	}
	return filepath.Join(root, name), nil
}

// translateSymlink converts safepath.ErrSymlink into a user-facing message
// scoped to the operation, so the GUI surfaces tampering without leaking
// the (possibly attacker-influenced) symlink target.
func translateSymlink(verb string, rel string, err error) error {
	if errors.Is(err, safepath.ErrSymlink) {
		return fmt.Errorf("refusing to %s symlink: %q", verb, rel)
	}
	return err
}

// SkillInfo summarizes a per-bot skill for the list view.
type SkillInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Files       int    `json:"files"`
}

// listIn returns every skill bundle directly under root. Symlinked entries
// are silently skipped — listing them would let an attacker make a tampered
// link appear as a real bundle.
func listIn(root string) ([]SkillInfo, error) {
	entries, err := safepath.SafeReadDir(root, "")
	if err != nil {
		if os.IsNotExist(err) {
			return []SkillInfo{}, nil
		}
		return nil, err
	}
	out := []SkillInfo{}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || e.Type()&os.ModeSymlink != 0 || !e.IsDir() {
			continue
		}
		files, _ := filesIn(root, name)
		out = append(out, SkillInfo{
			Name:        name,
			Description: descriptionIn(root, name),
			Files:       len(files),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// descriptionIn extracts the `description:` from a skill's SKILL.md frontmatter.
// Returns empty string for any failure (missing file, symlink, parse error) —
// the GUI shows "(no description)" rather than surfacing a per-bundle error.
func descriptionIn(root, name string) string {
	br, err := bundleRoot(root, name)
	if err != nil {
		return ""
	}
	b, err := safepath.SafeRead(br, "SKILL.md", 1<<20) // 1 MiB cap; SKILL.md is tiny
	if err != nil {
		return ""
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return ""
	}
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "---" {
			break
		}
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "description:"); ok {
			return strings.TrimSpace(strings.Trim(strings.TrimSpace(rest), `"'`))
		}
	}
	return ""
}

// filesIn lists the relative paths of every file in a skill bundle (sorted).
// Walks via SafeReadDir recursively so symlinks are skipped at every level.
func filesIn(root, name string) ([]string, error) {
	br, err := bundleRoot(root, name)
	if err != nil {
		return nil, err
	}
	var out []string
	var walk func(rel string) error
	walk = func(rel string) error {
		entries, err := safepath.SafeReadDir(br, rel)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.Type()&os.ModeSymlink != 0 {
				continue
			}
			child := e.Name()
			childRel := child
			if rel != "" {
				childRel = rel + "/" + child
			}
			if e.IsDir() {
				if werr := walk(childRel); werr != nil {
					return werr
				}
				continue
			}
			out = append(out, childRel)
		}
		return nil
	}
	if err := walk(""); err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

func readFileIn(root, name, rel string) (string, error) {
	br, err := bundleRoot(root, name)
	if err != nil {
		return "", err
	}
	b, err := safepath.SafeRead(br, rel, 0)
	if err != nil {
		return "", translateSymlink("read", rel, err)
	}
	return string(b), nil
}

func writeFileIn(root, name, rel, content string) error {
	br, err := bundleRoot(root, name)
	if err != nil {
		return err
	}
	// Ensure parent dir exists (safepath.SafeWrite refuses an absent
	// parent chain). MkdirAll is idempotent and itself symlink-safe.
	parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(rel)))
	if parent != "." && parent != "" {
		if err := safepath.SafeMkdirAll(br, parent, 0o755); err != nil {
			return translateSymlink("write through", rel, err)
		}
	}
	if err := safepath.SafeWrite(br, rel, []byte(content), 0o600); err != nil {
		return translateSymlink("write through", rel, err)
	}
	return nil
}

func deleteFileIn(root, name, rel string) error {
	br, err := bundleRoot(root, name)
	if err != nil {
		return err
	}
	if err := safepath.SafeRemove(br, rel); err != nil {
		return translateSymlink("delete through", rel, err)
	}
	return nil
}

func createIn(root, name string) error {
	if !safepath.ValidSlug(name) {
		return fmt.Errorf("invalid skill name %q", name)
	}
	if safepath.SafeExists(root, name) {
		return fmt.Errorf("skill %q already exists", name)
	}
	if err := safepath.SafeMkdirAll(root, name, 0o755); err != nil {
		return translateSymlink("create through", name, err)
	}
	tmpl := fmt.Sprintf("---\nname: %s\ndescription: One line on when the agent should use this skill.\n---\n\n# %s\n\nDescribe what this skill does and how to use it.\n", name, name)
	if err := safepath.SafeWrite(root, name+"/SKILL.md", []byte(tmpl), 0o644); err != nil {
		return translateSymlink("write through", name+"/SKILL.md", err)
	}
	return nil
}

// ---- Per-bot skills (~/.octobuddy/<id>/.claude/skills) ----

// BotList returns the bot's skill bundles.
func BotList(botID string) ([]SkillInfo, error) {
	root, err := botDir(botID)
	if err != nil {
		return nil, err
	}
	return listIn(root)
}

// BotFiles lists files in one of the bot's skill bundles.
func BotFiles(botID, name string) ([]string, error) {
	root, err := botDir(botID)
	if err != nil {
		return nil, err
	}
	return filesIn(root, name)
}

// BotRead reads a file within one of the bot's skill bundles.
func BotRead(botID, name, rel string) (string, error) {
	root, err := botDir(botID)
	if err != nil {
		return "", err
	}
	return readFileIn(root, name, rel)
}

// BotWrite writes a file within one of the bot's skill bundles.
func BotWrite(botID, name, rel, content string) error {
	root, err := botDir(botID)
	if err != nil {
		return err
	}
	return writeFileIn(root, name, rel, content)
}

// BotDeleteFile removes a file within one of the bot's skill bundles.
func BotDeleteFile(botID, name, rel string) error {
	root, err := botDir(botID)
	if err != nil {
		return err
	}
	return deleteFileIn(root, name, rel)
}

// BotCreate scaffolds a new per-bot skill bundle.
func BotCreate(botID, name string) error {
	root, err := botDir(botID)
	if err != nil {
		return err
	}
	// the prior `os.MkdirAll(root, 0o755)` followed any
	// symlinked intermediate component — an agent that planted
	// `~/.octobuddy/<id>/.claude → /etc` would silently get `/etc/skills`
	// created before the SafeMkdirAll guard inside createIn could refuse.
	// Walk from $HOME via dirfd so every component is O_NOFOLLOW-checked.
	if err := ensureBotSkillsDir(botID); err != nil {
		return err
	}
	return createIn(root, name)
}

// ensureBotSkillsDir creates ~/.octobuddy/<botID>/<agent-config-dir>/skills via
// the dirfd-walk SafeMkdirAll so every intermediate component is symlink-refused.
// Skipping a regular MkdirAll here closes Sec #4.
func ensureBotSkillsDir(botID string) error {
	if !safepath.ValidSlug(botID) {
		return fmt.Errorf("invalid bot id %q", botID)
	}
	home, _ := os.UserHomeDir()
	// `~/` is operator-trusted (operator shell == operator's own dirs);
	// every component below is agent-reachable and gets checked.
	return safepath.SafeMkdirAll(home, ".octobuddy/"+botID+"/"+agentdir.Name(botID)+"/skills", 0o755)
}

// BotDelete removes one of the bot's skill bundles entirely.
func BotDelete(botID, name string) error {
	root, err := botDir(botID)
	if err != nil {
		return err
	}
	if !safepath.ValidSlug(name) {
		return fmt.Errorf("invalid skill name %q", name)
	}
	if err := safepath.SafeRemoveAll(root, name); err != nil {
		return translateSymlink("delete", name, err)
	}
	return nil
}
