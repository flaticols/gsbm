# implement custom codecs for external value types (functional API)

## Overview

Add a `bin:"N,custom=CodecName"` tag option that lets a field opt out of normal schema traversal and use a named, pre-registered encode/decode pair. Today the schema walker descends into external types like `time.Time` and `decimal.Decimal` and reports their private fields as missing `bin` tags or rejects them as `type/external`. With custom codecs, those types get stable domain-specific wire encodings without the schema contract being coupled to the third-party's internal layout.

Resolves [issue #10](https://github.com/flaticols/gsbm/issues/10). API shape: **functional** — users register a pair of plain Go functions against a name; codegen emits direct calls (no interface, no vtable).

## Context

- Today the schema walker (`tools/gsbmschema/discover.go`) descends into struct fields unconditionally. External types surface as `tag/missing` (on private fields) or `type/external`.
- The wire-format spec does not need to change. A custom-codec field is encoded exactly like any other field with the wire type the codec advertises (VARINT, LENGTH_DELIM, FIXED64, FIXED32). The schema snapshot records the codec name so a change/removal is visible in diffs.
- The existing `//gsbm:opaque` marker is a partial answer (skips traversal) but doesn't provide a way to actually encode the type. Custom codecs complete that story.
- Codec API decision (locked in user answer): **functional**. `func EncodeXxx(*gsbm.Writer, T) error` + `func DecodeXxx(*gsbm.Reader, *T) error`. Each codec also declares its wire type. No interface; codegen calls the functions directly. Cheaper than vtable dispatch on hot paths.

## Development Approach

- **Testing approach**: Regular — implement registry + tag parser + validator + codegen, then a real-codec fixture (UnixNano timestamp, string-formatted decimal).
- Land in four commits: tag parser; codec registry; codegen emit; snapshot/classifier integration.
- Update this plan when scope changes during implementation.

### CRITICAL: do not commit the plan file to the branch

`docs/` contains only `spec.md` going forward. Stage files individually; never `git add -A`. If a plan lands in a commit, `git rm --cached docs/plans/**` and amend.

### CRITICAL: do not fix code or tests to make tests green

Investigate root cause. Production wrong → fix production; test wrong → cite spec, then fix. Forbidden: loosening assertions, swapping `errors.Is` for `err != nil`, swallowing errors, `t.Skip`, regenerating goldens to dodge hand-written tests, editing the codec contract to make a stuck test pass. Surface `⚠️` blockers.

## Testing Strategy

- Fixture package `tools/gsbmcodegen/fixtures/customcodec/` with a `Record` struct using two real codecs: `TimeUnixNano` (encodes `time.Time` as int64 nanoseconds via VARINT) and `DecimalString` (encodes a `DecimalAmount` as string via LENGTH_DELIM).
- Round-trip test: encode → decode → DeepEqual on `Record`.
- Nullable test: `*time.Time` with the same codec must round-trip nil correctly (the wire is LENGTH_DELIM-wrapped per spec §5.1, and the codec runs only on the present-and-non-zero state).
- Snapshot test: `schema_snapshot.json` records the codec name; removing the codec attribute on a field → classified breaking (wire-affecting); changing the codec name → classified breaking.
- Registry-error test: codegen against a schema referencing an unregistered codec name fails with a clear `codec/unregistered` diagnostic.

## Progress Tracking

- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document blockers with ⚠️ prefix

## Technical Details

### Tag parser

In `tools/gsbmschema/parse.go`'s `parseFieldTag` (or wherever `bin:` is parsed today), accept the `custom=CodecName` qualifier alongside the existing `deprecated` and `compat_write` qualifiers. `CodecName` is a Go identifier (no path; the codec lives in the user's project or a registered package). The codec name is stored on `FieldDecl.CustomCodec string`.

A field with `custom=` short-circuits schema traversal: the validator records the field with its declared wire type but does NOT descend into the field's Go type. Therefore `time.Time`'s private fields never appear in diagnostics.

### Codec registry (codegen-side)

A new package `tools/gsbmcodegen/codecs/` (or a section in `tools/gsbmcodegen/codegen.go`) holds the codec registry:

```go
type CodecDecl struct {
    Name      string      // user-facing name, e.g. "TimeUnixNano"
    GoType    string      // fully-qualified Go type the codec handles
    WireType  string      // "WireVarint", "WireLengthDelim", "WireFixed32", "WireFixed64"
    EncodeFn  string      // fully-qualified encode function name
    DecodeFn  string      // fully-qualified decode function name
    PkgImport string      // import path users must add to use this codec
}
```

Registration is at codegen time, not runtime: the user (or a built-in default) registers codecs via a small Go file or a YAML config that codegen reads. v1 ships built-in codecs for `time.Time` (TimeUnixNano) and a generic `DecimalString` template; users register their own by adding entries to the codec config.

### Codegen emit

For a field with `custom=Name`, codegen emits:

- Encode: `w.WriteTag(<tag>, gsbm.<WireType>)` then `codecs.<EncodeFn>(w, v.<Field>)`.
- Decode: tag-validate against `gsbm.<WireType>`; then `codecs.<DecodeFn>(r, &v.<Field>)`.
- Optional fields (`*T` with `custom=Name`): emit the standard `LENGTH_DELIM`+presence-byte wrapper; the codec body only runs on `PresenceNonZero`. `PresenceNil` and `PresenceZero` are handled by the wrapper.

The codec's `EncodeFn`/`DecodeFn` use the existing `*gsbm.Writer`/`*gsbm.Reader` API — no new runtime surface needed.

### Snapshot + classifier

Add `CustomCodec string` to the snapshot's field entry. Classifier rules:

