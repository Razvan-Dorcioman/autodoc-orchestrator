package verify

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"autodoc/internal/gofacts"
)

// Claim is one line of a docs page with the code it cites: what the judge
// fact-checks in one model call.
type Claim struct {
	Page      string     `json:"page"`
	Line      int        `json:"line"`
	Text      string     `json:"text"`
	Context   string     `json:"context,omitempty"`   // the section heading, and the parent list item or table header
	Inherited bool       `json:"inherited,omitempty"` // the citations are the parent item's or the section's
	Citations []Citation `json:"citations"`
}

// Citation is a reference on a claim's line and the source it resolves to.
type Citation struct {
	Ref     string `json:"ref"`  // as written, e.g. "internal/shop/orders.go:(*Store).Get" or "GET /orders/{id}"
	File    string `json:"file"` // slash path from the repository root
	Snippet string `json:"-"`    // the cited code, with line numbers
}

// PageClaims is what SplitClaims finds on one page.
type PageClaims struct {
	Claims  []Claim
	Uncited []int // lines with prose but no citation that resolves to code
}

// maxSnippet bounds the code sent for one citation; maxWholeFile is the
// largest file cited without a symbol that is sent whole.
const (
	maxSnippet   = 8 << 10
	maxWholeFile = 4 << 10
)

var (
	listRe     = regexp.MustCompile(`^(\s*)(?:[-*+]|\d+[.)])\s+`)
	ruleRe     = regexp.MustCompile(`^\s*\|?\s*:?-{3,}`)
	proseRe    = regexp.MustCompile(`[A-Za-z]{2,}`)
	makeSpanRe = regexp.MustCompile(`^make ([\w.\-/]+)$`)
)

// SplitClaims splits the docs pages under dir into claims: each line outside
// code blocks and headings that cites code becomes one claim, with the cited
// code as its snippet. A citation is `file:symbol`, `file:line`, a module's
// `pkg.Symbol`, `make target` (its recipe) or `METHOD /path` (its handler).
// A line without one inherits its parent list item's, else its section
// heading's. A line with prose but nothing to check it against is listed as
// uncited; a line that is only references (gate B checked those) is neither.
func SplitClaims(dir string, f *gofacts.Facts) (map[string]PageClaims, error) {
	k := newKnown(dir, f)
	out := map[string]PageClaims{}
	for _, page := range DocPages {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(page)))
		if err != nil {
			return nil, err
		}
		out[page] = k.claims(page, string(data))
	}
	return out, nil
}

// block is a heading or list item whose citations the lines under it inherit.
type block struct {
	level int // heading level, or list indent
	text  string
	cites []Citation
}

func (k *known) claims(page, src string) PageClaims {
	var pc PageClaims
	var headings, items []block
	header := "" // the current table's header row
	fence := ""
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	for i, line := range lines {
		n := i + 1
		t := strings.TrimSpace(line)
		if fence != "" {
			if strings.HasPrefix(t, fence) {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fence = t[:3]
			continue
		}
		if t == "" {
			continue
		}
		if level := len(t) - len(strings.TrimLeft(t, "#")); level > 0 {
			for len(headings) > 0 && headings[len(headings)-1].level >= level {
				headings = headings[:len(headings)-1]
			}
			headings = append(headings, block{level, t, k.headingCitations(t)})
			items, header = nil, ""
			continue
		}
		if strings.HasPrefix(t, "|") {
			if ruleRe.MatchString(t) {
				continue
			}
			if i+1 < len(lines) && ruleRe.MatchString(strings.TrimSpace(lines[i+1])) {
				header = t
				continue
			}
		} else {
			header = ""
		}

		c := Claim{Page: page, Line: n, Text: t, Citations: k.citations(line)}
		var context []string
		if len(headings) > 0 {
			context = append(context, headings[len(headings)-1].text)
		}
		if m := listRe.FindStringSubmatch(line); m != nil {
			indent := len(strings.ReplaceAll(m[1], "\t", "    "))
			for len(items) > 0 && items[len(items)-1].level >= indent {
				items = items[:len(items)-1]
			}
			if len(items) > 0 {
				parent := items[len(items)-1]
				context = append(context, parent.text)
				if len(c.Citations) == 0 && len(parent.cites) > 0 {
					c.Citations, c.Inherited = parent.cites, true
				}
			}
			items = append(items, block{indent, t, c.Citations})
		} else if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			items = nil
		}
		if header != "" {
			context = append(context, header)
		}
		if len(c.Citations) == 0 {
			for j := len(headings) - 1; j >= 0; j-- {
				if len(headings[j].cites) > 0 {
					c.Citations, c.Inherited = headings[j].cites, true
					break
				}
			}
		}
		c.Context = strings.Join(context, "\n")

		if len(proseRe.FindAllString(prose(line), -1)) < 3 && !strings.HasPrefix(t, "|") {
			continue
		}
		if len(c.Citations) == 0 {
			pc.Uncited = append(pc.Uncited, n)
			continue
		}
		pc.Claims = append(pc.Claims, c)
	}
	return pc
}

