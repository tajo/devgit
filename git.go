package main

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// change describes a single file delta in the working tree relative to HEAD.
type change struct {
	// path is the repo-root-relative POSIX path.
	path string
	// deleted is true if the file no longer exists in the working tree.
	deleted bool
	// mode is the git file mode, e.g. "100644" or "100755". Ignored when deleted.
	mode string
}

// repoRoot returns the absolute path of the enclosing git repo.
func repoRoot() (string, error) {
	out, err := runGit("", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("not inside a git repository: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// headSHA returns the current HEAD commit SHA.
func headSHA(root string) (string, error) {
	out, err := runGit(root, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("read HEAD: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// currentBranch returns the short name of the currently checked-out branch
// (e.g. "feature/login"). Errors if HEAD is detached, since there is no
// meaningful branch to push to in that state.
func currentBranch(root string) (string, error) {
	out, err := runGit(root, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("read current branch: %w", err)
	}
	b := strings.TrimSpace(out)
	if b == "HEAD" || b == "" {
		return "", errors.New("HEAD is detached; checkout a branch first")
	}
	return b, nil
}

// localGitAuthor reads `user.name` and `user.email` from git config (which
// merges system / global / repo-local levels). Returns nil if either is
// unset, so callers can fall back cleanly to the App's identity.
func localGitAuthor(root string) *commitAuthor {
	name, errN := runGit(root, "config", "user.name")
	email, errE := runGit(root, "config", "user.email")
	if errN != nil || errE != nil {
		return nil
	}
	n := strings.TrimSpace(name)
	e := strings.TrimSpace(email)
	if n == "" || e == "" {
		return nil
	}
	return &commitAuthor{Name: n, Email: e}
}

// commitAuthor holds the human identity to stamp onto created commits.
// Committer is intentionally separate (and left default) so GitHub still
// signs the commit as the App, which is what produces the Verified badge.
type commitAuthor struct {
	Name  string
	Email string
}

// originOwnerRepo parses the `origin` remote URL and returns (owner, repo).
// Supports both ssh (git@github.com:owner/repo.git) and https
// (https://github.com/owner/repo.git) forms.
func originOwnerRepo(root string) (string, string, error) {
	out, err := runGit(root, "remote", "get-url", "origin")
	if err != nil {
		return "", "", fmt.Errorf("read origin remote: %w", err)
	}
	raw := strings.TrimSpace(out)

	var path string
	switch {
	case strings.HasPrefix(raw, "git@"):
		// git@github.com:owner/repo.git
		_, after, ok := strings.Cut(raw, ":")
		if !ok {
			return "", "", fmt.Errorf("unrecognized ssh remote %q", raw)
		}
		path = after
	case strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://"):
		u, err := url.Parse(raw)
		if err != nil {
			return "", "", fmt.Errorf("parse origin URL: %w", err)
		}
		path = strings.TrimPrefix(u.Path, "/")
	default:
		return "", "", fmt.Errorf("unsupported remote URL %q", raw)
	}

	path = strings.TrimSuffix(path, ".git")
	owner, repo, ok := strings.Cut(path, "/")
	if !ok || owner == "" || repo == "" {
		return "", "", fmt.Errorf("unrecognized github path %q in remote", path)
	}
	return owner, repo, nil
}

// collectChanges returns every file that differs from HEAD: staged,
// unstaged, and untracked (but not ignored). It ignores submodules.
func collectChanges(root string) ([]change, error) {
	// -z: NUL-terminated records; the only safe format for arbitrary paths.
	// --untracked-files=all: include each untracked file, not just the dir.
	// --no-renames: treat rename as delete+add so paths line up with HEAD blobs.
	out, err := runGit(root, "status", "--porcelain=v1", "-z",
		"--untracked-files=all", "--no-renames", "--ignored=no")
	if err != nil {
		return nil, fmt.Errorf("git status: %w", err)
	}

	var changes []change
	seen := make(map[string]bool)

	// Each record: "XY path\x00". A status code never contains NUL.
	for _, rec := range strings.Split(out, "\x00") {
		if len(rec) < 4 {
			continue
		}
		code := rec[:2]
		path := rec[3:]
		if seen[path] {
			continue
		}
		seen[path] = true

		// Skip unmerged entries — there's nothing sensible we can push.
		if strings.ContainsAny(code, "U") || code == "DD" || code == "AA" {
			return nil, fmt.Errorf("path %q has merge conflicts; resolve before pushing", path)
		}

		deleted := code[0] == 'D' || code[1] == 'D'
		if deleted {
			changes = append(changes, change{path: path, deleted: true})
			continue
		}

		mode, err := workingTreeMode(root, path)
		if err != nil {
			return nil, err
		}
		changes = append(changes, change{path: path, mode: mode})
	}

	return changes, nil
}

// workingTreeMode returns the git mode string for a file on disk.
func workingTreeMode(root, path string) (string, error) {
	abs := filepath.Join(root, path)
	info, err := os.Lstat(abs)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "120000", nil
	}
	if info.Mode()&0o111 != 0 {
		return "100755", nil
	}
	return "100644", nil
}

// readBlob returns the raw bytes of a file in the working tree. Symlinks
// are returned as the link target (matching git's blob storage for mode 120000).
func readBlob(root, path string) ([]byte, error) {
	abs := filepath.Join(root, path)
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(abs)
		if err != nil {
			return nil, fmt.Errorf("readlink %s: %w", path, err)
		}
		return []byte(target), nil
	}
	return os.ReadFile(abs)
}

// runGit invokes `git` with the given args inside dir. If dir is empty
// the inherited working directory is used.
func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}

// runGitInherit runs git and streams output to the user's terminal. Used
// for fetch / reset where the user benefits from seeing progress.
func runGitInherit(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return nil
}
