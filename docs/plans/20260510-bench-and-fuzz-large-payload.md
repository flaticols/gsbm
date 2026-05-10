# benchmarks and fuzz testing with large (1-2 MB) payloads

## Overview

Add the missing performance and fuzz coverage to gsbm. Today the repo has zero `Benchmark*` functions and one fuzz harness (`FuzzArenaDecodeAgainstHeap` in the sample fixture). The codebase documents allocation targets in `docs/implementation.md` (warm decode → near-zero, encode → ~1 alloc/op for typical payloads, BDD prototype hits 5,241 cold-decode allocs on 40 offers) but nothing measures whether the implementation actually meets them. This plan lands a benchmark suite plus expanded fuzz coverage, both centered on **large (~1-2 MB) realistic payloads** so the numbers reflect the production hot path (Spanner offer batches), not micro-fixtures.

The plan deliberately measures two separate things:
- **Throughput / allocation profile** — `Benchmark*` functions exercising encode and decode on the heap path (cold, warm-pooled) and the arena path (cold, warm with pool reuse). Each benchmark uses `b.ReportAllocs()` and asserts a numeric allocation budget via `testing.AllocsPerRun` so regressions surface immediately rather than lurking in raw ns/op numbers.
- **Decoder robustness** — fuzz harnesses for the writer→reader path on the wire format directly (not only via arena cross-check), seeded with the large payload generator. The existing `FuzzArenaDecodeAgainstHeap` stays; new harnesses cover (a) malformed-blob rejection: any byte sequence the decoder accepts must be re-encodable to byte-identical output (canonical-form invariant), and (b) header-field corruption: random flags / fmtVer / schemaHint mutations must surface the documented error sentinels, not panic.

## Context

