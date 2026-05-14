package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const defaultServerURL = "http://127.0.0.1:3000"

// Custom-prefix refs bypass branch-protection rules, which only match
// `refs/heads/*` (and a few other well-known prefixes). We push HEAD here
// so the unsigned objects land in GitHub's DB without tripping any rule.
const tempRefPrefix = "refs/devgit/sign-"

// Fallback when HEAD has no upstream tracked. Flip to "master" for older repos.
const fallbackBaseBranch = "main"

func runSign(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: devgit sign")
	}

	root, err := repoRoot()
	if err != nil {
		return err
	}
	owner, repo, err := originOwnerRepo(root)
	if err != nil {
		return err
	}

	// Find the boundary between "on origin already, signed or not, none of
	// our business" and "ahead of upstream, our responsibility to verify".
	base, err := upstreamMergeBase(root, fallbackBaseBranch)
	if err != nil {
		return err
	}

	chain, err := commitsBetween(root, base, "HEAD")
	if err != nil {
		return err
	}
	if len(chain) == 0 {
		fmt.Printf("HEAD is at the upstream merge-base (%s); nothing to push, nothing to sign\n", shortSHA(base))
		return nil
	}

	cut := firstUnverified(chain)
	if cut < 0 {
		fmt.Printf("all %d commit(s) ahead of upstream are already verified; nothing to do\n", len(chain))
		return nil
	}
	toRewrite := chain[cut:]

	// We can't faithfully reproduce a merge commit through CreateCommit
	// without tracking new SHAs for each side of the merge. Refuse rather
	// than silently collapse one parent.
	for _, c := range toRewrite {
		if len(c.Parents) > 1 {
			return fmt.Errorf("commit %s in the rewrite range is a merge (%d parents); "+
				"signing chains containing merges is not yet supported",
				shortSHA(c.SHA), len(c.Parents))
		}
	}

	headSHA := chain[len(chain)-1].SHA
	fmt.Printf("→ %s/%s: rewriting %d commit(s) starting at %s via %s\n",
		owner, repo, len(toRewrite), shortSHA(toRewrite[0].SHA), serverURL())
	for i, c := range toRewrite {
		fmt.Printf("    %d. %s [%s] %s\n", i+1, shortSHA(c.SHA), c.Sig, firstLine(c.Message))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	token, err := requestInstallationToken(ctx, owner, repo)
	if err != nil {
		return err
	}

	// Single pack transfer uploads every object the rewrite will reference.
	tempRef, err := pushObjectsToTempRef(root, token, owner, repo, headSHA)
	if err != nil {
		return err
	}
	defer func() {
		if err := deleteRemoteRef(root, token, owner, repo, tempRef); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not delete %s on remote: %v\n", tempRef, err)
		}
	}()
	fmt.Printf("✓ uploaded objects via %s\n", tempRef)

	author := localGitAuthor(root)
	if author != nil {
		fmt.Printf("  co-author: %s <%s>\n", author.Name, author.Email)
	}

	// First rewritten commit keeps its original parent (grandfathered on origin);
	// each subsequent rewrite parents off the previous result's new SHA.
	parents := toRewrite[0].Parents
	var newSHA string
	for i, c := range toRewrite {
		newSHA, err = requestSignedCommit(ctx, signRequest{
			Owner:    owner,
			Repo:     repo,
			TreeSHA:  c.Tree,
			Parents:  parents,
			Message:  c.Message,
			CoAuthor: author,
		})
		if err != nil {
			return fmt.Errorf("sign commit %d of %d (%s): %w",
				i+1, len(toRewrite), shortSHA(c.SHA), err)
		}
		fmt.Printf("  ✓ %s → %s\n", shortSHA(c.SHA), shortSHA(newSHA))
		parents = []string{newSHA}
	}

	// Fetch by raw SHA: GitHub has allowAnySHA1InWant on repo-wide, so we
	// don't need to create a ref for the new commit just to pull it down.
	if err := runGitWithToken(root, token, "fetch", originURL(owner, repo), newSHA); err != nil {
		return fmt.Errorf("fetch new signed HEAD %s: %w", shortSHA(newSHA), err)
	}
	if err := runGitInherit(root, "reset", "--hard", newSHA); err != nil {
		return fmt.Errorf("reset HEAD to %s: %w", shortSHA(newSHA), err)
	}
	fmt.Printf("✓ HEAD is now %s (chain of %d signed commit(s)). Run `git push` to publish.\n",
		shortSHA(newSHA), len(toRewrite))

	return nil
}

