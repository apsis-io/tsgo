package tsapi

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The outcome protocol, exactly as the consumer types it: a discriminated
// union with literal `o` fields. The literal types are what make the
// checkpoint decision type-directed.
const contEntry = "/src/step.ts"

const contPrelude = `type Effect = { op: 'get' | 'ensure'; args: any }
type Outcome =
  | { o: 'yield'; carry?: unknown }
  | { o: 'quiesce'; resume: { after: number } }
  | { o: 'terminate' }
`

var contOptions = ContinuationOptions{
	FramePath:  "/apis/apps/v1/namespaces/default/deployments/cont-marker",
	FrameField: "data.frame",
}

func contProject(t *testing.T, src string, cont *ContinuationOptions) *Project {
	t.Helper()
	p, err := NewProject(map[string]string{contEntry: contPrelude + src}, []string{contEntry},
		Options{Strict: true, SkipLibCheck: true, Continuation: cont})
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}

	return p
}

func contEmit(t *testing.T, src string, cont *ContinuationOptions) EmitResult {
	t.Helper()
	res, err := contProject(t, src, cont).EmitJS(context.Background(), contEntry)
	if err != nil {
		t.Fatalf("EmitJS: %v", err)
	}

	return res
}

// The straight-line park - the acceptance contract's step, as an AUTHOR
// writes it: increment, park; on resume increment again, commit, terminate.
const contStraightLine = contPrelude + `
export const step = function* (): Generator<Effect, Outcome, any> {
  let total = 0
  total = total + 1
  return { o: 'quiesce', resume: { after: 5000 } }
  total = total + 1
  yield { op: 'ensure', args: { path: '/m', field: 'data.final', value: total } }
  return { o: 'terminate' }
}
`

func TestContinuationStraightLinePark(t *testing.T) {
	res := contEmit(t, contStraightLine, &contOptions)
	if res.JS == "" {
		t.Fatalf("emitted no JavaScript; diagnostics:\n%s", FormatDiagnostics(res.Diagnostics))
	}
	if len(res.Diagnostics) != 0 {
		t.Fatalf("emit diagnostics:\n%s", FormatDiagnostics(res.Diagnostics))
	}
	// the prologue: one frame read, before everything
	if !strings.Contains(res.JS, `op: "get"`) {
		t.Fatalf("no frame read in the emitted step:\n%s", res.JS)
	}
	// the dispatch and the checkpoint
	for _, want := range []string{
		`"start"`,        // the fresh pc
		`case "resume1"`, // the park's continuation
		`pc: "resume1"`,  // the checkpoint frame
		`JSON.stringify`, // the frame rides as JSON
		`data.frame`,     // at the caller's field
		`'quiesce'`,      // the original outcome returns after the write
	} {
		if !strings.Contains(res.JS, want) {
			t.Errorf("emitted step is missing %s:\n%s", want, res.JS)
		}
	}
	if strings.Index(res.JS, `op: "get"`) > strings.Index(res.JS, `case "resume1"`) {
		t.Errorf("the frame read must precede the dispatch:\n%s", res.JS)
	}
}

func TestContinuationFrameCrossesTheRuntimeBoundary(t *testing.T) {
	// The consumer's TestContinuation, mirrored: two runs against one host
	// object, nothing shared between them but the frame. The local (total)
	// must cross.
	res := contEmit(t, contStraightLine, &contOptions)
	scenario := runEngine(t, res.JS, []engineRun{{}, {}}, contOptions.FramePath, map[string]any{
		contOptions.FramePath: map[string]any{"data": map[string]any{}},
	})
	runs := scenario["runs"].([]any)
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(runs))
	}
	outcome1 := runs[0].(map[string]any)["outcome"].(map[string]any)
	if outcome1["o"] != "quiesce" {
		t.Fatalf("run one outcome = %v, want parked (the continuation point is the park)", outcome1["o"])
	}
	outcome2 := runs[1].(map[string]any)["outcome"].(map[string]any)
	if outcome2["o"] != "terminate" {
		t.Fatalf("run two outcome = %v, want terminated", outcome2["o"])
	}
	// total must be 2 - one increment per run - and the frame is the only
	// thing the two runs share
	store := scenario["store"].(map[string]any)
	body := store[contOptions.FramePath].(map[string]any)
	data := body["data"].(map[string]any)
	if got, ok := data["final"].(float64); !ok || got != 2 {
		t.Fatalf("the continuation did not deliver the local across the boundary: %v - total should be 2", data["final"])
	}
	frame, ok := data["frame"].(string)
	if !ok || !strings.Contains(frame, `"pc":"done"`) {
		t.Fatalf("the frame did not reach its terminal state: %v", data["frame"])
	}
}

