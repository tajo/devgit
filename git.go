package main

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

// repoRoot returns the absolute path of the enclosing git repo.
func repoRoot() (string, error) {
	out, err := runGit("", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("not inside a git repository: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// commitInfo describes a single commit in enough detail for the server's
// /commits/sign endpoint to recreate it. Sig is git's `%G?` status:
//
//	N - no signature                  (needs replacement)
//	B - bad signature                 (needs replacement)
//	G - good signature, trusted key   (verified)
//	U - good signature, untrusted key (verified — App signatures usually land here)
//	X - good, but expired             (needs replacement)
//	Y - good, but key expired         (needs replacement)
//	R - good, but key revoked         (needs replacement)
//	E - cannot verify (missing key)   (needs replacement, to be safe)
type commitInfo struct {
	SHA     string
	Tree    string
	Parents []string
	Message string
	Sig     string
}

// isVerified returns true if a `%G?` status character is one that GitHub's
// branch-protection "require signed commits" rule would accept. Locally we
// only see G or U for valid signatures (the App's signature shows as U on
// most machines because the App's key isn't in the user's keyring).
func isVerified(status string) bool {
	return status == "G" || status == "U"
}

// upstreamMergeBase returns the SHA of the most recent common ancestor of
// HEAD and the tracked upstream branch. Falls back to origin/<fallback> if
// HEAD has no upstream configured (e.g., a brand-new local branch).
func upstreamMergeBase(root, fallback string) (string, error) {
	if out, err := runGit(root, "merge-base", "HEAD", "@{upstream}"); err == nil {
		return strings.TrimSpace(out), nil
	}
	fallbackRef := "origin/" + fallback
	out, err := runGit(root, "merge-base", "HEAD", fallbackRef)
	if err != nil {
		return "", fmt.Errorf("no upstream tracked and could not merge-base against %s: %w", fallbackRef, err)
	}
	return strings.TrimSpace(out), nil
}

// commitsBetween returns the commits in the half-open range (from, to] in
// oldest-first order, fully populated with tree/parents/message/signature.
//
// The single git-log invocation uses `-z` to put NULs *between* records,
// and `%x00` to put NULs *between* fields within each record. So output is
// one flat NUL-delimited stream: each commit contributes exactly 5 fields,
// in the order SHA, tree, parents, sig, message.
func commitsBetween(root, from, to string) ([]commitInfo, error) {
	rng := fmt.Sprintf("%s..%s", from, to)
	out, err := runGit(root, "log", "--reverse", "-z",
		"--format=%H%x00%T%x00%P%x00%G?%x00%B", rng)
	if err != nil {
		return nil, fmt.Errorf("list commits %s: %w", rng, err)
	}
	if out == "" {
		return nil, nil
	}

	fields := strings.Split(out, "\x00")
	if len(fields) > 0 && fields[len(fields)-1] == "" {
		fields = fields[:len(fields)-1]
	}
	if len(fields)%5 != 0 {
		return nil, fmt.Errorf("unexpected git log output: got %d fields, want multiple of 5", len(fields))
	}

	commits := make([]commitInfo, 0, len(fields)/5)
	for i := 0; i < len(fields); i += 5 {
		commits = append(commits, commitInfo{
			SHA:     fields[i],
			Tree:    fields[i+1],
			Parents: strings.Fields(fields[i+2]),
			Sig:     fields[i+3],
			Message: strings.TrimRight(fields[i+4], "\n"),
		})
	}
	return commits, nil
}

// localGitAuthor returns the author identity git would stamp on a new
// commit (merging system / global / repo-local config). `git var
// GIT_AUTHOR_IDENT` resolves both name and email in one subprocess and
// applies git's own validation; if either is missing it exits non-zero
// and we fall back to nil so the caller can use the App's identity.
//
// Output format: `Name <email> <unix-timestamp> <tz>`.
func localGitAuthor(root string) *commitAuthor {
	out, err := runGit(root, "var", "GIT_AUTHOR_IDENT")
	if err != nil {
		return nil
	}
	s := strings.TrimSpace(out)
	emailStart := strings.LastIndex(s, " <")
	emailEnd := strings.LastIndex(s, "> ")
	if emailStart < 0 || emailEnd <= emailStart {
		return nil
	}
	name := strings.TrimSpace(s[:emailStart])
	email := s[emailStart+2 : emailEnd]
	if name == "" || email == "" {
		return nil
	}
	return &commitAuthor{Name: name, Email: email}
}

// commitAuthor holds the human identity surfaced via a Co-Authored-By trailer.
// Author/Committer on the actual commit stay as the App (default) so GitHub
// auto-signs it server-side.
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

// gitCommand builds an *exec.Cmd for `git <args>` rooted at dir (empty =
// inherit cwd). Callers attach stdout/stderr and call Run themselves.
func gitCommand(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	return cmd
}

// runGit runs git and returns captured stdout. On failure the returned
// error contains git's stderr verbatim so the caller can surface it.
func runGit(dir string, args ...string) (string, error) {
	cmd := gitCommand(dir, args...)
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

// runGitInherit runs git and streams its stdout/stderr to the user's
// terminal. For fetch / reset / push where progress matters.
func runGitInherit(dir string, args ...string) error {
	cmd := gitCommand(dir, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return nil
}