// firstUnverified scans oldest → newest, returning the index of the first
// commit that needs to be rewritten (status != G/U), or -1 if all clear.
func firstUnverified(chain []commitInfo) int {
	for i, c := range chain {
		if !isVerified(c.Sig) {
			return i
		}
	}
	return -1
}

// firstLine returns the subject line of a commit message.
func firstLine(s string) string {
	before, _, _ := strings.Cut(s, "\n")
	return before
}

// serverURL resolves the signing service URL once per process. OnceValue
// memoizes the result so the env-var lookup happens exactly one time even
// across many requestSignedCommit calls during a chain rewrite.
var serverURL = sync.OnceValue(func() string {
	if v := os.Getenv("DEVGIT_SERVER"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultServerURL
})

var httpClient = &http.Client{Timeout: 30 * time.Second}

type tokenRequest struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
}

type tokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expiresAt"`
}

func requestInstallationToken(ctx context.Context, owner, repo string) (string, error) {
	var out tokenResponse
	if err := postJSON(ctx, "/tokens", tokenRequest{Owner: owner, Repo: repo}, &out); err != nil {
		return "", fmt.Errorf("mint installation token: %w", err)
	}
	return out.Token, nil
}

type signRequest struct {
	Owner    string        `json:"owner"`
	Repo     string        `json:"repo"`
	TreeSHA  string        `json:"treeSHA"`
	Parents  []string      `json:"parents"`
	Message  string        `json:"message"`
	CoAuthor *commitAuthor `json:"coAuthor,omitempty"`
}

type signResponse struct {
	SHA string `json:"sha"`
}

func requestSignedCommit(ctx context.Context, req signRequest) (string, error) {
	var out signResponse
	if err := postJSON(ctx, "/commits/sign", req, &out); err != nil {
		return "", fmt.Errorf("create signed commit: %w", err)
	}
	return out.SHA, nil
}

// postJSON sends a JSON POST and decodes a JSON response. Non-2xx responses
// surface the server's error message verbatim so the CLI user can see what
// the server complained about (missing install, bad SHA, etc.).
func postJSON(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, serverURL()+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call %s: %w (is the devgit server running at %s?)",
			path, err, serverURL())
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s returned %d: %s", path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response from %s: %w", path, err)
	}
	return nil
}

// --- git operations with App-token auth -------------------------------------

func originURL(owner, repo string) string {
	return fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
}

// pushObjectsToTempRef pushes the local commit (and transitively all the
// objects it needs that aren't already on the remote) to a randomly-named
// ref under refs/devgit/sign-. Branch-protection rules only match
// refs/heads/*, so the push goes through even when signing is required
// on heads. The objects land in GitHub's object DB and are available for
// the subsequent API CreateCommit call.
func pushObjectsToTempRef(root, token, owner, repo, sha string) (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate temp ref name: %w", err)
	}
	tempRef := fmt.Sprintf("%s%x", tempRefPrefix, buf)

	if err := runGitWithToken(root, token, "push", originURL(owner, repo), sha+":"+tempRef); err != nil {
		return "", fmt.Errorf("push to %s: %w", tempRef, err)
	}
	return tempRef, nil
}

// deleteRemoteRef removes a ref from origin via authenticated git push.
// Used for temp-ref cleanup; caller logs failures non-fatally.
func deleteRemoteRef(root, token, owner, repo, ref string) error {
	return runGitWithToken(root, token, "push", originURL(owner, repo), "--delete", ref)
}

// runGitWithToken runs git with an inline credential helper that pulls the
// password from $DEVGIT_TOKEN. The helper script appears in `ps` output
// but does not contain the secret — only the env var does, and env vars
// are not surfaced by default `ps`.
func runGitWithToken(root, token string, args ...string) error {
	const helper = `!f() { if test "$1" = get; then echo username=x-access-token; echo "password=$DEVGIT_TOKEN"; fi; }; f`

	gitArgs := []string{
		"-C", root,
		"-c", "credential.helper=", // wipe any inherited helpers (osxkeychain, etc.)
		"-c", "credential.helper=" + helper,
	}
	gitArgs = append(gitArgs, args...)

	cmd := exec.Command("git", gitArgs...)
	cmd.Env = append(os.Environ(), "DEVGIT_TOKEN="+token)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func shortSHA(s string) string {
	if len(s) < 7 {
		return s
	}
	return s[:7]
}