func TestContinuationTwoParksBranch(t *testing.T) {
	const src = `
export const step = function* (): Generator<Effect, Outcome, any> {
  let total = 0
  total = total + 1
  return { o: 'quiesce', resume: { after: 1 } }
  total = total + 10
  if (total > 10) {
    return { o: 'quiesce', resume: { after: 2 } }
  }
  yield { op: 'ensure', args: { path: '/m', field: 'data.final', value: total } }
  return { o: 'terminate' }
}
`
	res := contEmit(t, src, &contOptions)
	if res.JS == "" {
		t.Fatalf("emitted no JavaScript; diagnostics:\n%s", FormatDiagnostics(res.Diagnostics))
	}
	scenario := runEngine(t, res.JS, []engineRun{{}, {}, {}}, contOptions.FramePath, map[string]any{
		contOptions.FramePath: map[string]any{"data": map[string]any{}},
	})
	runs := scenario["runs"].([]any)
	want := []string{"quiesce", "quiesce", "terminate"}
	for i, w := range want {
		outcome := runs[i].(map[string]any)["outcome"].(map[string]any)
		if outcome["o"] != w {
			t.Fatalf("run %d outcome = %v, want %s", i+1, outcome["o"], w)
		}
	}
	store := scenario["store"].(map[string]any)
	data := store[contOptions.FramePath].(map[string]any)["data"].(map[string]any)
	// run 2 resumed at resume1 (+10), parked again inside the branch; run 3
	// resumed at resume2 - the statement AFTER the if, with total=11 restored
	// from the frame - and committed it. Both resumes landed on the right
	// statement with the right locals.
	if got, ok := data["final"].(float64); !ok || got != 11 {
		t.Fatalf("final = %v, want 11 (1 + 10, restored from the frame into the second resume)", data["final"])
	}
}

func TestContinuationRefuses(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string // a substring of the expected refusal
	}{
		{
			name: "park inside a for-of",
			src: `
export const step = function* (): Generator<Effect, Outcome, any> {
  let total = 0
  for (const item of [1, 2]) {
    total = total + item
    return { o: 'quiesce', resume: { after: 1 } }
  }
  return { o: 'terminate' }
}
`,
			want: "a loop",
		},
		{
			name: "park inside a while",
			src: `
export const step = function* (): Generator<Effect, Outcome, any> {
  let n = 3
  while (n > 0) {
    n = n - 1
    return { o: 'quiesce', resume: { after: 1 } }
  }
  return { o: 'terminate' }
}
`,
			want: "a loop",
		},
		{
			name: "park inside a try/finally",
			src: `
export const step = function* (): Generator<Effect, Outcome, any> {
  let total = 0
  try {
    total = total + 1
    return { o: 'quiesce', resume: { after: 1 } }
  } finally {
    total = 0
  }
}
`,
			want: "a try block",
		},
		{
			name: "function-valued local crossing a park",
			src: `
export const step = function* (): Generator<Effect, Outcome, any> {
  const bump = () => 1
  let total = bump()
  return { o: 'quiesce', resume: { after: 1 } }
  total = total + bump()
  return { o: 'terminate' }
}
`,
			want: "not provably JSON-safe",
		},
		{
			name: "class instance crossing a park",
			src: `
class Counter { n = 1 }
export const step = function* (): Generator<Effect, Outcome, any> {
  const c = new Counter()
  let total = c.n
  return { o: 'quiesce', resume: { after: 1 } }
  total = total + c.n
  return { o: 'terminate' }
}
`,
			want: "not provably JSON-safe",
		},
		{
			name: "destructured local crossing a park",
			src: `
export const step = function* (): Generator<Effect, Outcome, any> {
  const { a, b } = { a: 1, b: 2 }
  let total = a + b
  return { o: 'quiesce', resume: { after: 1 } }
  total = total + a
  return { o: 'terminate' }
}
`,
			want: "destructured",
		},
		{
			name: "this used after a park",
			src: `
export const step = function* (this: any): Generator<Effect, Outcome, any> {
  let total = 0
  total = total + 1
  return { o: 'quiesce', resume: { after: 1 } }
  total = total + (this.extra ?? 0)
  return { o: 'terminate' }
}
`,
			want: "this`/`arguments",
		},
		{
			name: "one return covering two outcome arms",
			src: `
export const step = function* (done: boolean): Generator<Effect, Outcome, any> {
  let total = 0
  total = total + 1
  return done ? { o: 'terminate' } : { o: 'quiesce', resume: { after: 1 } }
  total = total + 1
  return { o: 'terminate' }
}
`,
			want: "both the 'quiesce' and 'terminate' arms",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := contEmit(t, tc.src, &contOptions)
			if res.JS != "" {
				t.Fatalf("refused programs must emit nothing, got:\n%s", res.JS)
			}
			if len(res.Diagnostics) == 0 {
				t.Fatal("expected a refusal diagnostic, got none")
			}
			var texts []string
			for _, d := range res.Diagnostics {
				texts = append(texts, d.Error())
				if d.Code != continuationRefusalCode {
					t.Errorf("refusal code = %d, want %d", d.Code, continuationRefusalCode)
				}
				if d.Line == 0 || d.Char == 0 {
					t.Errorf("refusal is not positioned: %+v", d)
				}
			}
			if !strings.Contains(strings.Join(texts, "\n"), tc.want) {
				t.Errorf("refusals %q do not mention %q", texts, tc.want)
			}
		})
	}
}

