package gofacts

import (
	"go/ast"
	"go/constant"
	"go/types"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

var wildcard = regexp.MustCompile(`\{([^}]*)\}`)

// routes finds net/http registrations (ServeMux.HandleFunc / Handle and the
// package-level equivalents) with a constant pattern, and analyses each handler.
func (x *extractor) routes(sites []ResponseSite) []Route {
	var out []Route
	x.eachFile(func(p *packages.Package, file *ast.File) {
		info := p.TypesInfo
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			fn := callee(info, call)
			if fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != "net/http" || (fn.Name() != "HandleFunc" && fn.Name() != "Handle") {
				return true
			}
			pattern, ok := constString(info, call.Args[0])
			if !ok {
				return true
			}
			r := parsePattern(pattern)
			r.Params, r.Responses = []Param{}, []ResponseSite{} // "no params" is [], never null
			r.Registered = x.pos(call.Pos())
			r.Handler = x.src(call.Args[1])
			if h := x.handlerDecl(info, call.Args[1]); h != nil {
				r.Handler = h.pkg.Name + "." + FuncName(h.decl)
				r.HandlerPos = x.pos(h.decl.Pos())
				x.analyzeHandler(&r, *h)
				for _, s := range sites {
					if s.Func == r.Handler {
						r.Responses = append(r.Responses, s)
					}
				}
			}
			out = append(out, r)
			return true
		})
	})
	return out
}

// parsePattern splits a Go 1.22 ServeMux pattern "[METHOD ][HOST]/PATH".
func parsePattern(pattern string) Route {
	r := Route{Pattern: pattern}
	rest := pattern
	if method, after, ok := strings.Cut(pattern, " "); ok && !strings.Contains(method, "/") {
		r.Method, rest = method, strings.TrimLeft(after, " ")
	}
	if i := strings.Index(rest, "/"); i > 0 {
		rest = rest[i:] // drop the host
	}
	path := wildcard.ReplaceAllStringFunc(rest, func(m string) string {
		name := m[1 : len(m)-1]
		switch {
		case name == "$":
			r.ExactMatch = true
			return ""
		case strings.HasSuffix(name, "..."):
			return "{" + strings.TrimSuffix(name, "...") + "}"
		}
		return m
	})
	if path == "" {
		path = "/"
	}
	r.Path = path
	return r
}

// handlerDecl resolves a handler expression (h.index, handleX) to its declaration.
func (x *extractor) handlerDecl(info *types.Info, e ast.Expr) *funcDecl {
	var obj types.Object
	switch h := ast.Unparen(e).(type) {
	case *ast.Ident:
		obj = info.Uses[h]
	case *ast.SelectorExpr:
		obj = info.Uses[h.Sel]
	}
	fn, ok := obj.(*types.Func)
	if !ok {
		return nil
	}
	d, ok := x.decls[fn]
	if !ok {
		return nil
	}
	return &d
}

// analyzeHandler records the handler's inputs (r.PathValue, url.Values.Get),
// the module constants it uses and the module functions it calls.
func (x *extractor) analyzeHandler(r *Route, h funcDecl) {
	if h.decl.Body == nil {
		return
	}
	info := h.pkg.TypesInfo
	seenParam := map[string]bool{}
	consts := map[string]bool{}
	calls := map[string]bool{}
	ast.Inspect(h.decl.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			fn := callee(info, n)
			if fn == nil {
				return true
			}
			in := ""
			switch {
			case fn.Name() == "PathValue" && recvType(fn) == "*net/http.Request":
				in = "path"
			case (fn.Name() == "Get" || fn.Name() == "Has") && recvType(fn) == "net/url.Values":
				in = "query"
			}
			if in != "" && len(n.Args) == 1 {
				if name, ok := constString(info, n.Args[0]); ok && !seenParam[in+name] {
					seenParam[in+name] = true
					r.Params = append(r.Params, Param{Name: name, In: in, Pos: x.pos(n.Pos())})
				}
			}
			if x.inModule(fn.Pkg()) {
				calls[x.qualifiedFunc(fn)] = true
			}
		case *ast.Ident:
			if c, ok := info.Uses[n].(*types.Const); ok && x.inModule(c.Pkg()) {
				consts[c.Pkg().Name()+"."+c.Name()] = true
			}
		}
		return true
	})
	r.ConstsUsed = sortedKeys(consts)
	r.Calls = sortedKeys(calls)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// responseSites finds every call that passes a net/http Status constant, in
