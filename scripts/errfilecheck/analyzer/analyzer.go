// Package analyzer implements errfile/catch-must-report for Go (pm/error_err.mdx §2 R1-Go, §13.1).
//
// Go has no `catch`; these are the Go error sites, and each must reach the error file:
//
//  1. an error-nil test block (`if err != nil { … }`, the else of `== nil`, `case err != nil:`):
//     it passes when the error is handed on (the identifier appears anywhere in the block), or
//     reported (a call to errfile.*, to a net-covered pkg/log or cli logger function, or panic);
//  2. a recover() call: its value must reach errfile.Recovered, panic, a log.* call, or any call;
//  3. a discarded error result used as a bare expression statement (`os.Remove(p)`);
//  4. a `go` statement whose function is not a literal starting with `defer errfile.RecoverNet(…)()`.
//
// A companion check keeps `doing` a literal (or a const, or a `+` of literals and route/verb
// selectors, or c.FullPath()), so the fold key stays stable and no ledger value can reach the
// file through it.
package analyzer

import (
	"flag"
	"go/ast"
	"go/token"
	"go/types"
	"strings"
	"sync"

	"golang.org/x/tools/go/analysis"
)

// Diagnostic categories.
const (
	CatUnreported        = "unreported"
	CatUnreportedRecover = "unreportedRecover"
	CatDiscardedError    = "discardedError"
	CatNakedGo           = "nakedGo"
	CatDynamicDoing      = "dynamicDoing"
	CatSite              = "site"
)

var reportSites bool

// Analyzer is errfile/catch-must-report for Go.
var Analyzer = &analysis.Analyzer{
	Name:  "errfile",
	Doc:   "errfile/catch-must-report: every Go error site reports to pkg/errfile, hands the error on, or marks it expected (pm/error_err.mdx §13.1)",
	Run:   run,
	Flags: flags(),
}

func flags() flag.FlagSet {
	var fs flag.FlagSet
	fs.BoolVar(&reportSites, "report-sites", false, "also emit one `site` diagnostic per error site, so a coverage script can count sites per file")

	return fs
}

// errfilePaths are the import paths of the library (the source and its vendored copy).
var errfilePaths = []string{
	"github.com/mayswind/ezbookkeeping/pkg/errfile",
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/errfile",
}

// reportingFuncs are the net-covered logging functions per package suffix (§8 N6; the CLI's logger).
var reportingFuncs = map[string]map[string]bool{
	"/pkg/log":             {"Errorf": true, "Warnf": true, "ErrorfWithExtra": true, "BootErrorf": true, "BootWarnf": true, "CliErrorf": true, "CliWarnf": true},
	"/cli/internal/logger": {"Error": true, "Warn": true},
}

// doingFuncs take `doing` as their first argument.
var doingFuncs = map[string]bool{"Caught": true, "Warn": true, "Expected": true, "Fatal": true, "Recovered": true, "Go": true, "RecoverNet": true}

// doingSelectors are the selector names allowed inside a dynamic `doing`.
var doingSelectors = map[string]bool{"Method": true, "Path": true, "Name": true, "Route": true}

// neverFails are call targets whose error result is a formality (fmt printing, in-memory
// builders); a bare call to one is not a discarded error.
var neverFailsFuncs = map[string]bool{
	"fmt.Print": true, "fmt.Println": true, "fmt.Printf": true,
	"fmt.Fprint": true, "fmt.Fprintln": true, "fmt.Fprintf": true,
}

var neverFailsMethods = map[string]bool{
	"(*strings.Builder).Write": true, "(*strings.Builder).WriteString": true, "(*strings.Builder).WriteByte": true, "(*strings.Builder).WriteRune": true,
	"(*bytes.Buffer).Write": true, "(*bytes.Buffer).WriteString": true, "(*bytes.Buffer).WriteByte": true, "(*bytes.Buffer).WriteRune": true,
}

type checker struct {
	pass      *analysis.Pass
	errorType *types.Interface
	file      *ast.File
	// funcBodies maps each recover() call to its enclosing function body, for the value walk.
	stack []ast.Node
}

