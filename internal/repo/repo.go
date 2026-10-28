// Package repo clones or opens the target repository, creates the run branch, commits the result and pushes it back.
package repo

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
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
// too, so the run never works in the original checkout: the result reaches
// it only as that branch, through PushLocal.
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
	return git(ctx, dir, nil, args...)
}

// git is Git with extra environment variables, which keep secrets out of
// the arguments that error messages repeat.
func git(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Author is who the run's commit is by: the tool, not whoever runs it, so
// the commit needs no git identity configured on the machine.
const Author = "autodoc <autodoc@users.noreply.localhost>"

// Commit stages every change in the working copy and commits it with msg.
// It returns the commit and the committed paths; with nothing to commit,
// both are empty and no commit is made. allowed vets every staged path
// first: one it rejects fails the commit and is named in the error.
func Commit(ctx context.Context, dir, msg string, allowed func(path string) bool) (sha string, files []string, err error) {
	if _, err := Git(ctx, dir, "add", "--all"); err != nil {
		return "", nil, err
	}
	out, err := Git(ctx, dir, "diff", "--cached", "--name-only", "--no-renames")
	if err != nil || out == "" {
		return "", nil, err
	}
	files = strings.Split(out, "\n")
	var outside []string
	for _, f := range files {
		if !allowed(f) {
			outside = append(outside, f)
		}
	}
	if len(outside) > 0 {
		return "", nil, fmt.Errorf("changes outside the run's scope: %s", strings.Join(outside, ", "))
	}
	name, email, _ := strings.Cut(strings.TrimSuffix(Author, ">"), " <")
	if _, err := Git(ctx, dir, "-c", "user.name="+name, "-c", "user.email="+email, "commit", "--quiet", "-m", msg); err != nil {
		return "", nil, err
	}
	sha, err = Git(ctx, dir, "rev-parse", "HEAD")
	return sha, files, err
}

// Push pushes branch to origin over HTTPS with token as the password, and
// returns origin's URL. The token travels in an environment-scoped
// http.extraHeader, so it is never in the arguments, the remote URL or
// .git/config. The branch is the run's own (autodoc/<base sha>): a later
// run on the same base replaces it, unless someone pushed to it since this
// clone (--force-with-lease).
func Push(ctx context.Context, dir, branch, token string) (string, error) {
	url, err := Git(ctx, dir, "remote", "get-url", "origin")
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(url, "https://") {
		return url, fmt.Errorf("origin %s is not an https URL, so the token cannot authenticate the push", url)
	}
	auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	env := []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic " + auth}
	if _, err := git(ctx, dir, env, "push", "--quiet", "--force-with-lease", "origin", branch); err != nil {
		return url, errors.New(strings.ReplaceAll(err.Error(), token, "***"))
	}
	return url, nil
}

// IsLocal reports whether src, as given to Prepare, is a repository on disk
// rather than a URL.
func IsLocal(src string) bool {
	fi, err := os.Stat(src)
	return err == nil && fi.IsDir()
}

// PushLocal pushes branch to origin, a repository on disk, and returns
// origin's path. Only the branch ref and its objects are written there: the
// working tree, the index and the checked-out branch stay as they are, and
// git refuses the push if branch itself is checked out. As with Push, a later
// run on the same base replaces the branch unless it moved since the clone.
func PushLocal(ctx context.Context, dir, branch string) (string, error) {
	path, err := Git(ctx, dir, "remote", "get-url", "origin")
	if err != nil {
		return "", err
	}
	if !IsLocal(path) {
		return path, fmt.Errorf("origin %s is not a repository on disk", path)
	}
	_, err = Git(ctx, dir, "push", "--quiet", "--force-with-lease", "origin", branch)
	return path, err
}

// Show returns the content of path (a slash path) at rev, byte for byte.
func Show(ctx context.Context, dir, rev, path string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", "show", rev+":"+path)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git show %s:%s: %w: %s", rev, path, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// Restore puts the working copy back to its last commit: the index and the
// tracked files are reset, so a failed commit's staged changes go too, and
// untracked files (not ignored ones) are removed.
func Restore(ctx context.Context, dir string) error {
	if _, err := Git(ctx, dir, "reset", "--quiet", "--hard", "HEAD"); err != nil {
		return err
	}
	_, err := Git(ctx, dir, "clean", "-fdq")
	return err
}
