package gofacts

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"strconv"
)

// Constraint is a value the handler's code shows for a param: the default it
// falls back to, or a bound it rejects values beyond.
type Constraint struct {
	Value string `json:"value"`
	Pos   string `json:"pos"`
}

// EnumConstraint is the fixed set of values a handler accepts for a param.
type EnumConstraint struct {
	Values []string `json:"values"`
	Pos    string   `json:"pos"`
	Via    string   `json:"via,omitempty"` // how the set is known, e.g. "catalog.ParseCurrency returns a catalog.Currency"
}

// paramConstraints finds, in the handler's body, the constraints the code
// enforces on each param in r.Params. It follows the param's value through
// local variables (raw := q.Get("x"); n, err := strconv.Atoi(raw); limit = n)
// and reads, for those variables:
//   - a default: a variable declared with a constant before the param is
//     assigned to it (limit := defaultLimit);
//   - min/max: comparisons with a constant in an if that answers 400 or 422
//     and returns (if err != nil || n < 1 || n > maxLimit);
//   - an enum: a parse call returning (T, error) where T is a named basic type
//     with constants declared next to it (c, err := ParseCurrency(raw)), a
//     switch on the value whose default rejects, or an if rejecting every
//     value but a few constants (if v != "a" && v != "b").
//
// Only patterns that are certain are read; anything else is left unknown.
func (x *extractor) paramConstraints(r *Route, h funcDecl) {
	if len(r.Params) == 0 || h.decl.Body == nil {
		return
	}
	info := h.pkg.TypesInfo
	index := map[string]int{}
	for i, p := range r.Params {
		index[p.In+" "+p.Name] = i
	}
	vars := map[*types.Var]int{} // local variable -> the param whose value it holds
	varOf := func(e ast.Expr) *types.Var {
		id, ok := ast.Unparen(e).(*ast.Ident)
		if !ok {
			return nil
		}
		if v, ok := info.Defs[id].(*types.Var); ok {
			return v
		}
		v, _ := info.Uses[id].(*types.Var)
		return v
	}
	// paramOf is the param e reads or holds, or -1.
	paramOf := func(e ast.Expr) int {
		if v := varOf(e); v != nil {
			if i, ok := vars[v]; ok {
				return i
			}
			return -1
		}
		call, ok := ast.Unparen(e).(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return -1
		}
		fn := callee(info, call)
		if fn == nil {
			return -1
		}
		in := ""
		switch {
		case fn.Name() == "PathValue" && recvType(fn) == "*net/http.Request":
			in = "path"
		case fn.Name() == "Get" && recvType(fn) == "net/url.Values":
			in = "query"
		}
		name, ok := constString(info, call.Args[0])
		if i, found := index[in+" "+name]; in != "" && ok && found {
			return i
		}
		return -1
	}
	// source is the param an assigned expression carries: the param itself,
	// or a call that takes it as an argument (a parse).
	source := func(e ast.Expr) (int, *ast.CallExpr) {
		if i := paramOf(e); i >= 0 {
			return i, nil
		}
		call, ok := ast.Unparen(e).(*ast.CallExpr)
		if !ok {
			return -1, nil
		}
		for _, a := range call.Args {
			if i := paramOf(a); i >= 0 {
				return i, call
			}
		}
		return -1, nil
	}

	type initial struct {
		value constant.Value
		pos   token.Pos
	}
	inits := map[*types.Var]initial{}
	type enumCall struct {
		param int
		v     *types.Var
		call  *ast.CallExpr
	}
	var parses []enumCall

	// Two passes: a variable may be declared before the param reaches it.
	for range 2 {
		ast.Inspect(h.decl.Body, func(n ast.Node) bool {
			var lhs, rhs []ast.Expr
			define := false
			switch s := n.(type) {
			case *ast.AssignStmt:
				lhs, rhs, define = s.Lhs, s.Rhs, s.Tok == token.DEFINE
			case *ast.ValueSpec:
				for _, id := range s.Names {
					lhs = append(lhs, id)
				}
				rhs, define = s.Values, true
			default:
				return true
			}
			if len(rhs) == 1 {
				v := varOf(lhs[0])
				if v == nil {
					return true
				}
				if tv, ok := info.Types[rhs[0]]; ok && tv.Value != nil && define && len(lhs) == 1 {
					inits[v] = initial{tv.Value, n.Pos()}
				}
				if i, call := source(rhs[0]); i >= 0 {
					if _, seen := vars[v]; !seen {
						vars[v] = i
						if call != nil && len(lhs) == 2 {
							parses = append(parses, enumCall{i, v, call})
						}
					}
				}
				return true
			}
			for k := range min(len(lhs), len(rhs)) {
				if v := varOf(lhs[k]); v != nil {
					if i, _ := source(rhs[k]); i >= 0 {
						vars[v] = i
					}
				}
			}
			return true
		})
	}

	set := func(slot **Constraint, value constant.Value, pos token.Pos) {
		if *slot == nil {
			*slot = &Constraint{Value: constText(value), Pos: x.pos(pos)}
		}
	}
	setEnum := func(i int, values []constant.Value, pos token.Pos, via string) {
		if r.Params[i].Enum != nil || len(values) < 2 {
			return
		}
		e := &EnumConstraint{Pos: x.pos(pos), Via: via}
		for _, v := range values {
			e.Values = append(e.Values, constText(v))
		}
		r.Params[i].Enum = e
	}

	// A default is the constant a variable starts with before the param is
	// assigned to it; the variable the param is first parsed into has none.
	for v, init := range inits {
		if i, ok := vars[v]; ok {
			set(&r.Params[i].Default, init.value, init.pos)
		}
	}

	for _, pc := range parses {
		sig, ok := info.Types[pc.call.Fun].Type.(*types.Signature)
		if !ok || sig.Results().Len() != 2 || !isError(sig.Results().At(1).Type()) {
			continue
		}
		named, ok := types.Unalias(pc.v.Type()).(*types.Named)
		if !ok {
			continue
		}
		if _, basic := named.Underlying().(*types.Basic); !basic || named.Obj().Pkg() == nil {
			continue
		}
		var values []constant.Value
		var poss []token.Pos
		scope := named.Obj().Pkg().Scope()
		for _, name := range scope.Names() {
			if c, ok := scope.Lookup(name).(*types.Const); ok && types.Identical(c.Type(), named) {
				values, poss = append(values, c.Val()), append(poss, c.Pos())
			}
		}
		sortByPos(values, poss)
		typ := named.Obj().Pkg().Name() + "." + named.Obj().Name()
		via := typ
		if fn := callee(info, pc.call); fn != nil && fn.Pkg() != nil {
			via = fn.Pkg().Name() + "." + fn.Name() + " returns a " + typ + "; these are its constants"
		}
		setEnum(pc.param, values, pc.call.Pos(), via)
	}

	ast.Inspect(h.decl.Body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.IfStmt:
			if !rejects(info, s.Body.List) {
				return true
			}
			x.bounds(info, s.Cond, varOf, vars, func(i int, minimum bool, v constant.Value, pos token.Pos) {
				if minimum {
					set(&r.Params[i].Minimum, v, pos)
				} else {
					set(&r.Params[i].Maximum, v, pos)
				}
			})
			if i, values := allowList(info, s.Cond, varOf, vars); i >= 0 {
				setEnum(i, values, s.Cond.Pos(), "the handler rejects any other value")
			}
		case *ast.SwitchStmt:
			v := varOf(s.Tag)
			i, ok := vars[v]
			if v == nil || !ok {
				return true
			}
			var values []constant.Value
			rejected := false
			for _, st := range s.Body.List {
				cc := st.(*ast.CaseClause)
				if cc.List == nil {
					rejected = rejects(info, cc.Body)
					continue
				}
				for _, e := range cc.List {
					if tv, ok := info.Types[e]; ok && tv.Value != nil {
						values = append(values, tv.Value)
					}
				}
			}
			if rejected {
				setEnum(i, values, s.Pos(), "the handler's switch rejects any other value")
			}
		}
		return true
	})
}

