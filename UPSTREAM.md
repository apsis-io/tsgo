# Upstream and provenance

This repository is a TRIMMED distribution of the TypeScript compiler in Go
(typescript-go), carrying one added package, and it exists for one reason:
upstream's packages all live under `internal/`, invisible to every other
module, so no one could import the compiler as a library.

- **Upstream pin**: `microsoft/TypeScript` @ `a1ef42b9`
  ("Fix flaky diagnostic on JS constructor-defined properties (#64646)").
- **The bridge**: `tsapi/` — the ONE importable surface (in-memory project
  against the bundled libs, diagnostics, JS-only emit captured in memory,
  checker primitives for reading types off the AST). Generic by design; no
  consumer semantics; every exported signature uses tsapi's own opaque
  types. Upstream + tsapi were developed on the `apsis/main` branch of the
  full clone (shim commit `4c46b2ab`) and trimmed from there.
- **The trim**: the 49 `internal/*` packages reachable from `tsapi` (the
  parser, checker, compiler, printer, transformers, tsoptions/vfs/bundled
  machinery) plus their embedded assets (bundled libs, diagnostics
  localization). Their `_test.go` files are stripped here — upstream's
  conformance suite (test262 et al.) lives in the full clone and stays the
  authority for upstream behavior; `tsapi`'s own tests ship.
- **The path**: upstream's module path was
  `github.com/microsoft/TypeScript/tsc`; everything here imports
  `github.com/apsis-io/tsgo`. That rename is the price of a small module a
  consumer can `require` plainly, with no `replace`. The cost is mechanical:
  carrying an upstream change means re-applying it through the rename
  (targeted cherry-picks, not wholesale merges).
- **License**: upstream is Apache-2.0; this distribution carries the same
  license (LICENSE, from the upstream tree). The tsapi additions are
  (c) Malformed C, Apache-2.0.

To re-pin: take the new upstream sha into the full clone, rebase/refresh
`apsis/main`, re-run the trim (the reachable set is `go list -deps
./tsapi`), re-apply this file's steps, tag `apsis/vX.Y.Z`.

## Known upstream issues found through this bridge (2026-10-06)

- **Segfault** on `export const f = (): number => 'not a number'` - a
  top-level arrow with a wrong return type crashes the compiler
  (SIGSEGV in the checker) rather than producing TS2322. Found by the
  kinetics typecheck-gate tests; a malformed step must produce a
  diagnostic, never a crash, so this is worth upstreaming.
