# wire-format & schema-evolution test suite

## Overview

Add three sibling fixture packages under `tools/gsbmcodegen/fixtures/` to close the test-coverage gaps the recent wire-format spec polish exposed. Today the `sample` fixture is a single shape that grew organically; it pins per-feature behavior well but does not cover several composite-encoding interactions, the duplicate-handling rules added in spec §3.3, the length-bounded-region rejection rule, the tag boundary at `2^29-1`, the evolution-pair classifier round-trip, or the anonymous-field rejection path.

The new fixtures are scoped by intent:

- `fixtures/graph/` — one rich struct shape exercised end-to-end. Stresses the composite encoding interactions: slices of nullable, maps of nullable values, deep named composition, tag at the upper varint boundary. Round-trip + a small set of malformed-blob negative tests (duplicate tags, duplicate map keys, length-bounded-region overflow).
- `fixtures/evolution/` — paired before/after struct definitions feeding the schema classifier. Each pair asserts the classifier label (safe / warning / breaking) and exercises the codegen path on both sides where the change is implementable. Covers the four lifecycle transitions the user called out: add (safe), replace via `compat_write` (safe with `--allow-stop-compat-write` ack), wire-type/type/tag change (breaking), and field removal (breaking — the project's append-only policy means removal needs explicit `--allow-breaking`).
- `fixtures/rejection/` — small fixture that uses an anonymous embedded Go field and asserts the schema validator surfaces the `field/anonymous` issue from `tools/gsbmschema/discover.go:278`. Proves the rule the validator already enforces is wired all the way through `Analyze` + `FormatIssues`.

## Context (from discovery)

- Existing fixture lives at `tools/gsbmcodegen/fixtures/sample/`. Types: `Order`, `Customer`, `Item`, `Total`, `Label`, `Quantity`, `DeepNested`, `Branch`, `Leaf`, `Renamed`. Per-test files: `sample_test.go`, `arena_test.go`, `reset_test.go`, `renamed_test.go`. Coverage already includes round-trip, presence bitmap, arena cross-mode, sync.Pool reuse, tag-1/15/16 boundaries, varint overflow, missing-tag zero-fill, unknown LENGTH_DELIM tag skip, header round-trip.
- Schema classifier lives at `tools/gsbmschema/classifier.go` with table-driven tests in `classifier_test.go` covering `field/added`, `field/deprecated`, `field/resurrected`, `field/compat-write-added/removed`, `field/removed-deprecated`, `field/removed-compat-write`, `field/custom-added/removed`, `reserved/added`, `reserved/dropped`. Untested-against-real-source: `field/wire-changed`, `field/tag-changed`, `field/type-changed`, `field/optional-changed`, `field/renamed`, classifier-on-actual-fixture-pairs.
- Codegen lives at `tools/gsbmcodegen/`. Golden files committed alongside fixtures, regenerated via `REGEN_GOLDEN=1 go test ./tools/gsbmcodegen/ -run TestRegenGolden`. Codegen path used for graph + evolution fixtures; the rejection fixture intentionally fails schema validation so codegen never runs on it.
- Anonymous-field rejection is enforced at `tools/gsbmschema/discover.go:278-284` (raises `Issue{Code: "field/anonymous"}` and continues). No test currently exercises this path with real source.
- Spec §3.3 (duplicate tags / duplicate map keys → last-wins) was added in the recent polish pass; no test exercises it.
- Spec §3 length-bounded-region rule (decoder MUST reject LENGTH_DELIM whose declared length exceeds enclosing region) is enforced in `storage/gsbm/reader.go` (210-285) but no malformed-input integration test runs through generated decoder code.

## Development Approach

- **Testing approach**: Regular (write fixtures + helpers first, then add tests in the same Task; matches the existing repo pattern in `sample_test.go`)
- Complete each Task fully before moving to the next; later Tasks consume earlier ones (classifier tests in Task 4 consume the evolution fixtures from Task 3; round-trip tests in Task 2 consume the graph fixture from Task 1)
- Keep each new fixture package self-contained: own `types.go`, own `*_test.go`, own goldens. No cross-package imports between fixture packages.
- Prefer narrow per-test fixtures over bolting onto `Order`. Each new shape exists to pin one interaction; adding to `Order` would re-couple unrelated tests.
- Negative tests use hand-crafted blobs with `gsbm.NewWriter` primitives, not generated `MarshalGSBM` (the generated encoder will not produce duplicate tags or oversized lengths)
- Update this plan when scope changes during implementation

### CRITICAL: do not fix code or tests to make tests green

When a test fails, **investigate the root cause**. Never modify production code or rewrite the test assertion just to flip red to green.

- A failing test means **either** the production code is wrong, **or** the test's expectation is wrong (often a copy-paste typo, a stale fixture, or a misunderstanding of the spec). Diagnose which **before** touching either side.
- If the production code is wrong: fix the production code; the test stays as written.
- If the test expectation is wrong: explain why in the commit message, cite the spec section or the source-of-truth that proves the new expectation is correct, then update the test.
- **Forbidden patterns** (do not do these):
  - Loosening an assertion (`==` → `!=`, exact match → substring match) without a documented reason.
  - Replacing `errors.Is(err, gsbm.ErrXxx)` with `err != nil` to dodge a sentinel mismatch.
  - Catching and ignoring an error that the code under test is supposed to surface.
  - Wrapping the failing assertion in `t.Skip` or `if testing.Short()`.
  - Regenerating goldens (`REGEN_GOLDEN=1`) when goldens disagree with hand-written tests — first decide whether the hand-written test or the codegen is correct.
  - Editing the wire format, the spec, or `gsbm.Err*` sentinels to make a stuck test pass.
- If you cannot determine the root cause within reasonable effort, **stop and surface the failure** in a `⚠️` plan note rather than papering over it. A failing test left visible is more valuable than a passing test that proves nothing.
- This rule applies to every `run project tests - must pass before next task` checkbox in the Implementation Steps below.

## Testing Strategy

- **Unit tests**: required for every Task. Round-trip tests use generated `MarshalGSBM`/`UnmarshalGSBM`; negative tests use hand-crafted blobs with `gsbm.Writer` primitives.
- **Golden-file tests**: every graph and evolution fixture produces committed `*_gsbm.go` and `*_gsbm_arena.go` files; existing `TestGoldenSample` pattern is replicated per package via `regen_golden_test.go` siblings.
- **Classifier tests**: pair-based — load before-package and after-package via `gsbmschema.ParseSource` or a fixture-loader, run `classifier.Compare`, assert change codes and labels by exact match.
- **Validator-rejection test**: load the rejection fixture, call `gsbmschema.Analyze`, assert the issue list contains exactly one `Issue{Code:"field/anonymous"}` for the named field and zero codegen output is produced.
- **Run full suite after each Task**: `go test ./... -count=1` must pass before the next Task.
- The codebase has no UI / e2e layer; this is library-only work.

## Progress Tracking

- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope

## Technical Details

### `fixtures/graph/` — types (pivoted; see Task 1 ⚠️)

```go
package graph

//gsbm:root
type Catalog struct {
    ID       string         `bin:"1"`
    Sections []Section      `bin:"2"`         // []<named-struct>
    Tags     map[string]Tag `bin:"3"`         // map<string, named-struct>
    Tail     EdgeMarker     `bin:"536870911"` // 2^29 - 1, the tag-boundary marker
}

type Section struct {
    Name  string `bin:"1"`
    Items []Item `bin:"2"`
}

type Item struct {
    SKU  string  `bin:"1"`
    Note *string `bin:"2"` // optional primitive inside a struct
}

type Tag struct {
    Slug   string `bin:"1"`
    Weight *int64 `bin:"2"`
}

type EdgeMarker struct {
    Marker bool `bin:"1"`
}
```

Why this shape: `Section → Item` is two-deep named composition, complementing the existing `DeepNested → Branch → Leaf` shape; `Tags map[string]Tag` exercises a `map<string, named-struct>` shape (sample only covers `map[string]Label` where `Label` is named-not-struct); `Tail EdgeMarker` at tag `536870911 = 2^29 - 1` pins the upper varint boundary for the field-key encoding (one more would overflow per spec §3.1). The original plan included slice-of-nullable / map-of-nullable / named-string-key shapes; those are unsupported by the schema validator today (see Task 1 ⚠️) and are tracked as a known coverage gap.

### `fixtures/graph/` — negative tests

Three hand-crafted blob tests, all targeting the same generated `Catalog.UnmarshalGSBM`:

1. **Duplicate tag, last-wins**: write tag 1 ("first"), then tag 1 ("second"); assert `Catalog.ID == "second"` per spec §3.3.
2. **Duplicate map key, last-wins**: write a `Tags` map payload where the same key appears twice with different `*Tag` values; assert the second value wins.
3. **Length-bounded-region overflow**: write a `Sections` field whose declared LENGTH_DELIM length exceeds the bytes that follow; assert `errors.Is(err, gsbm.ErrTruncated)` (or the appropriate sentinel for length-overrun).

### `fixtures/evolution/` — pair shape

Each scenario lives in two sub-packages: `before/` and `after/`. The classifier test loads both and asserts the diff. Where the change is implementable on both sides (i.e., not breaking), both packages compile and codegen runs; where the change is breaking, only one side compiles meaningfully and the test asserts the classifier output without running codegen.

| Scenario | before | after | Expected classifier |
|---|---|---|---|
| add field (safe) | tags 1, 2 | tags 1, 2, 3 | `field/added` label `safe` |
| compat_write replace (safe lifecycle) | tag 2 active | tag 2 deprecated+compat_write, tag 3 added | `field/compat-write-added`, `field/added`; both label `safe` |
| wire-type change (breaking) | tag 1 `string` | tag 1 `int64` | `field/wire-changed`, label `breaking`; require `--allow-breaking` ack |
| type change (breaking) | tag 1 `int32` | tag 1 `int64` | `field/type-changed`, label `breaking` |
| tag change (breaking) | field `Carrier` at tag 1 | field `Carrier` at tag 2 | `field/tag-changed`, label `breaking` |
| field removed (breaking — append-only policy) | tags 1, 2 | tag 1 only (tag 2 gone, NOT marked deprecated, NOT marked reserved) | `field/removed`, label `breaking`; require `--allow-breaking` |

The append-only design intent: removing a field outright violates the policy. The expected workflow is `active → deprecated[+compat_write] → reserved` — never direct removal. The "removed" scenario asserts the classifier flags this and that the override flag is required to land the change.

### `fixtures/rejection/` — types

```go
package rejection

type Inner struct {
    Value int64 `bin:"1"`
}

//gsbm:root
type WithEmbed struct {
    Inner          // anonymous (embedded) field — schema validator MUST reject
    ID    string `bin:"1"`
}
```

The test calls `gsbmschema.Analyze` on this package, asserts `len(issues) >= 1`, asserts at least one issue has `Code == "field/anonymous"` and `Message` contains `"Inner"`. No codegen runs (codegen depends on `Analyze` returning a clean schema).

### Classifier tests (Task 4)

The classifier currently exercises `parseSource` strings inline in `classifier_test.go`. For fixture-pair tests, prefer a small helper that loads each `before/`/`after/` package via `gsbmschema.ParseSource(packagePath)`, runs `classifier.Compare(beforeSchema, afterSchema)`, and returns the diff for assertion. Reuse — do not re-implement — `gsbmschema.ParseSource` and `classifier.Compare` from `tools/gsbmschema/`. The new helper goes in `fixtures/evolution/loader.go` (test-only file).

### What this plan does NOT do

- Does not change the wire format. All new tests cover the spec as it stands.
- Does not change generated-code shape. Goldens grow because new fixtures need new goldens; existing goldens are untouched.
- Does not implement the cycle-break-via-id codegen path (currently rejected by codegen with a TODO; out of scope).
- Does not re-cover gaps already covered by `sample_test.go` (presence bitmap, arena cross-mode, tag 1/15/16, varint overflow, missing-tag zero-fill, unknown-tag skip on LENGTH_DELIM, sync.Pool reuse).

## Implementation Steps

### Task 1: Add `fixtures/graph` package — types + golden codegen

⚠️ Scope deviation discovered during Task 1: the schema validator rejects `[]*Section`, `map[string]*Tag`, and `map[Code]*Tag` as `type/unsupported` (slice-of-pointer / map-value-pointer) and `map/bad-key` (named-string key) — see `tools/gsbmschema/discover.go:398-430` and `tools/gsbmschema/validate.go:137-143`. The codegen has no decode path for these shapes, and per the plan's "does NOT do" clause we are not extending the wire format or codegen. Pivoted the fixture to `[]Section`, `map[string]Tag`, and dropped the `Hot map[Code]*Tag` field and the `Code` named-string type. Slice-of-nullable / map-of-nullable / named-string-key coverage remains a real follow-up gap that requires schema + codegen extension; it is intentionally out of scope for this plan.

➕ Codegen bug found and fixed during Task 1: the optional-primitive zero-elide branch emitted `z := <untyped 0>` then `field = &z`, producing `*int` for any numeric pointer field. The sample fixture only covered `*string` / `*[]byte` so the bug was latent. Switched the emit at `tools/gsbmcodegen/emit.go:526-532` to `var z TYPE` for unambiguous typing. Sample goldens regenerated (string version is functionally identical: `z := ""` → `var z string`).

- [x] create `tools/gsbmcodegen/fixtures/graph/types.go` with the (pivoted) type set: `Catalog`, `Section`, `Item`, `Tag`, `EdgeMarker` — no `Code`, no `Hot` field; `Sections []Section`, `Tags map[string]Tag`
- [x] create `tools/gsbmcodegen/fixtures/graph/regen_golden_test.go` mirroring `tools/gsbmcodegen/regen_golden_test.go` so `REGEN_GOLDEN=1` regenerates this package's goldens too (also includes `TestGoldenGraph` + `TestGoldenGraphArena` drift checks)
- [x] generate goldens: `REGEN_GOLDEN=1 go test ./tools/gsbmcodegen/fixtures/graph/ -run TestRegenGolden`
- [x] verify the generated `catalog_gsbm.go` compiles, includes `MarshalGSBM`/`UnmarshalGSBM`/`Reset`/`FieldPresent`, and emits the high-tag field-key without varint truncation (tag 536870911 emitted in switch and Marshal; codegen prints a documented warning that the tag exceeds `gsbm.MaxTrackedTag`, which is expected — sidecar bitmap won't track this tag)
- [x] add a smoke test in `graph_test.go` that round-trips an empty `Catalog{}` (asserts the package wires up before tackling complex shapes in Task 2)
- [x] write tests for the smoke round-trip
- [x] run project tests - must pass before next task

### Task 2: `fixtures/graph` round-trip + negative tests

- [ ] add `TestCatalogRoundTrip` table-driven test covering: fully populated `Catalog` (with `[]*Section`, `[]*Item`, `map[string]*Tag`, `map[Code]*Tag`); nil-everywhere; one slice-of-nullable populated, rest nil; one map-of-nullable populated, rest nil
- [ ] add `TestCatalogPresenceBitmap` — assert `FieldPresent` correctly distinguishes missing vs present-zero on the high-tag (`Tail`) field and the nullable-collection fields
- [ ] add `TestCatalogDuplicateTagLastWins` — hand-crafted blob writes tag 1 twice with different values; assert the second wins per spec §3.3
- [ ] add `TestCatalogDuplicateMapKeyLastWins` — hand-crafted map payload with duplicate key; assert second value wins per spec §3.3
- [ ] add `TestCatalogLengthBoundedRegionOverflow` — hand-crafted blob whose `Sections` LENGTH_DELIM declares more bytes than remain; assert decode rejects with the appropriate `gsbm.Err*` sentinel (likely `ErrTruncated`); verify the error chain via `errors.Is`
- [ ] add `TestCatalogHighTagRoundTrip` — round-trip a `Catalog` with `Tail.Marker = true`; assert the `2^29 - 1` field key encodes as a 5-byte varint and decodes correctly
- [ ] write tests for any new helpers added during this Task
- [ ] run project tests - must pass before next task

### Task 3: Add `fixtures/evolution` package — paired before/after sub-packages

- [ ] create `tools/gsbmcodegen/fixtures/evolution/` with one sub-package per scenario, each containing `before/types.go` and `after/types.go` for the six scenarios in Technical Details (add, compat_write replace, wire-type change, type change, tag change, field removed)
- [ ] for each scenario where both sides are schema-valid (add, compat_write replace), run codegen via `REGEN_GOLDEN=1` so the test can also exercise round-trip from before-encoder to after-decoder where applicable
- [ ] for breaking-only scenarios (wire-type / type / tag / removed), the after side does not need golden codegen — only its source needs to be parseable by `gsbmschema.ParseSource` so the classifier can diff
- [ ] add a small loader helper in `evolution/loader.go` (test-only build tag if needed) that wraps `gsbmschema.ParseSource(packageDir)` and returns a schema; reuse — do not duplicate — `ParseSource` and the existing `Analyze` helpers
- [ ] write tests for the loader (asserts it returns a schema with the expected struct decls for one before-package and one after-package)
- [ ] run project tests - must pass before next task

### Task 4: Classifier round-trip tests on `fixtures/evolution` pairs

- [ ] add `tools/gsbmcodegen/fixtures/evolution/classifier_test.go` with one test function per scenario (`TestEvolutionAddField`, `TestEvolutionCompatWriteReplace`, `TestEvolutionWireTypeChange`, `TestEvolutionTypeChange`, `TestEvolutionTagChange`, `TestEvolutionFieldRemoved`)
- [ ] each test loads `before/` and `after/`, calls `classifier.Compare`, and asserts: exact set of change codes; severity per change; overall MaxSeverity matches the table in Technical Details
- [ ] for breaking scenarios, also assert that running with `--allow-breaking` (or the equivalent `Acknowledged` field on the change) flips the result from blocked to allowed; assert without the ack the diff is blocked
- [ ] for the compat_write replace scenario, additionally assert: with `--allow-stop-compat-write` cleared, the after→`later-after` (a third package where compat_write is removed and the field becomes plain deprecated) is `breaking`; with the flag set, it is `safe`
- [ ] write a round-trip test for the safe-add scenario: encode with `before/`'s generated Marshal, decode with `after/`'s generated Unmarshal; assert the new tag's field is the zero value, every old tag round-trips
- [ ] write a round-trip test for the safe-add scenario in reverse: encode with `after/` (including the new tag), decode with `before/`; assert the new tag is skipped via `SkipField` and old tags round-trip (uses the existing forward-compat path)
- [ ] write tests for any helpers added in this Task
- [ ] run project tests - must pass before next task

### Task 5: Add `fixtures/rejection` package — anonymous-field validator test

- [ ] create `tools/gsbmcodegen/fixtures/rejection/types.go` with the `Inner` + `WithEmbed` types from Technical Details (anonymous embedded field with `//gsbm:root` on the outer struct)
- [ ] do NOT generate goldens for this package; it intentionally fails schema validation
- [ ] add `tools/gsbmcodegen/fixtures/rejection/rejection_test.go` calling `gsbmschema.Analyze` on this package, asserting at least one `Issue{Code:"field/anonymous"}` with `Message` containing `"Inner"`
- [ ] add a complementary positive test in the same file: a sibling struct in the same package that uses named composition (`Embed Inner `bin:"2"``) and produces zero anonymous-field issues; proves the rule is anonymous-only, not composition-only
- [ ] write tests for both above
- [ ] run project tests - must pass before next task

### Task 6: Documentation pass

- [ ] add a short subsection to `docs/implementation.md` (under §3 or a new §6 "Test fixtures") naming the four fixture packages and one-line summarizing each scope; link to this plan
- [ ] cross-check `docs/spec.md` §3.3 (duplicate handling) and the length-bounded-region rule are now backed by tests; add a sentence in each spec section noting "see fixtures/graph for round-trip evidence"
- [ ] update `docs/plans/20260509-gsbm-compat-write-lifecycle-impl.md` (the lifecycle Task 5 verify step) to point at `fixtures/evolution/` for the integration coverage
- [ ] write tests if any new docs include runnable examples (likely none; this is doc-only)
- [ ] run project tests - must pass before next task

### Task 7: Verify acceptance criteria

- [ ] verify all requirements from Overview are implemented: three fixture packages (`graph`, `evolution`, `rejection`); round-trip + presence + duplicate + length-overflow + high-tag tests on `graph`; classifier diff tests on every evolution scenario; anonymous-field rejection test on `rejection`
- [ ] run `go test ./... -count=1`; all packages green
- [ ] run `go vet ./...` and the project linter (whatever the repo uses; check `.github/workflows/` for the CI command); fix any new issues introduced by this plan
- [ ] verify test coverage on `tools/gsbmschema/classifier.go` reaches the previously-untested change codes (`field/wire-changed`, `field/tag-changed`, `field/type-changed`, `field/removed`)
- [ ] confirm the design source plan(s) related to spec §3.3 and the validator-rejection rule are still consistent with what the new fixtures pin
- [ ] move this plan to `docs/plans/completed/` per project convention (or let ralphex move it automatically on completion)

## Post-Completion

*Items requiring manual intervention - no checkboxes, informational only*

- Goldens are committed alongside fixtures; future codegen changes that affect emitted code will need `REGEN_GOLDEN=1` runs across all four fixture packages (`sample`, `graph`, `evolution/*`), not just `sample`. Document this in the contributing notes if/when CONTRIBUTING.md is added.
- The `2^29 - 1` tag is at the upper boundary the spec allows; this fixture pins the boundary but normal user code should use small tags (1-15 are 1-byte varints; tag-bound usage is anti-pattern outside this fixture).
- The `fixtures/evolution/` scenarios document the project's append-only stance via tests, not just docs. If the policy is ever relaxed, those tests need updating too — they are the source of truth for the policy contract.
