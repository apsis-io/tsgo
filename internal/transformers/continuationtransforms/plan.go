// Package continuationtransforms rewrites opted-in generator functions into
// park-boundary state machines: a generator can RETURN an outcome at a "park"
// ({ o: 'quiesce', ... }), the run's realm is torn down, and the next run
// re-invokes the function from scratch - the transform makes the frame
// (program counter + locals) survive that boundary through caller-supplied
// durable storage, so the re-invocation RESUMES where the park left off.
//
// Yields inside a run are untouched: within one run, native generator
// semantics carry them. Only the park boundary is checkpointed. The rewrite
// fails closed - a construct the transform cannot linearize or a local it
// cannot prove JSON-safe across a park produces a refusal diagnostic instead
// of an output, because a miscompiled checkpoint restores into a lie.
//
// Deliberately generic: nothing here knows what a park MEANS to any consumer.
// The outcome protocol is read off the types: a return statement whose
// expression has a literal `o` property of 'quiesce' parks, 'terminate'
// clears the frame, anything else is a plain return.
package continuationtransforms

import (
	"fmt"
	"slices"
	"strings"

	"github.com/apsis-io/tsgo/internal/ast"
	"github.com/apsis-io/tsgo/internal/checker"
)

// Spec carries the caller-supplied frame placement: which durable object
// holds the frame (FramePath) and at which dotted field inside the object
// body it lives (FrameField, e.g. "data.frame"). Which durable tier backs
// the object is the caller's business, not the compiler's.
type Spec struct {
	FramePath  string
	FrameField string
}

// Validate reports whether the spec names a place a frame can live.
func (s Spec) Validate() error {
	switch {
	case strings.TrimSpace(s.FramePath) == "":
		return fmt.Errorf("continuation: FramePath must name the durable object that holds the frame")
	case strings.TrimSpace(s.FrameField) == "":
		return fmt.Errorf("continuation: FrameField must be a dotted field path, e.g. \"data.frame\"")
	}
	for seg := range strings.SplitSeq(s.FrameField, ".") {
		if !isIdentifierText(seg) {
			return fmt.Errorf("continuation: FrameField %q must be a dotted path of identifiers", s.FrameField)
		}
	}

	return nil
}

func isIdentifierText(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_' || r == '$':
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}

	return true
}

// Refusal is a fail-closed diagnostic: the transform would miscompile the
// construct at Node, so the program is refused instead of rewritten.
type Refusal struct {
	Node    *ast.Node
	Message string
}

func (r Refusal) String() string {
	return r.Message
}

// Hoisted is one local lifted into the frame. Name is the frame key (the
// source name); Binding is the emitted binding name, deduplicated when two
// scopes use the same source name.
type Hoisted struct {
	Decl    *ast.Node // the VariableDeclaration or Parameter node
	Name    string
	Binding string
	IsParam bool
}

// ParkPoint is one park return: Ret checkpoints the frame at Label and
// returns; the statements after it are the continuation that Label resumes.
type ParkPoint struct {
	Ret   *ast.Node
	Label string
}

// Plan is a validated rewrite for one generator: Hoisted names the frame
// locals, Parks the checkpoint points, Terms the terminate returns (each
// gets a tombstone write). A Plan exists only when the rewrite is sound -
// refusals are reported instead of plans, never both.
type Plan struct {
	Fn      *ast.Node
	Hoisted []*Hoisted
	Parks   []*ParkPoint
	Terms   []*ast.Node
	// Names is every identifier text in the generator; helper bindings pick
	// non-colliding names from it.
	Names map[string]bool
}

// PlanFile analyzes every eligible generator in the file. A generator is
// eligible when it has at least one type-detected park return; generators
// without parks are left untouched (there is nothing to checkpoint, and
// rewriting them would add frame traffic to programs that never park).
//
// The returned plans map is keyed by the generator's function-like node, and
// the transform (which runs later, inside emit, on the same unmodified AST)
// looks its plans up by that identity.
func PlanFile(file *ast.SourceFile, c *checker.Checker, spec Spec) (map[*ast.Node]*Plan, []Refusal) {
	plans := make(map[*ast.Node]*Plan)
	var refusals []Refusal
	var generators []*ast.Node
	collectGenerators(file.AsNode(), &generators)
	for _, fn := range generators {
		a := &analyzer{c: c, fn: fn}
		a.run()
		if len(a.refusals) > 0 {
			refusals = append(refusals, a.refusals...)
			continue
		}
		if len(a.parks) == 0 {
			continue
		}
		plans[fn] = &Plan{Fn: fn, Hoisted: a.hoistedList, Parks: a.parkPoints(), Terms: a.terms, Names: a.usedNames}
	}

	return plans, refusals
}

