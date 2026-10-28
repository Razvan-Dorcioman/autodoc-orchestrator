package verify

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"autodoc/internal/gofacts"
	"autodoc/internal/guard"
	"autodoc/internal/specroute"
	"autodoc/internal/strip"
	"autodoc/internal/tools"
)

// DocPages are the pages the docs agent writes, in order, as slash paths
// from the repository root. Gate B requires each of them.
var DocPages = []string{
	tools.DocsDir + "/overview.md",
	tools.DocsDir + "/structure.md",
	tools.DocsDir + "/request-flow.md",
	tools.DocsDir + "/api.md",
	tools.DocsDir + "/business-rules.md",
	tools.DocsDir + "/errors.md",
	tools.DocsDir + "/configuration.md",
	tools.DocsDir + "/testing.md",
}

// Mention is a fact a page must name for gate B.
type Mention struct {
	Kind   string   `json:"kind"`             // route, param, field, error_code, status, constant, env, setting, make_target, package
	Text   string   `json:"text"`             // the spelling to use, e.g. "GET /products/{id}" or "invalid_qty"
	Alts   []string `json:"alts,omitempty"`   // other spellings that count, e.g. a constant's value
	Source string   `json:"source,omitempty"` // where the fact comes from
}

// DocMentions lists, per page, the facts gate B requires it to mention. Each
// fact has one page, so a missing one goes back to the session that writes it.
func DocMentions(f *gofacts.Facts) map[string][]Mention {
	out := map[string][]Mention{}
	seen := map[string]bool{}
	add := func(page string, m Mention) {
		page = tools.DocsDir + "/" + page
		if key := page + "\x00" + m.Kind + "\x00" + m.Text; !seen[key] {
			seen[key] = true
			out[page] = append(out[page], m)
		}
	}

	for _, p := range f.Packages {
		if p.Dir != "" && p.Dir != "." && p.Dir != strip.SpecDir {
			add("structure.md", Mention{Kind: "package", Text: p.Dir, Source: p.Path})
		}
	}

	var okBodies, errBodies []string
	for _, r := range f.Routes {
		// Named as served, "GET /" for a registered "GET /{$}"; a route
		// without a method by each method its handler checks, else its path.
		methods := r.OpMethods()
		if len(methods) == 0 {
			methods = []string{""}
		}
		for _, method := range methods {
			m := Mention{Kind: "route", Text: strings.TrimSpace(method + " " + r.Path), Source: r.Registered}
			if m.Text != r.Pattern && len(methods) == 1 {
				m.Alts = []string{r.Pattern}
			}
			add("api.md", m)
		}
		if r.Body != nil {
			okBodies = append(okBodies, r.Body.Type)
		}
		for _, p := range r.Params {
			add("api.md", Mention{Kind: "param", Text: p.Name, Source: fmt.Sprintf("%s param of %s, read at %s", p.In, r.Pattern, p.Pos)})
		}
		for _, s := range r.Responses {
			if s.Status < 400 {
				okBodies = append(okBodies, s.Body)
			} else {
				errBodies = append(errBodies, s.Body)
			}
		}
	}
	fields := func(page string, bodies []string, skip map[string]bool) map[string]bool {
		in := map[string]bool{}
		for _, t := range f.Reachable(bodies...) {
			name := t.Package + "." + t.Name
			if skip[name] {
				continue
			}
			in[name] = true
			names, _ := jsonFields(t)
			for _, fld := range sortedKeys(names) {
				add(page, Mention{Kind: "field", Text: fld, Source: fmt.Sprintf("JSON field of %s (%s)", name, t.Pos)})
			}
		}
		return in
	}
	okTypes := fields("api.md", okBodies, nil)
	fields("errors.md", errBodies, okTypes)

	for _, s := range f.Responses {
		if s.Code != "" {
			add("errors.md", Mention{Kind: "error_code", Text: s.Code, Source: fmt.Sprintf("%d written by %s at %s", s.Status, s.Func, s.Pos)})
		}
	}
	for _, r := range f.ImplicitResponses {
		if r.Status >= 400 {
			add("errors.md", Mention{Kind: "status", Text: strconv.Itoa(r.Status), Source: r.Source + ": " + r.When})
		}
	}

	for _, c := range RuleConstants(f) {
		m := Mention{Kind: "constant", Text: c.Name, Source: c.Pos}
		if v := strings.Trim(c.Value, `"`); len(v) > 1 && v != c.Name {
			if _, err := strconv.ParseFloat(v, 64); err != nil {
				m.Alts = []string{v}
			}
		}
		add("business-rules.md", m)
	}

	for _, e := range f.Env {
		add("configuration.md", Mention{Kind: "env", Text: e.Name, Source: e.Pos})
	}
	for _, s := range f.ServerSettings {
		name := s.Name[strings.LastIndex(s.Name, ".")+1:]
		if name != "Addr" && name != "Handler" {
			add("configuration.md", Mention{Kind: "setting", Text: name, Source: fmt.Sprintf("%s = %s at %s", s.Name, s.Value, s.Pos)})
		}
	}

	for _, t := range f.MakeTargets {
		add("testing.md", Mention{Kind: "make_target", Text: t.Name, Source: "Makefile"})
	}
	return out
}

