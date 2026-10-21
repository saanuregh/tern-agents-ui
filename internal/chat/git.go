package chat

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// branchTTL is how long a branch answer is reused; the view is drawn many
// times a second and the branch rarely changes.
const branchTTL = 5 * time.Second

// branchLocked is the git branch of the chat's folder, or a short commit
// when HEAD is detached; "" outside a repository.
func (a *app) branchLocked() string {
	if time.Since(a.branchAt) < branchTTL {
		return a.branch
	}
	a.branch, a.branchAt = gitBranch(a.cwd), time.Now()
	return a.branch
}

// gitBranch reads HEAD of the repository holding dir, following a
// worktree's or submodule's "gitdir:" file.
func gitBranch(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		dot := filepath.Join(d, ".git")
		if st, err := os.Stat(dot); err == nil {
			gitDir := dot
			if !st.IsDir() {
				raw, err := os.ReadFile(dot)
				target, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir:")
				if err != nil || !ok {
					return ""
				}
				if gitDir = strings.TrimSpace(target); !filepath.IsAbs(gitDir) {
					gitDir = filepath.Join(d, gitDir)
				}
			}
			head, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
			if err != nil {
				return ""
			}
			h := strings.TrimSpace(string(head))
			if ref, ok := strings.CutPrefix(h, "ref: refs/heads/"); ok {
				return ref
			}
			return h[:min(len(h), 7)]
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}