// prose is a line without its code spans, bracketed references and link targets.
func prose(line string) string {
	for _, s := range append(codeSpans(line), bracketRefs(line)...) {
		line = strings.Replace(line, s, " ", 1)
	}
	return linkRe.ReplaceAllString(line, "]")
}

var bracketRe = regexp.MustCompile(`\[([^\[\]\s]+)\]`)

// bracketRefs are the "[file.go:Symbol]" references on a line outside code
// spans, a citation style models use besides backticks; "[text](target)"
// is a link, not one.
func bracketRefs(line string) []string {
	for _, s := range codeSpans(line) {
		line = strings.Replace(line, s, strings.Repeat(" ", len(s)), 1)
	}
	var out []string
	for _, m := range bracketRe.FindAllStringSubmatchIndex(line, -1) {
		if m[1] < len(line) && line[m[1]] == '(' {
			continue
		}
		out = append(out, line[m[2]:m[3]])
	}
	return out
}

// headingCitations are a heading's code spans, or the route it names.
func (k *known) headingCitations(h string) []Citation {
	if cs := k.citations(h); len(cs) > 0 {
		return cs
	}
	if c, ok := k.cite(strings.TrimSpace(strings.Trim(h, "#` "))); ok {
		return []Citation{c}
	}
	return nil
}

// citations resolves the code references on one line; one that does not
// resolve is gate B's to report, so it is left out here.
func (k *known) citations(line string) []Citation {
	var out []Citation
	seen := map[string]bool{}
	for _, s := range append(codeSpans(line), bracketRefs(line)...) {
		c, ok := k.cite(strings.TrimSpace(strings.Trim(s, "`")))
		if !ok || seen[c.Snippet] {
			continue
		}
		seen[c.Snippet] = true
		out = append(out, c)
	}
	return out
}

func (k *known) cite(s string) (Citation, bool) {
	cite := func(rel, sym string) (Citation, bool) {
		snip := k.snippet(rel, sym)
		return Citation{Ref: s, File: rel, Snippet: snip}, snip != ""
	}
	if m := routeRe.FindStringSubmatch(s); m != nil {
		if r, ok := k.routeFor(m[1], m[2]); ok {
			file, _, _ := strings.Cut(r.HandlerPos, ":")
			_, fn, _ := strings.Cut(r.Handler, ".") // drop the package: "(*handlers).product"
			return cite(file, fn)
		}
		return Citation{}, false
	}
	if m := makeSpanRe.FindStringSubmatch(s); m != nil && k.targets[m[1]] {
		return cite("Makefile", m[1])
	}
	if m := fileRe.FindStringSubmatch(s); m != nil {
		rel := path.Clean(m[1])
		if c := k.byBase[rel]; !strings.Contains(rel, "/") && len(c) == 1 {
			rel = c[0]
		}
		sym := m[2]
		if pkg := k.pkgName(rel); pkg != "" {
			sym = strings.TrimPrefix(sym, pkg+".") // "pkg.(T).M" would not normalize
		}
		if k.file(rel, sym) != "" {
			return Citation{}, false
		}
		return cite(rel, sym)
	}
	if m := qualRe.FindStringSubmatch(s); m != nil {
		sym := normalizeSymbol(m[2], "")
		for _, f := range k.pkgFiles[m[1]] {
			if k.declared(f)[sym] {
				return cite(f, sym)
			}
		}
	}
	return Citation{}, false
}

