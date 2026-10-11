package continuationtransforms

import (
	"fmt"
	"strings"

	"github.com/apsis-io/tsgo/internal/ast"
	"github.com/apsis-io/tsgo/internal/transformers"
)

// NewFactory builds the emit transformer applying the plans. It must run
// FIRST in the emit chain (compiler.EmitOptions.ScriptTransformersBefore):
// it rewrites the pristine parsed AST, and the standard transforms - type
// erasure in particular - run over its output. Plans are keyed by generator
// node identity, which holds because nothing has transformed the tree before
// this point.
//
// The frame I/O is emitted as the consumer's plain read and write effects -
// no new runtime surface, nothing needed from any SDK:
//
//	read:   const obs = yield { op: 'get', args: FramePath }
//	write:  yield { op: 'ensure', args: { path: FramePath,
//	                                     field: FrameField,
//	                                         value: JSON.stringify(frame) } }
func NewFactory(plans map[*ast.Node]*Plan, spec Spec) transformers.TransformerFactory {
	return func(opts *transformers.TransformOptions) *transformers.Transformer {
		tx := &transformer{plans: plans, spec: spec}
		tx.NewTransformer(tx.visit, opts.Context)

		return &tx.Transformer
	}
}

type transformer struct {
	transformers.Transformer
	plans map[*ast.Node]*Plan
	spec  Spec

	// per-rewrite state, valid while one generator is being rewritten
	plan        *Plan
	parkRet     map[*ast.Node]*ParkPoint
	parkChain   map[*ast.Node]*contNode
	termSet     map[*ast.Node]bool
	hoistByDecl map[*ast.Node]*Hoisted
	keys        map[string]string // binding -> frame key
	names       map[string]string // role -> emitted helper name
	taken       map[string]bool
	entry       []*ast.Node
}

// visit matches planned generator nodes and rewrites their bodies; everything
// else descends unchanged.
func (tx *transformer) visit(node *ast.Node) *ast.Node {
	plan, ok := tx.plans[node]
	if !ok {
		return tx.Visitor().VisitEachChild(node)
	}

	return tx.rewriteFunction(node, plan)
}

// ─── the rewrite ────────────────────────────────────────────────────────────
//
// The function body becomes: a hoisted frame read (the prologue), then one
// dispatch switch whose DEFAULT clause is the entry segment and whose cases
// are the parks' continuations. Every segment exits by returning - the entry
// through a park/terminate/return, a case through the same - so no segment
// ever falls into another, and every exit that is not a park or terminate
// writes the { pc: 'done' } tombstone first: a frame left pointing at a
// finished continuation would dispatch the next start-over run wrongly.

func (tx *transformer) rewriteFunction(fn *ast.Node, plan *Plan) *ast.Node {
	f := tx.Factory()
	tx.plan = plan
	tx.parkRet = make(map[*ast.Node]*ParkPoint, len(plan.Parks))
	tx.parkChain = make(map[*ast.Node]*contNode, len(plan.Parks))
	tx.termSet = make(map[*ast.Node]bool, len(plan.Terms))
	for _, p := range plan.Parks {
		tx.parkRet[p.Ret] = p
	}
	for _, t := range plan.Terms {
		tx.termSet[t] = true
	}
	tx.hoistByDecl = make(map[*ast.Node]*Hoisted, len(plan.Hoisted))
	for _, h := range plan.Hoisted {
		tx.hoistByDecl[h.Decl] = h
	}
	tx.initNames(plan)
	tx.initKeys(plan)

	body := fn.Body().AsBlock()
	tx.entry = tx.emitList(body.Statements.Nodes, nil, 0)
	stmts := append(tx.prologue(), tx.dispatch())

	// descend once more so generators nested inside the rewritten body get
	// their own rewrites
	for i, s := range stmts {
		stmts[i] = tx.Visitor().VisitNode(s)
	}

	newBody := f.NewBlock(f.NewNodeList(stmts), true)
	switch fn.Kind {
	case ast.KindFunctionDeclaration:
		d := fn.AsFunctionDeclaration()
		return f.UpdateFunctionDeclaration(d, fn.Modifiers(), d.AsteriskToken, d.Name(), d.TypeParameters, d.Parameters, d.Type, d.FullSignature, newBody)
	case ast.KindFunctionExpression:
		d := fn.AsFunctionExpression()
		return f.UpdateFunctionExpression(d, fn.Modifiers(), d.AsteriskToken, d.Name(), d.TypeParameters, d.Parameters, d.Type, d.FullSignature, newBody)
	case ast.KindMethodDeclaration:
		d := fn.AsMethodDeclaration()
		return f.UpdateMethodDeclaration(d, fn.Modifiers(), d.AsteriskToken, d.Name(), d.PostfixToken, d.TypeParameters, d.Parameters, d.Type, d.FullSignature, newBody)
	}
	panic("continuation: plan keyed on a non-function node")
}

