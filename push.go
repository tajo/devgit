package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v66/github"
)

// --- GitHub App credentials --------------------------------------------------
//
// Hardcoded for now per the bootstrap brief. Replace the zero values once the
// real App is provisioned. The private key must be the full PEM contents
// (including the BEGIN/END lines), copy-pasted into the backticked string.
const (
	githubAppID         int64 = 0 // TODO: set GitHub App ID
	githubInstallID     int64 = 0 // TODO: set GitHub App Installation ID for the target org/user
	githubAppPrivateKey       = `` // TODO: paste full PEM (-----BEGIN ... END-----)
)

// Base branch the PR targets. Most repos use "main"; flip to "master" if the
// repo predates the rename. We could detect this dynamically, but keeping it
// explicit avoids a surprising API call on every push.
const defaultBaseBranch = "main"

func runPush(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: devgit push <branch> <description>")
	}
	branch := args[0]
	description := strings.Join(args[1:], " ")

	if err := validateAppConfig(); err != nil {
		return err
	}

	root, err := repoRoot()
	if err != nil {
		return err
	}

	owner, repo, err := originOwnerRepo(root)
	if err != nil {
		return err
	}

	baseSHA, err := headSHA(root)
	if err != nil {
		return err
	}

	changes, err := collectChanges(root)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		return errors.New("no staged, unstaged, or untracked changes to push")
	}

	fmt.Printf("→ %s/%s: pushing %d change(s) onto branch %q (base %s)\n",
		owner, repo, len(changes), branch, shortSHA(baseSHA))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client, err := newAppClient()
	if err != nil {
		return err
	}

	newSHA, err := buildSignedCommit(ctx, client, owner, repo, baseSHA, description, changes, root)
	if err != nil {
		return err
	}
	fmt.Printf("✓ created signed commit %s\n", shortSHA(newSHA))

	if err := upsertBranch(ctx, client, owner, repo, branch, newSHA); err != nil {
		return err
	}
	fmt.Printf("✓ branch %s now points at %s\n", branch, shortSHA(newSHA))

	pr, err := ensurePullRequest(ctx, client, owner, repo, branch, defaultBaseBranch, description)
	if err != nil {
		return err
	}
	fmt.Printf("✓ pull request: %s\n", pr.GetHTMLURL())

	if err := syncLocalToRemote(root, branch); err != nil {
		return fmt.Errorf("local sync to %s: %w", branch, err)
	}
	fmt.Printf("✓ local branch %s is now at %s with a clean working tree\n", branch, shortSHA(newSHA))

	return nil
}

func validateAppConfig() error {
	if githubAppID == 0 || githubInstallID == 0 || strings.TrimSpace(githubAppPrivateKey) == "" {
		return errors.New("GitHub App credentials are not set: edit push.go constants " +
			"(githubAppID, githubInstallID, githubAppPrivateKey) before using devgit")
	}
	return nil
}

// newAppClient builds a go-github client whose underlying transport mints
// fresh installation tokens from the App's private key.
func newAppClient() (*github.Client, error) {
	tr, err := ghinstallation.New(http.DefaultTransport, githubAppID, githubInstallID,
		[]byte(githubAppPrivateKey))
	if err != nil {
		return nil, fmt.Errorf("init github app transport: %w", err)
	}
	return github.NewClient(&http.Client{Transport: tr}), nil
}