func run(pass *analysis.Pass) (any, error) {
	errorType := types.Universe.Lookup("error").Type().Underlying().(*types.Interface)

	for _, file := range pass.Files {
		if skipFile(pass, file) {
			continue
		}

		c := &checker{pass: pass, errorType: errorType, file: file}
		c.walk(file)
	}

	return nil, nil
}

// skipFile applies §13.4: tests, testdata, the two libraries, this tool, and generated files.
func skipFile(pass *analysis.Pass, file *ast.File) bool {
	name := pass.Fset.Position(file.Pos()).Filename
	name = strings.ReplaceAll(name, "\\", "/")

	if strings.HasSuffix(name, "_test.go") || ast.IsGenerated(file) {
		return true
	}

	if strings.Contains(name, "/testdata/") && !strings.Contains(name, "/errfilecheck/") {
		return true
	}

	for _, seg := range []string{"/pkg/errfile/", "/cli/internal/errfile/", "/scripts/errfilecheck/"} {
		if strings.Contains(name, seg) && !strings.Contains(name, "/errfilecheck/") {
			return true
		}
	}

	return false
}

func (c *checker) walk(root ast.Node) {
	ast.Inspect(root, func(n ast.Node) bool {
		if n == nil {
			c.stack = c.stack[:len(c.stack)-1]
			return true
		}

		c.stack = append(c.stack, n)

		switch x := n.(type) {
		case *ast.IfStmt:
			c.checkIf(x)
		case *ast.CaseClause:
			c.checkCase(x)
		case *ast.CallExpr:
			c.checkCall(x)
		case *ast.ExprStmt:
			c.checkExprStmt(x)
		case *ast.GoStmt:
			c.checkGo(x)
		}

		return true
	})
}

// reported dedupes diagnostics across passes: a standalone `./...` run analyses a package and its
// in-package test variant, which share every non-test file, and the second copy would double
// every count the coverage script reads. go vet runs one pass per package, so it is unaffected.
var reported sync.Map

func (c *checker) emit(pos token.Pos, category, message string) {
	key := c.pass.Fset.Position(pos).String() + "|" + category

	if _, dup := reported.LoadOrStore(key, true); dup {
		return
	}

	c.pass.Report(analysis.Diagnostic{Pos: pos, Category: category, Message: message})
}

func (c *checker) site(pos token.Pos, kind string) {
	if reportSites {
		c.emit(pos, CatSite, "error site: "+kind)
	}
}

func (c *checker) report(pos token.Pos, category, message string) {
	c.emit(pos, category, message)
}

// ── 1. error-nil test blocks ───────────────────────────────────────────────────────────────────

// nilTest is one `X != nil` / `X == nil` found in a condition, with X of type error.
type nilTest struct {
	expr     ast.Expr
	notEqual bool
}

func (c *checker) isErrorTyped(e ast.Expr) bool {
	tv, ok := c.pass.TypesInfo.Types[e]

	if !ok || tv.Type == nil {
		return false
	}

	t := tv.Type

	if types.Identical(t, types.Universe.Lookup("error").Type()) {
		return true
	}

	if _, isIface := t.Underlying().(*types.Interface); isIface {
		return types.Implements(t, c.errorType)
	}

	return false
}

func (c *checker) nilTests(cond ast.Expr) []nilTest {
	var out []nilTest

	ast.Inspect(cond, func(n ast.Node) bool {
		be, ok := n.(*ast.BinaryExpr)

		if !ok || (be.Op != token.NEQ && be.Op != token.EQL) {
			return true
		}

		var other ast.Expr

		switch {
		case isNil(be.Y):
			other = be.X
		case isNil(be.X):
			other = be.Y
		default:
			return true
		}

		if c.isErrorTyped(other) {
			out = append(out, nilTest{expr: other, notEqual: be.Op == token.NEQ})
		}

		return true
	})

	return out
}

func isNil(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)

	return ok && id.Name == "nil"
}

func (c *checker) checkIf(s *ast.IfStmt) {
	for _, t := range c.nilTests(s.Cond) {
		var taken ast.Node

		if t.notEqual {
			taken = s.Body
		} else {
			taken = s.Else
		}

		if taken == nil {
			continue
		}

		c.checkErrorBlock(taken, t.expr, "error-nil block")
	}
}

