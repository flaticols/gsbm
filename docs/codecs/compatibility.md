## Wire compatibility and long-lived storage

Once a codec ships and its output lands in Spanner BYTES, on-disk
archives, or replay fixtures, the codec body is part of the wire
contract for as long as those bytes outlive the deploy that wrote
them. This doc covers the rules that keep historic blobs decodable,
the classifier signals that catch a wire-affecting change in review,
and the shape choices that avoid pinning yourself into a corner.

For codec-shape mechanics see [`tools/gsbmcodegen/codecs/README.md`](../../tools/gsbmcodegen/codecs/README.md).
For the on-wire envelope rules see [`docs/spec.md`](../spec.md) §5.8.
For the per-pass determinism invariant a streaming codec must satisfy
see [`lifecycle.md`](lifecycle.md) — silent corruption from a body
that differs between passes corrupts the historic blob just as it
corrupts the second pass.

## The append-only schema policy

Custom codecs do not opt out of the gsbm compatibility rules in
[`docs/spec.md`](../spec.md) §7. A custom-codec field is wire-typed,
tag-keyed, and skip-by-wire-type just like any other field; forward
compatibility (§7.1), backward compatibility (§7.2), and rollback
(§7.4) apply unchanged. The append-only consequences for custom
codecs are:

- Adding a new tag with a custom codec is safe. Old readers skip the
  unknown tag by wire type; new readers decode it.
- Removing or repurposing a live tag is breaking. The same is true
  for the default codec; the custom case has no extra leeway.
- Toggling `bin:"N,custom=..."` on an active field is wire-affecting
  even when the surrounding Go type does not change. The classifier
  flags this — see the next section.
- The `compat_write` → `deprecated` dual-write window (§7.4)
  applies the same way as for default-codec fields. When replacing
  `custom=Old` with a new tag carrying `custom=New`, emit both for
  at least the deployment's rollback window before retiring `Old`.

## Codec name vs codec body

The schema snapshot records the codec **name**, not the body. The
field entry stores `custom: <Name>` as a single string
(`tools/gsbmschema/snapshot.go:86`); the snapshot has no view into
what `EncodeFn`, `EmitFn`, or `StreamFn` actually writes. Two
distinct classes of change follow:

**Wire-affecting (the body changes).** Anything that alters the bytes
the codec emits for a given `v` — varint → fixed-width, reordered
sub-fields, an added presence byte, a changed JSON key order —
silently corrupts every blob written before the change. The snapshot
sees an unchanged name and emits no diagnostic; author discipline is
the only safeguard.

**Non-breaking on the wire (the name changes, body is identical).**
Renaming `MyCodec` → `MyCodecV2` while keeping
`EncodeFn`/`SizeFn`/`EmitFn`/`StreamFn` byte-identical changes the
snapshot but not the wire. The classifier still emits
`field/custom-changed` at `SeverityBreaking`
(`tools/gsbmschema/classifier.go:363`) — it must assume a name swap
accompanies a body swap. Acknowledge with `//gsbm:allow-breaking
"renamed codec; body byte-identical"` (`parse.go:161`); the
classifier records the justification.

Never reuse a codec name for a new body shape. The name is the only
handle the classifier — and reviewers — have on the body's identity.

## Choosing a stable wire shape

The shape you pick on day one is the shape you decode forever. A few
rules push you toward formats that survive future extension.

- **Varint over fixed-width when range varies.** Fixed-width pins the
  value range at registration. Varint widens to the actual value and
  shrinks empty cases. Use `WireFixed32` / `WireFixed64` only when
  the type genuinely has a fixed width on the wire (a UUID, an IPv4
  octet quad, a hash). For anything counted, indexed, or summed,
  prefer `WireVarint`.
- **`LENGTH_DELIM` for anything that might extend.** A length-prefixed
  body lets a future reader skip the field by wire type even if the
  body grows new sub-fields. A self-framing VARINT/FIXED body
  cannot — a reader at an older codec version cannot skip a body
  longer than the wire type advertises. If there is any chance the
  body will gain sub-fields, pick `WireLengthDelim` from the start.
- **Explicit presence, not implicit sentinels.** A presence byte (the
  same one-byte header §5.1 uses for nullable scalars) survives
  zero-value drift; a sentinel value like `-1` for "absent" pins the
  zero meaning forever and breaks the day a real `-1` becomes valid.
  For nullable custom-codec fields (`*T` with `custom=...`), the
  framework wraps the body in §5.1's presence envelope automatically;
  for in-body optional sub-fields, encode presence explicitly.
