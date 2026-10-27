// Package tools holds the function tools the agents call. Every tool is
// scoped to the working copy: paths are relative to a root and may not
// escape it.
package tools

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

// maxReadBytes keeps one tool result from flooding the agent's context.
const maxReadBytes = 64 << 10

type ReadFileArgs struct {
	Path string `json:"path" jsonschema:"file path relative to the repository root, using forward slashes"`
}

type ReadFileResult struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated,omitempty"`
}

// NewReadFile returns the read_file tool, scoped to root.
func NewReadFile(root string) (tool.Tool, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return functiontool.New(functiontool.Config{
		Name:        "read_file",
		Description: "Read a text file from the repository. Paths are relative to the repository root.",
	}, func(_ agent.Context, args ReadFileArgs) (ReadFileResult, error) {
		return ReadFile(absRoot, args)
	})
}

// ReadFile is read_file without the ADK wrapper, so it can be tested directly.
func ReadFile(absRoot string, args ReadFileArgs) (ReadFileResult, error) {
	full, err := resolve(absRoot, args.Path)
	if err != nil {
		return ReadFileResult{}, err
	}
	data, err := os.ReadFile(full)
	if err != nil {
		// Report the relative path only; the agent never sees the workdir location.
		var pe *fs.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return ReadFileResult{}, fmt.Errorf("read %s: %w", args.Path, err)
	}
	res := ReadFileResult{Path: filepath.ToSlash(args.Path)}
	if len(data) > maxReadBytes {
		data, res.Truncated = data[:maxReadBytes], true
	}
	res.Content = string(data)
	return res, nil
}

// resolve joins rel onto absRoot and rejects anything that ends up outside it.
func resolve(absRoot, rel string) (string, error) {
	if rel == "" {
		return "", errors.New("path is empty")
	}
	if filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" {
		return "", fmt.Errorf("path %q must be relative to the repository root", rel)
	}
	full := filepath.Join(absRoot, filepath.FromSlash(rel))
	back, err := filepath.Rel(absRoot, full)
	if err != nil || back == ".." || strings.HasPrefix(back, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the repository", rel)
	}
	return full, nil
}
