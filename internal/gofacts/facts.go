// Package gofacts extracts facts.json from the target's Go source: routes,
// inputs, responses and error codes, constants, types, configuration, build
// targets and tests. It type-checks the code with go/packages, so constants
// and HTTP statuses are resolved values, not text matches. Nothing in the
// target is executed.
//
// The facts are the grounding source for the agents and the deterministic
// gates. Every fact carries a file:line position so it can be cited.
package gofacts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/format"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Facts is the content of facts.json.
type Facts struct {
	Module            string             `json:"module"`
	GoVersion         string             `json:"go_version"`
	Packages          []Package          `json:"packages"`
	Mains             []Main             `json:"mains"`
	Routes            []Route            `json:"routes"`
	Responses         []ResponseSite     `json:"responses"` // every place a status is written, handlers and middleware
	ImplicitResponses []ImplicitResponse `json:"implicit_responses"`
	Constants         []Constant         `json:"constants"`
	Vars              []Var              `json:"vars"`
	Types             []Type             `json:"types"`
	Functions         []Function         `json:"functions"`
	Env               []EnvVar           `json:"env"`
	ServerSettings    []Setting          `json:"server_settings"`
	MakeTargets       []MakeTarget       `json:"make_targets"`
	Dockerfile        []string           `json:"dockerfile,omitempty"`
	Tests             []Test             `json:"tests"`
}

type Package struct {
	Path  string   `json:"path"`
	Name  string   `json:"name"`
	Dir   string   `json:"dir"`
	Files []string `json:"files"`
	Doc   string   `json:"doc,omitempty"`
}

// Main is a command the module builds: a main package and its func main.
type Main struct {
	Package      string `json:"package"`
	File         string `json:"file"`
	Pos          string `json:"pos"`
	ServesRoutes bool   `json:"serves_routes"` // registers a route or imports, directly or not, a package that does
}

type Route struct {
	Pattern    string         `json:"pattern"` // as registered, e.g. "GET /products/{id}"
	Method     string         `json:"method,omitempty"`
	Path       string         `json:"path"` // OpenAPI form, e.g. "/products/{id}"
	ExactMatch bool           `json:"exact_match,omitempty"`
	Handler    string         `json:"handler"`
	HandlerPos string         `json:"handler_pos,omitempty"`
	Registered string         `json:"registered"`
	Params     []Param        `json:"params"`
	ConstsUsed []string       `json:"consts_used,omitempty"`
	Calls      []string       `json:"calls,omitempty"` // module functions the handler calls
	Responses  []ResponseSite `json:"responses"`
}

type Param struct {
	Name string `json:"name"`
	In   string `json:"in"` // path or query
	Pos  string `json:"pos"`
}

type ResponseSite struct {
	Func       string `json:"func"`
	Pos        string `json:"pos"`
	Status     int    `json:"status"`
	StatusName string `json:"status_name"`
	Helper     string `json:"helper"`
	Code       string `json:"code,omitempty"`
	Message    string `json:"message,omitempty"`
	Body       string `json:"body,omitempty"`
}

type ImplicitResponse struct {
	Status int    `json:"status"`
	When   string `json:"when"`
	Body   string `json:"body"`
	Source string `json:"source"`
}

type Constant struct {
	Name    string `json:"name"`
	Package string `json:"package"`
	Type    string `json:"type"`
	Value   string `json:"value"`
	Pos     string `json:"pos"`
	Doc     string `json:"doc,omitempty"`
}

type Var struct {
	Name    string `json:"name"`
	Package string `json:"package"`
	Type    string `json:"type"`
	Init    string `json:"init,omitempty"`
	Pos     string `json:"pos"`
	Doc     string `json:"doc,omitempty"`
}

type Type struct {
	Name       string  `json:"name"`
	Package    string  `json:"package"`
	Underlying string  `json:"underlying"`
	Pos        string  `json:"pos"`
	Doc        string  `json:"doc,omitempty"`
	Fields     []Field `json:"fields,omitempty"`
}

type Field struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	JSON      string `json:"json,omitempty"`
	OmitEmpty bool   `json:"omitempty,omitempty"`
	Embedded  bool   `json:"embedded,omitempty"`
	Doc       string `json:"doc,omitempty"`
}

