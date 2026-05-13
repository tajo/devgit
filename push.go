package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v84/github"
)

// --- GitHub App credentials --------------------------------------------------
//
// The PEM path is hardcoded rather than embedded so the secret never ends up
// in source control. ghinstallation v2 still wants the numeric App ID for the
// JWT `iss` claim; the Client ID is kept for reference / future migration when
// the library accepts string identifiers.
const (
	githubAppID             int64 = 3694599
	githubAppClientID             = "Iv23li8LdX9JSdjo8q0z"
	githubAppPrivateKeyPath       = "/Users/vojtech/Downloads/devgit-go.2026-05-12.private-key.pem"
)

// Base branch the PR targets. Most repos use "main"; flip to "master" if the
// repo predates the rename. We could detect this dynamically, but keeping it
// explicit avoids a surprising API call on every push.
const defaultBaseBranch = "main"

func runPush(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: devgit push [<branch>] <description>")
	}

	if err := validateAppConfig(); err != nil {
		return err
	}

	root, err := repoRoot()
	if err != nil {
		return err
	}

	cur, err := currentBranch(root)
	if err != nil {
		return err
	}

	branch, description, err := resolveBranchAndDescription(args, cur)
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

	client, err := newInstallClient(ctx, owner, repo)
	if err != nil {
		return err
	}

	// Decide whether we're creating a new branch or extending one we already
	// own. Either way, refuse to silently force-push over remote history.
	remoteTip, remoteExists, err := remoteBranchTip(ctx, client, owner, repo, branch)
	if err != nil {
		return err
	}
	if remoteExists {
		if cur != branch {
			return fmt.Errorf("branch %s already exists on origin; "+
				"check it out first to extend its PR, or pick a new name", branch)
		}
		if remoteTip != baseSHA {
			return fmt.Errorf("local %s is at %s but origin/%s is at %s; "+
				"`git fetch && git pull` before pushing",
				branch, shortSHA(baseSHA), branch, shortSHA(remoteTip))
		}
	}

	author := localGitAuthor(root)
	if author != nil {
		fmt.Printf("  author: %s <%s>\n", author.Name, author.Email)
	}

	newSHA, err := buildSignedCommit(ctx, client, owner, repo, baseSHA, description, changes, root, author)
	if err != nil {
		return err
	}
	fmt.Printf("✓ created signed commit %s\n", shortSHA(newSHA))

	if err := upsertBranch(ctx, client, owner, repo, branch, newSHA); err != nil {
		return err
	}
	if remoteExists {
		fmt.Printf("✓ branch %s advanced to %s (was %s)\n",
			branch, shortSHA(newSHA), shortSHA(remoteTip))
	} else {
		fmt.Printf("✓ branch %s created at %s\n", branch, shortSHA(newSHA))
	}

	pr, created, err := ensurePullRequest(ctx, client, owner, repo, branch, defaultBaseBranch, description)
	if err != nil {
		return err
	}
	if created {
		fmt.Printf("✓ opened pull request: %s\n", pr.GetHTMLURL())
	} else {
		fmt.Printf("✓ extended existing pull request: %s\n", pr.GetHTMLURL())
	}

	if err := syncLocalToRemote(root, branch); err != nil {
		return fmt.Errorf("local sync to %s: %w", branch, err)
	}
	fmt.Printf("✓ local branch %s is now at %s with a clean working tree\n", branch, shortSHA(newSHA))

	return nil
}

// resolveBranchAndDescription disambiguates the two CLI shapes:
//
//	devgit push <description>              → branch = current
//	devgit push <branch> <description>     → branch = explicit
//
// In the 1-arg form we refuse to commit to the base branch — that would
// turn `devgit push` into a way to commit directly to main, bypassing PR
// review, which is the opposite of what this tool exists for.
func resolveBranchAndDescription(args []string, current string) (branch, description string, err error) {
	if len(args) == 1 {
		if current == defaultBaseBranch {
			return "", "", fmt.Errorf("you're on %s; pass a target branch: "+
				"devgit push <branch> %q", defaultBaseBranch, args[0])
		}
		return current, args[0], nil
	}
	return args[0], strings.Join(args[1:], " "), nil
}

func validateAppConfig() error {
	if githubAppID == 0 || strings.TrimSpace(githubAppPrivateKeyPath) == "" {
		return errors.New("GitHub App credentials are not set: edit push.go constants " +
			"(githubAppID, githubAppPrivateKeyPath) before using devgit")
	}
	if _, err := os.Stat(githubAppPrivateKeyPath); err != nil {
		return fmt.Errorf("private key not readable at %s: %w", githubAppPrivateKeyPath, err)
	}
	return nil
}