// collectGenerators gathers every generator function-like with a body, in
// source order - declarations, expressions, and class methods. Nested
// generators are independent units and get their own plans.
func collectGenerators(n *ast.Node, out *[]*ast.Node) {
	if n == nil {
		return
	}
	if isGeneratorFunctionLike(n) && n.Body() != nil {
		*out = append(*out, n)
	}
	n.ForEachChild(func(child *ast.Node) bool {
		collectGenerators(child, out)
		return false
	})
}

func isGeneratorFunctionLike(n *ast.Node) bool {
	switch n.Kind {
	case ast.KindFunctionDeclaration:
		return n.AsFunctionDeclaration().AsteriskToken != nil
	case ast.KindFunctionExpression:
		return n.AsFunctionExpression().AsteriskToken != nil
	case ast.KindMethodDeclaration:
		return n.AsMethodDeclaration().AsteriskToken != nil
	}

	return false
}

// ─── analysis ───────────────────────────────────────────────────────────────
//
// One analyzer per generator. The walk is scope-aware (block scopes, function
// scopes, var hoisting to function scope) so identifier uses resolve to the
// declaration they actually reference; shadowed siblings must not merge.

type declFact struct {
	name    string
	node    *ast.Node // VariableDeclaration or Parameter node
	isParam bool
	pattern bool // destructured binding
	hoist   bool
}

type useFact struct {
	decl   *declFact
	node   *ast.Node
	pos    int
	nested bool // use occurs inside a nested function-like
}

type analyzer struct {
	c         *checker.Checker
	fn        *ast.Node
	scopes    []map[string]*declFact
	varBase   int // innermost function scope: where var declarations bind
	decls     []*declFact
	uses      []*useFact
	thisUses  []*useFact // `this` / `arguments` uses owned by this generator
	parks     []*ast.Node
	parkPos   []int
	terms     []*ast.Node
	refusals  []Refusal
	usedNames map[string]bool

	hoistedList []*Hoisted
}

func (a *analyzer) run() {
	a.usedNames = make(map[string]bool)
	a.pushScope() // function scope; var declarations bind here
	a.varBase = 0
	for _, p := range a.fn.Parameters() {
		a.registerBinding(p, p.Name(), true /*isParam*/)
		if init := p.Initializer(); init != nil {
			a.walkExprNested(init, false)
		}
		a.collectNames(p.Type())
	}
	if body := a.fn.Body(); body != nil {
		a.walkStmt(body.AsNode(), nil)
	}
	a.popScope()
	a.decideHoisting()
}

func (a *analyzer) pushScope() {
	a.scopes = append(a.scopes, make(map[string]*declFact))
}

func (a *analyzer) popScope() {
	a.scopes = a.scopes[:len(a.scopes)-1]
}

func (a *analyzer) declare(name string, node *ast.Node, isParam bool) *declFact {
	d := &declFact{name: name, node: node, isParam: isParam}
	a.decls = append(a.decls, d)
	a.scopes[len(a.scopes)-1][name] = d

	return d
}

func (a *analyzer) resolve(name string) *declFact {
	for _, scope := range slices.Backward(a.scopes) {
		if d, ok := scope[name]; ok {
			return d
		}
	}

	return nil
}

func (a *analyzer) registerUse(d *declFact, n *ast.Node, nested bool) {
	a.uses = append(a.uses, &useFact{decl: d, node: n, pos: n.Pos(), nested: nested})
}

func (a *analyzer) registerBinding(decl *ast.Node, nameNode *ast.Node, isParam bool) {
	switch {
	case nameNode == nil:
	case ast.IsIdentifier(nameNode):
		a.declare(nameNode.Text(), decl, isParam)
		a.usedNames[nameNode.Text()] = true
	default:
		// binding pattern: register each leaf binding; hoisting a pattern is
		// refused in decideHoisting rather than rewritten element-wise
		a.registerPattern(decl, nameNode)
	}
}

