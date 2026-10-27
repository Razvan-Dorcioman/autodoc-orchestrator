package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteDoc(t *testing.T) {
	root := t.TempDir()
	got, err := WriteDoc(root, nil, WriteDocArgs{Path: "docs/api.md", Content: "# API\r\n\r\nText.\n\n\n"})
	if err != nil {
		t.Fatalf("WriteDoc: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "docs", "api.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "# API\n\nText.\n" || got.Path != "docs/api.md" || got.Lines != 3 {
		t.Errorf("WriteDoc = %+v, file %q", got, data)
	}

	// A bare name is shorthand for docs/<name>, and a wrapping fence is dropped.
	if _, err := WriteDoc(root, nil, WriteDocArgs{Path: "overview.md", Content: "```markdown\n# Overview\n```"}); err != nil {
		t.Fatalf("WriteDoc(overview.md): %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "docs", "overview.md")); string(data) != "# Overview\n" {
		t.Errorf("overview.md = %q", data)
	}
}

func TestWriteDocRejects(t *testing.T) {
	root := t.TempDir()
	for _, args := range []WriteDocArgs{
		{Path: "", Content: "x"},
		{Path: "../README.md", Content: "# x"},
		{Path: "docs/sub/a.md", Content: "x"},
		{Path: "docs/../main.go", Content: "x"},
		{Path: "docs/a.txt", Content: "x"},
		{Path: "internal/a.md", Content: "x"},
		{Path: "docs/a.md", Content: "  \n"},
		{Path: "docs/a.md", Content: strings.Repeat("x", maxDocBytes+1)},
	} {
		if _, err := WriteDoc(root, nil, args); err == nil {
			t.Errorf("WriteDoc(%q, %d bytes) succeeded, want error", args.Path, len(args.Content))
		}
	}
	if _, err := os.Stat(filepath.Join(root, "main.go")); err == nil {
		t.Error("a rejected write created main.go")
	}
}

func TestWriteDocScope(t *testing.T) {
	root := t.TempDir()
	scope := &DocScope{}
	scope.Set("docs/api.md")
	_, err := WriteDoc(root, scope, WriteDocArgs{Path: "docs/errors.md", Content: "# Errors"})
	if err == nil || !strings.Contains(err.Error(), `"docs/api.md"`) {
		t.Errorf("out-of-scope write: err = %v, want it to name docs/api.md", err)
	}
	if _, err := os.Stat(filepath.Join(root, "docs", "errors.md")); err == nil {
		t.Error("the out-of-scope write was applied")
	}
	if _, err := WriteDoc(root, scope, WriteDocArgs{Path: "docs/api.md", Content: "# API"}); err != nil {
		t.Errorf("in-scope write: %v", err)
	}
	scope.Clear()
	if _, err := WriteDoc(root, scope, WriteDocArgs{Path: "docs/errors.md", Content: "# Errors"}); err != nil {
		t.Errorf("write after Clear: %v", err)
	}
}
