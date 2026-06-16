// Package skills manages the install-wide skill library at ~/.xclaw/skills.
// Each skill is a directory (a multi-file bundle) containing a SKILL.md plus any
// supporting files; the daemon links a bot's selected skills into its session
// sandbox so the Claude agent discovers them. This package backs the desktop
// Skills window: list/create/delete skills and read/write/delete files within a
// skill bundle, with slug + path-traversal validation.
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Dir is ~/.xclaw/skills (the global catalog the daemon reads).
func Dir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".xclaw", "skills")
}

var slugRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func validSlug(s string) bool { return s != "" && s != "." && s != ".." && slugRe.MatchString(s) }

// skillDir resolves and validates a skill's directory.
func skillDir(name string) (string, error) {
	if !validSlug(name) {
		return "", fmt.Errorf("invalid skill name %q — letters, digits, . _ - only", name)
	}
	return filepath.Join(Dir(), name), nil
}

// resolveInSkill validates that rel is a clean relative path inside the skill
// dir and returns the absolute path. Rejects empty, absolute, and any ".."
// segment outright (rather than silently rewriting), with a final containment
// check as defense in depth.
func resolveInSkill(name, rel string) (string, error) {
	dir, err := skillDir(name)
	if err != nil {
		return "", err
	}
	rel = filepath.ToSlash(rel)
	if rel == "" {
		return "", fmt.Errorf("empty path")
	}
	if strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("absolute path not allowed: %q", rel)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return "", fmt.Errorf("path escapes skill directory: %q", rel)
		}
	}
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if full != dir && !strings.HasPrefix(full, dir+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes skill directory: %q", rel)
	}
	return full, nil
}

// SkillInfo summarizes a skill for the list view.
type SkillInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Files       int    `json:"files"`
}

// List returns every skill in the catalog (dirs containing a SKILL.md surface
// their description; others still list).
func List() ([]SkillInfo, error) {
	entries, err := os.ReadDir(Dir())
	if err != nil {
		if os.IsNotExist(err) {
			return []SkillInfo{}, nil
		}
		return nil, err
	}
	out := []SkillInfo{}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		files, _ := Files(e.Name())
		out = append(out, SkillInfo{
			Name:        e.Name(),
			Description: descriptionOf(e.Name()),
			Files:       len(files),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// descriptionOf extracts the `description:` from a skill's SKILL.md frontmatter.
func descriptionOf(name string) string {
	dir, err := skillDir(name)
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

// Files lists the relative paths of every file in a skill bundle (sorted).
func Files(name string) ([]string, error) {
	dir, err := skillDir(name)
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

// ReadFile returns the contents of a file within a skill bundle.
func ReadFile(name, rel string) (string, error) {
	full, err := resolveInSkill(name, rel)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// WriteFile creates or overwrites a file within a skill bundle.
func WriteFile(name, rel, content string) error {
	full, err := resolveInSkill(name, rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, []byte(content), 0o644)
}

// DeleteFile removes a file within a skill bundle.
func DeleteFile(name, rel string) error {
	full, err := resolveInSkill(name, rel)
	if err != nil {
		return err
	}
	return os.Remove(full)
}

// Create scaffolds a new skill with a starter SKILL.md.
func Create(name string) error {
	dir, err := skillDir(name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("skill %q already exists", name)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmpl := fmt.Sprintf("---\nname: %s\ndescription: One line on when the agent should use this skill.\n---\n\n# %s\n\nDescribe what this skill does and how to use it.\n", name, name)
	return os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(tmpl), 0o644)
}

// Delete removes a skill bundle entirely.
func Delete(name string) error {
	dir, err := skillDir(name)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}