func (a *analyzer) registerPattern(decl *ast.Node, pattern *ast.Node) {
	pattern.ForEachChild(func(child *ast.Node) bool {
		switch {
		case ast.IsBindingElement(child) || child.Kind == ast.KindArrayBindingPattern || child.Kind == ast.KindObjectBindingPattern:
			a.registerPattern(decl, child)
		case ast.IsIdentifier(child):
			a.declare(child.Text(), decl, false).pattern = true
			a.usedNames[child.Text()] = true
		}
		return false
	})
}

func (a *analyzer) collectNames(n *ast.Node) {
	if n == nil {
		return
	}
	n.ForEachChild(func(child *ast.Node) bool {
		if child.Kind == ast.KindIdentifier {
			a.usedNames[child.Text()] = true
		}
		a.collectNames(child)
		return false
	})
}

// walkStmt walks statements tracking the forbidden constructs (loops, try,
// switch, labeled statement) a park may not live inside. nested marks uses
// collected inside a nested function-like.
func (a *analyzer) walkStmt(s *ast.Node, forbidden []string) {
	a.walkStmtNested(s, forbidden, false)
}

func (a *analyzer) walkStmtNested(s *ast.Node, forbidden []string, nested bool) {
	switch s.Kind {
	case ast.KindBlock:
		a.pushScope()
		for _, st := range s.AsBlock().Statements.Nodes {
			a.walkStmtNested(st, forbidden, nested)
		}
		a.popScope()

	case ast.KindVariableStatement:
		a.walkVariableStatement(s, nested)

	case ast.KindExpressionStatement:
		a.walkExprNested(s.AsExpressionStatement().Expression, nested)

	case ast.KindIfStatement:
		i := s.AsIfStatement()
		a.walkExprNested(i.Expression, nested)
		a.walkStmtNested(i.ThenStatement, forbidden, nested)
		if i.ElseStatement != nil {
			a.walkStmtNested(i.ElseStatement, forbidden, nested)
		}

	case ast.KindReturnStatement:
		if expr := s.AsReturnStatement().Expression; expr != nil {
			a.walkExprNested(expr, nested)
		}
		if !nested {
			a.classifyReturn(s, forbidden)
		}

	case ast.KindWhileStatement:
		w := s.AsWhileStatement()
		a.walkExprNested(w.Expression, nested)
		a.walkStmtNested(w.Statement, a.ban(forbidden, "a loop"), nested)

	case ast.KindDoStatement:
		d := s.AsDoStatement()
		a.walkStmtNested(d.Statement, a.ban(forbidden, "a loop"), nested)
		a.walkExprNested(d.Expression, nested)

	case ast.KindForStatement:
		f := s.AsForStatement()
		a.pushScope()
		if f.Initializer != nil {
			a.walkForInitializer(f.Initializer.AsNode(), nested)
		}
		if f.Condition != nil {
			a.walkExprNested(f.Condition, nested)
		}
		if f.Incrementor != nil {
			a.walkExprNested(f.Incrementor, nested)
		}
		a.walkStmtNested(f.Statement, a.ban(forbidden, "a loop"), nested)
		a.popScope()

	case ast.KindForInStatement, ast.KindForOfStatement:
		f := s.AsForInOrOfStatement()
		a.pushScope()
		a.walkExprNested(f.Expression, nested)
		if f.Initializer != nil {
			a.walkForInitializer(f.Initializer.AsNode(), nested)
		}
		a.walkStmtNested(f.Statement, a.ban(forbidden, "a loop"), nested)
		a.popScope()

	case ast.KindTryStatement:
		t := s.AsTryStatement()
		if t.TryBlock != nil {
			a.walkStmtNested(t.TryBlock.AsNode(), a.ban(forbidden, "a try block"), nested)
		}
		if t.CatchClause != nil {
			cl := t.CatchClause.AsCatchClause()
			a.pushScope()
			if cl.VariableDeclaration != nil {
				a.registerBinding(cl.VariableDeclaration, cl.VariableDeclaration.AsVariableDeclaration().Name(), false)
			}
			a.walkStmtNested(cl.Block.AsNode(), a.ban(forbidden, "a catch clause"), nested)
			a.popScope()
		}
		if t.FinallyBlock != nil {
			a.walkStmtNested(t.FinallyBlock.AsNode(), a.ban(forbidden, "a finally block"), nested)
		}

	case ast.KindSwitchStatement:
		sw := s.AsSwitchStatement()
		a.walkExprNested(sw.Expression, nested)
		a.pushScope() // one scope shared by all clauses
		for _, cl := range sw.CaseBlock.AsCaseBlock().Clauses.Nodes {
			a.walkStmtNested(cl, a.ban(forbidden, "a switch statement"), nested)
		}
		a.popScope()

	case ast.KindCaseClause, ast.KindDefaultClause:
		cl := s.AsCaseOrDefaultClause()
		if cl.Expression != nil {
			a.walkExprNested(cl.Expression, nested)
		}
		for _, st := range cl.Statements.Nodes {
			a.walkStmtNested(st, forbidden, nested)
		}

	case ast.KindLabeledStatement:
		l := s.AsLabeledStatement()
		a.walkStmtNested(l.Statement, a.ban(forbidden, "a labeled statement"), nested)

	case ast.KindThrowStatement:
		if e := s.AsThrowStatement().Expression; e != nil {
			a.walkExprNested(e, nested)
		}

	case ast.KindFunctionDeclaration,
		ast.KindFunctionExpression,
		ast.KindMethodDeclaration,
		ast.KindArrowFunction,
		ast.KindConstructor,
		ast.KindGetAccessor,
		ast.KindSetAccessor:
		a.walkNestedFunction(s)

	case ast.KindWithStatement:
		w := s.AsWithStatement()
		a.walkExprNested(w.Expression, nested)
		a.walkStmtNested(w.Statement, forbidden, nested)

	default:
		// debugger, empty, class declaration, etc. - names only
		a.collectNames(s)
	}
}

