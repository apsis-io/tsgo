// Copyright (c) Microsoft Corporation.
// Modifications copyright (C) 2026 Malformed C. Licensed Apache-2.0.
//
// Package tsapi exposes a small, importable slice of the TypeScript compiler.
//
// Everything else in this module lives under internal/ and cannot be imported
// from outside it. This package is the one bridge: an in-memory project
// (overlaid sources served against the bundled libs), diagnostics, JavaScript
// emit, and the checker primitives needed to read types off the AST.
//
// Deliberately generic: no project-specific semantics live here, so the
// package tracks upstream with a minimal conflict surface. Every exported
// signature references only tsapi's own opaque types - an upstream ast or
// checker type must never appear in one, because every consumer of this
// package lives in another module and could not name it.
package tsapi

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/apsis-io/tsgo/internal/ast"
	"github.com/apsis-io/tsgo/internal/bundled"
	"github.com/apsis-io/tsgo/internal/checker"
	"github.com/apsis-io/tsgo/internal/collections"
	"github.com/apsis-io/tsgo/internal/compiler"
	"github.com/apsis-io/tsgo/internal/core"
	"github.com/apsis-io/tsgo/internal/locale"
	"github.com/apsis-io/tsgo/internal/scanner"
	"github.com/apsis-io/tsgo/internal/tsoptions"
	"github.com/apsis-io/tsgo/internal/tspath"
	"github.com/apsis-io/tsgo/internal/vfs/vfstest"
)

// Project is a compiled program over in-memory sources. Create with
// NewProject; every method on it is safe to call repeatedly.
type Project struct {
	prog *compiler.Program
}

// Options configures NewProject.
type Options struct {
	// Strict turns on the strict family. A step language wants this on.
	Strict bool
	// SkipLibCheck skips checking the bundled .d.ts files. On unless you are
	// debugging the libs themselves.
	SkipLibCheck bool
	// Libs names bundled default-lib files to include, e.g.
	// "lib.es2022.d.ts". Empty means the compiler's default set for Target.
	Libs []string
	// Paths maps import specifiers into the overlay, Bundler-style, e.g.
	// "@perseid/sdk/*" -> "/node_modules/@perseid/sdk/*". Layout the overlay
	// as a real node_modules tree instead when you can; Paths is for the
	// cases where you cannot.
	Paths map[string][]string
	// BaseDir is the directory Paths targets resolve against. Defaults to "/".
	BaseDir string
	// Target selects the emit target. Zero means ES2022.
	Target ScriptTarget
}

// ScriptTarget mirrors the compiler's target enum, so callers do not name
// upstream identifiers.
type ScriptTarget int

const (
	TargetES2020 ScriptTarget = ScriptTarget(core.ScriptTargetES2020)
	TargetES2022 ScriptTarget = ScriptTarget(core.ScriptTargetES2022)
	TargetESNext ScriptTarget = ScriptTarget(core.ScriptTargetESNext)
)

// Diagnostic is one compiler diagnostic, positioned.
type Diagnostic struct {
	File    string
	Line    int // 1-based
	Char    int // 1-based
	Code    int
	Message string
}

func (d Diagnostic) Error() string {
	return fmt.Sprintf("%s:%d:%d: TS%d: %s", d.File, d.Line, d.Char, d.Code, d.Message)
}

// FormatDiagnostics renders diagnostics one per line, sorted by position.
func FormatDiagnostics(ds []Diagnostic) string {
	sorted := make([]Diagnostic, len(ds))
	copy(sorted, ds)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].File != sorted[j].File {
			return sorted[i].File < sorted[j].File
		}
		if sorted[i].Line != sorted[j].Line {
			return sorted[i].Line < sorted[j].Line
		}

		return sorted[i].Char < sorted[j].Char
	})
	lines := make([]string, 0, len(sorted))
	for _, d := range sorted {
		lines = append(lines, d.Error())
	}

	return strings.Join(lines, "\n")
}

