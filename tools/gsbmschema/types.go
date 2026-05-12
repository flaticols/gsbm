// Package gsbmschema discovers and validates the type closure that the
// gsbm codegen emits encoders for. It is a build-time tool — it has no
// runtime dependency on the storage/gsbm package and never reflects on
// values. The schema is derived from handwritten Go source: structs with
// `bin:"N"` tags reachable from types marked `//gsbm:root`.
//
// The package owns three artifacts:
//
//   - The in-memory Schema graph (this file).
//   - schema.yaml / schema_snapshot.json snapshots written next to the
//     source tree (snapshot.go).
//   - A diff classifier that labels schema changes safe / warning /
//     breaking against an append-only policy (classifier.go).
//
// fmtVer is a wire-format version (frozen at 1 for the foreseeable future)
// and is unrelated to schemaHint, which is a weak grouping hint over the
// structural shape of the schema and is surfaced in the blob header for
// observability only — decoders MUST NOT branch on it.
package gsbmschema

// FmtVer is the wire-format version this schema tooling emits for. The
// header byte at offset 4 of every blob carries this exact value; bumping
// it requires a coordinated wire-format change, not a schema change.
const FmtVer uint8 = 1

// Schema is the closed graph of struct types reachable from one or more
// //gsbm:root markers, plus the metadata the codegen and classifier need
// to act on the graph.
//
// The shape is JSON / YAML serializable; field ordering inside Structs
// and inside each StructDecl.Fields is normalized at construction time so
// snapshot files have stable byte content (and so the schemaHint hash is
// stable across runs).
type Schema struct {
	FmtVer     uint8         `json:"fmtVer" yaml:"fmtVer"`
	SchemaHint uint16        `json:"schemaHint" yaml:"schemaHint"`
	Roots      []TypeRef     `json:"roots" yaml:"roots"`
	Structs    []*StructDecl `json:"structs" yaml:"structs"`
}

// TypeRef identifies a named type by its import path and identifier. For
// generic instantiations we keep the parameters too so two distinct
// instantiations don't collide on (PkgPath, Name). Underlying is
// populated for named slice aliases (`type ItemList []Item`) and records
// the slice's element type so the classifier can distinguish a rename
// (same Underlying) from a wire-affecting change (different Underlying).
type TypeRef struct {
	PkgPath    string    `json:"pkgPath" yaml:"pkgPath"`
	Name       string    `json:"name" yaml:"name"`
	TypeArgs   []TypeRef `json:"typeArgs,omitempty" yaml:"typeArgs,omitempty"`
	Underlying *TypeRef  `json:"underlying,omitempty" yaml:"underlying,omitempty"`
}

// StructDecl describes one struct in the closure: its identity, fields
// (sorted by tag), reserved tag set, generic type-parameter names if any,
// and whether the schema author opted the type out of validation via
// //gsbm:opaque.
type StructDecl struct {
	Type     TypeRef      `json:"type" yaml:"type"`
	Fields   []*FieldDecl `json:"fields,omitempty" yaml:"fields,omitempty"`
	Reserved []uint32     `json:"reserved,omitempty" yaml:"reserved,omitempty"`
	Opaque   bool         `json:"opaque,omitempty" yaml:"opaque,omitempty"`
	Generic  []string     `json:"generic,omitempty" yaml:"generic,omitempty"`
	// AllowBreaking carries the justification text from
	// //gsbm:allow-breaking on the struct, used by the classifier to admit
	// a breaking change with a recorded reason.
	AllowBreaking string `json:"allowBreaking,omitempty" yaml:"allowBreaking,omitempty"`
	// TrackPresence is true when the struct carries //gsbm:track-presence.
	// The codegen emits a hidden `gsbmPresent [N]uint64` field on the
	// generated companion so post-decode `FieldPresent(tag)` can observe
	// which tags appeared on the wire. Wire format is unchanged — this is
	// purely a Go-side opt-in to stored presence bits. Mutually exclusive
	// with //gsbm:opaque (the opaque codec owns its own presence shape).
	TrackPresence bool `json:"trackPresence,omitempty" yaml:"trackPresence,omitempty"`
}

