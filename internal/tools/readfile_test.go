package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadFile(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ReadFile(root, ReadFileArgs{Path: "sub/a.go"})
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got.Content != "package a\n" || got.Path != "sub/a.go" || got.Truncated {
		t.Errorf("ReadFile = %+v", got)
	}
}

func TestReadFileRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"", "../secret", "sub/../../secret", filepath.Join(root, "x")} {
		if _, err := ReadFile(root, ReadFileArgs{Path: p}); err == nil {
			t.Errorf("ReadFile(%q) succeeded, want error", p)
		}
	}
}

func TestReadFileHidesWorkdir(t *testing.T) {
	root := t.TempDir()
	_, err := ReadFile(root, ReadFileArgs{Path: "missing.go"})
	if err == nil {
		t.Fatal("ReadFile(missing.go) succeeded, want error")
	}
	if strings.Contains(err.Error(), root) {
		t.Errorf("error %q leaks the workdir path", err)
	}
}

func TestReadFileTruncates(t *testing.T) {
	root := t.TempDir()
	big := strings.Repeat("x", maxReadBytes+10)
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFile(root, ReadFileArgs{Path: "big.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated || len(got.Content) != maxReadBytes {
		t.Errorf("truncated = %v, len = %d; want true, %d", got.Truncated, len(got.Content), maxReadBytes)
	}
}