type Function struct {
	Name      string `json:"name"`
	Package   string `json:"package"`
	Signature string `json:"signature"`
	Pos       string `json:"pos"`
	Doc       string `json:"doc,omitempty"`
}

type EnvVar struct {
	Name    string `json:"name"`
	Default string `json:"default,omitempty"`
	Via     string `json:"via,omitempty"`
	Pos     string `json:"pos"`
}

type Setting struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Pos   string `json:"pos"`
}

type MakeTarget struct {
	Name   string   `json:"name"`
	Deps   []string `json:"deps,omitempty"`
	Recipe []string `json:"recipe,omitempty"`
}

type Test struct {
	Name    string `json:"name"`
	Package string `json:"package"`
	Pos     string `json:"pos"`
}

// Extract type-checks the module rooted at root and collects its facts.
func Extract(ctx context.Context, root string) (*Facts, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	pkgs, err := packages.Load(&packages.Config{
		Context: ctx,
		Dir:     absRoot,
		Fset:    fset,
		Env:     append(os.Environ(), "GOWORK=off"),
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedModule | packages.NeedImports,
	}, "./...")
	if err != nil {
		return nil, fmt.Errorf("load packages: %w", err)
	}
	var loadErrs []string
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			loadErrs = append(loadErrs, e.Error())
		}
	})
	if len(loadErrs) > 0 {
		return nil, fmt.Errorf("type-check: %s", strings.Join(loadErrs, "; "))
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no Go packages under %s", root)
	}
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].PkgPath < pkgs[j].PkgPath })

	x := &extractor{root: absRoot, fset: fset, pkgs: pkgs, decls: map[*types.Func]funcDecl{}}
	x.indexFuncs()

	f := &Facts{}
	if m := pkgs[0].Module; m != nil {
		f.Module, f.GoVersion = m.Path, m.GoVersion
	}
	x.module = f.Module
	x.collectDecls(f)
	f.Responses = x.responseSites()
	f.Routes = x.routes(f.Responses)
	f.Mains = x.mains(f.Routes)
	f.ImplicitResponses = implicitResponses(f.Routes)
	f.Env = x.env()
	f.ServerSettings = x.serverSettings()
	if f.MakeTargets, err = readMakefile(filepath.Join(absRoot, "Makefile")); err != nil {
		return nil, err
	}
	if f.Dockerfile, err = readDockerfile(filepath.Join(absRoot, "Dockerfile")); err != nil {
		return nil, err
	}
	if f.Tests, err = readTests(absRoot); err != nil {
		return nil, err
	}
	return f, nil
}

// Entrypoint returns the file declaring func main of the one command that
// serves the routes: swag reads the general API info from there. It fails
// when no command or more than one does.
func (f *Facts) Entrypoint() (string, error) {
	var files []string
	for _, m := range f.Mains {
		if m.ServesRoutes {
			files = append(files, m.File)
		}
	}
	switch len(files) {
	case 1:
		return files[0], nil
	case 0:
		return "", errors.New("no main package serves the routes, so there is no func main for the general API info; name its file with --main")
	}
	return "", fmt.Errorf("several main packages serve the routes (%s); name the one to document with --main", strings.Join(files, ", "))
}

// Reachable returns the module types named in names ("pkg.Type") and,
// transitively, the module types their fields use, each once, in visit order.
func (f *Facts) Reachable(names ...string) []Type {
	byName := map[string]Type{}
	for _, t := range f.Types {
		byName[t.Package+"."+t.Name] = t
	}
	var out []Type
	seen := map[string]bool{}
	var visit func(name string)
	visit = func(name string) {
		t, ok := byName[name]
		if !ok || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, t)
		for _, fl := range t.Fields {
			ft := strings.TrimLeft(fl.Type, "*[]")
			if !strings.Contains(ft, ".") {
				ft = t.Package + "." + ft
			}
			visit(ft)
		}
	}
	for _, name := range names {
		visit(name)
	}
	return out
}