// contNode is one link of a continuation chain: a statement that runs next,
// or a scope barrier (stmt == nil) marking that the chain leaves the braces
// it is currently inside. A barrier stops in-place materialization (the rest
// of the chain runs after the block) and is transparent when a park's
// continuation is materialized flat into its own case - hoisted bindings make
// out-of-braces references valid.
type contNode struct {
	stmt *ast.Node
	next *contNode
}

func (tx *transformer) barrier(after *contNode) *contNode {
	return &contNode{next: after}
}

// emitList materializes a statement list into segment statements, splitting
// at parks. next is the continuation that runs when the list completes
// normally; depth 0 is the function body, where natural completion appends
// the tombstone.
func (tx *transformer) emitList(stmts []*ast.Node, next *contNode, depth int) []*ast.Node {
	suffix := make([]*contNode, len(stmts)+1)
	suffix[len(stmts)] = next
	for i := len(stmts) - 1; i >= 0; i-- {
		suffix[i] = &contNode{stmt: stmts[i], next: suffix[i+1]}
	}
	out := make([]*ast.Node, 0, len(stmts)+2)
	for i, s := range stmts {
		if tx.emitStmt(s, suffix[i+1], &out, depth) {
			return out // path cut: the rest is the park's continuation, or dead
		}
	}
	if depth == 0 {
		out = append(out, tx.doneWrite())
	}

	return out
}

// emitChain materializes a park's continuation flat: barriers are transparent
// because a case body has no braces of its own to leave.
func (tx *transformer) emitChain(c *contNode) []*ast.Node {
	out := make([]*ast.Node, 0, 8)
	for ; c != nil; c = c.next {
		if c.stmt == nil {
			continue
		}
		if tx.emitStmt(c.stmt, c.next, &out, 0) {
			return out
		}
	}
	out = append(out, tx.doneWrite())

	return out
}

// emitStmt appends the materialized form of s to out, threading after as the
// continuation that runs when s completes normally. It reports whether s cut
// the path (park or terminate): what follows a park belongs to its
// continuation, and what follows a terminate is unreachable.
func (tx *transformer) emitStmt(s *ast.Node, after *contNode, out *[]*ast.Node, depth int) bool {
	f := tx.Factory()
	switch {
	case ast.IsReturnStatement(s):
		if point, isPark := tx.parkRet[s]; isPark {
			tx.parkChain[s] = after
			*out = append(*out, tx.frameWrite(point.Label), s)
			return true
		}
		*out = append(*out, tx.doneWrite())
		*out = append(*out, s)

		return tx.termSet[s]

	case s.Kind == ast.KindBlock:
		inner := tx.emitList(s.AsBlock().Statements.Nodes, tx.barrier(after), depth+1)
		*out = append(*out, f.NewBlock(f.NewNodeList(inner), true))

	case s.Kind == ast.KindIfStatement:
		i := s.AsIfStatement()
		then := tx.branch(i.ThenStatement, after, depth)
		var elseNode *ast.Node
		if i.ElseStatement != nil {
			elseNode = tx.branch(i.ElseStatement, after, depth)
		}
		*out = append(*out, f.UpdateIfStatement(i, i.Expression, then, elseNode))

	case s.Kind == ast.KindVariableStatement:
		*out = append(*out, tx.rewriteDeclaration(s)...)

	default:
		// loops, try, switch, labeled statements: the plan guarantees no park
		// inside, so the structure stays; only plain/terminate returns inside
		// gain their tombstone
		*out = append(*out, tx.rewriteReturnsIn(s))
	}

	return false
}

// branch materializes an if-branch (block or single statement), always
// braced: case clauses share one switch-body scope, and unbraced branch lets
// would collide across segments.
func (tx *transformer) branch(s *ast.Node, after *contNode, depth int) *ast.Node {
	stmts := []*ast.Node{s}
	if s.Kind == ast.KindBlock {
		stmts = s.AsBlock().Statements.Nodes
	}
	inner := tx.emitList(stmts, tx.barrier(after), depth+1)

	return tx.Factory().NewBlock(tx.Factory().NewNodeList(inner), true)
}