// FieldDecl describes one field of a struct in the closure.
type FieldDecl struct {
	Name       string `json:"name" yaml:"name"`
	Tag        uint32 `json:"tag" yaml:"tag"`
	Type       string `json:"type" yaml:"type"`
	Wire       string `json:"wire" yaml:"wire"`
	Optional   bool   `json:"optional,omitempty" yaml:"optional,omitempty"`
	Deprecated bool   `json:"deprecated,omitempty" yaml:"deprecated,omitempty"`
	// CompatWrite is true for `bin:"N,deprecated,compat_write"` — the field
	// is being phased out, but the encoder MUST still emit it during the
	// rollback window so that a rollback to old code does not see business
	// data disappear. Only valid in combination with Deprecated.
	CompatWrite bool `json:"compatWrite,omitempty" yaml:"compatWrite,omitempty"`
	// CycleBreak is true when the field carries an //gsbm:cycle_break_via_id
	// directive, signalling that the codegen will encode an ID reference
	// rather than walk the type closure through this field.
	CycleBreak bool `json:"cycleBreak,omitempty" yaml:"cycleBreak,omitempty"`
	// MapKey / MapValue / Elem describe composite-type structure for the
	// classifier's type-equivalence check; they are also surfaced in the
	// human-readable yaml.
	MapKey   string `json:"mapKey,omitempty" yaml:"mapKey,omitempty"`
	MapValue string `json:"mapValue,omitempty" yaml:"mapValue,omitempty"`
	Elem     string `json:"elem,omitempty" yaml:"elem,omitempty"`
	// MapKeyUnderlying captures the BasicKind name of a named map-key's
	// underlying primitive (spec §5.3 allows e.g. `type Code string` as a
	// map key). Empty for raw-primitive keys (`map[string]V`) and for
	// non-map fields. Recorded separately from MapKey so the classifier
	// can flag a change to the named key's underlying type as wire-affecting
	// even when the named type's identifier is unchanged.
	MapKeyUnderlying string `json:"mapKeyUnderlying,omitempty" yaml:"mapKeyUnderlying,omitempty"`
	// Custom is the optional `bin:"N,custom=Foo"` annotation, naming a
	// custom marshaler. Adding a custom annotation is a warning per the
	// append-only policy; removing or changing it is breaking because the
	// emitted codec body changes shape on the wire.
	Custom string `json:"custom,omitempty" yaml:"custom,omitempty"`
	// AliasType captures the structured identity of a named slice alias
	// used as the field's top-level type (e.g. `Groups ItemList` where
	// `type ItemList []Item`). The TypeRef's Name carries the alias's
	// identifier; the TypeRef's Underlying carries the slice element's
	// type. The wire-bytes-relevant shape lives in Type/Elem; AliasType
	// lets the classifier flag a rename (same Underlying) as safe rather
	// than as a field/type-changed event. nil for fields whose top type
	// is not a named slice alias.
	AliasType *TypeRef `json:"aliasType,omitempty" yaml:"aliasType,omitempty"`
	// FlattenedFrom records the chain of anonymous embedded types this
	// field was promoted through, joined with `.` (e.g. `Base` for a
	// direct embed, `Outer.Base` for a two-level embed where Outer also
	// embeds Base). Empty for fields declared directly on the struct.
	// The wire encoding is identical to a hand-flattened struct; this
	// field is metadata so the classifier can treat a refactor that
	// pushes a tag into an embedded base as safe so long as the tag,
	// type, and wire shape are preserved.
	FlattenedFrom string `json:"flattenedFrom,omitempty" yaml:"flattenedFrom,omitempty"`
	// FlattenedFromPointer is true when any embed in the FlattenedFrom
	// chain is a pointer-to-struct (e.g. `Outer{*Base}`, or `Outer{Mid}`
	// over `Mid{*Base}`). Codegen uses this to emit a nil-check on encode
	// and lazy allocation on decode for every pointer hop along the chain.
	FlattenedFromPointer bool `json:"flattenedFromPointer,omitempty" yaml:"flattenedFromPointer,omitempty"`
}

// Wire types as strings (matches storage/gsbm/wire.go constants by name).
const (
	WireVarint      = "varint"
	WireFixed64     = "fixed64"
	WireFixed32     = "fixed32"
	WireLengthDelim = "length-delim"
)
