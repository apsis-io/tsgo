package tsapi

import (
	"context"
	"strings"
	"testing"
)

// A fake SDK with the shape that matters: effect types carrying a type-only
// string-literal `wit` marker, and a defineStep whose parameter and result
// name the same union.
const testSDKSource = `export type GetEff = { readonly wit?: 'radiant:reconcile/observe@0.1.0'; op: 'get' }
export type EnsureEff = { readonly wit?: 'radiant:reconcile/ensure@0.1.0'; op: 'ensure' }
export type StatusEff = { readonly wit?: 'radiant:reconcile/status@0.1.0'; op: 'status' }
export declare function defineStep(g: () => Generator<GetEff | EnsureEff | StatusEff, string, unknown>): () => Generator<GetEff | EnsureEff | StatusEff, string, unknown>
`

// The const + defineStep shape - the one export spelling that a name-only
// lookup misses, and the one every real step uses.
const testStepSource = `import { defineStep } from '@perseid/sdk'

export const step = defineStep(function* () {
  yield { op: 'get' as const }
  yield { op: 'ensure' as const }
  yield { op: 'status' as const }
  return 'quiesced'
})
`

const testEntry = "/src/step.ts"

func testProject(t *testing.T, stepSrc string) *Project {
	t.Helper()

	return testProjectSDK(t, stepSrc, testSDKSource)
}

func testProjectSDK(t *testing.T, stepSrc, sdkSrc string) *Project {
	t.Helper()
	p, err := NewProject(map[string]string{
		testEntry:                             stepSrc,
		"/node_modules/@perseid/sdk/index.ts": sdkSrc,
	}, []string{testEntry}, Options{Strict: true, SkipLibCheck: true})
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}

	return p
}

func TestProjectTypechecks(t *testing.T) {
	p := testProject(t, testStepSource)
	ctx := context.Background()
	if ds := p.Diagnostics(ctx); len(ds) != 0 {
		t.Fatalf("expected a clean project, got:\n%s", FormatDiagnostics(ds))
	}
}

func TestDeriveProjection(t *testing.T) {
	p := testProject(t, testStepSource)
	ctx := context.Background()

	sf := p.SourceFile(testEntry)
	if sf == nil {
		t.Fatalf("entry %q is not part of the program", testEntry)
	}
	node, ok := sf.ExportByName("step")
	if !ok {
		t.Fatal("no export named 'step' (function declaration or const shape)")
	}

	c := p.Checker(ctx)
	defer c.Close()

	stepType := c.TypeOf(node)
	sigs := c.CallSignatures(stepType)
	if len(sigs) == 0 {
		t.Fatalf("'step' is not callable: %s", c.TypeString(stepType))
	}
	ret := c.ReturnTypeOf(sigs[0])
	args := c.TypeArguments(ret)
	if len(args) == 0 {
		t.Fatalf("return type is not a generic reference: %s", c.TypeString(ret))
	}
	yielded := args[0]

	members := yielded.UnionTypes()
	if got := len(members); got != 3 {
		t.Fatalf("yield type = %s, want a union of 3 effects", c.TypeString(yielded))
	}

	var wits []string
	for _, m := range members {
		sym, ok := c.Property(m, "wit")
		if !ok {
			t.Fatalf("effect %s carries no 'wit' marker - the derive would fail closed here", c.TypeString(m))
		}
		mt := c.TypeOfSymbolAt(sym, node)
		lits := mt.StringLiterals()
		if len(lits) != 1 {
			t.Fatalf("'wit' of %s is not one string literal: %s", c.TypeString(m), c.TypeString(mt))
		}
		wits = append(wits, lits[0])
	}

	want := []string{
		"radiant:reconcile/ensure@0.1.0",
		"radiant:reconcile/observe@0.1.0",
		"radiant:reconcile/status@0.1.0",
	}
	for i, w := range want {
		if wits[i] != w {
			t.Errorf("member %d: got %q, want %q", i, wits[i], w)
		}
	}
}

func TestUnmarkedEffectIsVisible(t *testing.T) {
	// The fail-closed arm's premise: an effect without the marker is
	// DISTINGUISHABLE from a marked one. Property(m, "wit") must say so.
	const src = `import { defineStep } from '@perseid/sdk'
export const step = defineStep(function* () {
  yield { op: 'unmarked' as const }
  return ''
})
`
	const sdk = `export type GetEff = { readonly wit?: 'radiant:reconcile/observe@0.1.0'; op: 'get' }
export type UnmarkedEff = { op: 'unmarked' }
export declare function defineStep(g: () => Generator<GetEff | UnmarkedEff, string, unknown>): () => Generator<GetEff | UnmarkedEff, string, unknown>
`
	p := testProjectSDK(t, src, sdk)
	ctx := context.Background()

	node, ok := p.SourceFile(testEntry).ExportByName("step")
	if !ok {
		t.Fatal("no export named 'step'")
	}
	c := p.Checker(ctx)
	defer c.Close()

	sig := c.CallSignatures(c.TypeOf(node))[0]
	yielded := c.TypeArguments(c.ReturnTypeOf(sig))[0]
	for _, m := range yielded.UnionTypes() {
		if _, ok := c.Property(m, "wit"); !ok {
			return // the unmarked member is visible - this is the pass
		}
	}
	t.Fatal("every member carried a wit marker; the unmarked one was not distinguishable")
}

func TestDiagnosticsCatchTypeError(t *testing.T) {
	const src = `import { defineStep } from '@perseid/sdk'
export const step = defineStep(function* () {
  yield { op: 'get' as const }
  const n: number = 'not a number'
  return ''
})
`
	p := testProject(t, src)
	ds := p.Diagnostics(context.Background())
	found := false
	for _, d := range ds {
		if d.Line == 4 && strings.Contains(d.Message, "string") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a type error on line 4, got:\n%s", FormatDiagnostics(ds))
	}
}

func TestEmitJS(t *testing.T) {
	p := testProject(t, testStepSource)
	ctx := context.Background()
	res, err := p.EmitJS(ctx, testEntry)
	if err != nil {
		t.Fatalf("EmitJS: %v", err)
	}
	if len(res.Diagnostics) != 0 {
		t.Fatalf("emit diagnostics:\n%s", FormatDiagnostics(res.Diagnostics))
	}
	if res.JS == "" {
		t.Fatal("emitted no JavaScript")
	}
	if !strings.Contains(res.JS, "function*") {
		t.Errorf("emitted JS lost the generator:\n%s", res.JS)
	}
	// The import SPECIFIER must survive emit - the runtime module loader
	// resolves it to the embedded SDK bundle. An emit that rewrites or
	// inlines it breaks the loader contract.
	if !strings.Contains(res.JS, "@perseid/sdk") {
		t.Errorf("emitted JS does not carry the '@perseid/sdk' specifier:\n%s", res.JS)
	}
}