// rewriteDeclaration splits a variable statement whose declarators are
// hoisted: hoisted bindings become in-place assignments (the prologue owns
// the binding), the rest keep their declaration. A hoisted declarator without
// an initializer disappears - the prologue binding covers it. On resume the
// restored value must not be clobbered, which holds because the assignment
// lives in an earlier segment than every use that follows the park.
func (tx *transformer) rewriteDeclaration(s *ast.Node) []*ast.Node {
	f := tx.Factory()
	list := s.AsVariableStatement().DeclarationList.AsVariableDeclarationList()
	decls := list.Declarations.Nodes
	anyHoisted := false
	for _, d := range decls {
		if _, ok := tx.hoistByDecl[d]; ok {
			anyHoisted = true
			break
		}
	}
	if !anyHoisted {
		return []*ast.Node{s}
	}
	out := make([]*ast.Node, 0, len(decls))
	for _, d := range decls {
		decl := d.AsVariableDeclaration()
		h, hoisted := tx.hoistByDecl[d]
		switch {
		case !hoisted:
			out = append(out, f.NewVariableStatement(nil,
				f.NewVariableDeclarationList(f.NewNodeList([]*ast.Node{d}), s.AsVariableStatement().DeclarationList.Flags)))
		case decl.Initializer != nil:
			out = append(out, tx.assign(h.Binding, decl.Initializer))
		}
	}

	return out
}

// rewriteReturnsIn rewrites plain/terminate returns inside constructs the
// transform does not restructure (loops, try, switch, labeled statements).
// Parks cannot occur here - the plan refuses them - and constructs without
// such returns keep their original node.
func (tx *transformer) rewriteReturnsIn(s *ast.Node) *ast.Node {
	if !tx.hasRewritableReturn(s) {
		return s
	}
	f := tx.Factory()
	block := func(stmts []*ast.Node) *ast.Node {
		return f.NewBlock(f.NewNodeList(tx.rewriteReturnsList(stmts)), true)
	}
	single := func(s *ast.Node) *ast.Node {
		if s == nil {
			return nil
		}
		if s.Kind == ast.KindBlock {
			return block(s.AsBlock().Statements.Nodes)
		}
		return block([]*ast.Node{s})
	}
	switch s.Kind {
	case ast.KindBlock:
		b := s.AsBlock()
		return f.UpdateBlock(b, f.NewNodeList(tx.rewriteReturnsList(b.Statements.Nodes)), b.MultiLine)

	case ast.KindIfStatement:
		i := s.AsIfStatement()
		var elseNode *ast.Node
		if i.ElseStatement != nil {
			elseNode = single(i.ElseStatement)
		}
		return f.UpdateIfStatement(i, i.Expression, single(i.ThenStatement), elseNode)

	case ast.KindWhileStatement:
		w := s.AsWhileStatement()
		return f.UpdateWhileStatement(w, w.Expression, single(w.Statement))

	case ast.KindDoStatement:
		d := s.AsDoStatement()
		return f.UpdateDoStatement(d, single(d.Statement), d.Expression)

	case ast.KindForStatement:
		fv := s.AsForStatement()
		return f.UpdateForStatement(fv, fv.Initializer, fv.Condition, fv.Incrementor, single(fv.Statement))

	case ast.KindForInStatement, ast.KindForOfStatement:
		fv := s.AsForInOrOfStatement()
		return f.UpdateForInOrOfStatement(fv, fv.AwaitModifier, fv.Initializer, fv.Expression, single(fv.Statement))

	case ast.KindTryStatement:
		t := s.AsTryStatement()
		var catch *ast.Node
		if t.CatchClause != nil {
			cl := t.CatchClause.AsCatchClause()
			catch = f.UpdateCatchClause(cl, cl.VariableDeclaration, block(cl.Block.AsBlock().Statements.Nodes))
		}
		var finally *ast.Node
		if t.FinallyBlock != nil {
			finally = block(t.FinallyBlock.AsBlock().Statements.Nodes)
		}
		return f.UpdateTryStatement(t, block(t.TryBlock.AsBlock().Statements.Nodes), catch, finally)

	case ast.KindSwitchStatement:
		sw := s.AsSwitchStatement()
		oldClauses := sw.CaseBlock.AsCaseBlock().Clauses.Nodes
		clauses := make([]*ast.Node, 0, len(oldClauses))
		for _, cl := range oldClauses {
			c := cl.AsCaseOrDefaultClause()
			clauses = append(clauses, f.UpdateCaseOrDefaultClause(c, c.Expression,
				f.NewNodeList(tx.rewriteReturnsList(c.Statements.Nodes))))
		}
		return f.UpdateSwitchStatement(sw, sw.Expression,
			f.UpdateCaseBlock(sw.CaseBlock.AsCaseBlock(), f.NewNodeList(clauses)))

	case ast.KindLabeledStatement:
		l := s.AsLabeledStatement()
		return f.UpdateLabeledStatement(l, l.Label, single(l.Statement))
	}

	return s
}