// mains lists the main packages and whether each serves the routes.
func (x *extractor) mains(routes []Route) []Main {
	byPath := map[string]*packages.Package{}
	pkgOf := map[string]string{} // repo-relative file -> import path
	for _, p := range x.pkgs {
		byPath[p.PkgPath] = p
		for _, file := range p.GoFiles {
			pkgOf[x.rel(file)] = p.PkgPath
		}
	}
	routePkgs := map[string]bool{}
	for _, r := range routes {
		file, _, _ := strings.Cut(r.Registered, ":")
		if p, ok := pkgOf[file]; ok {
			routePkgs[p] = true
		}
	}
	var reaches func(path string, seen map[string]bool) bool
	reaches = func(path string, seen map[string]bool) bool {
		if routePkgs[path] {
			return true
		}
		p, ok := byPath[path] // only module packages: std and dependencies register no routes we know of
		if !ok || seen[path] {
			return false
		}
		seen[path] = true
		for imp := range p.Imports {
			if reaches(imp, seen) {
				return true
			}
		}
		return false
	}

	var out []Main
	for _, p := range x.pkgs {
		if p.Name != "main" {
			continue
		}
		for _, file := range p.Syntax {
			for _, d := range file.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "main" {
					pos := x.pos(fd.Pos())
					rel, _, _ := strings.Cut(pos, ":")
					out = append(out, Main{Package: p.PkgPath, File: rel, Pos: pos, ServesRoutes: reaches(p.PkgPath, map[string]bool{})})
				}
			}
		}
	}
	return out
}

type funcDecl struct {
	decl *ast.FuncDecl
	pkg  *packages.Package
}

type extractor struct {
	root   string
	module string
	fset   *token.FileSet
	pkgs   []*packages.Package
	decls  map[*types.Func]funcDecl
}

func (x *extractor) indexFuncs() {
	x.eachFuncDecl(func(p *packages.Package, fd *ast.FuncDecl) {
		if fn, ok := p.TypesInfo.Defs[fd.Name].(*types.Func); ok {
			x.decls[fn] = funcDecl{decl: fd, pkg: p}
		}
	})
}

func (x *extractor) eachFile(fn func(p *packages.Package, f *ast.File)) {
	for _, p := range x.pkgs {
		for _, f := range p.Syntax {
			fn(p, f)
		}
	}
}

func (x *extractor) eachFuncDecl(fn func(p *packages.Package, fd *ast.FuncDecl)) {
	x.eachFile(func(p *packages.Package, f *ast.File) {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				fn(p, fd)
			}
		}
	})
}

// pos renders a position as a repo-relative "path:line".
func (x *extractor) pos(p token.Pos) string {
	position := x.fset.Position(p)
	rel, err := filepath.Rel(x.root, position.Filename)
	if err != nil {
		rel = position.Filename
	}
	return fmt.Sprintf("%s:%d", filepath.ToSlash(rel), position.Line)
}

func (x *extractor) rel(path string) string {
	rel, err := filepath.Rel(x.root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

// src prints an AST node back to Go source.
func (x *extractor) src(n ast.Node) string {
	var b bytes.Buffer
	if err := format.Node(&b, x.fset, n); err != nil {
		return ""
	}
	return b.String()
}

func (x *extractor) inModule(p *types.Package) bool {
	return p != nil && (p.Path() == x.module || strings.HasPrefix(p.Path(), x.module+"/"))
}

// typeName renders a type with package names instead of import paths.
func typeName(t types.Type) string {
	return types.TypeString(t, func(p *types.Package) string { return p.Name() })
}

// FuncName is "Name" for functions and "(*Recv).Name" / "(Recv).Name" for methods.
func FuncName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	var recv string
	switch t := fd.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		recv = "*" + exprName(t.X)
	default:
		recv = exprName(t)
	}
	return "(" + recv + ")." + fd.Name.Name
}

func exprName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr: // embedded http.ResponseWriter is named ResponseWriter
		return t.Sel.Name
	case *ast.StarExpr:
		return exprName(t.X)
	case *ast.IndexExpr:
		return exprName(t.X)
	case *ast.IndexListExpr:
		return exprName(t.X)
	}
	return "?"
}