- **Pin endianness, sort order, and float format.** A custom codec
  that writes `binary.LittleEndian.Uint64(...)` on one machine and
  decodes with `binary.BigEndian` on another will round-trip on a
  single host and corrupt across an architecture migration. Pick one
  endianness — the gsbm helpers use little-endian for fixed-width
  forms (§4.3) — and write it down in the codec doc comment.

## Long-lived storage gotchas

These are the recurring sources of "the blob decoded yesterday but
not today" reports. All of them break the streaming-determinism
invariant ([`lifecycle.md`](lifecycle.md)) and corrupt stored bytes.

- **`time.Time.String()` is locale- and zone-dependent.** Two
  processes encoding the same `time.Time` can emit different bytes
  if their `time.Local` differs. Prefer `t.UTC().Format(time.RFC3339Nano)`
  with an explicit format, or use the analytic `Time` codec
  (`SizeTime` / `EncodeTime` in the codec README) which writes the
  Unix seconds + nanoseconds pair directly.
- **`fmt.Sprintf("%g", f)` precision drifts across Go versions.** Pin
  the precision (`%.17g` for round-trip-stable `float64`) or write
  the IEEE 754 bits as `WireFixed64`.
- **Ranging over a Go map is non-deterministic.** `encoding/json`
  sorts map keys, but a hand-rolled canonicalizer that ranges over
  the map itself does not. The streaming-kind body must be
  byte-identical across the size and write passes (codec README); a
  body that re-orders keys writes a `bodyLen` that disagrees with
  itself. The fixture's `TestRecordRoundTripStreamingPayload`
  (`tools/gsbmcodegen/fixtures/customcodec/customcodec_test.go:481`)
  pins the round-trip; project codecs inherit the obligation.
- **`reflect`-driven field ordering picks up source-order changes.**
  Pin the wire order with explicit field emission, not
  `reflect.Type.Field(i)`.

## Snapshot diff classes

The schema classifier ([`tools/gsbmschema/classifier.go`](../../tools/gsbmschema/classifier.go))
emits one of three severities for every change in
`Schema.Structs[...].Fields[...]`:

| Severity   | When                                                                              | Custom-codec example                                                                              |
|------------|-----------------------------------------------------------------------------------|---------------------------------------------------------------------------------------------------|
| `Safe`     | New tag added; field renamed at the same tag with same shape.                     | New field with `custom=MyCodec` at a fresh tag.                                                   |
| `Warning`  | Wire-shape annotation added; readers without the new annotation will misdecode.   | `field/custom-added` — adding `custom=X` to a previously-untagged active field (`classifier.go:355`). |
| `Breaking` | Live tag removed, repurposed, or wire-shape changed under it.                     | `field/custom-removed`, `field/custom-changed` (`classifier.go:359`, `classifier.go:363`).        |

`Breaking` blocks the PR unless every breaking change carries
`//gsbm:allow-breaking "<justification>"` on its containing type
(`parse.go:161`); the classifier records the acknowledgement on the
change. The classifier cannot inspect codec function bodies — a body
change under an unchanged name produces no diagnostic.

## Width override (`type=`) changes

The `bin:"N,type=<width>"` option ([`docs/spec.md`](../spec.md) §5.9)
is a per-field wire-range contract: it overrides the default wire
width the encoder enforces on the field's value and the decoder
accepts off the wire. The eight legal width lexemes are
`int8/16/32/64` and `uint8/16/32/64`; the override sign MUST match the
Go type's sign and the override width MUST NOT exceed the Go type's
width (with one platform-sized exception — `int`/`uint`/`uintptr` may
opt up to the 64-bit width, since their default wire range is the
portable 32 bits). The override is recorded on the schema snapshot
(`wireOverride` on the field entry) and the classifier emits one of
three diff codes — `field/wire-widened`, `field/wire-narrowed`,
`field/wire-intent-changed` — depending on the numeric width
comparison between the old and new effective wire widths. These codes
fire on every integer kind, signed or unsigned, including named
aliases that resolve to a basic integer underlying type.

**Widening — effective wire width grows (`field/wire-widened`).**
Examples: un-annotated `int` (or `type=int32`) → `type=int64`;
un-annotated `uint` → `type=uint64`; `int64 type=int16` → `int64
type=int32`. Forward-compatible for any reader at the new schema: the
decoder accepts the wider range and reads any value the writer
produced. (On a 32-bit host the platform-sized cases — `int`, `uint`,
`uintptr` — still guard the assignment with `math.MinInt`/`math.MaxInt`
and surface `ErrIntegerOverflow` for values outside the platform
range; graceful rejection, never silent truncation.) For a reader at
the old schema, values inside the old (narrower) range decode
correctly; values outside surface `ErrIntegerOverflow` and the field
is rejected — graceful rejection, not silent corruption. Sequence the
rollout so all readers that must see out-of-range values have the
widened schema deployed before any writer starts emitting them.