// bounds reports each comparison of a param variable with a constant in a
// rejecting condition: cond's top-level || terms. "n < 1" rejects below 1, so
// the minimum is 1; "n > 10" rejects above 10, so the maximum is 10. A strict
// bound on an integer moves by one (n <= 0 means a minimum of 1); one on a
// float is exclusive, which the facts do not record.
func (x *extractor) bounds(info *types.Info, cond ast.Expr, varOf func(ast.Expr) *types.Var, vars map[*types.Var]int, report func(i int, minimum bool, v constant.Value, pos token.Pos)) {
	for _, term := range disjuncts(cond) {
		b, ok := term.(*ast.BinaryExpr)
		if !ok {
			continue
		}
		op, side, other := b.Op, b.X, b.Y
		if varOf(side) == nil {
			op, side, other = flip(op), b.Y, b.X
		}
		v := varOf(side)
		i, ok := vars[v]
		if v == nil || !ok {
			continue
		}
		tv, ok := info.Types[other]
		if !ok || tv.Value == nil || (tv.Value.Kind() != constant.Int && tv.Value.Kind() != constant.Float) {
			continue
		}
		basic, ok := v.Type().Underlying().(*types.Basic)
		if !ok || basic.Info()&types.IsNumeric == 0 {
			continue
		}
		integer := basic.Info()&types.IsInteger != 0
		one := constant.MakeInt64(1)
		switch {
		case op == token.LSS:
			report(i, true, tv.Value, b.Pos())
		case op == token.GTR:
			report(i, false, tv.Value, b.Pos())
		case op == token.LEQ && integer:
			report(i, true, constant.BinaryOp(tv.Value, token.ADD, one), b.Pos())
		case op == token.GEQ && integer:
			report(i, false, constant.BinaryOp(tv.Value, token.SUB, one), b.Pos())
		}
	}
}