// A generator the transform must leave alone - no park returns - passes
// through BYTE-IDENTICAL whether the opt-in is on or off. An automatic
// rewrite of every generator would be a language change; this is the proof
// it is not one.
func TestContinuationOptInSparesNonParks(t *testing.T) {
	const src = contPrelude + `
export const step = function* (): Generator<Effect, string, any> {
  yield { op: 'get', args: '/m' }
  yield { op: 'ensure', args: { path: '/m', field: 'data.n', value: 1 } }
  return 'done'
}
`
	off := contEmit(t, src, nil)
	on := contEmit(t, src, &contOptions)
	if off.JS == "" || on.JS == "" {
		t.Fatalf("both emits must produce JavaScript (off %d bytes, on %d bytes)", len(off.JS), len(on.JS))
	}
	if off.JS != on.JS {
		t.Fatalf("a generator without parks changed under the opt-in:\n--- off ---\n%s\n--- on ---\n%s", off.JS, on.JS)
	}
	// and a program WITH parks under the opt-in OFF must stay plain too: the
	// fixture's source never mentions the compiled dispatch
	plain := contEmit(t, contStraightLine, nil)
	if strings.Contains(plain.JS, `case "resume1"`) || strings.Contains(plain.JS, `"start"`) {
		t.Fatalf("the opt-in OFF rewrote the generator:\n%s", plain.JS)
	}
}

func TestContinuationSpecValidation(t *testing.T) {
	for _, tc := range []ContinuationOptions{
		{FrameField: "data.frame"},
		{FramePath: "/m"},
		{FramePath: "/m", FrameField: "not an identifier"},
		{FramePath: "/m", FrameField: "data.frame", WriteArgs: "blob"},
		{FramePath: "/m", FrameField: "data.frame", WriteOp: "not an op"},
	} {
		if _, err := contProject(t, contStraightLine, &tc).EmitJS(context.Background(), contEntry); err == nil {
			t.Errorf("spec %+v must be refused", tc)
		}
	}
}

// ─── the engine harness ─────────────────────────────────────────────────────

type engineRun struct{}

// driverApply teaches the driver one extra write op: effects named Op carry
// their args to the run object's dotted Field (e.g. a host's carry spelling
// writing status.carry).
type driverApply struct {
	op    string
	field string
}