// MaxRuleConstants bounds the constants the business rules page must name,
// so a large module still fits one page and one prompt.
const MaxRuleConstants = 40

// RuleConstants are the constants the business rules page must name: the
// ones the route handlers reach, nearest first, then in source order, at
// most MaxRuleConstants. Constants no handler reaches (CLI flags, migration
// versions) are not business rules of the API.
func RuleConstants(f *gofacts.Facts) []gofacts.Constant {
	var out []gofacts.Constant
	for _, c := range f.Constants {
		if c.Reach > 0 && c.Package != "apispec" {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Reach < out[j].Reach })
	if len(out) > MaxRuleConstants {
		out = out[:MaxRuleConstants]
	}
	return out
}

// GateBInput is what gate B checks the docs against.
type GateBInput struct {
	Dir    string
	Facts  *gofacts.Facts
	Before guard.Snapshot // the working copy before the docs agent ran
}

// GateB checks the docs: every page exists and starts with a title, mentions
// the facts DocMentions gives it, and references only files, symbols,
// routes, Make targets and environment variables that exist; and nothing but
// the pages changed. Every check runs, so one report lists every problem.
func GateB(in GateBInput) Report {
	rep := Report{Gate: "B", Issues: []Issue{}}
	add := func(is ...Issue) { rep.Issues = append(rep.Issues, is...) }

	known := newKnown(in.Dir, in.Facts)
	mentions := DocMentions(in.Facts)
	for _, page := range DocPages {
		data, err := os.ReadFile(filepath.Join(in.Dir, filepath.FromSlash(page)))
		if err != nil {
			add(Issue{Check: "docs", Where: page, Message: "the page was not written: call write_doc with the complete page"})
			continue
		}
		doc := parseMarkdown(string(data))
		if title := firstLine(doc.lines); !strings.HasPrefix(title, "# ") {
			add(Issue{Check: "docs", Where: page + ":1", Message: "the page must start with a '# ' title"})
		}
		add(checkMentions(page, doc, mentions[page])...)
		add(known.check(page, doc)...)
	}

	cur, err := guard.Take(in.Dir)
	if err != nil {
		add(Issue{Check: "scope", Message: "snapshot the working copy: " + err.Error()})
	} else {
		add(checkDocsScope(in.Before, cur)...)
	}
	return rep
}

// checkDocsScope proves that the docs step changed nothing but the pages.
func checkDocsScope(before, cur guard.Snapshot) []Issue {
	pages := map[string]bool{}
	for _, p := range DocPages {
		pages[p] = true
	}
	paths := map[string]bool{}
	for p := range before.Files {
		paths[p] = true
	}
	for p := range cur.Files {
		paths[p] = true
	}
	var out []Issue
	for _, rel := range sortedKeys(paths) {
		b, inBefore := before.Files[rel]
		c, inCur := cur.Files[rel]
		switch {
		case pages[rel] || inBefore && inCur && b.SHA256 == c.SHA256:
		case !inCur:
			out = append(out, Issue{Check: "scope", Where: rel, Message: "file was deleted by the docs step"})
		case !inBefore:
			out = append(out, Issue{Check: "scope", Where: rel, Message: "new file outside the documentation pages"})
		default:
			out = append(out, Issue{Check: "scope", Where: rel, Message: "file changed by the docs step; only the documentation pages may change"})
		}
	}
	return out
}

