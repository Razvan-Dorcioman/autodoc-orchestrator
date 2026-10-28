package gofacts

import (
	"go/ast"
	"go/constant"
	"go/token"
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
				x.handlers = append(x.handlers, *h)
				if r.Method == "" {
					if split := x.dispatch(r, *h, sites); split != nil {
						out = append(out, split...)
						return true
					}
					r.Methods = methodsChecked(*h)
				}
				x.analyzeHandler(&r, *h, sites)
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

// httpMethods are the methods a route can be documented under.
var httpMethods = map[string]bool{"GET": true, "HEAD": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "OPTIONS": true}

// dispatch splits a method-less route whose handler switches on r.Method,
// with a call to one module handler function per case, into one route per
// method, each analysed as that function. The default case, typically the
// dispatcher's own 405, belongs to no route. It returns nil when the handler
// is not such a dispatcher.
func (x *extractor) dispatch(r Route, h funcDecl, sites []ResponseSite) []Route {
	if h.decl.Body == nil {
		return nil
	}
	info := h.pkg.TypesInfo
	for _, stmt := range h.decl.Body.List {
		sw, ok := stmt.(*ast.SwitchStmt)
		if !ok || sw.Init != nil || sw.Tag == nil || !isRequestField(info, sw.Tag, "Method") {
			continue
		}
		var out []Route
		var delegates []funcDecl
		for _, s := range sw.Body.List {
			clause := s.(*ast.CaseClause)
			if clause.List == nil {
				continue
			}
			d := x.delegate(info, clause.Body)
			if d == nil {
				return nil
			}
			for _, e := range clause.List {
				m, ok := constString(info, e)
				if !ok || !httpMethods[m] {
					return nil
				}
				sub := r
				sub.Method = m
				sub.Pattern = m + " " + r.Pattern
				sub.Dispatcher = r.Handler
				sub.Handler = d.pkg.Name + "." + FuncName(d.decl)
				sub.HandlerPos = x.pos(d.decl.Pos())
				x.analyzeHandler(&sub, *d, sites)
				out = append(out, sub)
				delegates = append(delegates, *d)
			}
		}
		x.handlers = append(x.handlers, delegates...)
		return out
	}
	return nil
}

// delegate returns the module function a case body hands the request to:
// a single call passing the http.ResponseWriter, optionally followed by a
// bare return.
func (x *extractor) delegate(info *types.Info, body []ast.Stmt) *funcDecl {
	if len(body) == 2 {
		if ret, ok := body[1].(*ast.ReturnStmt); ok && len(ret.Results) == 0 {
			body = body[:1]
		}
	}
	if len(body) != 1 {
		return nil
	}
	es, ok := body[0].(*ast.ExprStmt)
	if !ok {
		return nil
	}
	call, ok := es.X.(*ast.CallExpr)
	if !ok {
		return nil
	}
	fn := callee(info, call)
	if fn == nil {
		return nil
	}
	d, ok := x.decls[fn]
	if !ok {
		return nil
	}
	for _, a := range call.Args {
		if t := info.TypeOf(a); t != nil && isResponseWriter(t) {
			return &d
		}
	}
	return nil
}

// methodsChecked lists the methods the handler compares r.Method with, in
// an ==, a != or a switch case, sorted.
func methodsChecked(h funcDecl) []string {
	if h.decl.Body == nil {
		return nil
	}
	info := h.pkg.TypesInfo
	seen := map[string]bool{}
	check := func(e ast.Expr) {
		if m, ok := constString(info, e); ok && httpMethods[m] {
			seen[m] = true
		}
	}
	ast.Inspect(h.decl.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BinaryExpr:
			if n.Op != token.EQL && n.Op != token.NEQ {
				return true
			}
			if isRequestField(info, n.X, "Method") {
				check(n.Y)
			} else if isRequestField(info, n.Y, "Method") {
				check(n.X)
			}
		case *ast.SwitchStmt:
			if n.Tag != nil && isRequestField(info, n.Tag, "Method") {
				for _, s := range n.Body.List {
					for _, e := range s.(*ast.CaseClause).List {
						check(e)
					}
				}
			}
		}
		return true
	})
	if len(seen) == 0 {
		return nil
	}
	return sortedKeys(seen)
}

