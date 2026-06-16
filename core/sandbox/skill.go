package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SkillSource is one directory of operator assets to link, with an optional
// allow-list. Allow == nil links every asset found; a non-nil Allow (even empty)
// links only the named ones — used to scope the global catalog to a bot's
// selection. Reused for both skills (dirs) and workflows (.js files).
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
// direct child directory of a source is one skill; a source's Allow list (when
// non-nil) restricts which are linked.
//
// Best-effort: errors are logged and skipped, never returned — a missing skill
// only degrades capability; it must not break the turn.
func LinkSkillsIntoSandbox(sandboxDir string, sources []SkillSource) error {
	return applyLinks(filepath.Join(sandboxDir, ".claude", "skills"), "skill",
		collect(sources, qualifySkill))
}

// LinkWorkflowsIntoSandbox is the workflow analogue: it links operator workflow
// scripts into <sandboxDir>/.claude/workflows/<name>.js, so the agent's Workflow
// tool resolves them by name. Each direct-child `*.js` file of a source is one
// workflow; the allow-list keys on the name WITHOUT the .js extension.
func LinkWorkflowsIntoSandbox(sandboxDir string, sources []SkillSource) error {
	return applyLinks(filepath.Join(sandboxDir, ".claude", "workflows"), "workflow",
		collect(sources, qualifyWorkflow))
}

// qualifySkill reports the link filename + allow-list key for a skill entry
// (a directory or a symlink to one), or ok=false.
func qualifySkill(e os.DirEntry) (link, key string, ok bool) {
	n := e.Name()
	if strings.HasPrefix(n, ".") {
		return "", "", false
	}
	if e.IsDir() || e.Type()&os.ModeSymlink != 0 {
		return n, n, true
	}
	return "", "", false
}

// qualifyWorkflow reports the link filename (<name>.js) + allow-list key (<name>)
// for a workflow entry (a *.js file), or ok=false.
func qualifyWorkflow(e os.DirEntry) (link, key string, ok bool) {
	n := e.Name()
	if strings.HasPrefix(n, ".") || e.IsDir() || !strings.HasSuffix(n, ".js") {
		return "", "", false
	}
	return n, strings.TrimSuffix(n, ".js"), true
}

// collect builds linkName → absolute source path for the entries of each source
// that qualify and pass the source's allow-list. Later sources overwrite earlier
// ones (per-bot shadows global).
func collect(sources []SkillSource, qualify func(os.DirEntry) (string, string, bool)) map[string]string {
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
			link, key, ok := qualify(e)
			if !ok {
				continue
			}
			if allow != nil && !allow[key] {
				continue // not in this source's allow-list
			}
			desired[link] = filepath.Join(src.Dir, e.Name())
		}
	}
	return desired
}

// applyLinks reconciles <root> to exactly the desired symlinks: it creates the
// root, prunes managed (symlink) entries no longer wanted, and creates/repairs
// the rest. It only ever touches symlinks it created — real files/dirs (the
// agent's own) are left untouched. kind is used only for log messages.
func applyLinks(root, kind string, desired map[string]string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "[sandbox] %ss mkdir failed for %s: %v\n", kind, root, err)
		return nil // best-effort
	}

	// Prune managed symlinks no longer wanted (or pointing at a changed/dangling target).
	if existing, err := os.ReadDir(root); err == nil {
		for _, e := range existing {
			linkPath := filepath.Join(root, e.Name())
			info, err := os.Lstat(linkPath)
			if err != nil || info.Mode()&os.ModeSymlink == 0 {
				continue // not a symlink → not ours, never delete
			}
			target, err := os.Readlink(linkPath)
			want, wanted := desired[e.Name()]
			if !wanted || err != nil || target != want {
				_ = os.Remove(linkPath)
				continue
			}
			if _, statErr := os.Stat(linkPath); statErr != nil {
				_ = os.Remove(linkPath) // dangling
			}
		}
	}

	// Create / repair desired links.
	for name, target := range desired {
		linkPath := filepath.Join(root, name)
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
			fmt.Fprintf(os.Stderr, "[sandbox] %s symlink failed for %s: %v\n", kind, linkPath, err)
		}
	}
	return nil
}
