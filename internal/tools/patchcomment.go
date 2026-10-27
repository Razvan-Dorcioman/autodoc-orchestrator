package tools

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"autodoc/internal/guard"
	"autodoc/internal/strip"
)

// maxAnnotationLines bounds one call; a handler needs a dozen or two.
const maxAnnotationLines = 60

type PatchCommentArgs struct {
	File  string   `json:"file" jsonschema:"Go file relative to the repository root, e.g. internal/httpapi/handlers.go"`
	Func  string   `json:"func" jsonschema:"top-level function or method, e.g. main or (*handlers).product; a package prefix such as httpapi. is allowed"`
	Lines []string `json:"lines" jsonschema:"the complete set of annotation lines for this function, each starting with '// @', e.g. '// @Summary Get a product'"`
}

type PatchCommentResult struct {
	File     string `json:"file"`
	Func     string `json:"func"`
	Inserted int    `json:"inserted"`
	Replaced int    `json:"replaced,omitempty"` // earlier annotation lines of this function that were removed
	Comment  string `json:"comment"`            // the function's doc comment after the patch
}

// PatchScope restricts patch_comment to one function while it is set. The
// agent sets it for a turn about one handler, so a call that names another
// function (a model copying the func from an earlier call) is rejected with a
// message the model can act on, instead of overwriting that function's
// annotations. The zero value allows every function.
type PatchScope struct {
	mu   sync.Mutex
	file string // slash-separated, relative to the repository root
	fn   string // canonical form: "main", "Name" or "(*T).Name"
	set  bool
}

// Set limits patch_comment to fn in file until Clear.
func (s *PatchScope) Set(file, fn string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.file, s.fn, s.set = filepath.ToSlash(filepath.Clean(file)), fn, true
}

// Clear lifts the restriction.
func (s *PatchScope) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.set = false
}

// check reports why a patch of fn in file is out of scope; fn is the resolved name.
func (s *PatchScope) check(file, fn string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	file = filepath.ToSlash(filepath.Clean(file))
	if !s.set || file == s.file && fn == s.fn {
		return nil
	}
	return fmt.Errorf("patch rejected and not applied: this turn annotates %s in %s, not %s in %s. "+
		"Call patch_comment again with file %q and func %q; the other function was left unchanged", s.fn, s.file, fn, file, s.file, s.fn)
}

// NewPatchComment returns the patch_comment tool, scoped to root. A non-nil
// scope further limits which function it may patch.
func NewPatchComment(root string, scope *PatchScope) (tool.Tool, error) {
	p, err := newPatcher(root)
	if err != nil {
		return nil, err
	}
	p.scope = scope
	return functiontool.New(functiontool.Config{
		Name: "patch_comment",
		Description: "Set the swag annotation lines ('// @...') of one top-level function. The lines are inserted directly above " +
			"the func line, after any existing doc comment; annotation lines the function already has are replaced, so calling " +
			"it again for the same function overwrites the previous set. Code cannot be changed: any other kind of line is rejected.",
	}, func(_ agent.Context, args PatchCommentArgs) (PatchCommentResult, error) {
		return p.patch(args)
	})
}

// patcher serializes patches, since the model may issue several calls at once.
type patcher struct {
	mu    sync.Mutex
	root  string
	scope *PatchScope // optional
}

func newPatcher(root string) (*patcher, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return &patcher{root: abs}, nil
}

// PatchComment is patch_comment without the ADK wrapper, so it can be tested directly.
func PatchComment(absRoot string, args PatchCommentArgs) (PatchCommentResult, error) {
	p := &patcher{root: absRoot}
	return p.patch(args)
}

func (p *patcher) patch(args PatchCommentArgs) (PatchCommentResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := checkLines(args.Lines); err != nil {
		return PatchCommentResult{}, err
	}
	full, src, err := readGo(p.root, args.File)
	if err != nil {
		return PatchCommentResult{}, err
	}
	out, res, err := applyPatch(args.File, src, args.Func, args.Lines)
	if err != nil {
		return PatchCommentResult{}, err
	}
	if err := p.scope.check(args.File, res.Func); err != nil {
		return PatchCommentResult{}, err
	}
	if err := sameCode(args.File, src, out); err != nil {
		return PatchCommentResult{}, err // nothing was written
	}
	if err := os.WriteFile(full, out, 0o644); err != nil {
		return PatchCommentResult{}, fmt.Errorf("write %s: %w", args.File, err)
	}
	return res, nil
}

// checkLines accepts only single-line "// @Word…" comments.
func checkLines(lines []string) error {
	if len(lines) == 0 {
		return errors.New("lines is empty: pass the complete set of annotation lines")
	}
	if len(lines) > maxAnnotationLines {
		return fmt.Errorf("%d lines is more than the %d allowed in one call", len(lines), maxAnnotationLines)
	}
	for i, l := range lines {
		switch {
		case strings.ContainsAny(l, "\r\n"):
			return fmt.Errorf("line %d contains a line break; pass one annotation per list element", i+1)
		case !strings.HasPrefix(l, "// @") || len(l) == len("// @") || !isLetter(l[len("// @")]):
			return fmt.Errorf("line %d %q is not a swag annotation: every line must start with '// @' followed by an attribute name such as Summary, Param or Router; code and other comments cannot be changed", i+1, l)
		}
	}
	return nil
}