// qualifiedFunc names a module function relative to the module, e.g.
// "internal/catalog.ParseCurrency" or "(*internal/catalog.Store).Get".
func (x *extractor) qualifiedFunc(fn *types.Func) string {
	return strings.ReplaceAll(fn.FullName(), x.module+"/", "")
}

// callee returns the function or method a call invokes, if it is statically known.
func callee(info *types.Info, call *ast.CallExpr) *types.Func {
	switch f := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		fn, _ := info.Uses[f].(*types.Func)
		return fn
	case *ast.SelectorExpr:
		fn, _ := info.Uses[f.Sel].(*types.Func)
		return fn
	}
	return nil
}

// recvType is the receiver type of a method, or "" for a function.
func recvType(fn *types.Func) string {
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return ""
	}
	return types.TypeString(sig.Recv().Type(), nil)
}

func constString(info *types.Info, e ast.Expr) (string, bool) {
	tv, ok := info.Types[e]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(tv.Value), true
}

func constValue(v constant.Value) string {
	if v.Kind() == constant.String {
		return constant.StringVal(v)
	}
	return v.ExactString()
}

func docText(groups ...*ast.CommentGroup) string {
	for _, g := range groups {
		if g != nil {
			if t := strings.TrimSpace(g.Text()); t != "" {
				return t
			}
		}
	}
	return ""
}

// collectDecls collects packages, functions, constants, vars and types.
func (x *extractor) collectDecls(f *Facts) {
	for _, p := range x.pkgs {
		pkg := Package{Path: p.PkgPath, Name: p.Name}
		for _, file := range p.GoFiles {
			pkg.Files = append(pkg.Files, x.rel(file))
		}
		if len(p.GoFiles) > 0 {
			pkg.Dir = x.rel(filepath.Dir(p.GoFiles[0]))
		}
		for _, file := range p.Syntax {
			if pkg.Doc == "" {
				pkg.Doc = docText(file.Doc)
			}
		}
		f.Packages = append(f.Packages, pkg)
	}

	x.eachFile(func(p *packages.Package, file *ast.File) {
		for _, d := range file.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				f.Functions = append(f.Functions, Function{
					Name:      FuncName(d),
					Package:   p.Name,
					Signature: x.src(d.Type),
					Pos:       x.pos(d.Pos()),
					Doc:       docText(d.Doc),
				})
			case *ast.GenDecl:
				x.genDecl(f, p, d)
			}
		}
	})
}

func (x *extractor) genDecl(f *Facts, p *packages.Package, d *ast.GenDecl) {
	info := p.TypesInfo
	for _, spec := range d.Specs {
		switch s := spec.(type) {
		case *ast.ValueSpec:
			doc := docText(s.Doc, s.Comment)
			if len(d.Specs) == 1 && doc == "" {
				doc = docText(d.Doc)
			}
			for i, name := range s.Names {
				if name.Name == "_" {
					continue
				}
				switch obj := info.Defs[name].(type) {
				case *types.Const:
					f.Constants = append(f.Constants, Constant{
						Name: name.Name, Package: p.Name, Type: typeName(obj.Type()),
						Value: constValue(obj.Val()), Pos: x.pos(name.Pos()), Doc: doc,
					})
				case *types.Var:
					v := Var{Name: name.Name, Package: p.Name, Type: typeName(obj.Type()), Pos: x.pos(name.Pos()), Doc: doc}
					if len(s.Values) == len(s.Names) {
						v.Init = x.src(s.Values[i])
					}
					f.Vars = append(f.Vars, v)
				}
			}
		case *ast.TypeSpec:
			obj, ok := info.Defs[s.Name].(*types.TypeName)
			if !ok {
				continue
			}
			doc := docText(s.Doc, s.Comment)
			if len(d.Specs) == 1 && doc == "" {
				doc = docText(d.Doc)
			}
			t := Type{Name: s.Name.Name, Package: p.Name, Pos: x.pos(s.Pos()), Doc: doc}
			if st, ok := s.Type.(*ast.StructType); ok {
				t.Underlying = "struct"
				t.Fields = x.fields(st)
			} else {
				t.Underlying = typeName(obj.Type().Underlying())
			}
			f.Types = append(f.Types, t)
		}
	}
}