// handlers and middleware alike. For each one it records the status, the
// string constants that follow it (error code, message) and the response body
// type, following one level into module helpers such as writeError.
func (x *extractor) responseSites() []ResponseSite {
	var out []ResponseSite
	x.eachFuncDecl(func(p *packages.Package, fd *ast.FuncDecl) {
		if fd.Body == nil {
			return
		}
		info := p.TypesInfo
		owner := p.Name + "." + FuncName(fd)
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			idx, status, name := -1, 0, ""
			for i, a := range call.Args {
				if s, n, ok := httpStatus(info, a); ok {
					idx, status, name = i, s, n
					break
				}
			}
			if idx < 0 {
				return true
			}
			site := ResponseSite{Func: owner, Pos: x.pos(call.Pos()), Status: status, StatusName: name}
			fn := callee(info, call)
			if fn != nil {
				site.Helper = fn.Name()
			}
			var strs []string
			for _, a := range call.Args[idx+1:] {
				if s, ok := constString(info, a); ok {
					strs = append(strs, s)
				} else if f, ok := sprintfFormat(info, a); ok {
					strs = append(strs, f)
				} else if t := info.TypeOf(a); t != nil && site.Body == "" && !isResponseWriter(t) {
					site.Body = typeName(t)
				}
			}
			if len(strs) > 0 {
				site.Code = strs[0]
			}
			if len(strs) > 1 {
				site.Message = strs[1]
			}
			if site.Body == "" && fn != nil {
				if d, ok := x.decls[fn]; ok {
					site.Body = x.helperBody(d, idx)
				}
			}
			out = append(out, site)
			return true
		})
	})
	return out
}

// httpStatus reports whether e is a net/http Status* constant.
func httpStatus(info *types.Info, e ast.Expr) (int, string, bool) {
	var id *ast.Ident
	switch v := ast.Unparen(e).(type) {
	case *ast.Ident:
		id = v
	case *ast.SelectorExpr:
		id = v.Sel
	default:
		return 0, "", false
	}
	c, ok := info.Uses[id].(*types.Const)
	if !ok || c.Pkg() == nil || c.Pkg().Path() != "net/http" || !strings.HasPrefix(c.Name(), "Status") {
		return 0, "", false
	}
	v, ok := constant.Int64Val(c.Val())
	return int(v), c.Name(), ok
}

// sprintfFormat returns the format of a fmt.Sprintf call with a constant format.
func sprintfFormat(info *types.Info, e ast.Expr) (string, bool) {
	call, ok := ast.Unparen(e).(*ast.CallExpr)
	if !ok || len(call.Args) == 0 {
		return "", false
	}
	fn := callee(info, call)
	if fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != "fmt" || fn.Name() != "Sprintf" {
		return "", false
	}
	return constString(info, call.Args[0])
}

func isResponseWriter(t types.Type) bool {
	return types.TypeString(t, nil) == "net/http.ResponseWriter"
}

// helperBody finds the body type a helper writes when its status parameter is
// statusIdx: the type of another argument passed next to that parameter.
func (x *extractor) helperBody(h funcDecl, statusIdx int) string {
	params := paramObjects(h.pkg.TypesInfo, h.decl)
	if statusIdx >= len(params) || params[statusIdx] == nil || h.decl.Body == nil {
		return ""
	}
	info := h.pkg.TypesInfo
	statusObj := params[statusIdx]
	body := ""
	ast.Inspect(h.decl.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || body != "" {
			return body == ""
		}
		passesStatus := false
		for _, a := range call.Args {
			if id, ok := ast.Unparen(a).(*ast.Ident); ok && info.Uses[id] == statusObj {
				passesStatus = true
			}
		}
		if !passesStatus {
			return true
		}
		for _, a := range call.Args {
			t := info.TypeOf(a)
			if t == nil || isResponseWriter(t) {
				continue
			}
			if _, ok := t.Underlying().(*types.Struct); ok {
				body = typeName(t)
				return false
			}
		}
		return true
	})
	return body
}

// implicitResponses are the responses net/http.ServeMux produces on its own,
// which no handler code shows.
func implicitResponses(routes []Route) []ImplicitResponse {
	if len(routes) == 0 {
		return nil
	}
	out := []ImplicitResponse{{
		Status: 404,
		When:   "no registered pattern matches the request path",
		Body:   "text/plain: 404 page not found",
		Source: "net/http.ServeMux",
	}}
	hasMethod, hasGet := false, false
	for _, r := range routes {
		hasMethod = hasMethod || r.Method != ""
		hasGet = hasGet || r.Method == "GET"
	}
	if hasMethod {
		out = append(out, ImplicitResponse{
			Status: 405,
			When:   "a pattern matches the path but not the method; the Allow header lists the allowed methods",
			Body:   "text/plain: Method Not Allowed",
			Source: "net/http.ServeMux",
		})
	}
	if hasGet {
		out = append(out, ImplicitResponse{
			Status: 200,
			When:   "HEAD request on a GET pattern: GET patterns also match HEAD, and the body is discarded",
			Body:   "none",
			Source: "net/http.ServeMux",
		})
	}
	return out
}