// ban extends the forbidden-construct stack; refusals name the innermost one.
func (a *analyzer) ban(forbidden []string, what string) []string {
	return append(slices.Clone(forbidden), what)
}

func (a *analyzer) walkVariableStatement(s *ast.Node, nested bool) {
	list := s.AsVariableStatement().DeclarationList.AsVariableDeclarationList()
	isVar := s.AsVariableStatement().DeclarationList.Flags&ast.NodeFlagsBlockScoped == 0
	// let/const bind in the enclosing scope; var binds at the innermost
	// enclosing function's scope
	target := len(a.scopes) - 1
	if isVar {
		target = a.varBase
	}
	a.scopes = append(a.scopes, a.scopes[target])
	a.walkDeclarations(list, nested)
	a.scopes = a.scopes[:len(a.scopes)-1]
}

func (a *analyzer) walkDeclarations(list *ast.VariableDeclarationList, nested bool) {
	for _, d := range list.Declarations.Nodes {
		decl := d.AsVariableDeclaration()
		a.registerBinding(d, decl.Name(), false)
		if decl.Initializer != nil {
			a.walkExprNested(decl.Initializer, nested)
		}
		a.collectNames(decl.Type)
	}
}

// walkForInitializer handles a for-loop head: a declaration list or a plain
// expression (for (i = 0; ...)), whose uses are ordinary identifier uses.
func (a *analyzer) walkForInitializer(init *ast.Node, nested bool) {
	if init.Kind == ast.KindVariableDeclarationList {
		list := init.AsVariableDeclarationList()
		isVar := init.Flags&ast.NodeFlagsBlockScoped == 0
		target := len(a.scopes) - 1
		if isVar {
			target = a.varBase // var-in-for binds at the function's scope
		}
		a.scopes = append(a.scopes, a.scopes[target])
		a.walkDeclarations(list, nested)
		a.scopes = a.scopes[:len(a.scopes)-1]

		return
	}
	a.walkExprNested(init, nested)
}

func (a *analyzer) walkNestedFunction(fn *ast.Node) {
	for _, p := range fn.Parameters() {
		if init := p.Initializer(); init != nil {
			a.walkExprNested(init, true)
		}
		a.collectNames(p.Type())
	}
	a.pushScope()
	savedBase := a.varBase
	a.varBase = len(a.scopes) - 1 // vars in the nested function bind to it
	if body := fn.Body(); body != nil {
		if body.Kind == ast.KindBlock {
			a.walkStmtNested(body.AsNode(), nil, true)
		} else {
			// expression-bodied arrow: the body is an expression
			a.walkExprNested(body, true)
		}
	}
	a.varBase = savedBase
	a.popScope()
}