// runEngine executes the emitted JS through the bun driver: one fresh module
// instantiation per run, an in-memory object store between them. Skips when
// bun is not installed; the textual fixtures above still hold everywhere.
func runEngine(t *testing.T, js string, runs []engineRun, marker string, store map[string]any, apply ...driverApply) map[string]any {
	t.Helper()
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun not available; the behavioral fixture needs it")
	}
	dir := t.TempDir()
	type runSpec struct {
		Module string `json:"module"`
		Path   string `json:"path"`
	}
	type spec struct {
		Path       string         `json:"path"`
		Export     string         `json:"export"`
		Store      map[string]any `json:"store"`
		ApplyOp    string         `json:"applyOp,omitempty"`
		ApplyField string         `json:"applyField,omitempty"`
		Runs       []runSpec      `json:"runs"`
	}
	s := spec{Path: marker, Export: "step", Store: store}
	if len(apply) > 0 {
		s.ApplyOp = apply[0].op
		s.ApplyField = apply[0].field
	}
	for i := range runs {
		name := filepath.Join(dir, "step"+string(rune('0'+i))+".mjs")
		if err := os.WriteFile(name, []byte(js), 0o644); err != nil {
			t.Fatalf("write module: %v", err)
		}
		s.Runs = append(s.Runs, runSpec{Module: name, Path: marker})
	}
	specJSON, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	specFile := filepath.Join(dir, "spec.json")
	if err := os.WriteFile(specFile, specJSON, 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	driver, err := filepath.Abs(filepath.Join("testdata", "continuation", "driver.mjs"))
	if err != nil {
		t.Fatalf("driver path: %v", err)
	}
	cmd := exec.Command(bun, driver, specFile)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bun driver: %v\n%s", err, out)
	}
	var scenario map[string]any
	if err := json.Unmarshal(out, &scenario); err != nil {
		t.Fatalf("driver output is not JSON: %v\n%s", err, out)
	}

	return scenario
}

// A local kept alive only through a closure declared AFTER the park: `base`
// has no direct use past the boundary, but the capture is a use the lexical
// positions cannot rule out, so it must be hoisted and restored.
func TestContinuationCapturedLocalCrossesTheBoundary(t *testing.T) {
	const src = contPrelude + `
export const step = function* (): Generator<Effect, Outcome, any> {
  let base = 2
  let total = base
  return { o: 'quiesce', resume: { after: 1 } }
  const wrap = () => base
  total = total + wrap()
  yield { op: 'ensure', args: { path: '/m', field: 'data.final', value: total } }
  return { o: 'terminate' }
}
`
	res := contEmit(t, src, &contOptions)
	if res.JS == "" {
		t.Fatalf("emitted no JavaScript; diagnostics:\n%s", FormatDiagnostics(res.Diagnostics))
	}
	scenario := runEngine(t, res.JS, []engineRun{{}, {}}, contOptions.FramePath, map[string]any{
		contOptions.FramePath: map[string]any{"data": map[string]any{}},
	})
	runs := scenario["runs"].([]any)
	outcome2 := runs[1].(map[string]any)["outcome"].(map[string]any)
	if outcome2["o"] != "terminate" {
		t.Fatalf("run two outcome = %v, want terminated", outcome2["o"])
	}
	store := scenario["store"].(map[string]any)
	data := store[contOptions.FramePath].(map[string]any)["data"].(map[string]any)
	// total = 2 (restored from the frame) + wrap() = 2 (the closure reading
	// the restored base) = 4: without the capture-driven hoist, base would be
	// undefined on resume and final would be NaN
	if got, ok := data["final"].(float64); !ok || got != 4 {
		t.Fatalf("final = %v, want 4 - the captured local must cross the park", data["final"])
	}
}

// The consumer's convention reserves the pc key; a user local literally named
// pc must get its own frame key, never collide with the program counter, and
// still round-trip.
func TestContinuationUserLocalNamedPc(t *testing.T) {
	const src = contPrelude + `
export const step = function* (): Generator<Effect, Outcome, any> {
  let pc = 0
  pc = pc + 1
  return { o: 'quiesce', resume: { after: 1 } }
  pc = pc + 1
  yield { op: 'ensure', args: { path: '/m', field: 'data.final', value: pc } }
  return { o: 'terminate' }
}
`
	res := contEmit(t, src, &contOptions)
	if res.JS == "" {
		t.Fatalf("emitted no JavaScript; diagnostics:\n%s", FormatDiagnostics(res.Diagnostics))
	}
	scenario := runEngine(t, res.JS, []engineRun{{}, {}}, contOptions.FramePath, map[string]any{
		contOptions.FramePath: map[string]any{"data": map[string]any{}},
	})
	runs := scenario["runs"].([]any)
	if got := runs[1].(map[string]any)["outcome"].(map[string]any)["o"]; got != "terminate" {
		t.Fatalf("run two outcome = %v, want terminated", got)
	}
	store := scenario["store"].(map[string]any)
	data := store[contOptions.FramePath].(map[string]any)["data"].(map[string]any)
	if got, ok := data["final"].(float64); !ok || got != 2 {
		t.Fatalf("final = %v, want 2 - the user's pc local must survive under its own frame key", data["final"])
	}
}