- Add `custom=Name` to a previously-untagged field → wire-affecting unless the field was previously deprecated/never-written → label per existing add rules.
- Remove `custom=Name` from a field → breaking (the wire shape silently changes from codec-driven to schema-traversal-driven).
- Change `custom=Name` to `custom=OtherName` → breaking.

### Built-in codecs

Ship two in `tools/gsbmcodegen/codecs/builtins/`:

- `TimeUnixNano`: `time.Time` ↔ `int64` (nanoseconds since Unix epoch). VARINT.
- `DecimalString`: `<user's decimal type>` ↔ `string` (decimal representation). LENGTH_DELIM. Generic over the user's exact type — user binds the codec at registration time.

## Implementation Steps

### Task 1: Tag parser accepts `custom=Name`

- [ ] in `tools/gsbmschema/parse.go`, extend `parseFieldTag` to accept the `custom=<identifier>` qualifier; store as `FieldDecl.CustomCodec string`
- [ ] validate the qualifier is mutually exclusive with `deprecated`/`compat_write` on the same field (a deprecated field with a custom codec is allowed and means "still readable via the codec"; a tag with both `custom=X` and `custom=Y` is malformed)
- [ ] in the validator (`tools/gsbmschema/discover.go`), short-circuit type traversal for fields carrying a `CustomCodec`: do not descend; do not emit `tag/missing` or `type/external` for the underlying type
- [ ] write tag-parser tests: accepts `custom=TimeUnixNano`; rejects `custom=` (empty); rejects two `custom=` qualifiers on the same field
- [ ] write validator tests: `time.Time` field with `custom=TimeUnixNano` produces no `tag/missing` issues
- [ ] run project tests - must pass before next task

### Task 2: Codec registry + built-in codecs

- [ ] create `tools/gsbmcodegen/codecs/` with `CodecDecl` struct and a registry (`map[string]CodecDecl`); registration via a small Go init or a `codecs.yaml` config loaded at codegen time
- [ ] ship built-in codecs: `TimeUnixNano` (`time.Time` ↔ int64 nanoseconds, VARINT) and a templated `DecimalString` (string-formatted decimal, LENGTH_DELIM)
- [ ] codegen errors with code `codec/unregistered` and a clear diagnostic if a field references an unknown codec name; the diagnostic must include the list of registered names so users see the typo
- [ ] write registry tests: register then lookup round-trips; duplicate registration of the same name is an error; lookup of unregistered name returns the unregistered error
- [ ] write tests for the two built-in codecs: encode + decode round-trip on values, including edge cases (zero time, negative nanoseconds for pre-1970, decimal with trailing zeros)
- [ ] run project tests - must pass before next task

### Task 3: Codegen emit calls the codec functions

- [ ] in `tools/gsbmcodegen/emit.go`, for fields with `CustomCodec` set, replace the normal `MarshalGSBM`/`UnmarshalGSBM` recursion with direct codec function calls
- [ ] handle nullable custom-codec fields (`*T` with `custom=Name`): emit the standard LENGTH_DELIM+presence-byte wrapper; codec body runs only on `PresenceNonZero`
- [ ] add fixture package `tools/gsbmcodegen/fixtures/customcodec/` with `Record{CreatedAt time.Time \`bin:"1,custom=TimeUnixNano"\`; Amount DecimalAmount \`bin:"2,custom=DecimalString"\`; OptionalAt *time.Time \`bin:"3,custom=TimeUnixNano"\`}`
- [ ] regenerate goldens; commit
- [ ] write round-trip tests for `Record`: value codec round-trip, pointer/nullable codec round-trip (nil, present-zero, present-non-zero)
- [ ] write a wire-format byte-equality test: `Record{CreatedAt: knownTime}` produces the exact byte sequence we'd hand-craft with `WriteTag + WriteVarint(unixNanos)`
- [ ] run project tests - must pass before next task

### Task 4: Snapshot + classifier integration

- [ ] add `CustomCodec string` to the snapshot's field entry struct in `tools/gsbmschema/snapshot.go`
- [ ] update `classifier.go`: add `custom=` to an active field → wire-affecting (unless field was never written); remove `custom=` from an active field → breaking; change codec name → breaking
- [ ] write classifier tests for each transition
- [ ] regenerate `schema_snapshot.json` files across all fixture packages
- [ ] run project tests - must pass before next task

### Task 5: Verify acceptance criteria

- [ ] verify all requirements from Overview are implemented: `bin:"N,custom=CodecName"` parses; validator skips type traversal for custom-codec fields; codegen emits direct codec calls; built-in `TimeUnixNano` and `DecimalString` codecs work; nullable custom-codec fields round-trip with presence semantics; snapshot records codec name; classifier flags codec changes as breaking
- [ ] run `go test ./... -count=1`; all green
- [ ] run `go vet ./...` and `go build ./...`; clean
- [ ] update `docs/spec.md` §5 with a short subsection documenting the custom-codec convention (field key uses the codec's declared wire type; nullable wrapper unchanged); add a row to §8 Constraints summary if needed
- [ ] close out: comment on issue #10 with the merge commit and a pointer to the built-in codec list

## Post-Completion

*Items requiring manual intervention - no checkboxes, informational only*

- Codec registry config (Go init vs YAML) is a project-wide decision the user makes. Document the chosen mechanism in the README.
- Built-in codecs ship with the gsbm repo; user codecs live in user code and register via the same mechanism. For codecs that are widely useful across orgs, contributions to the built-ins set are welcome — those land via separate PRs.
- The custom-codec path bypasses the schema's external-type classifier (the issue's "Why It Matters" follow-up). Once issue #4's loader work makes external-type classification reliable, the validator can offer a "did you mean to add `custom=`?" suggestion when it detects an unwrapped third-party type.