// routeFor is the route that serves method and path, as gate B matches it.
func (k *known) routeFor(method, p string) (gofacts.Route, bool) {
	p, _, _ = strings.Cut(p, "?")
	for _, r := range k.facts.Routes {
		if method+" "+p == r.Pattern {
			return r, true
		}
	}
	for _, r := range k.facts.Routes {
		if (r.Method == "" || r.Method == method || r.Method == "GET" && method == "HEAD") && pathMatches(r.Path, p) {
			return r, true
		}
	}
	return gofacts.Route{}, false
}

// snippet is the code a citation points at: a Go declaration, or those
// around a line, followed by the package's consts, vars and types that a
// cited function uses; a Make target's rule; a line window; or a small file whole.
func (k *known) snippet(rel, sym string) string {
	src, err := os.ReadFile(filepath.Join(k.dir, filepath.FromSlash(rel)))
	if err != nil {
		return ""
	}
	var out string
	switch {
	case strings.HasSuffix(rel, ".go") && sym != "":
		parts, used := declSnippet(src, sym, k.pkgName(rel))
		if len(parts) == 0 {
			break
		}
		if used != nil {
			for _, f := range k.siblings(rel) {
				other, err := os.ReadFile(filepath.Join(k.dir, filepath.FromSlash(f)))
				if err != nil {
					continue
				}
				for _, d := range usedDecls(other, used) {
					if f != rel {
						d = "// " + f + "\n" + d
					}
					if !slices.Contains(parts, d) {
						parts = append(parts, d)
					}
				}
			}
		}
		out = strings.Join(parts, "\n\n")
	case rel == "Makefile" && sym != "" && lineRe.FindStringSubmatch(sym) == nil:
		out = makeRecipe(src, sym)
	case sym != "":
		out = lineWindow(src, sym)
	case len(src) <= maxWholeFile:
		out = numbered(src, 1)
	}
	if len(out) > maxSnippet {
		out = out[:maxSnippet] + "\n… (truncated)"
	}
	return out
}

// siblings are the non-test files of rel's package, rel first.
func (k *known) siblings(rel string) []string {
	out := []string{rel}
	for _, p := range k.facts.Packages {
		if slices.Contains(p.Files, rel) {
			for _, f := range p.Files {
				if f != rel && !strings.HasSuffix(f, "_test.go") {
					out = append(out, f)
				}
			}
		}
	}
	return out
}

// declSnippet returns the declaration sym names, or the declarations
// around the line sym gives, numbered; and when one is a function, the
// identifiers it uses.
func declSnippet(src []byte, sym, pkg string) (parts []string, used map[string]bool) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, nil
	}
	tf := fset.File(file.Pos())
	text := func(n ast.Node, doc *ast.CommentGroup) string {
		start := n.Pos()
		if doc != nil {
			start = doc.Pos()
		}
		return numbered(src[tf.Offset(start):tf.Offset(n.End())], tf.Line(start))
	}
	var fns []*ast.FuncDecl
	if m := lineRe.FindStringSubmatch(sym); m != nil {
		from, _ := strconv.Atoi(m[1])
		to := from
		if m[2] != "" {
			to, _ = strconv.Atoi(m[2])
		}
		for _, d := range file.Decls {
			if tf.Line(d.End()) < from || tf.Line(d.Pos()) > to {
				continue
			}
			switch d := d.(type) {
			case *ast.FuncDecl:
				parts, fns = append(parts, text(d, d.Doc)), append(fns, d)
			case *ast.GenDecl:
				parts = append(parts, text(d, d.Doc))
			}
		}
		if len(parts) == 0 {
			if w := lineWindow(src, sym); w != "" {
				parts = []string{w}
			}
		}
	} else {
		name := normalizeSymbol(sym, pkg)
		typ, member, _ := strings.Cut(name, ".")
		// Functions first: "T.M" names a method before the type T.
		for _, d := range file.Decls {
			if d, ok := d.(*ast.FuncDecl); ok {
				recv := ""
				if d.Recv != nil && len(d.Recv.List) == 1 {
					recv = recvName(d.Recv.List[0].Type)
				}
				if name == d.Name.Name && (recv == "" || member == "") || recv != "" && name == recv+"."+d.Name.Name {
					parts, fns = append(parts, text(d, d.Doc)), append(fns, d)
					break
				}
			}
		}
		for _, d := range file.Decls {
			if d, ok := d.(*ast.GenDecl); ok && len(parts) == 0 {
				if s := genSpec(d, name, typ, member); s != nil {
					if d.Lparen.IsValid() && tf.Line(d.End())-tf.Line(d.Pos()) <= 40 {
						parts = append(parts, text(d, d.Doc)) // the whole group, so an enum comes with its siblings
					} else {
						parts = append(parts, text(s, specDoc(s, d)))
					}
				}
			}
		}
	}
	if len(fns) > 0 {
		used = map[string]bool{}
		for _, fn := range fns {
			ast.Inspect(fn, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok {
					used[id.Name] = true
				}
				return true
			})
		}
	}
	return parts, used
}