// A park with nothing after it: its continuation is the function's end. The
// resume runs the tombstone and ends the lifecycle; the run after THAT is a
// start-over run, dispatched from the top with a fresh frame read.
func TestContinuationTrailingParkStartsOver(t *testing.T) {
	const src = contPrelude + `
export const step = function* (): Generator<Effect, Outcome, any> {
  let n = 7
  yield { op: 'ensure', args: { path: '/m', field: 'data.seen', value: n } }
  return { o: 'quiesce', resume: { after: 1 } }
}
`
	res := contEmit(t, src, &contOptions)
	scenario := runEngine(t, res.JS, []engineRun{{}, {}, {}}, contOptions.FramePath, map[string]any{
		contOptions.FramePath: map[string]any{"data": map[string]any{}},
	})
	runs := scenario["runs"].([]any)
	if o := runs[0].(map[string]any)["outcome"].(map[string]any)["o"]; o != "quiesce" {
		t.Fatalf("run one outcome = %v, want quiesce", o)
	}
	// run two: dispatched at the park's continuation - the function's end -
	// writes the tombstone and returns undefined
	if o := runs[1].(map[string]any)["outcome"]; o != nil {
		t.Fatalf("run two outcome = %v, want undefined (clean end)", o)
	}
	// run three: the tombstone makes it a start-over run: the step runs from
	// the top and parks again
	if o := runs[2].(map[string]any)["outcome"].(map[string]any)["o"]; o != "quiesce" {
		t.Fatalf("run three outcome = %v, want quiesce (start-over)", o)
	}
	store := scenario["store"].(map[string]any)
	data := store[contOptions.FramePath].(map[string]any)["data"].(map[string]any)
	frame, ok := data["frame"].(string)
	if !ok || !strings.Contains(frame, `"pc":"resume1"`) {
		t.Fatalf("the start-over run did not re-checkpoint: %v", data["frame"])
	}
}

// The latch-read step kinetics-main filed as a transform bug (their
// continuation_transformed_test.go): a body-root total the step READS but
// nothing WRITES, with its only increment before the park. The trace pins
// the contract's exact behavior so it stays settled: run one checkpoints
// {pc:resume1, marker, total:1}; run two resumes with total restored = 1
// (the tombstone frame written in run two carries it - the restore worked),
// commits final=1, terminates. final=2 is unreachable under ANY coherent
// resume semantics: re-run-from-top would clobber the restore with
// `let total = 0` and park again (1 < 2); the single increment runs once
// per lifecycle by construction. Per-run work belongs in the continuation,
// which is exactly the hand-written contract's shape.
func TestContinuationLatchStepContractTraced(t *testing.T) {
	const src = contPrelude + `
export const step = function* (): Generator<Effect, Outcome, any> {
  const marker = '/apis/apps/v1/namespaces/default/deployments/cont-marker'
  const obs = yield { op: 'get', args: marker }
  let total = 0
  if (obs.t === 'known') {
    const f = JSON.parse(obs.v)
    total = f.total ?? 0
  }
  total = total + 1
  if (total < 2) {
    return { o: 'quiesce', resume: { after: 5000 } }
  }
  yield { op: 'ensure', args: { path: marker, field: 'data.final', value: total } }
  return { o: 'terminate' }
}
`
	res := contEmit(t, src, &contOptions)
	if res.JS == "" {
		t.Fatalf("emitted no JavaScript; diagnostics:\n%s", FormatDiagnostics(res.Diagnostics))
	}
	scenario := runEngine(t, res.JS, []engineRun{{}, {}}, contOptions.FramePath, map[string]any{
		contOptions.FramePath: map[string]any{"data": map[string]any{}},
	})
	runs := scenario["runs"].([]any)
	if o := runs[0].(map[string]any)["outcome"].(map[string]any)["o"]; o != "quiesce" {
		t.Fatalf("run one outcome = %v, want quiesce", o)
	}
	if o := runs[1].(map[string]any)["outcome"].(map[string]any)["o"]; o != "terminate" {
		t.Fatalf("run two outcome = %v, want terminate", o)
	}
	store := scenario["store"].(map[string]any)
	data := store[contOptions.FramePath].(map[string]any)["data"].(map[string]any)
	if got, ok := data["final"].(float64); !ok || got != 1 {
		t.Fatalf("final = %v, want 1 (the increment is pre-park: once per lifecycle)", got)
	}
	frame, ok := data["frame"].(string)
	if !ok || !strings.Contains(frame, `"total":1`) || !strings.Contains(frame, `"pc":"done"`) {
		t.Fatalf("the tombstone frame should carry the restored total=1: %v", data["frame"])
	}
}

