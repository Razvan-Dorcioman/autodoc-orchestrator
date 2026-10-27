// Package repo clones or opens the target repository, creates the run branch, commits the result and optionally pushes it.
package repo

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Repo is the working copy a run operates on.
type Repo struct {
	Dir     string `json:"dir"`
	Source  string `json:"source"`
	BaseSHA string `json:"base_sha"`
	Branch  string `json:"branch"`
}

// Prepare clones src (a git URL or a local repository path) into dir and
// checks out a new branch autodoc/<short base sha>. A local path is cloned
// too, so the original checkout is never modified.
func Prepare(ctx context.Context, src, dir string) (*Repo, error) {
	// Check out the exact bytes in the repository: no CRLF conversion on Windows,
	// so hashes, gofmt and generated files are the same on every OS.
	if _, err := Git(ctx, "", "clone", "--quiet", "-c", "core.autocrlf=false", "-c", "core.eol=lf", src, dir); err != nil {
		return nil, err
	}
	sha, err := Git(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	branch := "autodoc/" + sha[:min(12, len(sha))]
	if _, err := Git(ctx, dir, "checkout", "--quiet", "-b", branch); err != nil {
		return nil, err
	}
	return &Repo{Dir: dir, Source: src, BaseSHA: sha, Branch: branch}, nil
}

// Git runs git in dir and returns its trimmed stdout. It never prompts for credentials.
func Git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}