- Branch: `benchmarks-and-fuzz-large-payload` off `latest @ aa19c5d` in worktree `/Users/den/Developer/gsbm-bench-fuzz/`.
- No `Benchmark*` functions exist anywhere in the repo today (verified by grep across `*_test.go`).
- One fuzz function exists: `FuzzArenaDecodeAgainstHeap` at `tools/gsbmcodegen/fixtures/sample/arena_test.go:180` with a single seed corpus directory at `tools/gsbmcodegen/fixtures/sample/testdata/fuzz/FuzzArenaDecodeAgainstHeap/`.
- Allocation targets are documented at `docs/implementation.md:88-101`. The cold-decode line cites a prototype number (5,241 allocs on 40 offers) — useful as a sanity ceiling but not a regression budget.
- Existing alloc test patterns to **mirror** (don't reinvent): `reset_test.go:176-188` (TestPoolWarmupConvergence), `reset_test.go:228-245` (TestEncodeWarmAllocsBoundedByPool), `arena_test.go:121-145` (TestArenaAllocsScaleSublinearly), `sample_test.go:506-515` (TestFieldPresentSidecarBoundedAllocs). All use `testing.AllocsPerRun(N, fn)`.
- The user-facing seam for arena-vs-heap dispatch is the **separate package** (`storage/gsbmarena`), not `SetAllocator`. Generated arena-mode files (`*_gsbm_arena.go`) call `DecodeOrder(blob, *Arena)` rather than `Order.UnmarshalGSBM(*Reader)`. Benchmarks must use the right entrypoint per mode.
- CI (`.github/workflows/gsbm-schema.yml`) only runs `gsbmschema lint/snapshot` — no `go test` invocation. Benchmarks will be manual-only initially; a separate workflow is added under Task 5 if desired.
- Map encoding is deterministic on the wire (sorted keys per spec §5.3) but Go map iteration order is not. Re-encode-equality assertions must compare wire bytes, not Go-side iteration.
- Presence-tracking sidecar (`storage/gsbm/presence_track.go`) leaks ~144 bytes per ad-hoc receiver until `gsbm.ForgetPresence(v)` is called or the address is reused. Benchmarks that allocate fresh receivers must call `ForgetPresence` (or use a pool) or the alloc numbers will balloon and not reflect production behavior.

## Development Approach

- **Testing approach**: Regular (write the payload generator first, then layer benchmarks and fuzz on top; tests for the generator itself land in the same Task as the generator)
- Complete each Task fully before moving to the next; benchmarks in Task 2 consume the generator from Task 1, fuzz harnesses in Task 4 consume the generator too
- Benchmarks live in the same package as the code they exercise (Go convention). Fuzz harnesses live where the existing one lives (`tools/gsbmcodegen/fixtures/sample/`) plus new ones in `storage/gsbm/` for direct-wire fuzzing
- Numeric assertions over `testing.AllocsPerRun` accompany every `Benchmark*` so regressions block CI when wired up; ns/op alone is too noisy for a guard
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
  - Loosening a benchmark allocation budget (`testing.AllocsPerRun` floor) without a spec/perf-doc citation explaining why the new floor is correct.
  - Regenerating goldens (`REGEN_GOLDEN=1`) when goldens disagree with hand-written tests — first decide whether the hand-written test or the codegen is correct.
  - Editing the wire format, the spec, or `gsbm.Err*` sentinels to make a stuck test pass.
- If you cannot determine the root cause within reasonable effort, **stop and surface the failure** in a `⚠️` plan note rather than papering over it. A failing test left visible is more valuable than a passing test that proves nothing.
- This rule applies to every `run project tests - must pass before next task` checkbox in the Implementation Steps below.

## Testing Strategy

- **Unit tests**: required for every Task that adds non-test helper code. The big-payload generator (Task 1) needs a unit test asserting it produces a blob in the [1 MiB, 2 MiB] range deterministically.
- **Benchmarks**: every `Benchmark*` includes `b.ReportAllocs()` and is partnered with a `Test*BenchmarkBudget` that wraps the same function in `testing.AllocsPerRun(N, fn)` and asserts a numeric ceiling. Without the ceiling, regressions slip through CI.
- **Fuzz harnesses**: each new `Fuzz*` ships with a seed corpus committed under `testdata/fuzz/<FuzzName>/` so fuzzing has a non-trivial starting set. The 1-2 MB payload from Task 1 is one seed; smaller hand-crafted edge cases (empty body, header-only, single-tag-only, deeply-nested-only) round out the corpus.
- **Allocation regression guard**: benchmarks assert against documented targets (encode warm ≤ 4 allocs/op for `makeRichOrder`-class payloads per `reset_test.go:228`; decode warm with pool ≤ string-floor + slack). Targets for 1-2 MB payloads will be empirically determined in Task 2 and locked in once measured — the test's job is to catch *regressions*, not to re-derive the target each run.
- **Run project tests after each Task before proceeding** (`go test ./...`); benchmarks are run with `go test -bench=. -benchmem -run='^$'` (no Test*) on demand.

## Progress Tracking

- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope

## Technical Details

### Big payload generator

A new file `internal/bench/payload.go` (private package, used only by tests) exports:

```go
// MakeLargeOrder returns an Order graph that encodes to a blob in the
// [targetMin, targetMax] byte range. Deterministic for a given seed: same
// seed produces byte-identical output across calls. Used by benchmarks and
// fuzz seeds so regressions are reproducible.
func MakeLargeOrder(seed int64, targetMin, targetMax int) sample.Order

// MakeLargeCatalog is the graph-fixture equivalent — exercises the
// nullable-in-slice and nullable-in-map paths.
func MakeLargeCatalog(seed int64, targetMin, targetMax int) graph.Catalog

// EncodedSize returns the on-the-wire byte count of the given root.
func EncodedSize(v gsbm.Marshaler) (int, error)
```

Why a private package and not `tools/gsbmcodegen/fixtures/sample/`: the generator depends on **both** `sample` and `graph` fixtures, and lives outside the codegen-tool tree because the goal is benchmark realism, not codegen golden reproduction. Putting it in `internal/bench/` keeps it test-only (Go's `internal/` rule).

The generator builds payloads by **scaling existing fixture-builder functions**. For Order it inflates the slice/map cardinalities (Items, Tags, Counts, QtyList, etc.) until `EncodedSize` lands in the target range. For Catalog it inflates the Section count and per-Section Item count. String fields are filled with deterministic pseudo-random ASCII (seeded from the `seed` parameter) so byte content is realistic-shaped (varying lengths, no all-zero strings) but reproducible.

### Benchmark categories

| Category | Bench name | What it measures | Asserted alloc budget |
|---|---|---|---|
| Encode (heap, pooled buffer) | `BenchmarkLargeOrderEncodeHeapPooled` | `Order.MarshalGSBM` into a `sync.Pool`-managed `[]byte` | ≤ 1 alloc/op (per `implementation.md:84`) |
| Encode (heap, fresh buffer) | `BenchmarkLargeOrderEncodeHeapFresh` | Same, but new buffer per op | bounded vs pooled (warm only) |
| Decode (heap, cold) | `BenchmarkLargeOrderDecodeHeapCold` | Fresh `*Order` per op, no pool | scaled vs payload size; logged for visibility |
| Decode (heap, warm + pool + DecodeInto) | `BenchmarkLargeOrderDecodeHeapWarm` | `sync.Pool` of `*Order`, `gsbm.DecodeInto` | ≤ string-floor + slack (per `reset_test.go:147`) |
| Decode (arena, single-shot) | `BenchmarkLargeOrderDecodeArenaShot` | Fresh `Arena` per op, immediate `Release()` | single-digit allocs/op target (per `implementation.md` arena bound) |
| Decode (arena, pool reuse) | `BenchmarkLargeOrderDecodeArenaPool` | Arena from a `sync.Pool`, reset between ops | sub-single-digit warm |
| Catalog: heap cold + arena cold | `BenchmarkLargeCatalogDecodeHeap` / `Arena` | Same axes on the graph fixture | scaled targets |
| Round-trip cycle | `BenchmarkLargeOrderRoundTrip` | Encode + decode in one op | sum of the two warm budgets |

Every `Benchmark*` is partnered with a `Test*Budget` in the same file using `testing.AllocsPerRun` so the budget is asserted on every `go test` invocation, not only on `go test -bench=.`. Pattern:

```go
func TestBenchmarkLargeOrderEncodeHeapPooledBudget(t *testing.T) {
    if testing.Short() { t.Skip("alloc budget test runs full") }
    p := sync.Pool{New: func() any { return new(bytes.Buffer) /* or []byte */ }}
    in := bench.MakeLargeOrder(0, 1<<20, 2<<20)
    avg := testing.AllocsPerRun(50, func() {
        // ... encode in into a pooled buffer
    })
    if avg > 1.5 {  // budget = 1, allow tiny float slack for amortisation
        t.Fatalf("encode allocs = %.2f, budget 1.5", avg)
    }
}
```

### Fuzz harnesses to add

1. **`FuzzReaderRobustness`** at `storage/gsbm/fuzz_test.go` — feeds the `Reader` arbitrary bytes and asserts: any error returned is one of the documented `gsbm.Err*` sentinels (no `*errors.errorString` from a raw `errors.New` deep in the call stack); the reader does **not** panic, `runtime.Stack`-up; if `ReadHeader` succeeds, every following `ReadTag` either returns a sentinel error or makes progress in the buffer (forward-progress invariant).
2. **`FuzzWriterReaderRoundTripCanonical`** at `storage/gsbm/fuzz_test.go` — fuzz inputs feed the `Reader`; if decode succeeds, re-encode the decoded value with `Writer` and assert the resulting bytes are byte-identical to the input (canonical-form invariant per spec §3.3 last-wins + spec §4.1 canonical varints). Skip the assertion when the decoded graph contains maps (Go iteration order is non-deterministic and the on-wire sort is the deterministic-key thing, not the round-trip-byte-identity thing — known carve-out, document inline).
3. **`FuzzHeaderCorruption`** at `storage/gsbm/fuzz_test.go` — seeded with a valid 1-2 MB blob; the fuzz function flips bytes inside the 8-byte header (`magic`, `fmtVer`, `flags`, `schemaHint`) and asserts the corresponding sentinel: bad magic → `ErrBadMagic`; non-zero flags → `ErrReservedFlags`; unsupported fmtVer → `ErrUnsupportedVer`. Body bytes are untouched so the failure mode is isolated to header validation.
4. **Extend `FuzzArenaDecodeAgainstHeap`** at `tools/gsbmcodegen/fixtures/sample/arena_test.go:180` — add the 1-2 MB payload from `MakeLargeOrder` to its seed corpus via `f.Add(largeBlob)`. The existing assertions (heap and arena agree on accept/reject and value) carry over.

Seed corpora ship under each fuzz function's `testdata/fuzz/<FuzzName>/` directory. Each seed is a small file with a binary blob; Go's fuzz tooling consumes them automatically. The 1-2 MB seeds are committed because reproducibility on a CI fuzz run requires a stable starting point.

### Allocation targets to lock in

Cold decode of a 1 MB payload will allocate proportionally to graph size (slice + map + sub-struct count). Empirical numbers will land in Task 2 and be encoded as the test budget. The shape of the assertion is:

```go
const largeOrderColdAllocCeiling = N  // measured in Task 2, then locked
```

Setting the ceiling once measured (rather than re-deriving) means the test is a *regression* guard: if a future change pushes cold decode from N to 1.5×N, the test fails and the diff explains why. Loosening the ceiling without a corresponding spec / impl note is forbidden per the discipline rule above.

### Out of scope

- Cross-language fuzz harnesses (no other-language gsbm decoder exists).
- Continuous benchmark dashboards (no Datadog / GitHub Actions benchmark workflow). The plan adds local benchmarks; CI integration is a follow-up.
- Profile-guided optimisation (PGO). Out of scope; the goal is a measurement floor, not a tuning pass.

## Implementation Steps

### Task 1: Big payload generator (1-2 MB) under `internal/bench`

- [x] create `internal/bench/payload.go` exporting `MakeLargeOrder(seed int64, min, max int) sample.Order`, `MakeLargeCatalog(seed int64, min, max int) graph.Catalog`, and `EncodedSize(v gsbm.Marshaler) (int, error)`
- [x] generator scales fixture-builder functions (existing `makeRichOrder`-style helpers in `tools/gsbmcodegen/fixtures/sample/reset_test.go:13-32` are the model, but copy out — do not import a `_test.go` symbol) until `EncodedSize` lands in the target range; use a deterministic `math/rand`-style source seeded from `seed` so two calls with the same seed produce byte-identical output
- [x] strings filled with deterministic pseudo-random ASCII of varying length so realism doesn't degrade to all-equal lengths
- [x] add `internal/bench/payload_test.go` asserting: `MakeLargeOrder(0, 1<<20, 2<<20)` produces a blob in `[1<<20, 2<<20]`; same seed twice produces byte-identical wire output (re-encode and `bytes.Equal`); `MakeLargeCatalog` likewise; `EncodedSize` matches `len(MarshalGSBM(...))` to the byte
- [x] write tests for both generators (positive: in-range; negative: rejects unreasonable target ranges like min > max)
- [x] run project tests - must pass before next task

### Task 2: Encode benchmarks + alloc budget guards

- [x] add `storage/gsbm/bench_encode_test.go` with `BenchmarkLargeOrderEncodeHeapPooled` (pool-managed `[]byte`, `b.ReportAllocs()`, uses `bench.MakeLargeOrder(0, 1<<20, 2<<20)` once outside the loop) and `BenchmarkLargeOrderEncodeHeapFresh` (fresh buffer per op)
- [x] add the `Test*Budget` partner for each benchmark using `testing.AllocsPerRun(20, fn)`; pooled budget = 4.0 (1 *Writer alloc + 1 sort-keys slice per map field × 2 maps in sample.Order, per spec §5.3 deterministic-key wire); fresh budget = 48.0 (locked from a 37 allocs/op baseline with ~25% slack for append-growth jitter)
- [x] add `BenchmarkLargeCatalogEncodeHeapPooled` in `tools/gsbmcodegen/fixtures/graph/bench_test.go` to confirm the pooled-encode budget holds for the graph fixture too
- [x] write tests for the budget assertions (the budgets themselves ARE the tests)
- [x] run benchmarks once locally with `go test -bench='^BenchmarkLarge' -benchmem -benchtime=3x ./...` to record baseline ns/op + allocs and capture in this Task's notes (informational, not a checked-in artifact)
  - Order pooled: 3.87 ms/op, 336 KB/op, 3 allocs/op
  - Order fresh: 3.87 ms/op, 6.98 MB/op, 37 allocs/op
  - Catalog pooled: 3.54 ms/op, 139 KB/op, 2 allocs/op
- [x] run project tests - must pass before next task

### Task 3: Decode benchmarks (heap cold, heap warm, arena, round-trip)

- [x] add `storage/gsbm/bench_decode_test.go` with `BenchmarkLargeOrderDecodeHeapCold` (fresh `*Order` per op; remember `gsbm.ForgetPresence(v)` after each iteration to prevent sidecar leak skewing numbers) and `BenchmarkLargeOrderDecodeHeapWarm` (`sync.Pool` of `*Order`, `gsbm.DecodeInto`)
- [x] add `storage/gsbmarena/bench_test.go` with `BenchmarkLargeOrderDecodeArenaShot` (fresh `Arena` per op, immediate `Release()`) and `BenchmarkLargeOrderDecodeArenaPool` (`sync.Pool` of `*Arena`, reset-and-reuse pattern; cite `gsbmarena/arena.go` for the reset semantics)
- [x] add `BenchmarkLargeOrderRoundTrip` in `storage/gsbm/bench_decode_test.go` doing encode-then-decode in one op, with the `sum of the two warm budgets` assertion
- [x] partner each benchmark with a `Test*Budget` using `testing.AllocsPerRun`; cold budgets are scaled-from-graph-size and recorded as constants with a measurement-source comment; warm budgets are tight (string-floor + slack per `reset_test.go:147`)
- [x] add `BenchmarkLargeCatalogDecodeHeap` and `BenchmarkLargeCatalogDecodeArena` in `tools/gsbmcodegen/fixtures/graph/bench_test.go` for the nullable-in-slice / nullable-in-map paths
- [x] document the `gsbm.ForgetPresence` requirement inline so future readers don't accidentally measure sidecar leak allocations
- [x] write tests for the budget assertions (the budgets themselves ARE the tests)
- [x] run benchmarks once locally and note the numbers
  - Order heap cold: 6.34 ms/op, 8.04 MB/op, 100045 allocs/op (b.Loop) — AllocsPerRun baseline 86509–94570 (budget 120000)
  - Order heap warm: 5.00 ms/op, 933 KB/op, 45075 allocs/op (budget 57000)
  - Order round-trip: 8.68 ms/op, 1.27 MB/op, 45077 allocs/op (budget 57000)
  - Arena shot: 5.07 ms/op, 6.62 MB/op, 36765 allocs/op (b.Loop) — AllocsPerRun 52413–55097 (budget 70000)
  - Arena pool: 6.40 ms/op, 8.21 MB/op, 56533 allocs/op (b.Loop) — AllocsPerRun 44079–49441 (budget 56000)
  - Catalog heap: 24.0 ms/op, 22.1 MB/op, 372647 allocs/op (b.Loop) — AllocsPerRun 289685–300585 (budget 380000)
  - Catalog arena: 24.2 ms/op, 18.9 MB/op, 233424 allocs/op (b.Loop) — AllocsPerRun 224505–235684 (budget 285000)
- [x] run project tests - must pass before next task

### Task 4: New fuzz harnesses

- [ ] add `storage/gsbm/fuzz_test.go` with `FuzzReaderRobustness`: feeds arbitrary bytes to `gsbm.NewReader`; asserts any returned error is one of the documented `gsbm.Err*` sentinels via `errors.Is`; asserts no panic via a `defer recover()` that fails the fuzz on panic
- [ ] add `FuzzWriterReaderRoundTripCanonical` to the same file: decode arbitrary bytes; if decode succeeds, re-encode and assert byte-identity with the input. Carve out the maps case with an inline comment citing spec §5.3 deterministic-key rule + Go iteration non-determinism
- [ ] add `FuzzHeaderCorruption` seeded with a valid `MakeLargeOrder` blob; the fuzz function reads the seed, mutates one or more of the first 8 bytes (the harness picks which based on the fuzz input), then asserts the corresponding `gsbm.Err*` sentinel surfaces and decode does not panic
- [ ] commit minimal seed corpora under each fuzz function's `testdata/fuzz/<FuzzName>/` directory: at least one valid blob and one hand-crafted malformed blob per harness. Generate the valid-blob seeds via `bench.MakeLargeOrder` so they're reproducible
- [ ] extend `FuzzArenaDecodeAgainstHeap` at `tools/gsbmcodegen/fixtures/sample/arena_test.go:180` with `f.Add(bench.MakeLargeOrder(...).MarshalGSBM(...))` to give it a 1-2 MB seed
- [ ] run each new fuzz harness for 30 seconds locally as a smoke test (`go test -fuzz=FuzzReaderRobustness -fuzztime=30s ./storage/gsbm/`); confirm no findings, no panics. Document the smoke run in the Task notes.
- [ ] write tests for any helper code added in this Task (the harness functions themselves; the seed-generation helper if non-trivial)
- [ ] run project tests - must pass before next task

### Task 5: Optional CI wiring

- [ ] add `.github/workflows/gsbm-test.yml` (separate from the existing `gsbm-schema.yml`) that runs `go test ./...` on PR; benchmarks themselves stay manual via `go test -bench=.` (CI-time fuzz runs are not free; gate behind a label or schedule, see below)
- [ ] add a `Makefile` (or `justfile` if the repo already has one — check before deciding) target `bench` that runs `go test -bench='^Benchmark' -benchmem -benchtime=3x ./...` and a target `fuzz` that runs each fuzz harness for a configurable duration via `FUZZTIME=30s make fuzz`
- [ ] gate fuzz CI runs on a PR label (e.g., `run-fuzz`) or a nightly cron via a separate workflow file `gsbm-fuzz.yml` — do not run fuzz on every PR (10s+ per harness × N harnesses adds latency)
- [ ] write tests for any non-trivial Makefile targets (smoke: `make bench` exits 0 on a clean tree)
- [ ] run project tests - must pass before next task

### Task 6: Documentation

- [ ] add a subsection to `docs/implementation.md` (under §3.3 "Allocation behavior") describing the benchmark suite, the budget-as-test pattern, and the `make bench` / `make fuzz` entrypoints
- [ ] note in `docs/spec.md` §3.3 (duplicate handling) and §4.1 (canonical varints) that `FuzzWriterReaderRoundTripCanonical` exercises these invariants — with a one-sentence pointer, not a duplicate description
- [ ] write tests if any docs include runnable examples (none expected; this is doc-only)
- [ ] run project tests - must pass before next task

### Task 7: Verify acceptance criteria

- [ ] verify all requirements from Overview are implemented: 1-2 MB payload generator (Task 1); encode/decode benchmarks for heap-pooled, heap-fresh, heap-warm, arena-shot, arena-pool, plus round-trip (Tasks 2-3); allocation budget assertions partnered with each benchmark; three new fuzz harnesses + extended existing one (Task 4); docs updated (Task 6)
- [ ] run `go test ./... -count=1`; all packages green including new budget tests
- [ ] run `go vet ./...` and the project linter; fix any new issues this plan introduced
- [ ] run all benchmarks once and record baseline numbers in commit message of the final commit (informational; not a checked-in artifact)
- [ ] confirm the fuzz harnesses survive a 30-second smoke run each with no findings; if any harness produces a finding, that is a real bug — file a `⚠️` blocker in this plan and STOP, do not silence the harness
- [ ] move this plan to `docs/plans/completed/` per project convention (or let ralphex move it automatically on completion)

## Post-Completion

*Items requiring manual intervention - no checkboxes, informational only*

- Baseline numbers (ns/op, allocs/op, MB/s) recorded in the final commit message become the comparison baseline for future PRs. A regression report tool comparing `benchstat` output is a follow-up.
- The 1-2 MB payload size targets the production hot path (Spanner offer batches per `docs/spanner-notes.md`). If production payload sizes shift materially (e.g., to 10 MB+ batches), the generator's target range and the alloc budgets need to scale with the new size — they are not absolute numbers, they are size-relative budgets.
- Continuous fuzzing (24/7 against a corpus that grows over time) is a separate operational concern. This plan adds the harnesses and seed corpora; running them in a long-lived fuzz cluster is a follow-up that needs infra ownership.
- Sidecar leak (`gsbm.ForgetPresence`): if a future production code path allocates ad-hoc `*Order` receivers without pooling them, the sidecar will grow unbounded. This is documented in `docs/implementation.md:134-178` and re-cited in the decode-cold benchmark; production reviewers should watch for it.