// allowList reads "v != a && v != b" as the enum {a, b} of v's param.
func allowList(info *types.Info, cond ast.Expr, varOf func(ast.Expr) *types.Var, vars map[*types.Var]int) (int, []constant.Value) {
	var terms []ast.Expr
	var split func(e ast.Expr)
	split = func(e ast.Expr) {
		if b, ok := ast.Unparen(e).(*ast.BinaryExpr); ok && b.Op == token.LAND {
			split(b.X)
			split(b.Y)
			return
		}
		terms = append(terms, ast.Unparen(e))
	}
	split(cond)
	param := -1
	var values []constant.Value
	for _, t := range terms {
		b, ok := t.(*ast.BinaryExpr)
		if !ok || b.Op != token.NEQ {
			return -1, nil
		}
		side, other := b.X, b.Y
		if varOf(side) == nil {
			side, other = b.Y, b.X
		}
		v := varOf(side)
		i, ok := vars[v]
		tv, isConst := info.Types[other]
		if v == nil || !ok || !isConst || tv.Value == nil || (param >= 0 && i != param) {
			return -1, nil
		}
		param, values = i, append(values, tv.Value)
	}
	return param, values
}

// rejects reports whether a block answers 400 or 422 and returns: the
// branch a handler takes for a value it does not accept.
func rejects(info *types.Info, body []ast.Stmt) bool {
	status, returns := false, false
	for _, st := range body {
		if _, ok := st.(*ast.ReturnStmt); ok {
			returns = true
		}
		ast.Inspect(st, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				for _, a := range call.Args {
					if code, _, ok := httpStatus(info, a); ok && (code == 400 || code == 422) {
						status = true
					}
				}
			}
			return true
		})
	}
	return status && returns
}

func disjuncts(e ast.Expr) []ast.Expr {
	e = ast.Unparen(e)
	if b, ok := e.(*ast.BinaryExpr); ok && b.Op == token.LOR {
		return append(disjuncts(b.X), disjuncts(b.Y)...)
	}
	return []ast.Expr{e}
}

// flip turns "c op v" into "v op' c".
func flip(op token.Token) token.Token {
	switch op {
	case token.LSS:
		return token.GTR
	case token.GTR:
		return token.LSS
	case token.LEQ:
		return token.GEQ
	case token.GEQ:
		return token.LEQ
	}
	return op
}

func isError(t types.Type) bool {
	return types.Identical(t, types.Universe.Lookup("error").Type())
}

func sortByPos(values []constant.Value, poss []token.Pos) {
	for i := 1; i < len(poss); i++ {
		for j := i; j > 0 && poss[j] < poss[j-1]; j-- {
			poss[j], poss[j-1] = poss[j-1], poss[j]
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// constText renders a constant as a spec would write it: strings unquoted,
// numbers in decimal.
func constText(v constant.Value) string {
	switch v.Kind() {
	case constant.String:
		return constant.StringVal(v)
	case constant.Float:
		f, _ := constant.Float64Val(v)
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return v.ExactString()
}