func isLetter(b byte) bool { return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' }

// readGo resolves a non-test .go file under root and reads it.
func readGo(absRoot, rel string) (string, []byte, error) {
	if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
		return "", nil, fmt.Errorf("file %q is not a non-test .go file", rel)
	}
	full, err := resolve(absRoot, rel)
	if err != nil {
		return "", nil, err
	}
	src, err := os.ReadFile(full)
	if err != nil {
		return "", nil, fmt.Errorf("read %s: %w", rel, hidePath(err))
	}
	return full, src, nil
}

// applyPatch removes the function's existing annotation lines from its doc
// comment and inserts lines directly above its func line.
func applyPatch(name string, src []byte, fn string, lines []string) ([]byte, PatchCommentResult, error) {
	fset := token.NewFileSet()
	f, fd, err := findFunc(fset, name, src, fn)
	if err != nil {
		return nil, PatchCommentResult{}, err
	}
	tf := fset.File(f.Pos())
	funcOff := tf.Offset(fd.Type.Func)
	lineStart := bytes.LastIndexByte(src[:funcOff], '\n') + 1
	if len(bytes.TrimSpace(src[lineStart:funcOff])) > 0 {
		return nil, PatchCommentResult{}, fmt.Errorf("%s in %s does not start its line; annotations can only go on a line of their own above it", fn, name)
	}

	// Drop earlier annotations in the doc comment, keeping everything else.
	var drop [][2]int
	if fd.Doc != nil {
		for _, c := range fd.Doc.List {
			if !strip.IsAnnotation(c.Text) {
				continue
			}
			off := tf.Offset(c.Slash)
			start := bytes.LastIndexByte(src[:off], '\n') + 1
			if len(bytes.TrimSpace(src[start:off])) > 0 {
				continue // trails code on the same line; sameCode would reject removing it
			}
			end := tf.Offset(c.End())
			if i := bytes.IndexByte(src[end:], '\n'); i >= 0 {
				end += i + 1
			}
			drop = append(drop, [2]int{start, end})
		}
	}

	nl := "\n"
	if bytes.Contains(src, []byte("\r\n")) {
		nl = "\r\n"
	}
	indent := string(src[lineStart:funcOff])
	var out bytes.Buffer
	last := 0
	for _, d := range drop {
		out.Write(src[last:d[0]])
		last = d[1]
	}
	out.Write(src[last:lineStart])
	for _, l := range lines {
		out.WriteString(indent + l + nl)
	}
	out.Write(src[lineStart:])

	res := PatchCommentResult{File: name, Func: funcName(fd), Inserted: len(lines), Replaced: len(drop)}
	// Report the doc comment as it now reads.
	if _, fd2, err := findFunc(token.NewFileSet(), name, out.Bytes(), fn); err == nil && fd2.Doc != nil {
		var b strings.Builder
		for _, c := range fd2.Doc.List {
			b.WriteString(c.Text + "\n")
		}
		res.Comment = b.String()
	}
	return out.Bytes(), res, nil
}

// sameCode rejects a patch that would change the file's code hash.
func sameCode(name string, before, after []byte) error {
	h1, err := guard.CodeHash(name, before)
	if err != nil {
		return err
	}
	h2, err := guard.CodeHash(name, after)
	if err != nil {
		return fmt.Errorf("patch rejected and not applied: the result does not scan: %w", err)
	}
	if h1 != h2 {
		return fmt.Errorf("patch rejected and not applied: it would change the code of %s, not only comments", name)
	}
	return nil
}

// findFunc parses src and returns the top-level func declaration named fn:
// "main", "Name", "(*T).Name" or "(T).Name", optionally prefixed with the
// package name ("httpapi.(*handlers).product", as facts.json writes it).
func findFunc(fset *token.FileSet, name string, src []byte, fn string) (*ast.File, *ast.FuncDecl, error) {
	f, err := parser.ParseFile(fset, name, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", name, err)
	}
	want := strings.TrimPrefix(strings.TrimSpace(fn), f.Name.Name+".")
	var names []string
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		got := funcName(fd)
		if got == want {
			return f, fd, nil
		}
		names = append(names, got)
	}
	return nil, nil, fmt.Errorf("no top-level func %q in %s; it declares: %s", fn, name, strings.Join(names, ", "))
}

// FuncSource returns the source of a top-level function, doc comment included.
func FuncSource(absRoot, rel, fn string) (string, error) {
	_, src, err := readGo(absRoot, rel)
	if err != nil {
		return "", err
	}
	fset := token.NewFileSet()
	f, fd, err := findFunc(fset, rel, src, fn)
	if err != nil {
		return "", err
	}
	tf := fset.File(f.Pos())
	start := fd.Pos()
	if fd.Doc != nil {
		start = fd.Doc.Pos()
	}
	return string(src[tf.Offset(start):tf.Offset(fd.End())]), nil
}

// funcName is "Name" for functions and "(*Recv).Name" / "(Recv).Name" for
// methods, the form gofacts uses.
func funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	t := fd.Recv.List[0].Type
	star := ""
	if s, ok := t.(*ast.StarExpr); ok {
		star, t = "*", s.X
	}
	switch x := t.(type) {
	case *ast.IndexExpr:
		t = x.X
	case *ast.IndexListExpr:
		t = x.X
	}
	recv := "?"
	if id, ok := t.(*ast.Ident); ok {
		recv = id.Name
	}
	return "(" + star + recv + ")." + fd.Name.Name
}
