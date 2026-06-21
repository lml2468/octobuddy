// Package skills manages a bot's own Claude skill bundles. Each skill is a
// directory with a SKILL.md plus supporting files; they live under the bot's
// CLAUDE_CONFIG_DIR (~/.xclaw/<id>/.claude/skills), so the agent's claude CLI
// auto-discovers them as user-scope assets every spawn — no per-turn sandbox
// linking. There is no shared marketplace anymore: every bot owns its own
// skills, period.
//
// Backs the desktop Skills window: list/create/edit/delete per-bot skills with
// slug + path-traversal validation.
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lml2468/xclaw/desktop/internal/safepath"
)

// botDir is ~/.xclaw/<botID>/.claude/skills — the bot's skills dir, sitting
// inside CLAUDE_CONFIG_DIR so the claude CLI loads it as user-scope on launch.
func botDir(botID string) (string, error) {
	if !safepath.ValidSlug(botID) {
		return "", fmt.Errorf("invalid bot id %q — letters, digits, . _ - only", botID)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".xclaw", botID, ".claude", "skills"), nil
}

// skillDirIn resolves and validates a skill's directory inside a given root.
func skillDirIn(root, name string) (string, error) {
	if !safepath.ValidSlug(name) {
		return "", fmt.Errorf("invalid skill name %q — letters, digits, . _ - only", name)
	}
	return filepath.Join(root, name), nil
}

// resolveInSkill validates that rel is a clean relative path inside the skill
// dir (under root) and returns the absolute path. Rejects empty, absolute, and
// any ".." segment outright (lexical), plus a real-path symlink-escape check so
// an intermediate symlinked component can't redirect a write outside the bundle.
func resolveInSkill(root, name, rel string) (string, error) {
	dir, err := skillDirIn(root, name)
	if err != nil {
		return "", err
	}
	full, err := safepath.ResolveLexical(dir, rel)
	if err != nil {
		return "", err
	}
	// dirOnly: the file itself may not exist yet (a create), so check the parent
	// chain in real-path space.
	if err := safepath.AssertNoSymlinkEscape(dir, full, true); err != nil {
		return "", err
	}
	return full, nil
}

// SkillInfo summarizes a per-bot skill for the list view.
type SkillInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Files       int    `json:"files"`
}

// listIn returns every skill bundle directly under root.
func listIn(root string) ([]SkillInfo, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return []SkillInfo{}, nil
		}
		return nil, err
	}
	out := []SkillInfo{}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		info, statErr := os.Stat(filepath.Join(root, name))
		if statErr != nil || !info.IsDir() {
			continue // stray file or unreadable — not a skill
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
func descriptionIn(root, name string) string {
	dir, err := skillDirIn(root, name)
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
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
func filesIn(root, name string) ([]string, error) {
	dir, err := skillDirIn(root, name)
	if err != nil {
		return nil, err
	}
	var out []string
	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

func readFileIn(root, name, rel string) (string, error) {
	full, err := resolveInSkill(root, name, rel)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func writeFileIn(root, name, rel, content string) error {
	full, err := resolveInSkill(root, name, rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, []byte(content), 0o644)
}

func deleteFileIn(root, name, rel string) error {
	full, err := resolveInSkill(root, name, rel)
	if err != nil {
		return err
	}
	return os.Remove(full)
}

func createIn(root, name string) error {
	dir, err := skillDirIn(root, name)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("skill %q already exists", name)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmpl := fmt.Sprintf("---\nname: %s\ndescription: One line on when the agent should use this skill.\n---\n\n# %s\n\nDescribe what this skill does and how to use it.\n", name, name)
	return os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(tmpl), 0o644)
}

// ---- Per-bot skills (~/.xclaw/<id>/.claude/skills) ----

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
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	return createIn(root, name)
}

// BotDelete removes one of the bot's skill bundles entirely.
func BotDelete(botID, name string) error {
	root, err := botDir(botID)
	if err != nil {
		return err
	}
	dir, err := skillDirIn(root, name)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}