func (tx *transformer) rewriteReturnsList(stmts []*ast.Node) []*ast.Node {
	out := make([]*ast.Node, 0, len(stmts))
	for _, s := range stmts {
		if ast.IsReturnStatement(s) {
			out = append(out, tx.doneWrite())
			out = append(out, s)
			continue
		}
		out = append(out, tx.rewriteReturnsIn(s))
	}

	return out
}

// hasRewritableReturn reports whether s contains a return this generator
// owns - the only thing the black-box rewrite changes. Nested function-likes
// keep their own returns.
func (tx *transformer) hasRewritableReturn(s *ast.Node) bool {
	if ast.IsReturnStatement(s) {
		return true
	}
	found := false
	var walk func(n *ast.Node) bool
	walk = func(n *ast.Node) bool {
		if found {
			return true
		}
		if n != s && isFunctionLikeAny(n) {
			return false // another function's returns are its own
		}
		if n != s && ast.IsReturnStatement(n) {
			found = true
			return true
		}
		n.ForEachChild(func(child *ast.Node) bool {
			return walk(child)
		})
		return found
	}
	walk(s)

	return found
}

func isFunctionLikeAny(n *ast.Node) bool {
	switch n.Kind {
	case ast.KindFunctionDeclaration, ast.KindFunctionExpression, ast.KindMethodDeclaration,
		ast.KindArrowFunction, ast.KindConstructor, ast.KindGetAccessor, ast.KindSetAccessor:
		return true
	}

	return false
}

// ─── the prologue ───────────────────────────────────────────────────────────

// prologue emits the frame read, hoisted bindings, and the restore, ending
// just before the dispatch. The read is HOISTED - once, unconditionally, at
// every invocation - rather than lazily placed at each resume point: the read
// is one cheap effect, and lazy placement would duplicate it, and the restore
// with it, once per park. Every run pays one get; nothing else about the run
// changes.
func (tx *transformer) prologue() []*ast.Node {
	f := tx.Factory()
	pc := tx.names["pc"]
	obs := tx.names["obs"]

	var hoistedLets []*ast.Node
	for _, h := range tx.plan.Hoisted {
		if !h.IsParam {
			hoistedLets = append(hoistedLets,
				f.NewVariableDeclaration(f.NewIdentifier(h.Binding), nil, nil, nil))
		}
	}

	stmts := make([]*ast.Node, 0, 4)
	stmts = append(stmts,
		f.NewVariableStatement(nil, f.NewVariableDeclarationList(f.NewNodeList([]*ast.Node{
			f.NewVariableDeclaration(f.NewIdentifier(obs), nil, nil,
				f.NewYieldExpression(nil, tx.getEffect())),
		}), ast.NodeFlagsConst)))
	if len(hoistedLets) > 0 {
		stmts = append(stmts,
			f.NewVariableStatement(nil, f.NewVariableDeclarationList(f.NewNodeList(hoistedLets), ast.NodeFlagsLet)))
	}
	stmts = append(stmts,
		f.NewVariableStatement(nil, f.NewVariableDeclarationList(f.NewNodeList([]*ast.Node{
			f.NewVariableDeclaration(f.NewIdentifier(pc), nil, nil, tx.str("start")),
		}), ast.NodeFlagsLet)))
	stmts = append(stmts, tx.restore())

	return stmts
}

