package tools

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

// DocsDir is where write_doc writes, relative to the repository root.
const DocsDir = "docs"

// maxDocBytes bounds one page; a page is a few hundred lines at most.
const maxDocBytes = 64 << 10

type WriteDocArgs struct {
	Path    string `json:"path" jsonschema:"page path relative to the repository root, e.g. docs/api.md"`
	Content string `json:"content" jsonschema:"the complete Markdown page; it replaces the file"`
}

type WriteDocResult struct {
	Path  string `json:"path"`
	Bytes int    `json:"bytes"`
	Lines int    `json:"lines"`
}

// DocScope restricts write_doc to one page while it is set, the way
// PatchScope does for patch_comment: a turn about one page cannot overwrite
// another. The zero value allows every page.
type DocScope struct {
	mu   sync.Mutex
	page string // slash path, e.g. docs/api.md
	set  bool
}

// Set limits write_doc to page until Clear.
func (s *DocScope) Set(page string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.page, s.set = page, true
}

// Clear lifts the restriction.
func (s *DocScope) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.set = false
}

func (s *DocScope) check(page string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.set || page == s.page {
		return nil
	}
	return fmt.Errorf("write rejected and not applied: this turn writes %s, not %s. Call write_doc again with path %q", s.page, page, s.page)
}

// NewWriteDoc returns the write_doc tool, which can only write Markdown
// pages directly under docs/ in root.
func NewWriteDoc(root string, scope *DocScope) (tool.Tool, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return functiontool.New(functiontool.Config{
		Name: "write_doc",
		Description: "Write one documentation page under docs/ as Markdown. The content replaces the whole file, " +
			"so always pass the complete page.",
	}, func(_ agent.Context, args WriteDocArgs) (WriteDocResult, error) {
		return WriteDoc(absRoot, scope, args)
	})
}

// WriteDoc is write_doc without the ADK wrapper, so it can be tested directly.
func WriteDoc(absRoot string, scope *DocScope, args WriteDocArgs) (WriteDocResult, error) {
	page, err := docPath(args.Path)
	if err != nil {
		return WriteDocResult{}, err
	}
	if err := scope.check(page); err != nil {
		return WriteDocResult{}, err
	}
	content := unwrapFence(strings.ReplaceAll(args.Content, "\r\n", "\n"))
	switch {
	case strings.TrimSpace(content) == "":
		return WriteDocResult{}, errors.New("content is empty: pass the complete page")
	case len(content) > maxDocBytes:
		return WriteDocResult{}, fmt.Errorf("content is %d bytes; a page may be at most %d", len(content), maxDocBytes)
	case !utf8.ValidString(content):
		return WriteDocResult{}, errors.New("content is not valid UTF-8")
	}
	content = strings.TrimRight(content, "\n") + "\n"
	full := filepath.Join(absRoot, filepath.FromSlash(page))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return WriteDocResult{}, hidePath(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		return WriteDocResult{}, hidePath(err)
	}
	return WriteDocResult{Path: page, Bytes: len(content), Lines: strings.Count(content, "\n")}, nil
}

// docPath accepts docs/<name>.md, and a bare <name>.md as shorthand for it.
func docPath(p string) (string, error) {
	p = strings.TrimSpace(filepath.ToSlash(p))
	if p == "" {
		return "", errors.New("path is empty")
	}
	if !strings.Contains(p, "/") {
		p = DocsDir + "/" + p
	}
	clean := path.Clean(p)
	dir, name := path.Split(clean)
	if dir != DocsDir+"/" || !strings.HasSuffix(name, ".md") || name == ".md" {
		return "", fmt.Errorf("path %q: pages go directly under %s/ and end in .md, e.g. %s/api.md", p, DocsDir, DocsDir)
	}
	return clean, nil
}

// unwrapFence drops a ```markdown fence around the whole page, which small
// models add when they mean the content itself.
func unwrapFence(s string) string {
	t := strings.TrimSpace(s)
	first, rest, ok := strings.Cut(t, "\n")
	if !ok || !strings.HasSuffix(rest, "```") {
		return s
	}
	switch strings.TrimSpace(first) {
	case "```markdown", "```md":
		return strings.TrimSuffix(rest, "```")
	}
	return s
}
