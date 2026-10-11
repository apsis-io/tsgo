// Mini effect engine for the continuation fixtures: drives one step
// generator per run against an in-memory object store, mirroring the
// consumer's semantics - `get` answers {t, v} with v the raw JSON string of
// the object body, `ensure` writes the raw value at the dotted field, and
// each run is a FRESH realm (a fresh module instantiation), so the only
// thing between runs is the store.
//
// Usage: bun driver.mjs <spec.json>
//   spec = { path, export?, store, applyOp?, applyField?, runs: [{ module }] }
//   applyOp/applyField: an extra write op the driver applies at the dotted
//   field applyField of the run's object (e.g. op 'carry' at 'status.carry'),
//   mirroring a host-side spelling the generic get/ensure pair does not cover.
// Emits one JSON document: { runs: [{ outcome, effects }], store }.

import { readFileSync } from 'node:fs'
import { pathToFileURL } from 'node:url'

const specFile = process.argv[2]
if (!specFile) {
  console.error('usage: bun driver.mjs <spec.json>')
  process.exit(2)
}
const spec = JSON.parse(readFileSync(specFile, 'utf8'))
const store = structuredClone(spec.store)
const runs = []

for (const r of spec.runs) {
  const mod = await import(pathToFileURL(r.module))
  const step = mod[spec.export ?? 'step']
  if (typeof step !== 'function') {
    throw new Error(`no export ${spec.export ?? 'step'} in ${r.module}`)
  }
  const gen = step()
  const effects = []
  let answer
  let outcome = null
  for (;;) {
    const res = gen.next(answer)
    if (res.done) {
      outcome = res.value === undefined ? null : res.value
      break
    }
    const eff = res.value
    effects.push(eff)
    if (eff.op === 'get') {
      const body = store[r.path]
      answer = body === undefined ? { t: 'absent' } : { t: 'known', v: JSON.stringify(body) }
    } else if (eff.op === 'ensure') {
      const parts = String(eff.args.field).split('.')
      const last = parts.pop()
      let node = store[r.path]
      if (node === undefined) {
        node = {}
        store[r.path] = node
      }
      for (const p of parts) {
        if (typeof node[p] !== 'object' || node[p] === null) node[p] = {}
        node = node[p]
      }
      node[last] = eff.args.value
      answer = undefined
    } else if (spec.applyOp !== undefined && eff.op === spec.applyOp) {
      const parts = String(spec.applyField).split('.')
      const last = parts.pop()
      let node = store[r.path]
      if (node === undefined) {
        node = {}
        store[r.path] = node
      }
      for (const p of parts) {
        if (typeof node[p] !== 'object' || node[p] === null) node[p] = {}
        node = node[p]
      }
      node[last] = eff.args
      answer = undefined
    } else {
      answer = undefined
    }
  }
  runs.push({ outcome, effects })
}

console.log(JSON.stringify({ runs, store }))