// NewProject compiles entries against an in-memory overlay of files.
//
// files maps ABSOLUTE NORMALIZED paths ("/src/step.ts") to contents. Entries
// must be keys of files. The bundled default libs are always available; an
// import of a package resolves against the overlay laid out as a
// node_modules tree, which is the layout module resolution expects.
func NewProject(files map[string]string, entries []string, opts Options) (*Project, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("tsapi: no entry files")
	}
	for _, e := range entries {
		if _, ok := files[e]; !ok {
			return nil, fmt.Errorf("tsapi: entry %q is not in the overlay", e)
		}
	}

	fs := vfstest.FromMap(files, tspath.CaseSensitive)
	fs = bundled.WrapFS(fs)

	copts := core.CompilerOptions{
		Target:           core.ScriptTarget(opts.Target),
		Module:           core.ModuleKindESNext,
		ModuleResolution: core.ModuleResolutionKindBundler,
		Strict:           tri(opts.Strict),
		SkipLibCheck:     tri(opts.SkipLibCheck),
	}
	if len(opts.Libs) > 0 {
		copts.Lib = opts.Libs
	}
	if len(opts.Paths) > 0 {
		base := opts.BaseDir
		if base == "" {
			base = "/"
		}
		copts.BaseUrl = tspath.RootedDirectoryPathFromNormalized(base)
		keys := make([]string, 0, len(opts.Paths))
		for k := range opts.Paths {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		pm := collections.NewOrderedMapWithSizeHint[string, []string](len(keys))
		for _, k := range keys {
			pm.Set(k, opts.Paths[k])
		}
		copts.Paths = pm
	}

	host := compiler.NewCompilerHost(fs, bundled.LibPath(), nil, nil, nil)

	names := make([]tspath.RootedFilePath, 0, len(entries))
	for _, e := range entries {
		names = append(names, tspath.RootedFilePathFromNormalized(e))
	}
	config := tsoptions.NewParsedCommandLine(&copts, names, nil,
		tspath.RootedDirectoryPathFromNormalized(tspath.GetDirectoryPath(entries[0])), fs.CaseSensitivity())

	return &Project{prog: compiler.NewProgram(compiler.ProgramOptions{
		Config: config,
		Host:   host,
	})}, nil
}

func tri(b bool) core.Tristate {
	if b {
		return core.TSTrue
	}

	return core.TSFalse
}

// Diagnostics collects config, global, syntactic and semantic diagnostics
// across the whole program. Empty means the project typechecks.
func (p *Project) Diagnostics(ctx context.Context) []Diagnostic {
	var out []Diagnostic
	out = append(out, format(p.prog.GetConfigFileParsingDiagnostics())...)
	out = append(out, format(p.prog.GetGlobalDiagnostics(ctx))...)
	for _, sf := range p.prog.SourceFiles() {
		out = append(out, format(p.prog.GetSyntacticDiagnostics(ctx, sf))...)
		out = append(out, format(p.prog.GetSemanticDiagnostics(ctx, sf))...)
	}

	return out
}

// HasDiagnostics reports whether Diagnostics would report anything, so a
// caller can gate without formatting the whole list twice.
func (p *Project) HasDiagnostics(ctx context.Context) bool {
	return len(p.Diagnostics(ctx)) > 0
}

func format(ds []*ast.Diagnostic) []Diagnostic {
	out := make([]Diagnostic, 0, len(ds))
	for _, d := range ds {
		out = append(out, Diagnostic{
			Code:    int(d.Code()),
			Message: d.Localize(locale.Default),
		})
		if f := d.File(); f != nil {
			out[len(out)-1].File = string(f.FileName())
			line, char := lineChar(f, d.Pos())
			out[len(out)-1].Line = line
			out[len(out)-1].Char = char
		}
	}

	return out
}

func lineChar(f *ast.SourceFile, pos int) (int, int) {
	line, char := scanner.GetECMALineAndUTF16CharacterOfPosition(f, pos)

	return line + 1, int(char) + 1
}

// EmitResult is what EmitJS produced for one entry.
type EmitResult struct {
	// JS is the emitted JavaScript for the entry, "" when nothing was
	// emitted (suppressed by noEmit, or the file produces no output).
	JS string
	// Diagnostics are emit diagnostics (declaration-emit flavors, which a
	// JS-only emit mostly does not produce).
	Diagnostics []Diagnostic
}

// EmitJS emits ONLY the JavaScript for one entry file, captured from the
// emitter's write callback - nothing touches the filesystem.
func (p *Project) EmitJS(ctx context.Context, entry string) (EmitResult, error) {
	sf := p.prog.GetSourceFile(tspath.RootedFilePathFromNormalized(entry))
	if sf == nil {
		return EmitResult{}, fmt.Errorf("tsapi: %q is not part of the program", entry)
	}
	var js string
	res := p.prog.Emit(ctx, compiler.EmitOptions{
		TargetSourceFiles: []*ast.SourceFile{sf},
		EmitOnly:          compiler.EmitOnlyJs,
		WriteFile: func(fileName tspath.RootedFilePath, text string, data *compiler.WriteFileData) error {
			// One call per output artifact; the source map and the d.ts carry
			// different suffixes, and data.SourceFile names which ENTRY the
			// output belongs to - never match on the entry name alone.
			if data != nil && data.SourceFile == sf && strings.HasSuffix(string(fileName), ".js") {
				js = text
			}

			return nil
		},
	})

	return EmitResult{
		JS:          js,
		Diagnostics: format(res.Diagnostics),
	}, nil
}

// ─── checker navigation ─────────────────────────────────────────────────────
//
// The derive use: given an exported symbol, read types off it. Every handle
// type below is opaque and tsapi-owned.

// Checker borrows the program's type checker. Call Close when done - the
// program pools checkers, and the release is part of that contract.
type Checker struct {
	c       *checker.Checker
	release func()
}

func (p *Project) Checker(ctx context.Context) *Checker {
	c, release := p.prog.GetTypeChecker(ctx)

	return &Checker{c: c, release: release}
}

// Close releases the borrowed checker. Safe to call more than once.
func (c *Checker) Close() {
	if c.release != nil {
		c.release()
		c.release = nil
	}
}