func specDoc(s ast.Spec, d *ast.GenDecl) *ast.CommentGroup {
	switch s := s.(type) {
	case *ast.TypeSpec:
		if s.Doc != nil {
			return s.Doc
		}
	case *ast.ValueSpec:
		if s.Doc != nil {
			return s.Doc
		}
	}
	if !d.Lparen.IsValid() {
		return d.Doc
	}
	return nil
}

// genSpec finds the spec of d that declares name, or the type typ when name
// is a member of it (a field or method cited as T.Name).
func genSpec(d *ast.GenDecl, name, typ, member string) ast.Spec {
	for _, s := range d.Specs {
		switch s := s.(type) {
		case *ast.TypeSpec:
			if s.Name.Name == name || member != "" && s.Name.Name == typ {
				return s
			}
		case *ast.ValueSpec:
			for _, n := range s.Names {
				if n.Name == name {
					return s
				}
			}
		}
	}
	return nil
}

// usedDecls returns, numbered and in file order, the const, var and type
// declarations of a Go file that declare a name in used.
func usedDecls(src []byte, used map[string]bool) []string {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	tf := fset.File(file.Pos())
	var out []string
	for _, d := range file.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok == token.IMPORT {
			continue
		}
		hit := false
		for _, s := range g.Specs {
			switch s := s.(type) {
			case *ast.TypeSpec:
				hit = hit || used[s.Name.Name]
			case *ast.ValueSpec:
				for _, n := range s.Names {
					hit = hit || used[n.Name]
				}
			}
		}
		if hit {
			start := g.Pos()
			if g.Doc != nil {
				start = g.Doc.Pos()
			}
			out = append(out, numbered(src[tf.Offset(start):tf.Offset(g.End())], tf.Line(start)))
		}
	}
	return out
}

// makeRecipe returns a Make target's rule line and recipe.
func makeRecipe(src []byte, target string) string {
	lines := strings.Split(strings.ReplaceAll(string(src), "\r\n", "\n"), "\n")
	for i, l := range lines {
		names, _, ok := strings.Cut(l, ":")
		if !ok || strings.HasPrefix(l, "\t") || strings.Contains(names, "=") || !hasToken(names, target, isPathChar) {
			continue
		}
		j := i + 1
		for j < len(lines) && strings.HasPrefix(lines[j], "\t") {
			j++
		}
		return numbered([]byte(strings.Join(lines[i:j], "\n")), i+1)
	}
	return ""
}

// lineWindow returns the cited line or range with three lines either side.
func lineWindow(src []byte, sym string) string {
	m := lineRe.FindStringSubmatch(sym)
	if m == nil {
		return ""
	}
	lines := bytes.Split(src, []byte("\n"))
	from, _ := strconv.Atoi(m[1])
	to := from
	if m[2] != "" {
		to, _ = strconv.Atoi(m[2])
	}
	from, to = max(from-3, 1), min(to+3, len(lines))
	if from > to {
		return ""
	}
	return numbered(bytes.Join(lines[from-1:to], []byte("\n")), from)
}

// numbered prefixes each line of src with its line number in the file. It
// drops "// @" annotation lines: the swagger agent wrote them, so they are
// not evidence for the docs.
func numbered(src []byte, first int) string {
	var b strings.Builder
	for i, l := range strings.Split(strings.TrimRight(strings.ReplaceAll(string(src), "\r\n", "\n"), "\n"), "\n") {
		if t := strings.TrimSpace(l); strings.HasPrefix(t, "// @") || strings.HasPrefix(t, "//@") {
			continue
		}
		fmt.Fprintf(&b, "%4d  %s\n", first+i, l)
	}
	return strings.TrimRight(b.String(), "\n")
}