// kinetics-main's live candidate shape, filed as transform bug #2: a park
// inside an if with statements AFTER the if (reachable on the fall-through
// path), including a const. The post-if statements legitimately live on BOTH
// the fall-through path and the park's continuation case - the clause bodies
// must be blocks, or the twin declarations share the switch-body scope and
// the module fails to LOAD ("has already been declared"), before any code
// runs.
func TestContinuationConstInContinuationLoads(t *testing.T) {
	const src = contPrelude + `
export const step = function* (): Generator<Effect, Outcome, any> {
  let total = 0
  total = total + 1
  if (total < 2) {
    return { o: 'quiesce', resume: { after: 1 } }
  }
  const cm = 'configmaps/cont-live'
  yield { op: 'ensure', args: { path: '/m', field: 'data.final', value: cm + ':' + total } }
  return { o: 'terminate' }
}
`
	res := contEmit(t, src, &contOptions)
	if res.JS == "" {
		t.Fatalf("emitted no JavaScript; diagnostics:\n%s", FormatDiagnostics(res.Diagnostics))
	}
	scenario := runEngine(t, res.JS, []engineRun{{}, {}}, contOptions.FramePath, map[string]any{
		contOptions.FramePath: map[string]any{"data": map[string]any{}},
	})
	runs := scenario["runs"].([]any)
	if o := runs[1].(map[string]any)["outcome"].(map[string]any)["o"]; o != "terminate" {
		t.Fatalf("run two outcome = %v, want terminated", o)
	}
	store := scenario["store"].(map[string]any)
	data := store[contOptions.FramePath].(map[string]any)["data"].(map[string]any)
	// run two resumes with total=1 restored from the frame; the continuation's
	// const is the write target. (The increment is pre-park: once per
	// lifecycle - see the ruling.)
	if got, ok := data["final"].(string); !ok || got != "configmaps/cont-live:1" {
		t.Fatalf("final = %v, want configmaps/cont-live:1 (const in the continuation loaded and ran)", data["final"])
	}
}

// The carry spelling: the frame rides the program's own ungated self-write -
// { op: 'carry', args: <frame JSON> } - with the read digging status.carry.
// WriteOp/WriteArgs are the caller's vocabulary; the compiler does not know
// what carry means, only how to spell it.
func TestContinuationCarrySpelledFrame(t *testing.T) {
	const src = contPrelude + `
export const step = function* (): Generator<Effect, Outcome, any> {
  let total = 0
  total = total + 1
  return { o: 'quiesce', resume: { after: 1 } }
  total = total + 1
  yield { op: 'ensure', args: { path: '/m', field: 'data.final', value: total } }
  return { o: 'terminate' }
}
`
	carry := ContinuationOptions{
		FramePath:  contOptions.FramePath,
		FrameField: "status.carry",
		WriteOp:    "carry",
		WriteArgs:  "string",
	}
	res := contEmit(t, src, &carry)
	for _, want := range []string{`op: "carry"`, `doc.status && doc.status.carry`, `JSON.parse(doc.status.carry)`} {
		if !strings.Contains(res.JS, want) {
			t.Errorf("emitted step is missing %s:\n%s", want, res.JS)
		}
	}
	if strings.Contains(res.JS, `op: "ensure", args: { path: "`) {
		t.Errorf("the frame write must not use the field spelling under the carry option:\n%s", res.JS)
	}
	scenario := runEngine(t, res.JS, []engineRun{{}, {}}, carry.FramePath, map[string]any{
		carry.FramePath: map[string]any{"status": map[string]any{"carry": ""}},
	}, driverApply{op: "carry", field: "status.carry"})
	runs := scenario["runs"].([]any)
	if o := runs[1].(map[string]any)["outcome"].(map[string]any)["o"]; o != "terminate" {
		t.Fatalf("run two outcome = %v, want terminated", o)
	}
	store := scenario["store"].(map[string]any)
	body := store[carry.FramePath].(map[string]any)
	status := body["status"].(map[string]any)
	frame, ok := status["carry"].(string)
	if !ok || !strings.Contains(frame, `"pc":"done"`) {
		t.Fatalf("the frame did not ride the carry: %v", status["carry"])
	}
	if !strings.Contains(frame, `"total":2`) {
		t.Fatalf("the carried frame lost the increment: %s", frame)
	}
	data := body["data"].(map[string]any)
	if got, ok := data["final"].(float64); !ok || got != 2 {
		t.Fatalf("final = %v, want 2", got)
	}
}
