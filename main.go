// devgit is a small CLI that creates signed commits and PRs on GitHub
// using a GitHub App, since the local environment has no SSH/GPG key.
//
// Usage:
//
//	devgit push <branch> <description>
//
// It must be run from inside a git repository whose `origin` remote is a
// GitHub repository accessible to the configured GitHub App installation.
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
	case "push":
		if err := runPush(os.Args[2:]); err != nil {
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
	fmt.Fprint(os.Stderr, `devgit — create signed commits & PRs via a GitHub App

Usage:
  devgit push <branch> <description>

Examples:
  devgit push feature/login "Add login form"
`)
}