// buildSignedCommit uploads blobs for every changed file, derives a new tree
// from the base commit's tree, then creates a commit. Because the commit is
// authored via App credentials, GitHub auto-signs it server-side.
func buildSignedCommit(
	ctx context.Context,
	client *github.Client,
	owner, repo, baseSHA, message string,
	changes []change,
	root string,
) (string, error) {
	baseCommit, _, err := client.Git.GetCommit(ctx, owner, repo, baseSHA)
	if err != nil {
		return "", fmt.Errorf("get base commit %s: %w", baseSHA, err)
	}
	baseTreeSHA := baseCommit.GetTree().GetSHA()

	entries := make([]*github.TreeEntry, 0, len(changes))
	for _, ch := range changes {
		path := ch.path
		if ch.deleted {
			// Nil SHA in a derived tree removes the path. Mode is still required.
			entries = append(entries, &github.TreeEntry{
				Path: github.String(path),
				Mode: github.String("100644"),
				Type: github.String("blob"),
				SHA:  nil,
			})
			continue
		}

		content, err := readBlob(root, path)
		if err != nil {
			return "", err
		}

		// Always upload as base64 — handles binary files and avoids encoding
		// gotchas with arbitrary text. The API accepts either content or
		// encoding+content; we keep one path for simplicity.
		blob, _, err := client.Git.CreateBlob(ctx, owner, repo, &github.Blob{
			Content:  github.String(base64.StdEncoding.EncodeToString(content)),
			Encoding: github.String("base64"),
		})
		if err != nil {
			return "", fmt.Errorf("create blob for %s: %w", path, err)
		}

		entries = append(entries, &github.TreeEntry{
			Path: github.String(path),
			Mode: github.String(ch.mode),
			Type: github.String("blob"),
			SHA:  blob.SHA,
		})
	}

	tree, _, err := client.Git.CreateTree(ctx, owner, repo, baseTreeSHA, entries)
	if err != nil {
		return "", fmt.Errorf("create tree: %w", err)
	}

	commit, _, err := client.Git.CreateCommit(ctx, owner, repo, &github.Commit{
		Message: github.String(message),
		Tree:    tree,
		Parents: []*github.Commit{{SHA: github.String(baseSHA)}},
	}, nil)
	if err != nil {
		return "", fmt.Errorf("create commit: %w", err)
	}
	return commit.GetSHA(), nil
}

// upsertBranch creates refs/heads/<branch> at sha, or force-updates it if it
// already exists. We force here because the branch is intended to be owned
// by devgit-driven flows; in a real multi-writer setting we'd add a
// `--force-with-lease`-style compare-and-swap.
func upsertBranch(
	ctx context.Context,
	client *github.Client,
	owner, repo, branch, sha string,
) error {
	ref := &github.Reference{
		Ref:    github.String("refs/heads/" + branch),
		Object: &github.GitObject{SHA: github.String(sha)},
	}
	_, _, err := client.Git.CreateRef(ctx, owner, repo, ref)
	if err == nil {
		return nil
	}
	// CreateRef returns 422 when the ref exists. Fall through to update.
	var errResp *github.ErrorResponse
	if !errors.As(err, &errResp) || errResp.Response.StatusCode != http.StatusUnprocessableEntity {
		return fmt.Errorf("create ref: %w", err)
	}
	if _, _, err := client.Git.UpdateRef(ctx, owner, repo, ref, true); err != nil {
		return fmt.Errorf("update ref: %w", err)
	}
	return nil
}

// ensurePullRequest opens a PR or returns the existing open PR for head→base.
func ensurePullRequest(
	ctx context.Context,
	client *github.Client,
	owner, repo, branch, base, description string,
) (*github.PullRequest, error) {
	title := firstLine(description)
	pr, _, err := client.PullRequests.Create(ctx, owner, repo, &github.NewPullRequest{
		Title: github.String(title),
		Head:  github.String(branch),
		Base:  github.String(base),
		Body:  github.String(description),
	})
	if err == nil {
		return pr, nil
	}

	// If a PR already exists for this head, GitHub returns 422. Look it up.
	var errResp *github.ErrorResponse
	if !errors.As(err, &errResp) || errResp.Response.StatusCode != http.StatusUnprocessableEntity {
		return nil, fmt.Errorf("create pull request: %w", err)
	}
	prs, _, listErr := client.PullRequests.List(ctx, owner, repo, &github.PullRequestListOptions{
		State: "open",
		Head:  owner + ":" + branch,
		Base:  base,
	})
	if listErr != nil {
		return nil, fmt.Errorf("create pull request: %w (and list fallback failed: %v)", err, listErr)
	}
	if len(prs) == 0 {
		return nil, fmt.Errorf("create pull request: %w", err)
	}
	return prs[0], nil
}

// syncLocalToRemote fetches the freshly-pushed branch and moves the local
// working copy onto it with a clean tree. The on-disk file contents already
// match the new commit (we just synthesized that commit from them), so the
// hard reset is non-destructive in practice.
func syncLocalToRemote(root, branch string) error {
	if err := runGitInherit(root, "fetch", "origin", branch); err != nil {
		return err
	}
	// `checkout -B` creates or repositions the local branch at the current HEAD
	// without touching the working tree, then switches to it. The subsequent
	// hard reset moves both branch tip and index to match origin/<branch>.
	if err := runGitInherit(root, "checkout", "-B", branch); err != nil {
		return err
	}
	if err := runGitInherit(root, "reset", "--hard", "origin/"+branch); err != nil {
		return err
	}
	return nil
}

func shortSHA(s string) string {
	if len(s) < 7 {
		return s
	}
	return s[:7]
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