func checkMentions(page string, doc mdDoc, want []Mention) []Issue {
	var missing []string
	for _, m := range want {
		if !doc.mentions(m) {
			alt := ""
			if len(m.Alts) > 0 {
				alt = " (or " + strings.Join(m.Alts, ", ") + ")"
			}
			missing = append(missing, fmt.Sprintf("%s `%s`%s, from %s", m.Kind, m.Text, alt, m.Source))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return []Issue{{Check: "coverage", Where: page, Message: "the page does not mention these facts; name each with this exact spelling and explain it:\n- " + strings.Join(missing, "\n- ")}}
}

// mdDoc is the part of a Markdown page gate B looks at.
type mdDoc struct {
	lines []string
	spans []mdRef // inline code spans outside fenced blocks
	code  []mdRef // lines of fenced code blocks
	links []mdRef // targets of [text](target) links
}

type mdRef struct {
	line int // 1-based
	text string
}

var linkRe = regexp.MustCompile(`\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)

func parseMarkdown(src string) mdDoc {
	doc := mdDoc{lines: strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")}
	fence := ""
	for i, line := range doc.lines {
		n := i + 1
		t := strings.TrimSpace(line)
		if fence != "" {
			if strings.HasPrefix(t, fence) {
				fence = ""
			} else {
				doc.code = append(doc.code, mdRef{n, line})
			}
			continue
		}
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fence = t[:3]
			continue
		}
		for _, s := range codeSpans(line) {
			doc.spans = append(doc.spans, mdRef{n, s})
		}
		for _, m := range linkRe.FindAllStringSubmatch(line, -1) {
			doc.links = append(doc.links, mdRef{n, m[1]})
		}
	}
	return doc
}

// codeSpans returns the contents of the inline code spans in one line: a run
// of backticks up to the next run of the same length.
func codeSpans(line string) []string {
	var out []string
	for i := 0; i < len(line); {
		if line[i] != '`' {
			i++
			continue
		}
		j := i
		for j < len(line) && line[j] == '`' {
			j++
		}
		tick := line[i:j]
		end := strings.Index(line[j:], tick)
		for end >= 0 && j+end+len(tick) < len(line) && line[j+end+len(tick)] == '`' {
			next := strings.Index(line[j+end+len(tick)+1:], tick)
			if next < 0 {
				end = -1
				break
			}
			end += len(tick) + 1 + next
		}
		if end < 0 {
			i = j
			continue
		}
		if s := strings.TrimSpace(line[j : j+end]); s != "" {
			out = append(out, s)
		}
		i = j + end + len(tick)
	}
	return out
}

func firstLine(lines []string) string {
	for _, l := range lines {
		if t := strings.TrimSpace(l); t != "" {
			return t
		}
	}
	return ""
}

// mentions reports whether the page names m: as a whole word, and a route as
// its method and path on one line.
func (d mdDoc) mentions(m Mention) bool {
	for _, text := range append([]string{m.Text}, m.Alts...) {
		method, p := "", text
		if m.Kind == "route" {
			if a, b, ok := strings.Cut(text, " "); ok {
				method, p = a, b
			}
		}
		for _, l := range d.lines {
			if m.Kind != "route" && hasToken(l, text, isWordChar) ||
				m.Kind == "route" && hasToken(l, p, isPathChar) && (method == "" || hasToken(l, method, isWordChar)) {
				return true
			}
		}
	}
	return false
}

