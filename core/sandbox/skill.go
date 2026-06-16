package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SkillSource is one directory of operator skills to link, with an optional
// allow-list. Allow == nil links every skill dir found; a non-nil Allow (even
// empty) links only the named skills — used to scope the global catalog to a
// bot's selected skills.
type SkillSource struct {
	Dir   string
	Allow []string
}

// LinkSkillsIntoSandbox symlinks operator-owned skill directories into a
// session's sandbox at <sandboxDir>/.claude/skills/<name>, so the agent CLI
// (which discovers project-scope skills under the cwd) finds them. Ported from
// cc-channel-octo's skill-linker.ts.
//
// sources is in ascending precedence — [globalSkillsDir, perBotSkillsDir] — so a
// per-bot skill shadows a global one of the same name (later source wins). Each
// direct child directory of a source is one skill, linked individually; a
// source's Allow list (when non-nil) restricts which of its skills are linked.
//
// Best-effort: every error is logged and skipped, never returned (the error in
// the signature is reserved for a future stricter mode; today it is always nil).
// A missing skill only degrades capability; it must not break the turn.
func LinkSkillsIntoSandbox(sandboxDir string, sources []SkillSource) error {
	skillsRoot := filepath.Join(sandboxDir, ".claude", "skills")

	// Collect desired links: skillName → absolute source path. Later sources
	// overwrite earlier ones (per-bot shadows global).
	desired := map[string]string{}
	for _, src := range sources {
		if src.Dir == "" {
			continue
		}
		var allow map[string]bool
		if src.Allow != nil {
			allow = make(map[string]bool, len(src.Allow))
			for _, n := range src.Allow {
				allow[n] = true
			}
		}
		entries, err := os.ReadDir(src.Dir)
		if err != nil {
			continue // missing / unreadable source — skip silently
		}
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") {
				continue // skip dotfiles
			}
			if allow != nil && !allow[name] {
				continue // not in this source's allow-list
			}
			full := filepath.Join(src.Dir, name)
			info, err := os.Lstat(full)
			if err != nil {
				continue
			}
			// A skill is a directory (or a symlink to one).
			if info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				desired[name] = full
			}
		}
	}

	if err := os.MkdirAll(skillsRoot, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "[sandbox] skills mkdir failed for %s: %v\n", skillsRoot, err)
		return nil // best-effort
	}

	// Prune managed symlinks that are no longer wanted. Only ever touch symlinks
	// we created — real files/dirs (the agent's own) are left untouched.
	if existing, err := os.ReadDir(skillsRoot); err == nil {
		for _, e := range existing {
			linkPath := filepath.Join(skillsRoot, e.Name())
			info, err := os.Lstat(linkPath)
			if err != nil || info.Mode()&os.ModeSymlink == 0 {
				continue // not a symlink → not ours, never delete
			}
			target, err := os.Readlink(linkPath)
			want, wanted := desired[e.Name()]
			// Remove if: no longer wanted, target changed, or dangling
			// (os.Stat follows the link → error means the target is gone).
			if !wanted || err != nil || target != want {
				_ = os.Remove(linkPath)
				continue
			}
			if _, statErr := os.Stat(linkPath); statErr != nil {
				_ = os.Remove(linkPath)
			}
		}
	}

	// Create / repair desired links.
	for name, target := range desired {
		linkPath := filepath.Join(skillsRoot, name)
		info, err := os.Lstat(linkPath)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				if cur, _ := os.Readlink(linkPath); cur == target {
					continue // already correct
				}
				_ = os.Remove(linkPath) // wrong target → replace
			} else {
				continue // a real file/dir occupies the name → respect the agent's own
			}
		}
		if err := os.Symlink(target, linkPath); err != nil {
			fmt.Fprintf(os.Stderr, "[sandbox] skill symlink failed for %s: %v\n", linkPath, err)
		}
	}

	return nil
}
