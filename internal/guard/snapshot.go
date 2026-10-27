// Package guard enforces what a run may change: only swag comment lines in
// .go files, docs/, internal/apispec/ and the one templated route line.
package guard

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// FileState fingerprints one file. CodeHash is set for .go files only.
type FileState struct {
	SHA256   string `json:"sha256"`
	CodeHash string `json:"code_hash,omitempty"`
}

// Snapshot fingerprints every file in a working copy, keyed by slash path.
type Snapshot struct {
	Files map[string]FileState `json:"files"`
}

// Take fingerprints every file under dir except the .git directory.
func Take(dir string) (Snapshot, error) {
	s := Snapshot{Files: map[string]FileState{}}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		s.Files[rel], err = Fingerprint(rel, data)
		return err
	})
	return s, err
}

// Fingerprint returns the state of a file at slash path rel with content data.
func Fingerprint(rel string, data []byte) (FileState, error) {
	sum := sha256.Sum256(data)
	st := FileState{SHA256: hex.EncodeToString(sum[:])}
	if strings.HasSuffix(rel, ".go") {
		var err error
		if st.CodeHash, err = CodeHash(rel, data); err != nil {
			return FileState{}, err
		}
	}
	return st, nil
}

// CodeHash hashes a Go file's token stream without comments or layout. Two
// files with the same CodeHash compile to the same program: adding, removing
// or editing comments and whitespace leaves it unchanged; any change to code,
// identifiers, literals or struct tags changes it.
func CodeHash(name string, src []byte) (string, error) {
	fset := token.NewFileSet()
	file := fset.AddFile(name, -1, len(src))
	var errs scanner.ErrorList
	var s scanner.Scanner
	s.Init(file, src, func(pos token.Position, msg string) { errs.Add(pos, msg) }, 0) // mode 0: comments are skipped

	h := sha256.New()
	// A semicolon is written only once the next token is known: Go lets it be
	// omitted before ")" and "}", so "struct{ A int }" and the multi-line form
	// (which gets an automatic semicolon) must hash the same.
	pendingSemi := false
	for {
		_, tok, lit := s.Scan()
		if tok == token.SEMICOLON {
			pendingSemi = true // inserted ("\n") or explicit (";"): same program
			continue
		}
		if pendingSemi && tok != token.RBRACE && tok != token.RPAREN && tok != token.EOF {
			fmt.Fprintf(h, "%d\n", token.SEMICOLON)
		}
		pendingSemi = false
		if tok == token.EOF {
			break
		}
		fmt.Fprintf(h, "%d %s\n", tok, lit)
	}
	if errs.Len() > 0 {
		return "", fmt.Errorf("scan %s: %w", name, errs.Err())
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
