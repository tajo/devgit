// Binds 127.0.0.1 by default — this process holds the App's private key and
// must not be reachable from the network without an explicit auth layer.

import express, {
  type ErrorRequestHandler,
  type Request,
  type Response,
} from "express";
import { z } from "zod";
import { mintInstallationToken, signCommit } from "./signing.js";

const app = express();
app.use(express.json({ limit: "1mb" }));

// --- Validation schemas ------------------------------------------------------

const tokensBody = z.object({
  owner: z.string().min(1),
  repo: z.string().min(1),
});

// SHA-1 git object names are 40 hex chars. We allow SHA-256 (64) too, just in
// case GitHub later supports the experimental hash algorithm here.
const sha = z.string().regex(/^[0-9a-f]{40,64}$/, "must be a hex SHA");

const signBody = z.object({
  owner: z.string().min(1),
  repo: z.string().min(1),
  treeSHA: sha,
  parents: z.array(sha).min(1),
  message: z.string().min(1),
  coAuthor: z
    .object({
      name: z.string().min(1),
      email: z.string().email(),
    })
    .optional(),
});

// --- Helpers -----------------------------------------------------------------

/**
 * Wraps an async handler so thrown errors propagate to the Express error
 * middleware instead of becoming unhandled rejections. Express 5 does this
 * automatically; we're on 4 for stability, so this shim is needed.
 */
function asyncHandler(
  fn: (req: Request, res: Response) => Promise<unknown>,
): (req: Request, res: Response, next: (err?: unknown) => void) => void {
  return (req, res, next) => {
    fn(req, res).catch(next);
  };
}

function badRequest(res: Response, message: string, details?: unknown): void {
  res.status(400).json({ error: message, details });
}

// --- Routes ------------------------------------------------------------------

app.post(
  "/tokens",
  asyncHandler(async (req, res) => {
    const parsed = tokensBody.safeParse(req.body);
    if (!parsed.success) {
      badRequest(res, "invalid request body", parsed.error.format());
      return;
    }
    const { owner, repo } = parsed.data;
    const minted = await mintInstallationToken(owner, repo);
    res.json(minted);
  }),
);

app.post(
  "/commits/sign",
  asyncHandler(async (req, res) => {
    const parsed = signBody.safeParse(req.body);
    if (!parsed.success) {
      badRequest(res, "invalid request body", parsed.error.format());
      return;
    }
    const newSHA = await signCommit(parsed.data);
    res.json({ sha: newSHA });
  }),
);

// Catch-all error handler. Reports GitHub API errors with their status if
// possible, otherwise generic 500. Always logs the full error server-side
// — the CLI only sees the public-safe message.
const errorHandler: ErrorRequestHandler = (err, _req, res, _next) => {
  console.error("[devgit-server] error:", err);

  // Octokit attaches `status` on RequestError instances.
  const status =
    typeof err === "object" && err !== null && "status" in err
      ? Number((err as { status: unknown }).status)
      : NaN;

  if (Number.isFinite(status) && status >= 400 && status < 600) {
    res.status(status).json({
      error: (err as Error).message ?? "github api error",
    });
    return;
  }

  res.status(500).json({
    error: err instanceof Error ? err.message : "internal server error",
  });
};
app.use(errorHandler);

// --- Bootstrap ---------------------------------------------------------------

const port = Number(process.env.PORT) || 3000;
const host = process.env.HOST || "127.0.0.1";

app.listen(port, host, () => {
  console.log(`[devgit-server] listening on http://${host}:${port}`);
});