// walkExprNested collects identifier uses (resolved through the scope stack
// to the declaration they reference) and helper-name collisions. It skips
// the positions where an identifier is not a reference to a local: property
// access names, object literal property names, declaration names, and type
// annotations. Inside nested function-likes everything is walked with
// nested=true - captures are conservative uses.
func (a *analyzer) walkExprNested(e *ast.Node, nested bool) {
	if e == nil {
		return
	}
	switch e.Kind {
	case ast.KindIdentifier:
		name := e.Text()
		a.usedNames[name] = true
		if d := a.resolve(name); d != nil {
			a.registerUse(d, e, nested)
		}

	case ast.KindThisKeyword:
		a.usedNames["this"] = true
		a.thisUses = append(a.thisUses, &useFact{node: e, pos: e.Pos(), nested: nested})

	case ast.KindSuperKeyword:
		a.usedNames["super"] = true

	case ast.KindPropertyAccessExpression:
		pa := e.AsPropertyAccessExpression()
		a.walkExprNested(pa.Expression, nested)
		if name := pa.Name(); name != nil {
			a.usedNames[name.Text()] = true
		}

	case ast.KindElementAccessExpression:
		ea := e.AsElementAccessExpression()
		a.walkExprNested(ea.Expression, nested)
		a.walkExprNested(ea.ArgumentExpression, nested)

	case ast.KindCallExpression:
		ce := e.AsCallExpression()
		a.walkExprNested(ce.Expression, nested)
		if ce.Arguments != nil {
			for _, arg := range ce.Arguments.Nodes {
				a.walkExprNested(arg, nested)
			}
		}

	case ast.KindNewExpression:
		ne := e.AsNewExpression()
		a.walkExprNested(ne.Expression, nested)
		if ne.Arguments != nil {
			for _, arg := range ne.Arguments.Nodes {
				a.walkExprNested(arg, nested)
			}
		}

	case ast.KindPropertyAssignment:
		pa := e.AsPropertyAssignment()
		if name := pa.Name(); name != nil && name.Kind == ast.KindComputedPropertyName {
			a.walkExprNested(name, nested)
		}
		a.walkExprNested(pa.Initializer, nested)

	case ast.KindFunctionDeclaration,
		ast.KindFunctionExpression,
		ast.KindMethodDeclaration,
		ast.KindArrowFunction,
		ast.KindConstructor,
		ast.KindGetAccessor,
		ast.KindSetAccessor:
		a.walkNestedFunction(e)

	default:
		e.ForEachChild(func(child *ast.Node) bool {
			a.walkExprNested(child, nested)
			return false
		})
	}
}

// classifyReturn reads the outcome protocol off the return expression's type.
func (a *analyzer) classifyReturn(s *ast.Node, forbidden []string) {
	expr := s.AsReturnStatement().Expression
	if expr == nil {
		return // plain
	}
	t := a.c.GetTypeAtLocation(expr)
	o := a.c.GetPropertyOfType(t, "o")
	if o == nil {
		return // not an outcome shape: plain
	}
	ot := a.c.GetTypeOfSymbolAtLocation(o, expr)
	members := []*checker.Type{ot}
	if ot.IsUnion() {
		members = ot.Types()
	}
	var lits []string
	for _, m := range members {
		if v, isStr := literalString(m); isStr {
			lits = append(lits, v)
		}
	}
	_, hasPark := contains(lits, "quiesce")
	_, hasTerm := contains(lits, "terminate")
	switch {
	case hasPark && hasTerm:
		a.refuse(s, "this return's type includes both the 'quiesce' and 'terminate' arms of the outcome union - "+
			"split it into one return per arm so the checkpoint decision is not a guess")
	case hasPark:
		if len(forbidden) > 0 {
			a.refuse(s, fmt.Sprintf("a park return inside %s cannot be checkpointed: %s state cannot resume - "+
				"the transform resumes at the statement after the park, so restructure until the park is not "+
				"nested inside one", forbidden[len(forbidden)-1], forbidden[len(forbidden)-1]))
			return
		}
		a.parks = append(a.parks, s)
		a.parkPos = append(a.parkPos, s.Pos())
	case hasTerm:
		// terminate ends the lifecycle: nothing resumes, so it may sit inside
		// any construct; only the checkpoint decision needed the linear shape
		a.terms = append(a.terms, s)
	}
}

