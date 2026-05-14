// Holds the GitHub App credentials in memory and exposes the two operations
// the HTTP layer can call: mint a scoped installation token, and create a
// signed commit on the user's behalf.

import { App } from "@octokit/app";
import { readFileSync } from "node:fs";

export interface CoAuthor {
  name: string;
  email: string;
}

export interface SignCommitInput {
  owner: string;
  repo: string;
  treeSHA: string;
  parents: string[];
  message: string;
  coAuthor?: CoAuthor;
}

export interface MintedToken {
  token: string;
  expiresAt: string;
}

function loadApp(): App {
  const appId = Number(process.env.GITHUB_APP_ID);
  const keyPath = process.env.GITHUB_APP_PRIVATE_KEY_PATH;

  if (!Number.isFinite(appId) || appId <= 0) {
    throw new Error(
      "GITHUB_APP_ID is not set or not numeric. " +
        "Set it from your App's settings page header.",
    );
  }
  if (!keyPath) {
    throw new Error(
      "GITHUB_APP_PRIVATE_KEY_PATH is not set. " +
        "Set it to the absolute path of the App's .pem file.",
    );
  }

  let privateKey: string;
  try {
    privateKey = readFileSync(keyPath, "utf8");
  } catch (err) {
    throw new Error(
      `Could not read private key at ${keyPath}: ${(err as Error).message}`,
    );
  }

  return new App({ appId, privateKey });
}

const app = loadApp();

// Installation IDs don't change unless the App is uninstalled/reinstalled
// on a repo, so cache for the process lifetime. Storing Promises (not the
// resolved value) deduplicates concurrent first-time lookups; a rejection
// evicts the entry so transient failures don't poison the cache.
const installationIdCache = new Map<string, Promise<number>>();

async function installationIdFor(owner: string, repo: string): Promise<number> {
  const key = `${owner}/${repo}`;
  let pending = installationIdCache.get(key);
  if (!pending) {
    pending = (async () => {
      try {
        const { data } = await app.octokit.request(
          "GET /repos/{owner}/{repo}/installation",
          { owner, repo },
        );
        return data.id;
      } catch (err) {
        installationIdCache.delete(key);
        throw err;
      }
    })();
    installationIdCache.set(key, pending);
  }
  return pending;
}

/**
 * Issues a short-lived (~1h) installation token scoped narrowly to the
 * single requested repo with only `contents: write`. The CLI uses it for
 * `git push` to the temp ref and the eventual delete; even if it leaks,
 * blast radius is one repo for one hour.
 */
export async function mintInstallationToken(
  owner: string,
  repo: string,
): Promise<MintedToken> {
  const installationId = await installationIdFor(owner, repo);

  const { data } = await app.octokit.request(
    "POST /app/installations/{installation_id}/access_tokens",
    {
      installation_id: installationId,
      repositories: [repo],
      permissions: { contents: "write" },
    },
  );

  return { token: data.token, expiresAt: data.expires_at };
}

/**
 * Creates a commit on the server side, referencing a tree already in
 * GitHub's object DB (the CLI's temp-ref push put it there). Author and
 * Committer are both omitted — GitHub stamps them as the App identity
 * and signs the commit with its web-flow key, producing the Verified badge.
 */
export async function signCommit(input: SignCommitInput): Promise<string> {
  const installationId = await installationIdFor(input.owner, input.repo);
  const octokit = await app.getInstallationOctokit(installationId);

  const message = withCoAuthorTrailer(input.message, input.coAuthor);

  const { data } = await octokit.request(
    "POST /repos/{owner}/{repo}/git/commits",
    {
      owner: input.owner,
      repo: input.repo,
      message,
      tree: input.treeSHA,
      parents: input.parents,
    },
  );

  return data.sha;
}

/**
 * Appends a Co-Authored-By trailer (GitHub renders these on the PR view).
 * Author/Committer on the actual commit stay as the App so signing fires.
 */
function withCoAuthorTrailer(message: string, coAuthor?: CoAuthor): string {
  if (!coAuthor) return message;
  const trimmed = message.replace(/\n+$/, "");
  return `${trimmed}\n\nCo-Authored-By: ${coAuthor.name} <${coAuthor.email}>\n`;
}