func (c *checker) checkCase(cc *ast.CaseClause) {
	for _, e := range cc.List {
		for _, t := range c.nilTests(e) {
			if !t.notEqual {
				continue
			}

			body := &ast.BlockStmt{List: cc.Body, Lbrace: cc.Colon, Rbrace: cc.End()}
			c.checkErrorBlock(body, t.expr, "error-nil case")
		}
	}
}

// checkErrorBlock is the heart of R1-Go: the block passes when the error is handed on or reported.
func (c *checker) checkErrorBlock(block ast.Node, errExpr ast.Expr, kind string) {
	c.site(block.Pos(), kind)

	if c.blockHandsOn(block, errExpr) || c.blockReports(block) {
		return
	}

	c.report(block.Pos(), CatUnreported, "this "+kind+" neither hands the error on, reports it, nor marks it expected — see pm/error_err.mdx §7")
}

// blockHandsOn: the error expression appears anywhere in the block — in a return, as a call
// argument, on the right of an assignment, appended, sent — which is how Go hands an error on.
func (c *checker) blockHandsOn(block ast.Node, errExpr ast.Expr) bool {
	obj := c.objectOf(errExpr)
	key := types.ExprString(errExpr)
	found := false

	ast.Inspect(block, func(n ast.Node) bool {
		if found {
			return false
		}

		switch x := n.(type) {
		case *ast.Ident:
			if obj != nil && c.pass.TypesInfo.Uses[x] == obj {
				found = true
			}
		case *ast.SelectorExpr, *ast.IndexExpr, *ast.CallExpr:
			if obj == nil && types.ExprString(x.(ast.Expr)) == key {
				found = true
			}
		}

		return !found
	})

	return found
}

func (c *checker) objectOf(e ast.Expr) types.Object {
	if id, ok := e.(*ast.Ident); ok {
		if obj := c.pass.TypesInfo.Uses[id]; obj != nil {
			return obj
		}

		return c.pass.TypesInfo.Defs[id]
	}

	return nil
}

// blockReports: a call to errfile.*, a net-covered log function, panic, or a testing failure.
func (c *checker) blockReports(block ast.Node) bool {
	found := false

	ast.Inspect(block, func(n ast.Node) bool {
		if found {
			return false
		}

		call, ok := n.(*ast.CallExpr)

		if !ok {
			return true
		}

		if c.isReportingCall(call) {
			found = true
		}

		return !found
	})

	return found
}

func (c *checker) isReportingCall(call *ast.CallExpr) bool {
	if id, ok := call.Fun.(*ast.Ident); ok {
		if id.Name == "panic" {
			if _, isBuiltin := c.pass.TypesInfo.Uses[id].(*types.Builtin); isBuiltin {
				return true
			}
		}

		return false
	}

	sel, ok := call.Fun.(*ast.SelectorExpr)

	if !ok {
		return false
	}

	if pkg := c.packageOf(sel); pkg != "" {
		if isErrfilePath(pkg) {
			return true
		}

		for suffix, names := range reportingFuncs {
			if strings.HasSuffix(pkg, suffix) && names[sel.Sel.Name] {
				return true
			}
		}

		return false
	}

	// t.Fatal* / t.Error* on a *testing.T or *testing.B
	if fn, ok := c.pass.TypesInfo.Uses[sel.Sel].(*types.Func); ok {
		if recv := fn.Type().(*types.Signature).Recv(); recv != nil {
			if strings.HasPrefix(types.TypeString(recv.Type(), nil), "*testing.") && (strings.HasPrefix(sel.Sel.Name, "Fatal") || strings.HasPrefix(sel.Sel.Name, "Error")) {
				return true
			}
		}
	}

	return false
}

// packageOf returns the import path when a selector is `pkg.Name`, else "".
func (c *checker) packageOf(sel *ast.SelectorExpr) string {
	id, ok := sel.X.(*ast.Ident)

	if !ok {
		return ""
	}

	if pn, ok := c.pass.TypesInfo.Uses[id].(*types.PkgName); ok {
		return pn.Imported().Path()
	}

	return ""
}

