## Custom-codec authoring guide

A field tagged `bin:"N,custom=Name"` opts out of the schema-driven emit
path and calls a user-supplied codec. This guide is the curated entry
point for authoring such codecs end-to-end: lifecycle, worked example,
correctness rules, performance trade-offs, and diagnostics. The Go-side
contract and the wire-format rules are linked under
[Reference](#reference).

## Getting started

- [Lifecycle](lifecycle.md) — declaration, registration, codegen, and
  how a `CodecDecl` becomes call-site Go.
- [Worked example: Money / Invoice](example-money.md) — a complete
  custom codec from struct to round-trip test, with all three codec
  shapes contrasted on the same domain type.

## Correctness

- [Compatibility rules](compatibility.md) — what you can change about
  a codec without breaking on-wire compatibility, and what requires
  `--allow-breaking`.
- [Testing patterns](testing.md) — round-trip, golden, and property
  tests; reusing the
  [`tools/gsbmcodegen/fixtures/customcodec/`](../../tools/gsbmcodegen/fixtures/customcodec/)
  fixture harness.
- [Streaming determinism](streaming-determinism.md) — why size-pass
  and write-pass output must be byte-identical, common map-ordering
  pitfalls, and how to assert determinism in tests.

## Performance

- [Performance and allocation traps](performance.md) — picking
  between analytic, materializing-cached, and streaming; standalone
  `SizeGSBM` cost; allocation budgets and the peak-heap benchmark.

## Reference

- [Diagnostics catalog](diagnostics.md) — every `codec/*` codegen
  diagnostic, with cause and remedy.
- [Package-level codec README](../../tools/gsbmcodegen/codecs/README.md)
  — the Go-side contract and Writer helper surface.
- [Wire-format spec §5.8](../spec.md) — the on-wire rules a codec
  must satisfy.