// Node is an AST node handle.
type Node struct {
	n *ast.Node
}

// Type is a type handle.
type Type struct {
	t *checker.Type
}

// Symbol is a symbol handle.
type Symbol struct {
	s *ast.Symbol
}

// Signature is a call signature handle.
type Signature struct {
	s *checker.Signature
}

// SourceFile is a parsed source file handle.
type SourceFile struct {
	f *ast.SourceFile
}

// SourceFile returns the parsed file for an absolute normalized path, or nil
// when the file is not part of the program.
func (p *Project) SourceFile(path string) *SourceFile {
	sf := p.prog.GetSourceFile(tspath.RootedFilePathFromNormalized(path))
	if sf == nil {
		return nil
	}

	return &SourceFile{f: sf}
}

// ExportByName finds a top-level export by name: a function declaration, or a
// variable statement binding an identifier. Both spellings are one fact -
// `export function step` and `export const step = defineStep(...)` are the
// same export to a consumer.
func (sf *SourceFile) ExportByName(name string) (*Node, bool) {
	var found *ast.Node
	// Visitor returns true to STOP, so the walk ends the moment we hold one.
	sf.f.ForEachChild(func(n *ast.Node) bool {
		if found != nil {
			return true
		}
		switch {
		case ast.IsFunctionDeclaration(n):
			if d := n.AsFunctionDeclaration(); d.Name() != nil && ast.IsIdentifier(d.Name()) && d.Name().Text() == name {
				found = n
			}
		case ast.IsVariableStatement(n):
			for _, d := range n.AsVariableStatement().DeclarationList.AsVariableDeclarationList().Declarations.Nodes {
				if ast.IsVariableDeclaration(d) {
					if nameNode := d.AsVariableDeclaration().Name(); nameNode != nil && ast.IsIdentifier(nameNode) && nameNode.Text() == name {
						found = d
					}
				}
			}
		}

		return found != nil
	})
	if found == nil {
		return nil, false
	}

	return &Node{n: found}, true
}

// TypeOf returns the checker's type for a node.
func (c *Checker) TypeOf(n *Node) *Type {
	return &Type{t: c.c.GetTypeAtLocation(n.n)}
}

// CallSignatures returns the call signatures of a type.
func (c *Checker) CallSignatures(t *Type) []*Signature {
	sigs := c.c.GetCallSignatures(t.t)
	out := make([]*Signature, 0, len(sigs))
	for _, s := range sigs {
		out = append(out, &Signature{s: s})
	}

	return out
}

// ReturnTypeOf returns a signature's return type.
func (c *Checker) ReturnTypeOf(s *Signature) *Type {
	return &Type{t: c.c.GetReturnTypeOfSignature(s.s)}
}

// TypeArguments returns the type arguments of a generic type reference.
func (c *Checker) TypeArguments(t *Type) []*Type {
	args := c.c.GetTypeArguments(t.t)
	out := make([]*Type, 0, len(args))
	for _, a := range args {
		out = append(out, &Type{t: a})
	}

	return out
}

// IsUnion reports whether the type is a union.
func (t *Type) IsUnion() bool {
	return t.t.IsUnion()
}

// UnionTypes returns the members of a union. For a non-union it returns the
// type itself, so callers need no branch: a union of one is the one.
func (t *Type) UnionTypes() []*Type {
	if !t.t.IsUnion() {
		return []*Type{t}
	}
	members := t.t.Types()
	out := make([]*Type, 0, len(members))
	for _, m := range members {
		out = append(out, &Type{t: m})
	}

	return out
}

// Property returns a property symbol by name.
func (c *Checker) Property(t *Type, name string) (*Symbol, bool) {
	s := c.c.GetPropertyOfType(t.t, name)
	if s == nil {
		return nil, false
	}

	return &Symbol{s: s}, true
}

// TypeOfSymbolAt returns a property's type as seen at a location, which is
// what an optional property needs: without the location the checker may
// answer from the declaration site rather than the use.
func (c *Checker) TypeOfSymbolAt(s *Symbol, loc *Node) *Type {
	return &Type{t: c.c.GetTypeOfSymbolAtLocation(s.s, loc.n)}
}

// TypeString renders a type for error messages.
func (c *Checker) TypeString(t *Type) string {
	return c.c.TypeToString(t.t)
}

// StringLiteral returns the literal value of a string literal type.
func (t *Type) StringLiteral() (string, bool) {
	if !t.t.IsStringLiteral() {
		return "", false
	}
	v, ok := t.t.AsLiteralType().Value().(string)

	return v, ok
}

// StringLiterals returns the string-literal members of a type, descending
// through unions. An optional property typed `lit` reads back as
// `lit | undefined`; callers after the literals want this, not
// StringLiteral, which answers only for a bare literal type.
func (t *Type) StringLiterals() []string {
	var out []string
	for _, m := range t.UnionTypes() {
		if v, ok := m.StringLiteral(); ok {
			out = append(out, v)
		}
	}

	return out
}