// newInstallClient mints an installation-scoped go-github client for the
// given repo. It does the two-step App auth dance: build a JWT-signed
// AppsTransport from the private key, ask GitHub which installation owns
// (owner, repo), then convert to an installation-token transport so the
// returned client can read and write that repo.
func newInstallClient(ctx context.Context, owner, repo string) (*github.Client, error) {
	keyBytes, err := os.ReadFile(githubAppPrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read app private key: %w", err)
	}

	atr, err := ghinstallation.NewAppsTransport(http.DefaultTransport, githubAppID, keyBytes)
	if err != nil {
		return nil, fmt.Errorf("init github apps transport: %w", err)
	}

	appClient := github.NewClient(&http.Client{Transport: atr})
	install, _, err := appClient.Apps.FindRepositoryInstallation(ctx, owner, repo)
	if err != nil {
		return nil, fmt.Errorf("find app installation for %s/%s "+
			"(install the app on this repo at https://github.com/settings/installations): %w",
			owner, repo, err)
	}

	itr := ghinstallation.NewFromAppsTransport(atr, install.GetID())

	// Narrow the access token to (a) just this one repo and (b) the minimum
	// permissions a push needs. The App may be installed across many repos
	// with broader permissions; the token GitHub mints for this process can
	// only ever touch <owner>/<repo>, only for Contents + Pull requests.
	// Leaking the in-memory token in this state can't escalate beyond that.
	itr.InstallationTokenOptions = &github.InstallationTokenOptions{
		Repositories: []string{repo},
		Permissions: &github.InstallationPermissions{
			Contents:     github.String("write"),
			PullRequests: github.String("write"),
		},
	}

	return github.NewClient(&http.Client{Transport: itr}), nil
}

// buildSignedCommit uploads blobs for every changed file, derives a new tree
// from the base commit's tree, then creates a commit. Because the commit is
// committed via App credentials, GitHub auto-signs it server-side. The
// author, when provided, is stamped onto the commit so the PR shows the
// human's identity while still benefiting from the App's verified signature.
func buildSignedCommit(
	ctx context.Context,
	client *github.Client,
	owner, repo, baseSHA, message string,
	changes []change,
	root string,
	author *commitAuthor,
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
		blob, _, err := client.Git.CreateBlob(ctx, owner, repo, github.Blob{
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

	newCommit := github.Commit{
		Message: github.String(message),
		Tree:    tree,
		Parents: []*github.Commit{{SHA: github.String(baseSHA)}},
	}
	if author != nil {
		newCommit.Author = &github.CommitAuthor{
			Name:  github.String(author.Name),
			Email: github.String(author.Email),
			Date:  &github.Timestamp{Time: time.Now()},
		}
	}
	commit, _, err := client.Git.CreateCommit(ctx, owner, repo, newCommit, nil)
	if err != nil {
		return "", fmt.Errorf("create commit: %w", err)
	}
	return commit.GetSHA(), nil
}

// remoteBranchTip reads the current tip SHA of refs/heads/<branch> on the
// remote. Returns (sha, true, nil) if the branch exists, ("", false, nil)
// if not, and surfaces any other API error.
func remoteBranchTip(
	ctx context.Context,
	client *github.Client,
	owner, repo, branch string,
) (string, bool, error) {
	ref, resp, err := client.Git.GetRef(ctx, owner, repo, "heads/"+branch)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read remote ref %s: %w", branch, err)
	}
	return ref.GetObject().GetSHA(), true, nil
}

// upsertBranch creates refs/heads/<branch> at sha, or fast-forwards it if
// the branch already exists. We use force=false so GitHub rejects the
// update if some other writer advanced the branch in between our sync
// check and this call — equivalent to `git push --force-with-lease`.
func upsertBranch(
	ctx context.Context,
	client *github.Client,
	owner, repo, branch, sha string,
) error {
	_, _, err := client.Git.CreateRef(ctx, owner, repo, github.CreateRef{
		Ref: "refs/heads/" + branch,
		SHA: sha,
	})
	if err == nil {
		return nil
	}
	// CreateRef returns 422 when the ref exists. Fall through to update.
	var errResp *github.ErrorResponse
	if !errors.As(err, &errResp) || errResp.Response.StatusCode != http.StatusUnprocessableEntity {
		return fmt.Errorf("create ref: %w", err)
	}
	_, _, err = client.Git.UpdateRef(ctx, owner, repo, "heads/"+branch, github.UpdateRef{
		SHA:   sha,
		Force: github.Bool(false),
	})
	if err != nil {
		return fmt.Errorf("update ref (non-fast-forward? someone else may have pushed): %w", err)
	}
	return nil
}

// ensurePullRequest opens a PR or, if one already exists for head→base,
// returns it untouched. The created return value lets the caller report
// "opened" vs "extended" so the user can see what actually happened.
//
// We never edit the title/body of an existing PR — each push's description
// is the commit message for that push, not a rewrite of the PR summary.
func ensurePullRequest(
	ctx context.Context,
	client *github.Client,
	owner, repo, branch, base, description string,
) (pr *github.PullRequest, created bool, err error) {
	title := firstLine(description)
	pr, _, err = client.PullRequests.Create(ctx, owner, repo, &github.NewPullRequest{
		Title: github.String(title),
		Head:  github.String(branch),
		Base:  github.String(base),
		Body:  github.String(description),
	})
	if err == nil {
		return pr, true, nil
	}

	// If a PR already exists for this head, GitHub returns 422. Look it up.
	var errResp *github.ErrorResponse
	if !errors.As(err, &errResp) || errResp.Response.StatusCode != http.StatusUnprocessableEntity {
		return nil, false, fmt.Errorf("create pull request: %w", err)
	}
	prs, _, listErr := client.PullRequests.List(ctx, owner, repo, &github.PullRequestListOptions{
		State: "open",
		Head:  owner + ":" + branch,
		Base:  base,
	})
	if listErr != nil {
		return nil, false, fmt.Errorf("create pull request: %w (and list fallback failed: %v)", err, listErr)
	}
	if len(prs) == 0 {
		return nil, false, fmt.Errorf("create pull request: %w", err)
	}
	return prs[0], false, nil
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