func isWordChar(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

func isPathChar(b byte) bool {
	return isWordChar(b) || strings.IndexByte("/{}.-$", b) >= 0
}

// hasToken reports whether tok occurs in s with no inner character on either side.
func hasToken(s, tok string, inner func(byte) bool) bool {
	for i := 0; ; {
		j := strings.Index(s[i:], tok)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(tok)
		if (start == 0 || !inner(s[start-1])) && (end == len(s) || !inner(s[end])) {
			return true
		}
		i = start + 1
	}
}

// known is what the docs may reference: the working copy's files and their
// declarations, the routes, Make targets and environment variables.
type known struct {
	dir      string
	facts    *gofacts.Facts
	decls    map[string]map[string]bool // .go file -> normalized declared names
	pkgFiles map[string][]string        // package name -> its .go files
	targets  map[string]bool
	envNames map[string]bool     // env vars, names in the Makefile or Dockerfile, constants
	byBase   map[string][]string // file name -> the files with that name
}

func newKnown(dir string, f *gofacts.Facts) *known {
	k := &known{dir: dir, facts: f, decls: map[string]map[string]bool{}, pkgFiles: map[string][]string{}, targets: map[string]bool{}, envNames: map[string]bool{}, byBase: map[string][]string{}}
	for _, p := range f.Packages {
		if p.Name != "main" {
			k.pkgFiles[p.Name] = append(k.pkgFiles[p.Name], p.Files...)
		}
	}
	for _, t := range f.MakeTargets {
		k.targets[t.Name] = true
	}
	for _, e := range f.Env {
		k.envNames[e.Name] = true
	}
	for _, c := range f.Constants {
		k.envNames[c.Name] = true
		k.envNames[strings.Trim(c.Value, `"`)] = true
	}
	if snap, err := guard.Take(dir); err == nil {
		for rel := range snap.Files {
			k.byBase[path.Base(rel)] = append(k.byBase[path.Base(rel)], rel)
		}
	}
	for _, name := range []string{"Makefile", "Dockerfile"} {
		data, _ := os.ReadFile(filepath.Join(dir, name))
		for _, w := range upperRe.FindAllString(string(data), -1) {
			k.envNames[w] = true
		}
	}
	return k
}

var (
	upperRe    = regexp.MustCompile(`\b[A-Z][A-Z0-9_]*\b`)
	routeRe    = regexp.MustCompile(`^(GET|HEAD|POST|PUT|PATCH|DELETE|OPTIONS|CONNECT|TRACE) (/\S*)$`)
	fileRe     = regexp.MustCompile(`^((?:[\w.\-]+/)*(?:[\w.\-]+\.(?:go|md|json|ya?ml|mod|sum|txt|toml|sh|sql|proto|env|example)|Makefile|Dockerfile))(?::(.+))?$`)
	qualRe     = regexp.MustCompile(`^\(?\*?([a-z]\w*)\.(.+)$`)
	lineRe     = regexp.MustCompile(`^(\d+)(?:-(\d+))?$`)
	makeRe     = regexp.MustCompile(`(?:^|[\s;&|(])make((?:\s+[^\s;&|)]+)+)`)
	envRefRe   = regexp.MustCompile(`\$\{?([A-Z][A-Z0-9_]*)\}?|(?:^|\s)([A-Z][A-Z0-9_]*)=`)
	upperSpan  = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	targetRe   = regexp.MustCompile(`^[\w.\-/]+$`)
	methodRe   = regexp.MustCompile(`^(GET|HEAD|POST|PUT|PATCH|DELETE|OPTIONS|CONNECT|TRACE)$`)
	toolchain  = regexp.MustCompile(`^(GO|CGO_)[A-Z0-9_]*$`)
	shellNames = map[string]bool{"PATH": true, "HOME": true, "PWD": true, "USER": true, "SHELL": true, "TMPDIR": true}
)

// check returns an issue per reference on the page that does not resolve.
func (k *known) check(page string, doc mdDoc) []Issue {
	var out []Issue
	add := func(line int, format string, args ...any) {
		out = append(out, Issue{Check: "reference", Where: fmt.Sprintf("%s:%d", page, line), Message: fmt.Sprintf(format, args...)})
	}
	for _, s := range doc.spans {
		if msg := k.span(page, s.text); msg != "" {
			add(s.line, "`%s`: %s", s.text, msg)
		}
	}
	for _, r := range append(append([]mdRef{}, doc.spans...), doc.code...) {
		for _, msg := range k.commands(r.text) {
			add(r.line, "%s", msg)
		}
	}
	// Broken links are one issue per page: models tend to repeat one mistake
	// (linking code by its path from the root) on every line.
	var broken []string
	first := 0
	for _, l := range doc.links {
		target := l.text
		if strings.Contains(target, "://") || strings.HasPrefix(target, "#") || strings.HasPrefix(target, "mailto:") {
			continue
		}
		target, _, _ = strings.Cut(target, "#")
		rel := path.Join(path.Dir(page), target)
		if strings.HasPrefix(target, "/") {
			rel = strings.TrimPrefix(path.Clean(target), "/")
		}
		if _, err := os.Stat(filepath.Join(k.dir, filepath.FromSlash(rel))); err != nil || strings.HasPrefix(rel, "../") {
			if first == 0 {
				first = l.line
			}
			broken = append(broken, fmt.Sprintf("line %d: (%s)", l.line, l.text))
		}
	}
	if len(broken) > 0 {
		add(first, "%d link(s) do not resolve from %s/: %s. Remove each of these links and keep its text: a citation is not a link, "+
			"and it starts with a file of the repository, in backticks, e.g. `go.mod` or `path/from/root.go:Symbol`; "+
			"use links only for the other pages, e.g. [API](api.md)", len(broken), tools.DocsDir, strings.Join(broken, ", "))
	}
	return out
}

// span checks one inline code span and returns why it does not resolve, or "".
func (k *known) span(page, s string) string {
	if m := routeRe.FindStringSubmatch(s); m != nil {
		return k.route(m[1], m[2])
	}
	if m := fileRe.FindStringSubmatch(s); m != nil {
		return k.file(m[1], m[2])
	}
	if m := qualRe.FindStringSubmatch(s); m != nil {
		files, ok := k.pkgFiles[m[1]]
		if !ok {
			return "" // not a package of the module: a variable or the standard library
		}
		sym := normalizeSymbol(m[2], "")
		for _, f := range files {
			if k.declared(f)[sym] {
				return ""
			}
		}
		return fmt.Sprintf("package %s declares no %s; cite symbols as they are declared, e.g. `%s`", m[1], sym, k.example(files))
	}
	if upperSpan.MatchString(s) && (strings.Contains(s, "_") || page == tools.DocsDir+"/configuration.md") && !k.envName(s) && !methodRe.MatchString(s) {
		return k.unknownEnv(s)
	}
	return ""
}

// commands checks make targets and environment variables used in code.
func (k *known) commands(text string) []string {
	var out []string
	for _, m := range makeRe.FindAllStringSubmatch(text, -1) {
		for _, w := range strings.Fields(m[1]) {
			if strings.HasPrefix(w, "-") || strings.Contains(w, "=") {
				continue
			}
			w = strings.Trim(w, "`'\".,:")
			if w == "" || !targetRe.MatchString(w) {
				break
			}
			if !k.targets[w] {
				out = append(out, fmt.Sprintf("make target %q does not exist; the Makefile has %s", w, k.targetList()))
			}
		}
	}
	for _, m := range envRefRe.FindAllStringSubmatch(text, -1) {
		name := m[1] + m[2]
		if !k.envName(name) {
			out = append(out, fmt.Sprintf("`%s`: %s", name, k.unknownEnv(name)))
		}
	}
	return out
}

func (k *known) envName(s string) bool {
	return k.envNames[s] || toolchain.MatchString(s) || shellNames[s]
}

func (k *known) unknownEnv(name string) string {
	var env []string
	for _, e := range k.facts.Env {
		env = append(env, e.Name)
	}
	list := "none"
	if len(env) > 0 {
		list = strings.Join(env, ", ")
	}
	return fmt.Sprintf("%s is not an environment variable the code reads, nor a name in the Makefile, Dockerfile or constants; the code reads %s", name, list)
}

func (k *known) targetList() string {
	if len(k.targets) == 0 {
		return "no targets"
	}
	return strings.Join(sortedKeys(k.targets), ", ")
}

// route reports whether method and path (a concrete one is fine) reach a
// registered route or the spec route.
func (k *known) route(method, p string) string {
	p, _, _ = strings.Cut(p, "?")
	patterns := []string{specroute.Pattern}
	for _, r := range k.facts.Routes {
		patterns = append(patterns, r.Pattern)
		if method+" "+p == r.Pattern {
			return ""
		}
		m := r.Method
		if m == "" || m == method || m == "GET" && method == "HEAD" {
			if pathMatches(r.Path, p) {
				return ""
			}
		}
	}
	if method == "GET" && p == strings.TrimPrefix(specroute.Pattern, "GET ") {
		return ""
	}
	return "no route serves it; the routes are " + strings.Join(patterns, ", ")
}

// pathMatches matches a concrete or templated path against a route path in
// OpenAPI form, where {name} is one segment and {name...} the rest.
func pathMatches(route, p string) bool {
	rs, ps := strings.Split(route, "/"), strings.Split(p, "/")
	for i, seg := range rs {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "...}") {
			return true
		}
		if i >= len(ps) {
			return false
		}
		if !strings.HasPrefix(seg, "{") && seg != ps[i] {
			return false
		}
	}
	return len(rs) == len(ps)
}

