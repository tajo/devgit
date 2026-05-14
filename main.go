// devgit is a small CLI that replaces an unsigned local HEAD commit with
// an App-signed equivalent, using a GitHub App so the local environment
// doesn't need an SSH or GPG signing key.
//
// Usage:
//
//	devgit sign
//
// It must be run from inside a git repository whose `origin` remote is a
// GitHub repository accessible to the configured GitHub App installation.
// Branch creation, pushing, and PR management are all left to plain git.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "sign":
		if err := runSign(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "devgit: %v\n", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "devgit: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `devgit — replace local HEAD with an App-signed equivalent

Usage:
  devgit sign

devgit sign walks your local commits ahead of the upstream merge-base,
finds the oldest one that's missing a verified signature, and asks the
devgit signing service to rewrite it and every descendant into App-signed
equivalents. Your local HEAD is then reset onto the new signed chain. If
every commit ahead of upstream is already verified, it's a no-op.

Typical workflow:
  git commit -m "Add login form"
  devgit sign
  git push

Environment:
  DEVGIT_SERVER  Base URL of the signing service. Defaults to
                 http://127.0.0.1:3000 — run the server in ./server (or
                 wherever it lives) before invoking devgit sign.

This lets git work normally while the signing service's GitHub App provides
the signature your remote's branch-protection rule requires.
`)
}