func (a *analyzer) refuse(n *ast.Node, msg string) {
	a.refusals = append(a.refusals, Refusal{Node: n, Message: msg})
}

func literalString(t *checker.Type) (string, bool) {
	if !t.IsStringLiteral() {
		return "", false
	}
	v, ok := t.AsLiteralType().Value().(string)

	return v, ok
}

func contains(ss []string, v string) (int, bool) {
	i := slices.Index(ss, v)

	return i, i >= 0
}

// ─── hoisting and JSON safety ───────────────────────────────────────────────

// decideHoisting picks the locals that cross a park boundary: parameters and
// declarations with any use after a park they precede, or any use captured by
// a nested function (a capture is a use the lexical positions cannot rule
// out). A declaration made AFTER a park lives wholly in that park's
// continuation and stays put. `this`/`arguments` used after a park refuse: a
// resumed run is a fresh invocation, and neither survives it.
func (a *analyzer) decideHoisting() {
	for _, d := range a.decls {
		for _, u := range a.uses {
			if u.decl != d {
				continue
			}
			if u.nested {
				d.hoist = true
				break
			}
			for _, pp := range a.parkPos {
				if u.pos > pp && d.node.Pos() < pp {
					d.hoist = true
					break
				}
			}
			if d.hoist {
				break
			}
		}
	}
	for _, u := range a.thisUses {
		if u.nested {
			continue // a nested function's own this/arguments is not the generator's
		}
		for _, pp := range a.parkPos {
			if u.pos > pp {
				a.refuse(u.node, "`this`/`arguments` is used after a park return - a resumed run is a fresh "+
					"invocation, so neither survives the boundary; bind it to a JSON-safe local before the park")
				break
			}
		}
	}

	// frame keys are the source names, deduplicated in declaration order
	reachable := a.reachableDecls()
	seen := make(map[string]int)
	for _, d := range a.decls {
		if !d.hoist {
			continue
		}
		if d.pattern {
			a.refuse(d.node, fmt.Sprintf("the destructured local %q crosses a park boundary - "+
				"restructure to element-wise locals (const a = obj.a; ...) so each frame key is a plain name", d.name))
			continue
		}
		if !d.isParam && !reachable[d.node] {
			a.refuse(d.node, fmt.Sprintf("the local %q crosses a park boundary but its declaration sits inside a "+
				"construct the transform does not restructure (a loop head, switch clause, or try block) - "+
				"move the declaration to the top level of the generator body", d.name))
			continue
		}
		if !a.jsonSafeType(d) {
			a.refuse(d.node, fmt.Sprintf("the local %q crosses a park boundary but its type %s is not provably JSON-safe - "+
				"the frame survives as JSON, so restrict it to string / number / boolean / null / undefined / arrays / "+
				"plain objects built from those", d.name, a.c.TypeToString(a.c.GetTypeAtLocation(d.node))))
			continue
		}
		n := seen[d.name]
		seen[d.name] = n + 1
		binding := d.name
		if n > 0 {
			binding = fmt.Sprintf("%s$%d", d.name, n+1)
		}
		a.hoistedList = append(a.hoistedList, &Hoisted{Decl: d.node, Name: d.name, Binding: binding, IsParam: d.isParam})
	}
}

func (a *analyzer) parkPoints() []*ParkPoint {
	out := make([]*ParkPoint, len(a.parks))
	for i, ret := range a.parks {
		out[i] = &ParkPoint{Ret: ret, Label: fmt.Sprintf("resume%d", i+1)}
	}

	return out
}

func (a *analyzer) jsonSafeType(d *declFact) bool {
	return jsonSafe(a.c, a.c.GetTypeAtLocation(d.node), d.node)
}

