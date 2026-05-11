# accept named primitive types as map keys

## Overview

Extend schema validation so a `map[K]V` field is accepted when `K` is a **named type whose underlying type is a supported primitive map-key type** (`string`, `bool`, signed/unsigned integers, `uintptr`). Today the validator rejects all named-keyed maps with `map/bad-key: ... map key "...(string)" must be a primitive or string`, forcing domain models to drop type safety and use `map[string]T` instead of `map[Code]T`.

Resolves [issue #7](https://github.com/flaticols/gsbm/issues/7).

## Context

- Spec §5.3 already lists `string`, `bool`, signed-int, unsigned-int, and "named types whose underlying type is one of the above" as allowed map key types. The validator is stricter than the spec; this plan brings them into agreement.
- Validator: `tools/gsbmschema/discover.go` around the map-key path (search for `map/bad-key` error code). The rejection lives in the type-classification step before codegen sees the field.
- Codegen: `tools/gsbmcodegen/emit.go` already handles map encode/decode for primitive keys; for named keys it must cast through the underlying primitive when writing the key varint and convert back when reading.
- Wire format: unchanged. The wire bytes for `map[Code]int64` with `Code` underlying `string` are identical to `map[string]int64`. Schema snapshot must record both the named type and the underlying primitive so a change to the underlying type is a wire-affecting diff.
- Float map keys remain rejected (spec §5.3); plan does not change that.

## Development Approach

- **Testing approach**: Regular — extend validator + codegen, then add fixtures and tests.
- Land in two commits: validator unblock + codegen support; snapshot schema integration.
- Update this plan when scope changes during implementation.

### CRITICAL: do not commit the plan file to the branch

`docs/` contains only `spec.md` going forward. Stage files individually (`git add <path>`), never `git add -A`. If a plan accidentally lands in a commit, `git rm --cached docs/plans/**` and amend before pushing.

### CRITICAL: do not fix code or tests to make tests green

Investigate root cause. Production wrong → fix production; test wrong → cite the spec, then fix the test. Forbidden: loosening assertions, swapping `errors.Is` for `err != nil`, swallowing errors, `t.Skip`, regenerating goldens to dodge hand-written tests. Surface `⚠️` blockers rather than papering over failures.

## Testing Strategy

- New fixture: `tools/gsbmcodegen/fixtures/sample/` (or a new `fixtures/namedkey/` package) with a `Counts` struct that has fields keyed by `Code` (named string), `Severity` (named int8), and `Bucket` (named uint16). Round-trip test asserts the wire bytes are byte-identical to a parallel hand-written `map[string]…` / `map[int8]…` / `map[uint16]…` blob.
- Validator test: a named-float key (e.g., `type Rate float64`) is still rejected with `map/bad-key`.
- Snapshot test: `schema_snapshot.json` for the new fixture records both `Code` (named) and its `string` underlying; changing `Code`'s underlying from `string` to `int64` shows up as wire-changed in the classifier.

## Progress Tracking

- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document blockers with ⚠️ prefix

## Technical Details

### Validator change

In `tools/gsbmschema/discover.go`, the map-key classification: currently a hard list of accepted `*types.Basic` kinds. Extend to: if the key type is `*types.Named`, recurse into its `Underlying()` and re-check against the accepted primitive list. Reject if the underlying primitive is `float32`/`float64`.

### Codegen change

In `tools/gsbmcodegen/emit.go`, the map-key encode/decode emit:

- Encode: cast through the underlying primitive — `w.WriteString(string(k))` for a named string, `w.WriteVarint(int64(k))` for a named signed int, etc. The cast is free at runtime (no allocation), but the codegen must know to emit it.
- Decode: read the primitive, then cast back to the named type before assigning to the map: `m[Code(s)] = v`.

### Schema snapshot

`tools/gsbmschema/snapshot.go` already records `TypeRef` with `PkgPath` + `Name`. For named keys, the snapshot needs an extra `Underlying` field on the map-key `TypeRef` so a future change of `Code` from `string` to `int64` (or any other underlying-primitive change) is visible. The classifier (`tools/gsbmschema/classifier.go`) consumes this field: if `Underlying` changes, the change is labeled `breaking` (wire-affecting).

## Implementation Steps

### Task 1: Unblock the validator for named primitive map keys

- [ ] in `tools/gsbmschema/discover.go`, locate the `map/bad-key` issue site; rewrite the key-type check so `*types.Named` is unwrapped to its `Underlying()` and re-tested against the accepted-primitive list
- [ ] preserve the existing rejection for floats (named or unnamed) and structs/slices/maps/nullable/`[]byte`
- [ ] write tests in `tools/gsbmschema/validate_test.go`: named string key accepted; named int64 key accepted; named bool key accepted; named float key still rejected with `map/bad-key`; struct key still rejected
- [ ] run project tests - must pass before next task

### Task 2: Codegen support for named map keys

- [ ] in `tools/gsbmcodegen/emit.go`, find the map encode/decode emit paths; add the cast-through-underlying logic for `*types.Named` keys
- [ ] add a fixture struct (e.g., `Counts` with `ByCode map[Code]int64` where `type Code string` is already accepted as a value type) to `tools/gsbmcodegen/fixtures/sample/types.go` if not already present; otherwise add a small new fixture package `fixtures/namedkey/`
- [ ] regenerate the golden for the touched fixture via `REGEN_GOLDEN=1 go test ./tools/gsbmcodegen/ -run TestRegenGolden`
- [ ] write round-trip tests asserting the wire bytes are byte-identical to a parallel `map[string]int64` baseline
- [ ] write tests for `map[NamedBool]V`, `map[NamedSignedInt]V`, `map[NamedUnsignedInt]V`
- [ ] run project tests - must pass before next task

### Task 3: Schema snapshot records the named/underlying pair

- [ ] extend `TypeRef` (or whichever struct represents the map-key type in `tools/gsbmschema/types.go`) with an `Underlying string` field that captures the underlying primitive's `BasicKind` name
- [ ] update `snapshot.go`'s marshalling to include the new field for map-key positions; default empty for non-named keys so existing snapshots stay readable
- [ ] update `classifier.go` (`compareField` or the equivalent) to flag a change to the map-key `Underlying` as wire-changed (breaking, requires `--allow-breaking` ack)
- [ ] regenerate every committed `schema_snapshot.json` under `tools/gsbmcodegen/fixtures/` to include the new field
- [ ] write classifier tests: same name, same underlying → no diff; same name, different underlying → breaking
- [ ] run project tests - must pass before next task

### Task 4: Verify acceptance criteria

- [ ] verify all requirements from Overview are implemented: named-primitive map keys accepted (validator + codegen); wire bytes byte-identical to underlying-primitive baseline; snapshot records both name and underlying; classifier flags underlying-type changes as breaking
- [ ] confirm spec §5.3 wording is consistent with the new behavior (it already permits named keys); no spec edit expected, but re-read to be sure
- [ ] run `go test ./... -count=1`; all green
- [ ] run `go vet ./...` and `go build ./...`; clean
- [ ] close out: comment on issue #7 with a pointer to the merge commit

## Post-Completion

*Items requiring manual intervention - no checkboxes, informational only*

- Snapshot schema gained a new field. Downstream consumers re-running `gsbmschema snapshot` will get new file contents (extra `Underlying` field on map-key entries). The hash will shift once; mention this in the PR description so consumers aren't surprised.