// isRequestField reports whether e selects the named field of a
// net/http.Request, as in r.Method or r.Body.
func isRequestField(info *types.Info, e ast.Expr, field string) bool {
	sel, ok := ast.Unparen(e).(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != field {
		return false
	}
	s, ok := info.Selections[sel]
	return ok && s.Kind() == types.FieldVal && strings.TrimPrefix(types.TypeString(s.Recv(), nil), "*") == "net/http.Request"
}

// requestBody finds the type the handler decodes the request body into:
// the target of a json Decoder's Decode or of json.Unmarshal in a function
// that reads the request's Body, either the handler itself or a module
// helper the handler passes the target to, such as decode(r, &in).
func (x *extractor) requestBody(h funcDecl) *RequestBody {
	info := h.pkg.TypesInfo
	for _, t := range decodeTargets(h) {
		if name, ok := bodyType(info.TypeOf(t.target)); ok {
			return &RequestBody{Type: name, Pos: x.pos(t.call.Pos())}
		}
	}
	var out *RequestBody
	ast.Inspect(h.decl.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || out != nil {
			return out == nil
		}
		fn := callee(info, call)
		if fn == nil {
			return true
		}
		d, ok := x.decls[fn]
		if !ok {
			return true
		}
		params := paramObjects(d.pkg.TypesInfo, d.decl)
		for _, t := range decodeTargets(d) {
			id, ok := ast.Unparen(t.target).(*ast.Ident)
			if !ok {
				continue
			}
			for i, p := range params {
				if p == nil || i >= len(call.Args) || d.pkg.TypesInfo.Uses[id] != p {
					continue
				}
				if name, ok := bodyType(info.TypeOf(call.Args[i])); ok {
					out = &RequestBody{Type: name, Pos: x.pos(call.Pos()), Via: d.pkg.Name + "." + FuncName(d.decl)}
					return false
				}
			}
		}
		return true
	})
	return out
}

type decodeTarget struct {
	call   *ast.CallExpr
	target ast.Expr
}

// decodeTargets lists what a function decodes JSON into, if it reads the
// body of a net/http.Request: the argument of (*json.Decoder).Decode and
// the second argument of json.Unmarshal.
func decodeTargets(fd funcDecl) []decodeTarget {
	if fd.decl.Body == nil {
		return nil
	}
	info := fd.pkg.TypesInfo
	readsBody := false
	var out []decodeTarget
	ast.Inspect(fd.decl.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			readsBody = readsBody || isRequestField(info, n, "Body")
		case *ast.CallExpr:
			fn := callee(info, n)
			if fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != "encoding/json" {
				return true
			}
			switch {
			case fn.Name() == "Decode" && recvType(fn) == "*encoding/json.Decoder" && len(n.Args) == 1:
				out = append(out, decodeTarget{n, n.Args[0]})
			case fn.Name() == "Unmarshal" && recvType(fn) == "" && len(n.Args) == 2:
				out = append(out, decodeTarget{n, n.Args[1]})
			}
		}
		return true
	})
	if !readsBody {
		return nil
	}
	return out
}

// bodyType names the type a decode target points to; an interface, such as
// a helper's v any, says nothing about the body.
func bodyType(t types.Type) (string, bool) {
	if t == nil {
		return "", false
	}
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	if types.IsInterface(t) {
		return "", false
	}
	return typeName(t), true
}

// reach sets, on every function and constant a route handler reaches
// through module calls, the call depth from the nearest handler, so the
// pages that cannot list everything of a large module list these first. A
// constant of a named type a reached function uses counts as used there:
// converting to the type is checking against its values.
func (x *extractor) reach(f *Facts) {
	funcs, consts, named := map[string]int{}, map[string]int{}, map[string]int{} // position or type name -> depth
	type item struct {
		d     funcDecl
		depth int
	}
	var queue []item
	for _, h := range x.handlers {
		queue = append(queue, item{h, 1})
	}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		pos := x.pos(it.d.decl.Pos())
		if _, done := funcs[pos]; done {
			continue
		}
		funcs[pos] = it.depth
		if it.d.decl.Body == nil {
			continue
		}
		info := it.d.pkg.TypesInfo
		ast.Inspect(it.d.decl.Body, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			switch obj := info.Uses[id].(type) {
			case *types.Const:
				if p := x.pos(obj.Pos()); x.inModule(obj.Pkg()) && consts[p] == 0 {
					consts[p] = it.depth
				}
			case *types.TypeName:
				if t := typeName(obj.Type()); x.inModule(obj.Pkg()) && named[t] == 0 {
					named[t] = it.depth
				}
			case *types.Func:
				if d, ok := x.decls[obj.Origin()]; ok {
					queue = append(queue, item{d, it.depth + 1})
				}
			}
			return true
		})
	}
	for i := range f.Functions {
		f.Functions[i].Reach = funcs[f.Functions[i].Pos]
	}
	for i := range f.Constants {
		c := &f.Constants[i]
		c.Reach = consts[c.Pos]
		if d := named[c.Type]; d > 0 && (c.Reach == 0 || d < c.Reach) {
			c.Reach = d
		}
	}
}

// analyzeHandler records the handler's inputs (r.PathValue, url.Values.Get,
// a JSON request body), the module constants it uses, the module functions
// it calls and the responses it writes.
func (x *extractor) analyzeHandler(r *Route, h funcDecl, sites []ResponseSite) {
	for _, s := range sites {
		if s.Func == r.Handler {
			r.Responses = append(r.Responses, s)
		}
	}
	if h.decl.Body == nil {
		return
	}
	r.Body = x.requestBody(h)
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
				} else if t := info.TypeOf(a); t != nil && site.Body == "" && !isResponseWriter(t) && !isString(t) {
					// A non-constant string, such as err.Error(), is a message, not the body.
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

func isString(t types.Type) bool {
	b, ok := t.(*types.Basic)
	return ok && b.Info()&types.IsString != 0
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