// restore emits
//
//	if (obs.t === 'known') {
//	  const doc = JSON.parse(obs.v)
//	  if (<dotted field guard>) {
//	    const frame = JSON.parse(<dotted field>)
//	    <binding> = frame.<key>   (for every hoisted local)
//	    pc = frame.pc
//	  }
//	}
//
// spelling out the guard per segment exactly as the consumer's hand-written
// contract does. A corrupt or foreign frame is the caller's trust boundary;
// the transform adds no parsing of its own.
func (tx *transformer) restore() *ast.Node {
	f := tx.Factory()
	pc := tx.names["pc"]
	obs := tx.names["obs"]
	doc := tx.names["doc"]
	frame := tx.names["frame"]

	each := f.NewIdentifier(doc)
	for seg := range strings.SplitSeq(tx.spec.FrameField, ".") {
		each = tx.prop(each, seg)
	}
	fieldAccess := each

	restores := []*ast.Node{
		f.NewVariableStatement(nil, f.NewVariableDeclarationList(f.NewNodeList([]*ast.Node{
			f.NewVariableDeclaration(f.NewIdentifier(frame), nil, nil, tx.jsonParse(fieldAccess)),
		}), ast.NodeFlagsConst)),
	}
	for _, h := range tx.plan.Hoisted {
		restores = append(restores, tx.assign(h.Binding, tx.prop(f.NewIdentifier(frame), tx.keys[h.Binding])))
	}
	restores = append(restores, tx.assign(pc, tx.prop(f.NewIdentifier(frame), "pc")))
	guarded := f.NewIfStatement(tx.fieldGuard(doc), f.NewBlock(f.NewNodeList(restores), true), nil)

	parsed := f.NewBlock(f.NewNodeList([]*ast.Node{
		f.NewVariableStatement(nil, f.NewVariableDeclarationList(f.NewNodeList([]*ast.Node{
			f.NewVariableDeclaration(f.NewIdentifier(doc), nil, nil,
				tx.jsonParse(tx.prop(f.NewIdentifier(obs), "v"))),
		}), ast.NodeFlagsConst)),
		guarded,
	}), true)
	known := f.NewBinaryExpression(nil,
		tx.prop(f.NewIdentifier(obs), "t"), nil,
		f.NewToken(ast.KindEqualsEqualsEqualsToken), tx.str("known"))

	return f.NewIfStatement(known, parsed, nil)
}

// fieldGuard emits the null-safe chain over the dotted field: doc.a && doc.b
// for field "a.b" - every segment guarded, as the hand-written contract does.
func (tx *transformer) fieldGuard(doc string) *ast.Node {
	f := tx.Factory()
	var accesses []*ast.Node
	each := f.NewIdentifier(doc)
	for seg := range strings.SplitSeq(tx.spec.FrameField, ".") {
		each = tx.prop(each, seg)
		accesses = append(accesses, each)
	}
	result := accesses[0]
	for _, a := range accesses[1:] {
		result = f.NewBinaryExpression(nil, result, nil, f.NewToken(ast.KindAmpersandAmpersandToken), a)
	}

	return result
}

// dispatch emits the switch: default = the entry segment, one case per park.
// Unknown and 'done' program counters fall into default and start over - a
// terminate tombstone makes the next run a start-over run, which is the
// lifecycle's contract.
//
// Every clause body is a BLOCK: the clauses share one switch-body scope, and
// segments legitimately duplicate statements (a park inside an if leaves the
// post-if code on BOTH the fall-through path and the resumed path), so an
// unbraced `const` in one segment redeclares in its twin and the module
// fails to load.
func (tx *transformer) dispatch() *ast.Node {
	f := tx.Factory()
	clause := func(kind ast.Kind, label *ast.Node, stmts []*ast.Node) *ast.Node {
		body := f.NewBlock(f.NewNodeList(stmts), true)

		return f.NewCaseOrDefaultClause(kind, label, f.NewNodeList([]*ast.Node{body}))
	}
	clauses := make([]*ast.Node, 0, len(tx.plan.Parks)+1)
	clauses = append(clauses, clause(ast.KindDefaultClause, nil, tx.entry))
	for _, p := range tx.plan.Parks {
		chain, ok := tx.parkChain[p.Ret]
		if !ok {
			continue // a park behind a terminate cut is unreachable dead code
		}
		clauses = append(clauses, clause(ast.KindCaseClause, tx.str(p.Label), tx.emitChain(chain)))
	}

	return f.NewSwitchStatement(f.NewIdentifier(tx.names["pc"]), f.NewCaseBlock(f.NewNodeList(clauses)))
}

// ─── frame I/O ──────────────────────────────────────────────────────────────

// getEffect emits the read effect expression `{ op: 'get', args: FramePath }`
// - args IS the path, per the consumer's effect grammar.
func (tx *transformer) getEffect() *ast.Node {
	f := tx.Factory()
	effect := f.NewObjectLiteralExpression(f.NewNodeList([]*ast.Node{
		tx.propAssign("op", tx.str("get")),
		tx.propAssign("args", tx.str(tx.spec.FramePath)),
	}), false)

	return effect
}