**Narrowing — effective wire width shrinks
(`field/wire-narrowed`).** Examples: `type=int64` → `type=int32` (or
removing a widened override); `int64 type=int32` → `int64 type=int16`;
`uint64` (no override) → `uint64 type=uint16`. Breaking for any stored
data that already carries values outside the narrower range — the new
schema will reject those blobs at decode with `ErrIntegerOverflow`.
Treat the change like any other breaking wire-shape change and gate
it behind `//gsbm:allow-breaking "<justification>"`. Do not narrow
without first proving (by audit or by sampling stored blobs) that no
historical value exceeds the target width.

**Intent change at the same effective width
(`field/wire-intent-changed`).** Examples: un-annotated `int32` ↔
`int32 type=int32` (identity marker on/off); `int` ↔ `int type=int32`
(both encode at int32-bounded width). Byte-identical on the wire —
the override and the un-annotated form emit the same bytes — so
toggling between them does not affect stored blobs. The snapshot
still records the change because the schema fingerprint reflects the
author's intent; the classifier treats it as a documentation-level
change with no wire consequence.

**Rejected at validate (no classifier event).** Some `(Go type,
override)` pairs are illegal up-front and never reach the snapshot:
cross-sign overrides (`int32 type=uint32`), widening on a fixed-width
Go type (`int32 type=int64`), overrides on float or non-numeric Go
types. Each is rejected with diagnostic `tag/type-width-mismatch` —
see [`diagnostics.md`](diagnostics.md).

## Body compression (the flags compression-method enum) is opt-in

Body compression is a framing-layer flag, not a codec-shape choice,
but it changes the on-disk bytes in the same way a `type=` width
override does — old readers see the new bytes and refuse them rather
than misdecoding. Bits 0-2 of the header `flags` byte
([`docs/spec.md`](../spec.md) §2.1) carry a compression-method enum
(0 none / 1 zstd / 2 gzip); a reader that predates a method sees its
flags byte as a reserved-bit violation and rejects with
`ErrReservedFlags` — graceful rejection, never silent corruption.
Turning compression on for a blob (or for a column of stored blobs), or
switching the codec, is therefore wire-affecting in the same operational
sense as `field/wire-widened`: any consumer at a reader build older than
the codec being written will refuse the new bytes, so the rollout must
sequence the reader deploy ahead of the writer flip.

This bites hardest now that **gzip is the default codec**: a writer that
opts into compression emits `flags = 0x02`, which a v0.0.5 (zstd-only)
reader rejects. Upgrade readers to v0.0.6 before writers, or pin
`Compression: CompressionZstd` (`flags = 0x01`) during a mixed-version
rollout. `Marshal(v, schemaHint)` (no opts) and
`MarshalWithOptions(v, schemaHint, Options{})` continue to emit
`flags = 0`, byte-identical to the pre-compression output; the flip
happens only when a compressing codec is selected via `Options`. There is
no classifier code for this transition because the choice is per-call,
not per-schema, and the schema snapshot has no view into which call sites
request compression. See [`compression.md`](compression.md) for the full
reader/writer contract, the codec tradeoff, and the ratio numbers.

## Migration patterns

**Introducing a custom codec on an existing field.** This is
`field/custom-added` at `SeverityWarning` (`classifier.go:355`). Old
readers decoded the field with the default codec; new writers emit
the custom shape. Sequence:

1. Mark the existing field `deprecated, compat_write` and add a new
   field at a fresh tag carrying `custom=NewCodec`.
2. Dual-write: encoders emit both tags. New readers prefer the new
   tag; old readers see the deprecated tag they already know.
3. After the rollback window has baked, run `gsbmschema` with
   `AllowStopCompatWrite` (`runner.go:54`) to retire the dual-write
   and drop `compat_write`. Drop the old tag only after a further
   bake; until then it remains `deprecated` and the wire stays the
   same.

**Migrating a custom codec to a new body shape.** Same dual-write
pattern, but with two codec names:

1. Register `MyCodecV2` alongside `MyCodec`. The two coexist in the
   registry; nothing on the wire references both yet.
2. Add a new field at a fresh tag with `custom=MyCodecV2`. Mark the
   old field `deprecated, compat_write` so encoders dual-emit.
3. After the bake, retire `compat_write` and remove the old field
   under `//gsbm:allow-breaking`. The old codec stays registered
   until no stored blob still references its tag.

**What not to do.** Edit the body of `MyCodec` in place. The
snapshot flags nothing; historic blobs hold the old bytes; the
symptom surfaces days later as a `bodyLen` mismatch or — worst case
— a silently-wrong value at a downstream consumer.