// file checks a file citation: the file exists and, when given, the symbol
// is declared in it, the Make target exists or the line is in range.
func (k *known) file(rel, sym string) string {
	rel = path.Clean(rel)
	// A bare file name is fine where only one file has it, as in a list of a package's files.
	if !strings.Contains(rel, "/") {
		if c := k.byBase[rel]; len(c) == 1 {
			rel = c[0]
		}
	}
	var data []byte
	err := fmt.Errorf("outside the repository")
	if !strings.HasPrefix(rel, "../") {
		data, err = os.ReadFile(filepath.Join(k.dir, filepath.FromSlash(rel)))
	}
	if err != nil {
		return fmt.Sprintf("file %s does not exist; cite files by their path from the repository root", rel)
	}
	if sym == "" {
		return ""
	}
	if m := lineRe.FindStringSubmatch(sym); m != nil {
		n := strings.Count(string(data), "\n") + 1
		for _, g := range m[1:] {
			if v, _ := strconv.Atoi(g); g != "" && (v < 1 || v > n) {
				return fmt.Sprintf("%s has %d lines", rel, n)
			}
		}
		return ""
	}
	switch {
	case strings.HasSuffix(rel, ".go"):
		decls := k.declared(rel)
		if decls == nil {
			return fmt.Sprintf("%s does not parse as Go", rel)
		}
		if !decls[normalizeSymbol(sym, k.pkgName(rel))] {
			return fmt.Sprintf("%s declares no %s; it declares %s", rel, sym, declList(decls))
		}
	case rel == "Makefile":
		if !k.targets[sym] {
			return fmt.Sprintf("Makefile has no target %s; it has %s", sym, k.targetList())
		}
	}
	return ""
}