// reachableDecls returns the variable declarations the rewrite can reach:
// those on the linear structure it restructures (the body list, bare blocks,
// if branches). A hoisted declaration anywhere else - a loop head, a switch
// clause, a try block - cannot be turned into an in-place assignment, and
// leaving the original `let` would collide with the hoisted binding, so the
// declaration refuses instead.
func (a *analyzer) reachableDecls() map[*ast.Node]bool {
	seen := make(map[*ast.Node]bool)
	var walkList func(stmts []*ast.Node)
	var walkStmt func(s *ast.Node)
	walkStmt = func(s *ast.Node) {
		switch s.Kind {
		case ast.KindBlock:
			walkList(s.AsBlock().Statements.Nodes)
		case ast.KindVariableStatement:
			for _, d := range s.AsVariableStatement().DeclarationList.AsVariableDeclarationList().Declarations.Nodes {
				seen[d] = true
			}
		case ast.KindIfStatement:
			i := s.AsIfStatement()
			walkStmt(i.ThenStatement)
			if i.ElseStatement != nil {
				walkStmt(i.ElseStatement)
			}
		}
	}
	walkList = func(stmts []*ast.Node) {
		for _, s := range stmts {
			walkStmt(s)
		}
	}
	if body := a.fn.Body(); body != nil && body.Kind == ast.KindBlock {
		walkList(body.AsBlock().Statements.Nodes)
	}

	return seen
}

// jsonSafe reports whether a value of t survives a JSON.stringify /
// JSON.parse round trip unchanged, for the allowlist: string, number,
// boolean, null, undefined, arrays and plain objects built from those.
// Bigint refuses (JSON.stringify throws on it); symbols, functions, enum
// members, class and interface instances, and anything not provable (any,
// unknown, type parameters) refuse - fail closed beats silently lossy.
func jsonSafe(c *checker.Checker, t *checker.Type, loc *ast.Node) bool {
	f := t.Flags()
	switch {
	case f&(checker.TypeFlagsString|checker.TypeFlagsStringLiteral|checker.TypeFlagsTemplateLiteral|
		checker.TypeFlagsNumber|checker.TypeFlagsNumberLiteral|
		checker.TypeFlagsBoolean|checker.TypeFlagsBooleanLiteral|
		checker.TypeFlagsUndefined|checker.TypeFlagsNull|checker.TypeFlagsVoid|checker.TypeFlagsNever) != 0:
		return true

	case f&checker.TypeFlagsUnion != 0:
		for _, m := range t.Types() {
			if !jsonSafe(c, m, loc) {
				return false
			}
		}
		return true

	case f&checker.TypeFlagsObject == 0:
		// bigint, symbol, enum, any, unknown, type parameters, conditional
		// types, intersections: nothing provably JSON-shaped
		return false
	}

	of := t.ObjectFlags()
	switch {
	case of&checker.ObjectFlagsTuple != 0:
		for _, arg := range c.GetTypeArguments(t) {
			if !jsonSafe(c, arg, loc) {
				return false
			}
		}
		return true

	case of&(checker.ObjectFlagsAnonymous|checker.ObjectFlagsObjectLiteral|checker.ObjectFlagsJSLiteral) != 0:
		if len(c.GetCallSignatures(t)) > 0 || len(c.GetConstructSignatures(t)) > 0 {
			return false
		}
		for _, prop := range c.GetPropertiesOfType(t) {
			if prop.Flags&(ast.SymbolFlagsFunction|ast.SymbolFlagsMethod|ast.SymbolFlagsGetAccessor|ast.SymbolFlagsSetAccessor) != 0 {
				return false
			}
			if !jsonSafe(c, c.GetTypeOfSymbolAtLocation(prop, loc), loc) {
				return false
			}
		}
		// index signatures must be JSON-safe too
		if num := c.GetNumberIndexType(t); num != nil && !jsonSafe(c, num, loc) {
			return false
		}
		if str := c.GetStringIndexType(t); str != nil && !jsonSafe(c, str, loc) {
			return false
		}
		return true

	case of&checker.ObjectFlagsReference != 0:
		// arrays only: the reference target must be the global Array (one
		// type argument), and the element type must round-trip
		if sym := t.Symbol(); sym != nil && sym.Name == "Array" {
			args := c.GetTypeArguments(t)
			if len(args) == 1 && jsonSafe(c, args[0], loc) {
				return true
			}
		}
		return false
	}

	return false
}