func isErrfilePath(path string) bool {
	for _, p := range errfilePaths {
		if path == p {
			return true
		}
	}

	// analysistest fixtures import the same paths from testdata/src
	return strings.HasSuffix(path, "/pkg/errfile") || strings.HasSuffix(path, "/cli/internal/errfile")
}

// ── 2. recover() ───────────────────────────────────────────────────────────────────────────────

func (c *checker) checkCall(call *ast.CallExpr) {
	if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "recover" {
		if _, isBuiltin := c.pass.TypesInfo.Uses[id].(*types.Builtin); isBuiltin {
			c.checkRecover(call)
		}

		return
	}

	if sel, ok := call.Fun.(*ast.SelectorExpr); ok && isErrfilePath(c.packageOf(sel)) && doingFuncs[sel.Sel.Name] && len(call.Args) > 0 {
		if !c.isStaticDoing(call.Args[0]) {
			c.report(call.Args[0].Pos(), CatDynamicDoing, "`doing` must be a string literal, a const, or a + of literals and .Method/.Path/.Name/.Route/verbName — dynamic parts go in fields (pm/error_err.mdx §6.1)")
		}
	}
}

func (c *checker) checkRecover(call *ast.CallExpr) {
	c.site(call.Pos(), "recover")

	if len(c.stack) < 2 {
		return
	}

	parent := c.stack[len(c.stack)-2]

	switch p := parent.(type) {
	case *ast.ExprStmt:
		// a bare `recover()` statement
		c.report(call.Pos(), CatUnreportedRecover, "the recovered value is dropped — pass it to errfile.Recovered (pm/error_err.mdx §7 G5)")
	case *ast.CallExpr:
		// recover() directly as an argument: reaches a call
		return
	case *ast.AssignStmt:
		for i, rhs := range p.Rhs {
			if rhs != ast.Expr(call) || i >= len(p.Lhs) {
				continue
			}

			id, ok := p.Lhs[i].(*ast.Ident)

			if !ok || id.Name == "_" {
				c.report(call.Pos(), CatUnreportedRecover, "the recovered value is dropped — pass it to errfile.Recovered (pm/error_err.mdx §7 G5)")
				return
			}

			obj := c.pass.TypesInfo.Defs[id]

			if obj == nil {
				obj = c.pass.TypesInfo.Uses[id]
			}

			if !c.valueReachesCall(obj) {
				c.report(call.Pos(), CatUnreportedRecover, "the recovered value never reaches errfile.Recovered, panic or a log call (pm/error_err.mdx §7 G5)")
			}
		}
	default:
		// `if recover() != nil {}` and other shapes where the value is compared and dropped
		c.report(call.Pos(), CatUnreportedRecover, "the recovered value is dropped — bind it and pass it to errfile.Recovered (pm/error_err.mdx §7 G5)")
	}
}

// enclosingFuncBody returns the body of the nearest enclosing function literal or declaration.
func (c *checker) enclosingFuncBody() *ast.BlockStmt {
	for i := len(c.stack) - 1; i >= 0; i-- {
		switch f := c.stack[i].(type) {
		case *ast.FuncLit:
			return f.Body
		case *ast.FuncDecl:
			return f.Body
		}
	}

	return nil
}

// valueReachesCall: the object is an argument of some call (errfile.Recovered, panic, log.*, …)
// inside the enclosing function.
func (c *checker) valueReachesCall(obj types.Object) bool {
	body := c.enclosingFuncBody()

	if body == nil || obj == nil {
		return false
	}

	found := false

	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}

		call, ok := n.(*ast.CallExpr)

		if !ok {
			return true
		}

		for _, a := range call.Args {
			if c.exprUsesObject(a, obj) {
				found = true
				return false
			}
		}

		return true
	})

	return found
}

func (c *checker) exprUsesObject(e ast.Expr, obj types.Object) bool {
	found := false

	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && c.pass.TypesInfo.Uses[id] == obj {
			found = true
		}

		return !found
	})

	return found
}

// ── 3. discarded error results ─────────────────────────────────────────────────────────────────