func (k *known) pkgName(rel string) string {
	for _, p := range k.facts.Packages {
		for _, f := range p.Files {
			if f == rel {
				return p.Name
			}
		}
	}
	return ""
}

func (k *known) example(files []string) string {
	for _, f := range files {
		for _, d := range sortedKeys(k.declared(f)) {
			if !strings.Contains(d, ".") {
				return f + ":" + d
			}
		}
	}
	return "file.go:Name"
}

// declared returns the names declared at the top level of a .go file, nil if
// it does not parse: funcs, types, consts and vars as Name, methods and
// struct fields as T.Name and methods also as Name.
func (k *known) declared(rel string) map[string]bool {
	if d, ok := k.decls[rel]; ok {
		return d
	}
	var decls map[string]bool
	src, err := os.ReadFile(filepath.Join(k.dir, filepath.FromSlash(rel)))
	if err == nil {
		decls = goDecls(src)
	}
	k.decls[rel] = decls
	return decls
}

func goDecls(src []byte) map[string]bool {
	file, err := parser.ParseFile(token.NewFileSet(), "", src, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	out := map[string]bool{}
	for _, d := range file.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			out[d.Name.Name] = true
			if d.Recv != nil && len(d.Recv.List) == 1 {
				out[recvName(d.Recv.List[0].Type)+"."+d.Name.Name] = true
			}
		case *ast.GenDecl:
			for _, s := range d.Specs {
				switch s := s.(type) {
				case *ast.TypeSpec:
					out[s.Name.Name] = true
					var list *ast.FieldList
					switch t := s.Type.(type) {
					case *ast.StructType:
						list = t.Fields
					case *ast.InterfaceType:
						list = t.Methods
					}
					if list != nil {
						for _, fl := range list.List {
							for _, n := range fl.Names {
								out[s.Name.Name+"."+n.Name] = true
							}
						}
					}
				case *ast.ValueSpec:
					for _, n := range s.Names {
						out[n.Name] = true
					}
				}
			}
		}
	}
	return out
}

func recvName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return recvName(t.X)
	case *ast.IndexExpr:
		return recvName(t.X)
	case *ast.IndexListExpr:
		return recvName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return ""
}

// normalizeSymbol turns the ways a symbol gets cited into the form declared
// uses: "(*T).M", "T.M()", "pkg.Name" and "writeError(w, …)" become "T.M",
// "T.M", "Name" and "writeError".
func normalizeSymbol(s, pkg string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "("); i > 0 {
		s = s[:i]
	}
	s = strings.NewReplacer("(", "", ")", "", "*", "").Replace(s)
	if pkg != "" {
		s = strings.TrimPrefix(s, pkg+".")
	}
	return s
}

func declList(decls map[string]bool) string {
	names := sortedKeys(decls)
	const max = 20
	if len(names) > max {
		return strings.Join(names[:max], ", ") + fmt.Sprintf(" and %d more", len(names)-max)
	}
	return strings.Join(names, ", ")
}