func (x *extractor) fields(st *ast.StructType) []Field {
	var out []Field
	for _, fl := range st.Fields.List {
		var jsonName string
		var omit bool
		if fl.Tag != nil {
			tag := strings.Trim(fl.Tag.Value, "`")
			if v, ok := reflect.StructTag(tag).Lookup("json"); ok {
				name, opts, _ := strings.Cut(v, ",")
				jsonName, omit = name, strings.Contains(","+opts+",", ",omitempty,")
			}
		}
		names := make([]string, 0, len(fl.Names))
		for _, n := range fl.Names {
			names = append(names, n.Name)
		}
		embedded := len(names) == 0
		if embedded {
			names = append(names, exprName(fl.Type))
		}
		for _, n := range names {
			out = append(out, Field{
				Name: n, Type: x.src(fl.Type), JSON: jsonName, OmitEmpty: omit, Embedded: embedded,
				Doc: docText(fl.Doc, fl.Comment),
			})
		}
	}
	return out
}

// env finds environment variables read with os.Getenv / os.LookupEnv, either
// directly or through a module helper that passes a parameter to them (for
// example envOr(key, fallback)).
func (x *extractor) env() []EnvVar {
	// Pass 1: helpers that call os.Getenv(param).
	wrappers := map[*types.Func]int{}
	x.eachFuncDecl(func(p *packages.Package, fd *ast.FuncDecl) {
		fn, _ := p.TypesInfo.Defs[fd.Name].(*types.Func)
		if fn == nil || fd.Body == nil {
			return
		}
		params := paramObjects(p.TypesInfo, fd)
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isEnvRead(callee(p.TypesInfo, call)) || len(call.Args) == 0 {
				return true
			}
			if id, ok := ast.Unparen(call.Args[0]).(*ast.Ident); ok {
				for i, obj := range params {
					if obj == p.TypesInfo.Uses[id] {
						wrappers[fn] = i
					}
				}
			}
			return true
		})
	})

	// Pass 2: calls with a constant variable name.
	var out []EnvVar
	x.eachFile(func(p *packages.Package, file *ast.File) {
		info := p.TypesInfo
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fn := callee(info, call)
			if fn == nil {
				return true
			}
			if isEnvRead(fn) && len(call.Args) > 0 {
				if name, ok := constString(info, call.Args[0]); ok {
					out = append(out, EnvVar{Name: name, Pos: x.pos(call.Pos())})
				}
				return true
			}
			idx, ok := wrappers[fn]
			if !ok || idx >= len(call.Args) {
				return true
			}
			name, ok := constString(info, call.Args[idx])
			if !ok {
				return true
			}
			e := EnvVar{Name: name, Via: fn.Name(), Pos: x.pos(call.Pos())}
			for i, a := range call.Args {
				if s, ok := constString(info, a); ok && i != idx {
					e.Default = s
					break
				}
			}
			out = append(out, e)
			return true
		})
	})
	return out
}

func isEnvRead(fn *types.Func) bool {
	return fn != nil && fn.Pkg() != nil && fn.Pkg().Path() == "os" && (fn.Name() == "Getenv" || fn.Name() == "LookupEnv")
}

// paramObjects lists a function's parameters in order.
func paramObjects(info *types.Info, fd *ast.FuncDecl) []types.Object {
	var out []types.Object
	for _, fl := range fd.Type.Params.List {
		if len(fl.Names) == 0 {
			out = append(out, nil)
		}
		for _, n := range fl.Names {
			out = append(out, info.Defs[n])
		}
	}
	return out
}

// serverSettings lists the fields set on every http.Server literal.
func (x *extractor) serverSettings() []Setting {
	var out []Setting
	x.eachFile(func(p *packages.Package, file *ast.File) {
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if t := p.TypesInfo.TypeOf(lit); t == nil || types.TypeString(t, nil) != "net/http.Server" {
				return true
			}
			for _, el := range lit.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					out = append(out, Setting{
						Name:  "http.Server." + x.src(kv.Key),
						Value: x.src(kv.Value),
						Pos:   x.pos(kv.Pos()),
					})
				}
			}
			return true
		})
	})
	return out
}