func (c *checker) checkExprStmt(s *ast.ExprStmt) {
	call, ok := s.X.(*ast.CallExpr)

	if !ok || !c.returnsError(call) || c.neverFails(call) {
		return
	}

	c.site(s.Pos(), "discarded error result")
	c.report(s.Pos(), CatDiscardedError, "the error this call returns is discarded — check it, or write `_ = …` to say the failure does not matter (pm/error_err.mdx §7 G4)")
}

func (c *checker) returnsError(call *ast.CallExpr) bool {
	tv, ok := c.pass.TypesInfo.Types[call]

	if !ok || tv.Type == nil {
		return false
	}

	switch t := tv.Type.(type) {
	case *types.Tuple:
		for i := 0; i < t.Len(); i++ {
			if c.isErrorType(t.At(i).Type()) {
				return true
			}
		}

		return false
	default:
		return c.isErrorType(t)
	}
}

func (c *checker) isErrorType(t types.Type) bool {
	if types.Identical(t, types.Universe.Lookup("error").Type()) {
		return true
	}

	if _, isIface := t.Underlying().(*types.Interface); isIface {
		return types.Implements(t, c.errorType)
	}

	return false
}

func (c *checker) neverFails(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)

	if !ok {
		return false
	}

	if pkg := c.packageOf(sel); pkg != "" {
		return neverFailsFuncs[pkg+"."+sel.Sel.Name]
	}

	if fn, ok := c.pass.TypesInfo.Uses[sel.Sel].(*types.Func); ok {
		if recv := fn.Type().(*types.Signature).Recv(); recv != nil {
			return neverFailsMethods["("+types.TypeString(recv.Type(), nil)+")."+fn.Name()]
		}
	}

	return false
}

// ── 4. go statements ───────────────────────────────────────────────────────────────────────────

func (c *checker) checkGo(s *ast.GoStmt) {
	c.site(s.Pos(), "go statement")

	if lit, ok := s.Call.Fun.(*ast.FuncLit); ok && len(lit.Body.List) > 0 {
		if d, ok := lit.Body.List[0].(*ast.DeferStmt); ok {
			// defer errfile.RecoverNet("…")()
			if inner, ok := d.Call.Fun.(*ast.CallExpr); ok {
				if sel, ok := inner.Fun.(*ast.SelectorExpr); ok && isErrfilePath(c.packageOf(sel)) && sel.Sel.Name == "RecoverNet" {
					return
				}
			}
		}
	}

	c.report(s.Pos(), CatNakedGo, "a panic in this goroutine ends the process with no record — use errfile.Go(doing, fn) or start the literal with `defer errfile.RecoverNet(doing)()` (pm/error_err.mdx §7 G6)")
}

// ── companion: doing must be static ────────────────────────────────────────────────────────────

func (c *checker) isStaticDoing(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.BasicLit:
		return x.Kind == token.STRING
	case *ast.ParenExpr:
		return c.isStaticDoing(x.X)
	case *ast.Ident:
		if x.Name == "verbName" {
			return true
		}

		switch obj := c.pass.TypesInfo.Uses[x].(type) {
		case *types.Const:
			return true
		case *types.Var:
			// a thin wrapper such as reportLost(doing string, err error) passes its own `doing`
			// parameter through; the wrapper's callers are held to the literal rule
			return x.Name == "doing" && obj.Parent() != nil && obj.Parent() != obj.Pkg().Scope() && isStringType(obj.Type())
		}

		return false
	case *ast.SelectorExpr:
		if _, isConst := c.pass.TypesInfo.Uses[x.Sel].(*types.Const); isConst {
			return true
		}

		return doingSelectors[x.Sel.Name]
	case *ast.CallExpr:
		// c.FullPath() — the route template, which pattern G7 uses (never the raw URL)
		if sel, ok := x.Fun.(*ast.SelectorExpr); ok && len(x.Args) == 0 && sel.Sel.Name == "FullPath" {
			return true
		}

		return false
	case *ast.BinaryExpr:
		return x.Op == token.ADD && c.isStaticDoing(x.X) && c.isStaticDoing(x.Y)
	}

	return false
}

func isStringType(t types.Type) bool {
	b, ok := t.Underlying().(*types.Basic)

	return ok && b.Kind() == types.String
}
