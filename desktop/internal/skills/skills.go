// Package skills manages two layers of Claude skill bundles. The global library
// at ~/.xclaw/skills is a READ-ONLY marketplace (each skill a directory with a
// SKILL.md plus supporting files). A bot uses a skill only after it is INSTALLED
// into the bot's own dir ~/.xclaw/<id>/skills — the install is a symlink to the
// catalog entry — and a bot may also author its own real skill bundles there.
// The daemon links ONLY ~/.xclaw/<id>/skills into the session sandbox, so a
// marketplace skill reaches the agent solely via the per-bot symlink.
//
// This package backs the desktop Skills windows: browse/maintain the catalog,
// install/uninstall catalog skills per bot, and author per-bot skills — all with
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

// Dir is ~/.xclaw/skills (the global read-only marketplace catalog).
func Dir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".xclaw", "skills")
}

// botDir is ~/.xclaw/<botID>/skills — the bot's own skills dir, the single source
// the daemon links into the session sandbox.
func botDir(botID string) (string, error) {
	if !safepath.ValidSlug(botID) {
		return "", fmt.Errorf("invalid bot id %q — letters, digits, . _ - only", botID)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".xclaw", botID, "skills"), nil
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

// SkillInfo summarizes a skill for the list view. Installed marks a per-bot entry
// that is a symlink into the marketplace catalog (vs. a real per-bot bundle).
type SkillInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Files       int    `json:"files"`
	Installed   bool   `json:"installed"`
}

// listIn returns every skill bundle directly under root (dirs, including symlinks
// to dirs). markInstalled reports whether an entry is a symlink (installed).
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
		// Accept real dirs and symlinks (resolve symlinks to a dir).
		info, err := os.Stat(filepath.Join(root, name))
		if err != nil || !info.IsDir() {
			continue
		}
		isLink := e.Type()&os.ModeSymlink != 0
		files, _ := filesIn(root, name)
		out = append(out, SkillInfo{
			Name:        name,
			Description: descriptionIn(root, name),
			Files:       len(files),
			Installed:   isLink,
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

// filesIn lists the relative paths of every file in a skill bundle (sorted),
// following the bundle dir (which may itself be a symlink to the catalog).
func filesIn(root, name string) ([]string, error) {
	dir, err := skillDirIn(root, name)
	if err != nil {
		return nil, err
	}
	// Resolve a symlinked bundle to its real dir so WalkDir descends into it.
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	var out []string
	err = filepath.WalkDir(real, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(real, path)
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

// ---- Global marketplace catalog (~/.xclaw/skills) ----

// List returns every skill in the global catalog.
func List() ([]SkillInfo, error) { return listIn(Dir()) }

// Files lists the relative paths of every file in a catalog skill bundle.
func Files(name string) ([]string, error) { return filesIn(Dir(), name) }

// ReadFile returns the contents of a file within a catalog skill bundle.
func ReadFile(name, rel string) (string, error) { return readFileIn(Dir(), name, rel) }

// WriteFile creates or overwrites a file within a catalog skill bundle.
func WriteFile(name, rel, content string) error { return writeFileIn(Dir(), name, rel, content) }

// DeleteFile removes a file within a catalog skill bundle.
func DeleteFile(name, rel string) error { return deleteFileIn(Dir(), name, rel) }

// Create scaffolds a new catalog skill with a starter SKILL.md.
func Create(name string) error { return createIn(Dir(), name) }

// Delete removes a catalog skill bundle entirely.
func Delete(name string) error {
	dir, err := skillDirIn(Dir(), name)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// ---- Per-bot skills (~/.xclaw/<id>/skills) ----

// BotList returns the bot's own + installed skills (Installed flags symlinks).
func BotList(botID string) ([]SkillInfo, error) {
	root, err := botDir(botID)
	if err != nil {
		return nil, err
	}
	return listIn(root)
}

// BotFiles lists files in one of the bot's skill bundles (own or installed).
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

// BotWrite writes a file within one of the bot's OWN skill bundles. Refuses to
// write into an installed (symlinked) bundle — those are read-only marketplace
// content; edit them in the catalog instead.
func BotWrite(botID, name, rel, content string) error {
	root, err := botDir(botID)
	if err != nil {
		return err
	}
	if linked, err := isInstalled(root, name); err != nil {
		return err
	} else if linked {
		return fmt.Errorf("skill %q is installed from the catalog (read-only); edit it in the marketplace", name)
	}
	return writeFileIn(root, name, rel, content)
}

// BotDeleteFile removes a file within one of the bot's OWN skill bundles.
func BotDeleteFile(botID, name, rel string) error {
	root, err := botDir(botID)
	if err != nil {
		return err
	}
	if linked, err := isInstalled(root, name); err != nil {
		return err
	} else if linked {
		return fmt.Errorf("skill %q is installed from the catalog (read-only)", name)
	}
	return deleteFileIn(root, name, rel)
}

// BotCreate scaffolds a new per-bot OWN skill bundle.
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

// BotDelete removes one of the bot's OWN skill bundles. Use Uninstall for an
// installed (symlinked) catalog skill.
func BotDelete(botID, name string) error {
	root, err := botDir(botID)
	if err != nil {
		return err
	}
	if linked, err := isInstalled(root, name); err != nil {
		return err
	} else if linked {
		return fmt.Errorf("skill %q is installed from the catalog — uninstall it instead", name)
	}
	dir, err := skillDirIn(root, name)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// Install symlinks a catalog skill into the bot's dir, making it available to the
// agent on its next turn. Idempotent: a correct existing symlink is left as-is.
// Refuses to overwrite a real (own) bundle of the same name.
func Install(botID, name string) error {
	if !safepath.ValidSlug(name) {
		return fmt.Errorf("invalid skill name %q", name)
	}
	src, err := skillDirIn(Dir(), name)
	if err != nil {
		return err
	}
	if info, err := os.Stat(src); err != nil || !info.IsDir() {
		return fmt.Errorf("skill %q not found in catalog", name)
	}
	root, err := botDir(botID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(root, name)
	if info, err := os.Lstat(dst); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			if cur, _ := os.Readlink(dst); cur == src {
				return nil // already installed, correct target
			}
			_ = os.Remove(dst) // stale symlink → replace
		} else {
			return fmt.Errorf("a per-bot skill named %q already exists", name)
		}
	}
	return os.Symlink(src, dst)
}

// Uninstall removes an installed (symlinked) catalog skill from the bot's dir.
// It only ever removes a symlink, so a real per-bot bundle is never deleted.
func Uninstall(botID, name string) error {
	root, err := botDir(botID)
	if err != nil {
		return err
	}
	dst, err := skillDirIn(root, name)
	if err != nil {
		return err
	}
	info, err := os.Lstat(dst)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // already gone
		}
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("skill %q is a per-bot bundle, not an installed skill — delete it instead", name)
	}
	return os.Remove(dst)
}

// isInstalled reports whether <root>/<name> is a symlink (an installed catalog
// skill) rather than a real per-bot bundle.
func isInstalled(root, name string) (bool, error) {
	dir, err := skillDirIn(root, name)
	if err != nil {
		return false, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return info.Mode()&os.ModeSymlink != 0, nil
}
