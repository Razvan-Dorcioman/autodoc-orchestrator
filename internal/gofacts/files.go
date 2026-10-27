package gofacts

import (
	"bufio"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// readMakefile lists rule targets (not variables or .PHONY) with their
// prerequisites and recipe lines. A missing Makefile yields no targets.
func readMakefile(path string) ([]MakeTarget, error) {
	lines, err := readLines(path)
	if err != nil || lines == nil {
		return nil, err
	}
	var out []MakeTarget
	var cur *MakeTarget
	for _, line := range lines {
		if strings.HasPrefix(line, "\t") {
			if cur != nil {
				cur.Recipe = append(cur.Recipe, strings.TrimSpace(line))
			}
			continue
		}
		cur = nil
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		name, deps, ok := strings.Cut(trimmed, ":")
		if !ok || strings.HasPrefix(deps, "=") || strings.ContainsAny(name, "=$ ") || strings.HasPrefix(name, ".") {
			continue // variable assignment, .PHONY or similar
		}
		t := MakeTarget{Name: name}
		if f := strings.Fields(deps); len(f) > 0 {
			t.Deps = f
		}
		out = append(out, t)
		cur = &out[len(out)-1]
	}
	return out, nil
}

// readDockerfile returns its instructions, without comments or blank lines.
func readDockerfile(path string) ([]string, error) {
	lines, err := readLines(path)
	var out []string
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out, err
}

// readLines returns nil, nil when the file does not exist.
func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines, sc.Err()
}

// readTests lists Test, Benchmark, Fuzz and Example functions in _test.go files.
func readTests(root string) ([]Test, error) {
	var out []Test
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || !isTestName(fd.Name.Name) {
				continue
			}
			out = append(out, Test{
				Name:    fd.Name.Name,
				Package: file.Name.Name,
				Pos:     filepath.ToSlash(rel) + ":" + strconv.Itoa(fset.Position(fd.Pos()).Line),
			})
		}
		return nil
	})
	return out, err
}

func isTestName(name string) bool {
	for _, p := range []string{"Test", "Benchmark", "Fuzz", "Example"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