// frameWrite emits the checkpoint: a write effect carrying
// JSON.stringify({ pc: label, ...hoisted }). The caller appends the original
// return right after. Every write carries the same frame shape - pc plus the
// JSON-safe locals - so the consumer's { pc, ...locals } convention holds at
// parks and at the tombstone alike. The spelling is the caller's: WriteOp
// names the op (default "ensure"), and WriteArgs picks {path, field, value}
// (default) or the frame JSON as args directly.
func (tx *transformer) frameWrite(label string) *ast.Node {
	f := tx.Factory()
	props := make([]*ast.Node, 0, len(tx.plan.Hoisted)+1)
	props = append(props, tx.propAssign("pc", tx.str(label)))
	for _, h := range tx.plan.Hoisted {
		props = append(props, tx.propAssign(tx.keys[h.Binding], f.NewIdentifier(h.Binding)))
	}
	frameObj := f.NewObjectLiteralExpression(f.NewNodeList(props), false)

	stringify := f.NewCallExpression(tx.prop(f.NewIdentifier("JSON"), "stringify"), nil, nil,
		f.NewNodeList([]*ast.Node{frameObj}), ast.NodeFlagsNone)

	var args *ast.Node
	if tx.spec.writeArgsIsString() {
		args = stringify
	} else {
		args = f.NewObjectLiteralExpression(f.NewNodeList([]*ast.Node{
			tx.propAssign("path", tx.str(tx.spec.FramePath)),
			tx.propAssign("field", tx.str(tx.spec.FrameField)),
			tx.propAssign("value", stringify),
		}), false)
	}
	effect := f.NewObjectLiteralExpression(f.NewNodeList([]*ast.Node{
		tx.propAssign("op", tx.str(tx.spec.writeOp())),
		tx.propAssign("args", args),
	}), false)

	return f.NewExpressionStatement(f.NewYieldExpression(nil, effect))
}

func (tx *transformer) doneWrite() *ast.Node {
	return tx.frameWrite("done")
}

// ─── node helpers ───────────────────────────────────────────────────────────

func (tx *transformer) str(text string) *ast.Node {
	return tx.Factory().NewStringLiteral(text, 0)
}

func (tx *transformer) prop(e *ast.Node, name string) *ast.Node {
	return tx.Factory().NewPropertyAccessExpression(e, nil, tx.Factory().NewIdentifier(name), ast.NodeFlagsNone)
}

func (tx *transformer) propAssign(name string, init *ast.Node) *ast.Node {
	return tx.Factory().NewPropertyAssignment(nil, tx.Factory().NewIdentifier(name), nil, nil, init)
}

func (tx *transformer) jsonParse(e *ast.Node) *ast.Node {
	f := tx.Factory()

	return f.NewCallExpression(tx.prop(f.NewIdentifier("JSON"), "parse"), nil, nil,
		f.NewNodeList([]*ast.Node{e}), ast.NodeFlagsNone)
}

func (tx *transformer) assign(binding string, init *ast.Node) *ast.Node {
	f := tx.Factory()

	return f.NewExpressionStatement(f.NewAssignmentExpression(f.NewIdentifier(binding), init))
}

// ─── naming ─────────────────────────────────────────────────────────────────

// initNames picks non-colliding helper names from the generator's own
// identifier inventory.
func (tx *transformer) initNames(plan *Plan) {
	tx.taken = make(map[string]bool, len(plan.Names))
	for name := range plan.Names {
		tx.taken[name] = true
	}
	tx.names = make(map[string]string, 4)
	for _, role := range []string{"pc", "obs", "doc", "frame"} {
		tx.names[role] = tx.pickName(role)
	}
}

func (tx *transformer) pickName(base string) string {
	for cand := base; ; cand += "$" {
		if !tx.taken[cand] {
			tx.taken[cand] = true

			return cand
		}
	}
}

// initKeys deduplicates frame keys: the source name when free, a numbered
// variant when a previous hoisted local took it or the name is "pc" - the
// pc key is reserved by the consumer's convention, and the two must never
// meet in one JSON object.
func (tx *transformer) initKeys(plan *Plan) {
	tx.keys = make(map[string]string, len(plan.Hoisted))
	taken := map[string]bool{"pc": true}
	for _, h := range plan.Hoisted {
		key := h.Name
		if taken[key] {
			for i := 2; taken[key]; i++ {
				key = fmt.Sprintf("%s$%d", h.Name, i)
			}
		}
		taken[key] = true
		tx.keys[h.Binding] = key
	}
}
