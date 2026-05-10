package gsbmcodegen

import (
	"fmt"
	"go/types"
	"io"
	"sort"

	"go.flaticols.dev/gsbm/tools/gsbmschema"
)

// fp wraps fmt.Fprintf, dropping the result. Codegen writes to an
// in-memory buffer that does not surface I/O errors at this layer; the
// outer caller checks io.Writer state. Wrapping the call here keeps the
// emitter sites free of `_, _ =` noise.
func fp(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// fieldEntry pairs a Schema FieldDecl with the corresponding *types.Var so
// the emitter has both the schema metadata (tag, deprecation) and the Go
// type (for codegen of value-level access).
type fieldEntry struct {
	decl *gsbmschema.FieldDecl
	gov  *types.Var
}

// writableFields returns the (schema, *types.Var) pairs of fields the
// codegen needs to encode/decode. Skipped (`bin:"-"`) fields are already
// absent from the schema. Deprecated fields stay in the schema for
// read-compat; the encoder normally MUST NOT emit them, but a deprecated
// field carrying `compat_write` is still written during the rollback
// window so a rollback to old code can still see the field's value. The
// per-field `Deprecated`/`CompatWrite` flags gate the encode side; the
// decode side is identical for both.
func writableFields(str *types.Struct, sd *gsbmschema.StructDecl) []fieldEntry {
	byName := map[string]*types.Var{}
	for f := range str.Fields() {
		byName[f.Name()] = f
	}
	out := make([]fieldEntry, 0, len(sd.Fields))
	for _, fd := range sd.Fields {
		v, ok := byName[fd.Name]
		if !ok {
			continue
		}
		out = append(out, fieldEntry{decl: fd, gov: v})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].decl.Tag < out[j].decl.Tag })
	return out
}

// wireType returns the wire type to put in the field key for f. Optional
// fields always use WireLengthDelim so older readers can SkipField past
// an unknown nullable tag without misinterpreting the presence byte.
func wireType(f fieldEntry) string {
	t := f.gov.Type()
	if _, ok := t.(*types.Pointer); ok {
		return "gsbm.WireLengthDelim"
	}
	return wireTypeForValue(t)
}

// wireTypeForValue returns the wire type for a non-pointer value type.
// Named-not-struct types unwrap to their underlying primitive — the field
// key MUST match the actual body encoding, otherwise SkipField on an
// unknown tag desyncs the parser past it.
func wireTypeForValue(t types.Type) string {
	switch tt := t.(type) {
	case *types.Basic:
		switch tt.Kind() {
		case types.Float32:
			return "gsbm.WireFixed32"
		case types.Float64:
			return "gsbm.WireFixed64"
		case types.String:
			return "gsbm.WireLengthDelim"
		}
		return "gsbm.WireVarint"
	case *types.Named:
		if _, ok := tt.Underlying().(*types.Struct); ok {
			return "gsbm.WireLengthDelim"
		}
		return wireTypeForValue(tt.Underlying())
	case *types.Slice, *types.Array, *types.Map:
		return "gsbm.WireLengthDelim"
	}
	return "gsbm.WireLengthDelim"
}

// emitReset writes `func (v *T) Reset()`. The body is capacity-preserving:
// slices truncate to length 0 (cap retained for the next decode pass);
// maps go through clear (Go 1.21+) so backing buckets stay; pointers go
// to nil; required nested structs recurse via their own Reset; primitive
// fields are zeroed so a tag missing from the next blob lands as zero.
// The trailing gsbm.ClearPresence drops the sidecar bitmap so callers
// re-querying FieldPresent after Reset see no stale presence bits.
//
// Order: inner Reset before outer truncation, per the plan, so the inner
// struct sees a fully-formed receiver before the slice header collapses.
func (e *emitter) emitReset(out io.Writer, named *types.Named, str *types.Struct, sd *gsbmschema.StructDecl) error {
	name := named.Obj().Name()
	fp(out, "func (v *%s) Reset() {\n", name)
	for _, f := range writableFields(str, sd) {
		expr := "v." + f.decl.Name
		if err := e.emitFieldReset(out, expr, f.gov.Type()); err != nil {
			return fmt.Errorf("%s.%s: %w", name, f.decl.Name, err)
		}
	}
	fp(out, "\tgsbm.ClearPresence(v)\n")
	fp(out, "}\n")
	return nil
}

// emitFieldPresent writes `func (v *T) FieldPresent(tag uint32) bool`.
// The body delegates to the package-level sidecar; the sidecar returns
// false for any tag that was never marked, including unknown tags and
// tags above gsbm.MaxTrackedTag.
func (e *emitter) emitFieldPresent(out io.Writer, named *types.Named) {
	fp(out, "func (v *%s) FieldPresent(tag uint32) bool {\n", named.Obj().Name())
	fp(out, "\treturn gsbm.IsPresent(v, tag)\n")
	fp(out, "}\n")
}

// emitForgetPresenceTree writes `func (v *T) ForgetPresenceTree()`. The
// method evicts v's sidecar entry plus the entries of every reachable
// nested struct (value fields, optional-struct pointees, slice-of-struct
// elements, and the heap-shared descendants of map-of-struct values).
// Called by the generated Reset and the optional-struct decode path
// before they nil or replace a struct pointee — without the recursive
// walk, nested struct entries inside the dropped pointee would orphan
// in the sidecar (~144 bytes per nested struct per cycle) since the
// top-level ForgetPresence only handles the pointee itself.
//
// For map-of-struct fields, the walk iterates loop-local copies of each
// map value and calls ForgetPresenceTree on them. The copy's pointer
// fields and slice headers share heap state with the map's stored copy
// (that's how the map-decode path's heap-shared queries work), so
// recursing into them evicts the right entries. ForgetPresence on the
// loop-local copy's own address is a harmless no-op (no entry there).
// Note: this method is only emitted when the parent is being torn down
// (Reset / pointee replacement); the per-decode map cleanup uses the
// stricter ForgetValuePresenceTree to keep heap-shared descendants alive.
func (e *emitter) emitForgetPresenceTree(out io.Writer, named *types.Named, str *types.Struct, sd *gsbmschema.StructDecl) {
	name := named.Obj().Name()
	fp(out, "func (v *%s) ForgetPresenceTree() {\n", name)
	for _, f := range writableFields(str, sd) {
		expr := "v." + f.decl.Name
		t := f.gov.Type()
		switch tt := t.(type) {
		case *types.Pointer:
			if n, ok := tt.Elem().(*types.Named); ok {
				if _, isStruct := n.Underlying().(*types.Struct); isStruct {
					fp(out, "\tif %s != nil { %s.ForgetPresenceTree() }\n", expr, expr)
				}
			}
		case *types.Named:
			if _, isStruct := tt.Underlying().(*types.Struct); isStruct {
				fp(out, "\t%s.ForgetPresenceTree()\n", expr)
			}
		case *types.Slice:
			if n, ok := tt.Elem().(*types.Named); ok {
				if _, isStruct := n.Underlying().(*types.Struct); isStruct {
					// Walk the full backing array, not just len. A prior decode
					// may have shrunk this slice via `s = s[:n]` while leaving
					// hidden-capacity elements with their own sidecar entries.
					// Iterating only len would skip those, and once the parent
					// is replaced (slice grow path or pointee replacement) the
					// backing array becomes unreachable and the entries orphan.
					fp(out, "\t{ all := %s[:cap(%s)]; for i := range all { all[i].ForgetPresenceTree() } }\n", expr, expr)
				}
			}
		case *types.Map:
			if n, ok := tt.Elem().(*types.Named); ok {
				if _, isStruct := n.Underlying().(*types.Struct); isStruct {
					fp(out, "\tfor _, vv := range %s { vv.ForgetPresenceTree() }\n", expr)
				}
			}
		}
	}
	fp(out, "\tgsbm.ForgetPresence(v)\n")
	fp(out, "}\n")
}

// emitForgetValuePresenceTree writes `func (v *T) ForgetValuePresenceTree()`.
// Used exclusively by the map-of-struct decode path, where `m[k] = vv`
// has already copied vv into the map. The walker forgets the temp's
// own sidecar entry plus entries for value-struct sub-fields (which live
// at &vv+offset and so are at different addresses from m[k]'s sub-fields,
// where no entry exists). It deliberately skips pointers, slices, and
// maps: those descendants share heap state with the map's copy via the
// pointer/slice header, so evicting them would invalidate FieldPresent
// queries the caller can still make through `m[k].Inner` or
// `items := m[k].Items; items[i]`.
func (e *emitter) emitForgetValuePresenceTree(out io.Writer, named *types.Named, str *types.Struct, sd *gsbmschema.StructDecl) {
	name := named.Obj().Name()
	fp(out, "func (v *%s) ForgetValuePresenceTree() {\n", name)
	for _, f := range writableFields(str, sd) {
		expr := "v." + f.decl.Name
		t := f.gov.Type()
		if named, ok := t.(*types.Named); ok {
			if _, isStruct := named.Underlying().(*types.Struct); isStruct {
				fp(out, "\t%s.ForgetValuePresenceTree()\n", expr)
			}
		}
	}
	fp(out, "\tgsbm.ForgetPresence(v)\n")
	fp(out, "}\n")
}

func (e *emitter) emitFieldReset(out io.Writer, expr string, t types.Type) error {
	switch tt := t.(type) {
	case *types.Pointer:
		// Drop the pointee; a pooled root keeps the parent struct, not its
		// nullable children, since the decoder always allocates fresh.
		// For nullable struct pointers the pointee owns its own sidecar
		// presence entry plus an entry per nested struct it contains, so
		// recurse via ForgetPresenceTree before nilling — without it the
		// nested entries orphan ~144 bytes each per decode/Reset cycle.
		if named, ok := tt.Elem().(*types.Named); ok {
			if _, isStruct := named.Underlying().(*types.Struct); isStruct {
				fp(out, "\tif %s != nil { %s.ForgetPresenceTree() }\n", expr, expr)
			}
		}
		fp(out, "\t%s = nil\n", expr)
		return nil
	case *types.Basic:
		fp(out, "\t%s = %s\n", expr, primitiveZero(tt))
		return nil
	case *types.Named:
		if _, ok := tt.Underlying().(*types.Struct); ok {
			// Recurse into the nested struct's generated Reset.
			fp(out, "\t%s.Reset()\n", expr)
			return nil
		}
		// Named-not-struct (e.g. type Label string) — zero via the
		// underlying primitive's literal, converted to the named type so
		// the assignment is type-clean.
		under, ok := tt.Underlying().(*types.Basic)
		if !ok {
			fp(out, "\t%s = %s{}\n", expr, e.typeExpr(tt))
			return nil
		}
		fp(out, "\t%s = %s(%s)\n", expr, e.typeExpr(tt), primitiveZero(under))
		return nil
	case *types.Slice:
		if isByteType(tt.Elem()) {
			fp(out, "\t%s = %s[:0]\n", expr, expr)
			return nil
		}
		// For slice-of-struct, recurse into each element's Reset before
		// collapsing the slice. The elements stay allocated within cap.
		if named, ok := tt.Elem().(*types.Named); ok {
			if _, isStruct := named.Underlying().(*types.Struct); isStruct {
				fp(out, "\tfor i := range %s { %s[i].Reset() }\n", expr, expr)
			}
		}
		fp(out, "\t%s = %s[:0]\n", expr, expr)
		return nil
	case *types.Map:
		// For map-of-struct values, evict heap-shared descendants of each
		// value before dropping the map keys: once clear runs, m[k].Inner
		// pointees and m[k].Items backings become unreachable, so their
		// sidecar entries would otherwise orphan. The per-iteration copy
		// shares heap state with the map's stored copy, so recursing
		// through it evicts the right entries. clear preserves the map's
		// bucket allocation; the decoder will repopulate.
		if n, ok := tt.Elem().(*types.Named); ok {
			if _, isStruct := n.Underlying().(*types.Struct); isStruct {
				fp(out, "\tfor _, vv := range %s { vv.ForgetPresenceTree() }\n", expr)
			}
		}
		fp(out, "\tclear(%s)\n", expr)
		return nil
	case *types.Array:
		// Fixed-size arrays: zero in place by assigning a zero value.
		fp(out, "\t%s = [%d]%s{}\n", expr, tt.Len(), e.typeExpr(tt.Elem()))
		return nil
	}
	return fmt.Errorf("unsupported reset type %T", t)
}

func primitiveZero(b *types.Basic) string {
	switch b.Kind() {
	case types.Bool:
		return "false"
	case types.String:
		return `""`
	case types.Float32, types.Float64:
		return "0"
	default:
		return "0"
	}
}

// emitMarshal writes `func (v *T) MarshalGSBM(w *gsbm.Writer) error { ... }`.
func (e *emitter) emitMarshal(out io.Writer, named *types.Named, str *types.Struct, sd *gsbmschema.StructDecl) error {
	name := named.Obj().Name()
	fp(out, "func (v *%s) MarshalGSBM(w *gsbm.Writer) error {\n", name)
	for _, f := range writableFields(str, sd) {
		if f.decl.Deprecated && !f.decl.CompatWrite {
			// Deprecated fields are read-only; never emit on the wire.
			fp(out, "\t// tag %d %s: deprecated, not written\n", f.decl.Tag, f.decl.Name)
			continue
		}
		if f.decl.CompatWrite {
			fp(out, "\t// tag %d %s: deprecated, compat_write (rollback window)\n", f.decl.Tag, f.decl.Name)
		} else {
			fp(out, "\t// tag %d %s\n", f.decl.Tag, f.decl.Name)
		}
		if err := e.emitFieldEncode(out, f); err != nil {
			return fmt.Errorf("%s.%s: %w", name, f.decl.Name, err)
		}
	}
	fp(out, "\treturn w.Err()\n}\n")
	return nil
}

// emitUnmarshal writes `func (v *T) UnmarshalGSBM(r *gsbm.Reader) error`.
// gsbm.ClearPresence is emitted at the top so a re-decode into the same
// receiver starts with an empty presence mask. After each known-tag case
// successfully decodes its value, gsbm.MarkPresent records the bit so a
// later FieldPresent call can distinguish "missing on the wire" from
// "present with the type's zero value". The default branch (unknown tag)
// does not mark presence — only declared tags are tracked.
func (e *emitter) emitUnmarshal(out io.Writer, named *types.Named, str *types.Struct, sd *gsbmschema.StructDecl) error {
	name := named.Obj().Name()
	fp(out, "func (v *%s) UnmarshalGSBM(r *gsbm.Reader) error {\n", name)
	fp(out, "\tgsbm.ClearPresence(v)\n")
	fp(out, "\tfor r.HasMore() {\n")
	fp(out, "\t\ttag, wt, err := r.ReadTag()\n")
	fp(out, "\t\tif err != nil { return err }\n")
	fp(out, "\t\tswitch tag {\n")
	for _, f := range writableFields(str, sd) {
		fp(out, "\t\tcase %d:\n", f.decl.Tag)
		// Validate the on-wire wire type matches what the schema says this
		// tag carries. The spec (§3.2) forbids skipping past a known tag
		// with the wrong wire type — mismatch is corruption, not a future
		// schema. SkipField on a known-tag mismatch can desync the parser
		// (e.g., wt=VARINT on a slice tag would consume a varint then walk
		// off into the body).
		fp(out, "\t\t\tif wt != %s { return gsbm.ErrWrongWireType }\n", wireType(f))
		if err := e.emitFieldDecode(out, f); err != nil {
			return fmt.Errorf("%s.%s: %w", name, f.decl.Name, err)
		}
		fp(out, "\t\t\tgsbm.MarkPresent(v, %d)\n", f.decl.Tag)
	}
	fp(out, "\t\tdefault:\n")
	fp(out, "\t\t\tif err := r.SkipField(wt); err != nil { return err }\n")
	fp(out, "\t\t}\n") // switch
	fp(out, "\t}\n")   // for
	fp(out, "\treturn r.Err()\n}\n")
	return nil
}

// emitFieldEncode emits the encode body for one field, key first.
func (e *emitter) emitFieldEncode(out io.Writer, f fieldEntry) error {
	tag := f.decl.Tag
	wt := wireType(f)
	expr := "v." + f.decl.Name
	t := f.gov.Type()

	if ptr, ok := t.(*types.Pointer); ok {
		return e.emitOptionalEncode(out, tag, wt, expr, ptr.Elem())
	}
	fp(out, "\tw.WriteTag(%d, %s)\n", tag, wt)
	return e.emitValueEncode(out, expr, t, true)
}

// emitOptionalEncode wraps the field in the standard optional layout:
// WireLengthDelim outer key, length-delim body containing presence byte
// and (when present) the value. WireLengthDelim is required so older
// readers can safely SkipField past an unknown optional tag.
func (e *emitter) emitOptionalEncode(out io.Writer, tag uint32, wt, expr string, elem types.Type) error {
	fp(out, "\tw.WriteTag(%d, %s)\n", tag, wt)
	fp(out, "\t{\n")
	fp(out, "\t\tm := w.BeginLengthDelim()\n")
	// *[]byte takes the builtin-primitive path (zero-elide eligible per
	// spec §5.1) but routes through WriteBytes — emitPrimitiveEncode does
	// not handle slices, and the zero check is len-based since []byte
	// equality with nil only matches a nil-slice, not an empty one.
	if s, ok := elem.(*types.Slice); ok && isByteType(s.Elem()) {
		fp(out, "\t\tswitch {\n")
		fp(out, "\t\tcase %s == nil:\n", expr)
		fp(out, "\t\t\tw.WritePresenceNil()\n")
		fp(out, "\t\tcase len(*%s) == 0:\n", expr)
		fp(out, "\t\t\tw.WritePresenceZero()\n")
		fp(out, "\t\tdefault:\n")
		fp(out, "\t\t\tw.WritePresenceNonZero()\n")
		fp(out, "\t\t\tw.WriteBytes(*%s)\n", expr)
		fp(out, "\t\t}\n")
		fp(out, "\t\tw.EndLengthDelim(m)\n")
		fp(out, "\t}\n")
		return nil
	}
	if isBuiltinPrimitive(elem) {
		zeroExpr := zeroValue(elem)
		fp(out, "\t\tswitch {\n")
		fp(out, "\t\tcase %s == nil:\n", expr)
		fp(out, "\t\t\tw.WritePresenceNil()\n")
		fp(out, "\t\tcase *%s == %s:\n", expr, zeroExpr)
		fp(out, "\t\t\tw.WritePresenceZero()\n")
		fp(out, "\t\tdefault:\n")
		fp(out, "\t\t\tw.WritePresenceNonZero()\n")
		if err := e.emitPrimitiveEncode(out, "*"+expr, elem); err != nil {
			return err
		}
		fp(out, "\t\t}\n")
	} else {
		// Non-builtin optional: zero-elide is forbidden by the spec, so
		// only Nil / NonZero are emitted on the wire.
		fp(out, "\t\tif %s == nil {\n", expr)
		fp(out, "\t\t\tw.WritePresenceNil()\n")
		fp(out, "\t\t} else {\n")
		fp(out, "\t\t\tw.WritePresenceNonZero()\n")
		// Inline the named-struct body directly inside the outer length-
		// delim — no second wrapper. The decoder uses HasMore against the
		// outer bound to read fields to the end.
		switch et := elem.(type) {
		case *types.Named:
			if _, ok := et.Underlying().(*types.Struct); ok {
				fp(out, "\t\t\tif err := %s.MarshalGSBM(w); err != nil { return err }\n", expr)
			} else {
				if err := e.emitValueEncode(out, "*"+expr, elem, false); err != nil {
					return err
				}
			}
		default:
			if err := e.emitValueEncode(out, "*"+expr, elem, false); err != nil {
				return err
			}
		}
		fp(out, "\t\t}\n")
	}
	fp(out, "\t\tw.EndLengthDelim(m)\n")
	fp(out, "\t}\n")
	return nil
}

// emitValueEncode emits encoder code for a value of type t accessed via
// expr. emitsKey indicates the key was already written by the caller.
func (e *emitter) emitValueEncode(out io.Writer, expr string, t types.Type, _ bool) error {
	switch tt := t.(type) {
	case *types.Basic:
		return e.emitPrimitiveEncode(out, expr, tt)
	case *types.Named:
		if _, ok := tt.Underlying().(*types.Struct); ok {
			fp(out, "\t{\n")
			fp(out, "\t\tm := w.BeginLengthDelim()\n")
			fp(out, "\t\tif err := %s.MarshalGSBM(w); err != nil { return err }\n", expr)
			fp(out, "\t\tw.EndLengthDelim(m)\n")
			fp(out, "\t}\n")
			return nil
		}
		// Defined-but-not-struct named type (e.g. type ID string): unwrap
		// to its underlying primitive for encoding.
		return e.emitValueEncode(out, fmt.Sprintf("(%s)(%s)", e.typeExpr(tt.Underlying()), expr), tt.Underlying(), false)
	case *types.Slice:
		if isByteType(tt.Elem()) {
			fp(out, "\tw.WriteBytes(%s)\n", expr)
			return nil
		}
		return e.emitSliceEncode(out, expr, tt)
	case *types.Map:
		return e.emitMapEncode(out, expr, tt)
	}
	return fmt.Errorf("unsupported encode type %T", t)
}

func (e *emitter) emitPrimitiveEncode(out io.Writer, expr string, t types.Type) error {
	b, ok := t.(*types.Basic)
	if !ok {
		return fmt.Errorf("emitPrimitiveEncode: %T not a basic type", t)
	}
	switch b.Kind() {
	case types.Bool:
		fp(out, "\tw.WriteBool(%s)\n", expr)
	case types.String:
		fp(out, "\tw.WriteString(%s)\n", expr)
	case types.Int8, types.Int16, types.Int32, types.Int64:
		fp(out, "\tw.WriteVarint(int64(%s))\n", expr)
	case types.Int:
		// `int` is platform-sized. Bound by 32-bit range on encode so blobs
		// are portable to a 32-bit reader (which the decoder also enforces).
		fp(out, "\tif int64(%s) < math.MinInt32 || int64(%s) > math.MaxInt32 { return gsbm.ErrIntegerOverflow }\n", expr, expr)
		fp(out, "\tw.WriteVarint(int64(%s))\n", expr)
		e.addImport("math")
	case types.Uint8, types.Uint16, types.Uint32, types.Uint64:
		fp(out, "\tw.WriteUvarint(uint64(%s))\n", expr)
	case types.Uint, types.Uintptr:
		// Platform-sized: bound to 32-bit so the wire is portable.
		fp(out, "\tif uint64(%s) > math.MaxUint32 { return gsbm.ErrIntegerOverflow }\n", expr)
		fp(out, "\tw.WriteUvarint(uint64(%s))\n", expr)
		e.addImport("math")
	case types.Float32:
		fp(out, "\tw.WriteFloat32(%s)\n", expr)
	case types.Float64:
		fp(out, "\tw.WriteFloat64(%s)\n", expr)
	default:
		return fmt.Errorf("unsupported basic kind %v", b.Kind())
	}
	return nil
}

func (e *emitter) emitSliceEncode(out io.Writer, expr string, t *types.Slice) error {
	elemT := t.Elem()
	fp(out, "\t{\n")
	fp(out, "\t\tm := w.BeginLengthDelim()\n")
	fp(out, "\t\tw.WriteUvarint(uint64(len(%s)))\n", expr)
	fp(out, "\t\tfor i := range %s {\n", expr)
	elemExpr := fmt.Sprintf("%s[i]", expr)
	if named, ok := elemT.(*types.Named); ok {
		if _, ok := named.Underlying().(*types.Struct); ok {
			fp(out, "\t\t\tinner := w.BeginLengthDelim()\n")
			fp(out, "\t\t\tif err := %s.MarshalGSBM(w); err != nil { return err }\n", elemExpr)
			fp(out, "\t\t\tw.EndLengthDelim(inner)\n")
			fp(out, "\t\t}\n")
			fp(out, "\t\tw.EndLengthDelim(m)\n")
			fp(out, "\t}\n")
			return nil
		}
	}
	if err := e.emitValueEncode(out, elemExpr, elemT, false); err != nil {
		return err
	}
	fp(out, "\t\t}\n")
	fp(out, "\t\tw.EndLengthDelim(m)\n")
	fp(out, "\t}\n")
	return nil
}

func (e *emitter) emitMapEncode(out io.Writer, expr string, t *types.Map) error {
	if !isPrimitiveKey(t.Key()) {
		return fmt.Errorf("map key must be primitive or string")
	}
	keyExpr := e.typeExpr(t.Key())
	fp(out, "\t{\n")
	fp(out, "\t\tm := w.BeginLengthDelim()\n")
	fp(out, "\t\tw.WriteUvarint(uint64(len(%s)))\n", expr)
	// Sort keys for deterministic output. Same logical map => same bytes,
	// so callers may take a stable hash of the encoded blob (audit, dedup,
	// content-addressed checkpointing).
	fp(out, "\t\tkeys := make([]%s, 0, len(%s))\n", keyExpr, expr)
	fp(out, "\t\tfor k := range %s { keys = append(keys, k) }\n", expr)
	if err := emitKeySort(out, t.Key()); err != nil {
		return err
	}
	e.addImport("sort")
	fp(out, "\t\tfor _, k := range keys {\n")
	fp(out, "\t\t\tvv := %s[k]\n", expr)
	if err := e.emitPrimitiveEncode(out, "k", t.Key()); err != nil {
		return err
	}
	if err := e.emitValueEncode(out, "vv", t.Elem(), false); err != nil {
		return err
	}
	fp(out, "\t\t}\n")
	fp(out, "\t\tw.EndLengthDelim(m)\n")
	fp(out, "\t}\n")
	return nil
}

// emitKeySort emits a sort.Slice call on `keys` using the natural ordering
// of the key's underlying primitive. Bool maps are sorted false→true.
func emitKeySort(out io.Writer, t types.Type) error {
	b, ok := t.(*types.Basic)
	if !ok {
		return fmt.Errorf("map key must be a basic type")
	}
	switch b.Kind() {
	case types.Bool:
		fp(out, "\t\tsort.Slice(keys, func(i, j int) bool { return !keys[i] && keys[j] })\n")
	case types.String:
		fp(out, "\t\tsort.Strings(keys)\n")
	case types.Int, types.Int8, types.Int16, types.Int32, types.Int64,
		types.Uint, types.Uint8, types.Uint16, types.Uint32, types.Uint64, types.Uintptr:
		fp(out, "\t\tsort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })\n")
	default:
		return fmt.Errorf("unsortable map key kind %v", b.Kind())
	}
	return nil
}

// emitFieldDecode emits the decode case body for one field.
func (e *emitter) emitFieldDecode(out io.Writer, f fieldEntry) error {
	t := f.gov.Type()
	expr := "v." + f.decl.Name
	if ptr, ok := t.(*types.Pointer); ok {
		return e.emitOptionalDecode(out, expr, ptr.Elem())
	}
	return e.emitValueDecode(out, expr, t)
}

func (e *emitter) emitOptionalDecode(out io.Writer, expr string, elem types.Type) error {
	// *[]byte is a builtin-primitive optional but the present-non-zero
	// branch reads via r.ReadBytes rather than the basic-type primitive
	// decoder; emit the full switch inline so we don't fall through to
	// emitPrimitiveDecodeAssign below (which rejects *types.Slice).
	if s, ok := elem.(*types.Slice); ok && isByteType(s.Elem()) {
		fp(out, "\t\t\tsaved, err := r.BeginLengthDelim()\n")
		fp(out, "\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\tstate, err := r.ReadPresenceByte(true)\n")
		fp(out, "\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\tswitch state {\n")
		fp(out, "\t\t\tcase gsbm.PresenceNil:\n")
		fp(out, "\t\t\t\t%s = nil\n", expr)
		fp(out, "\t\t\tcase gsbm.PresenceZero:\n")
		fp(out, "\t\t\t\tz := []byte{}\n")
		fp(out, "\t\t\t\t%s = &z\n", expr)
		fp(out, "\t\t\tcase gsbm.PresenceNonZero:\n")
		fp(out, "\t\t\t\tb, err := r.ReadBytes()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		// ReadBytes aliases the input buffer; copy so the decoded
		// *[]byte owns its bytes (heap-mode contract: callers may reuse
		// or mutate the source slice after decode). Mirrors the value
		// []byte path's append(dst[:0], b...).
		fp(out, "\t\t\t\tcp := append([]byte(nil), b...)\n")
		fp(out, "\t\t\t\t%s = &cp\n", expr)
		fp(out, "\t\t\t}\n")
		fp(out, "\t\t\tif err := r.EndLengthDelim(saved); err != nil { return err }\n")
		return nil
	}
	fp(out, "\t\t\tsaved, err := r.BeginLengthDelim()\n")
	fp(out, "\t\t\tif err != nil { return err }\n")
	allow := isBuiltinPrimitive(elem)
	fp(out, "\t\t\tstate, err := r.ReadPresenceByte(%v)\n", allow)
	fp(out, "\t\t\tif err != nil { return err }\n")
	// Nullable struct pointers carry their own sidecar presence entry plus
	// an entry per nested struct inside the pointee; the PresenceNonZero
	// branch always allocates a fresh pointee, and the PresenceNil branch
	// nils the field. Either way the prior pointee's tree of sidecar
	// entries would leak (~144 bytes each per decode under pooled root
	// reuse) without an explicit recursive eviction here.
	if named, ok := elem.(*types.Named); ok {
		if _, isStruct := named.Underlying().(*types.Struct); isStruct {
			fp(out, "\t\t\tif %s != nil { %s.ForgetPresenceTree() }\n", expr, expr)
		}
	}
	fp(out, "\t\t\tswitch state {\n")
	fp(out, "\t\t\tcase gsbm.PresenceNil:\n")
	fp(out, "\t\t\t\t%s = nil\n", expr)
	if allow {
		fp(out, "\t\t\tcase gsbm.PresenceZero:\n")
		fp(out, "\t\t\t\tz := %s\n", zeroValue(elem))
		fp(out, "\t\t\t\t%s = &z\n", expr)
	}
	fp(out, "\t\t\tcase gsbm.PresenceNonZero:\n")
	if named, ok := elem.(*types.Named); ok {
		if _, isStruct := named.Underlying().(*types.Struct); isStruct {
			fp(out, "\t\t\t\t%s = &%s{}\n", expr, e.typeExpr(named))
			fp(out, "\t\t\t\tif err := %s.UnmarshalGSBM(r); err != nil { return err }\n", expr)
			fp(out, "\t\t\t}\n")
			fp(out, "\t\t\tif err := r.EndLengthDelim(saved); err != nil { return err }\n")
			return nil
		}
		// Named-not-struct: decode underlying primitive into a temp, then
		// convert and take its address.
		fp(out, "\t\t\t\tvar u %s\n", e.typeExpr(named.Underlying()))
		if err := e.emitPrimitiveDecodeAssign(out, "u", named.Underlying()); err != nil {
			return err
		}
		fp(out, "\t\t\t\ttmp := %s(u)\n", e.typeExpr(named))
		fp(out, "\t\t\t\t%s = &tmp\n", expr)
		fp(out, "\t\t\t}\n")
		fp(out, "\t\t\tif err := r.EndLengthDelim(saved); err != nil { return err }\n")
		return nil
	}
	// Builtin primitive present-non-zero: decode into a temporary, then
	// take its address.
	fp(out, "\t\t\t\tvar tmp %s\n", e.typeExpr(elem))
	if err := e.emitPrimitiveDecodeAssign(out, "tmp", elem); err != nil {
		return err
	}
	fp(out, "\t\t\t\t%s = &tmp\n", expr)
	fp(out, "\t\t\t}\n")
	fp(out, "\t\t\tif err := r.EndLengthDelim(saved); err != nil { return err }\n")
	return nil
}

func (e *emitter) emitValueDecode(out io.Writer, expr string, t types.Type) error {
	switch tt := t.(type) {
	case *types.Basic:
		return e.emitPrimitiveDecodeAssign(out, expr, tt)
	case *types.Named:
		if _, ok := tt.Underlying().(*types.Struct); ok {
			fp(out, "\t\t\tsaved, err := r.BeginLengthDelim()\n")
			fp(out, "\t\t\tif err != nil { return err }\n")
			// Spec §3.3 last-wins: a duplicate occurrence of this tag
			// must replace the prior value, not merge into it. Reset
			// preserves nested slice/map capacity but zeroes scalar
			// subfields so an omitted-on-the-wire subfield doesn't keep
			// a stale value from the earlier occurrence.
			fp(out, "\t\t\t%s.Reset()\n", expr)
			fp(out, "\t\t\tif err := %s.UnmarshalGSBM(r); err != nil { return err }\n", expr)
			fp(out, "\t\t\tif err := r.EndLengthDelim(saved); err != nil { return err }\n")
			return nil
		}
		// Named-not-struct: decode underlying primitive then convert.
		fp(out, "\t\t\tvar tmp %s\n", e.typeExpr(tt.Underlying()))
		if err := e.emitPrimitiveDecodeAssign(out, "tmp", tt.Underlying()); err != nil {
			return err
		}
		fp(out, "\t\t\t%s = %s(tmp)\n", expr, e.typeExpr(tt))
		return nil
	case *types.Slice:
		if isByteType(tt.Elem()) {
			fp(out, "\t\t\tb, err := r.ReadBytes()\n")
			fp(out, "\t\t\tif err != nil { return err }\n")
			fp(out, "\t\t\t%s = append(%s[:0], b...)\n", expr, expr)
			return nil
		}
		return e.emitSliceDecode(out, expr, tt)
	case *types.Map:
		return e.emitMapDecode(out, expr, tt)
	}
	return fmt.Errorf("unsupported decode type %T", t)
}

func (e *emitter) emitPrimitiveDecodeAssign(out io.Writer, lhs string, t types.Type) error {
	b, ok := t.(*types.Basic)
	if !ok {
		return fmt.Errorf("emitPrimitiveDecodeAssign: %T not a basic type", t)
	}
	// Wrap each primitive decode in its own block so multiple back-to-back
	// reads in the same enclosing scope (e.g. map key + map value) don't
	// shadow each other on `x, err :=`.
	fp(out, "\t\t\t{\n")
	switch b.Kind() {
	case types.Bool:
		fp(out, "\t\t\t\tx, err := r.ReadBool()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\t%s = x\n", lhs)
	case types.String:
		fp(out, "\t\t\t\tx, err := r.ReadString()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\t%s = x\n", lhs)
	case types.Int8:
		fp(out, "\t\t\t\tx, err := r.ReadVarint()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\tif x < math.MinInt8 || x > math.MaxInt8 { return gsbm.ErrIntegerOverflow }\n")
		fp(out, "\t\t\t\t%s = int8(x)\n", lhs)
		e.addImport("math")
	case types.Int16:
		fp(out, "\t\t\t\tx, err := r.ReadVarint()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\tif x < math.MinInt16 || x > math.MaxInt16 { return gsbm.ErrIntegerOverflow }\n")
		fp(out, "\t\t\t\t%s = int16(x)\n", lhs)
		e.addImport("math")
	case types.Int32:
		fp(out, "\t\t\t\tx, err := r.ReadVarint()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\tif x < math.MinInt32 || x > math.MaxInt32 { return gsbm.ErrIntegerOverflow }\n")
		fp(out, "\t\t\t\t%s = int32(x)\n", lhs)
		e.addImport("math")
	case types.Int:
		// `int` is platform-sized (32 or 64). Bound by 32-bit range so the
		// blob round-trips between platforms; a 32-bit reader cannot accept
		// a 64-bit-only value anyway.
		fp(out, "\t\t\t\tx, err := r.ReadVarint()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\tif x < math.MinInt32 || x > math.MaxInt32 { return gsbm.ErrIntegerOverflow }\n")
		fp(out, "\t\t\t\t%s = int(x)\n", lhs)
		e.addImport("math")
	case types.Int64:
		fp(out, "\t\t\t\tx, err := r.ReadVarint()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\t%s = int64(x)\n", lhs)
	case types.Uint8:
		fp(out, "\t\t\t\tx, err := r.ReadUvarint()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\tif x > math.MaxUint8 { return gsbm.ErrIntegerOverflow }\n")
		fp(out, "\t\t\t\t%s = uint8(x)\n", lhs)
		e.addImport("math")
	case types.Uint16:
		fp(out, "\t\t\t\tx, err := r.ReadUvarint()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\tif x > math.MaxUint16 { return gsbm.ErrIntegerOverflow }\n")
		fp(out, "\t\t\t\t%s = uint16(x)\n", lhs)
		e.addImport("math")
	case types.Uint32:
		fp(out, "\t\t\t\tx, err := r.ReadUvarint()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\tif x > math.MaxUint32 { return gsbm.ErrIntegerOverflow }\n")
		fp(out, "\t\t\t\t%s = uint32(x)\n", lhs)
		e.addImport("math")
	case types.Uint:
		fp(out, "\t\t\t\tx, err := r.ReadUvarint()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\tif x > math.MaxUint32 { return gsbm.ErrIntegerOverflow }\n")
		fp(out, "\t\t\t\t%s = uint(x)\n", lhs)
		e.addImport("math")
	case types.Uint64:
		fp(out, "\t\t\t\tx, err := r.ReadUvarint()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\t%s = x\n", lhs)
	case types.Uintptr:
		// Platform-sized: bound to 32-bit so the wire is portable to a
		// 32-bit reader. Without this a 64-bit value silently truncates.
		fp(out, "\t\t\t\tx, err := r.ReadUvarint()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\tif x > math.MaxUint32 { return gsbm.ErrIntegerOverflow }\n")
		fp(out, "\t\t\t\t%s = uintptr(x)\n", lhs)
		e.addImport("math")
	case types.Float32:
		fp(out, "\t\t\t\tx, err := r.ReadFloat32()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\t%s = x\n", lhs)
	case types.Float64:
		fp(out, "\t\t\t\tx, err := r.ReadFloat64()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\t%s = x\n", lhs)
	default:
		return fmt.Errorf("unsupported decode kind %v", b.Kind())
	}
	fp(out, "\t\t\t}\n")
	return nil
}

func (e *emitter) emitSliceDecode(out io.Writer, expr string, t *types.Slice) error {
	elemT := t.Elem()
	elemTypeStr := e.typeExpr(elemT)
	isStructElem := false
	if named, ok := elemT.(*types.Named); ok {
		if _, isStruct := named.Underlying().(*types.Struct); isStruct {
			isStructElem = true
		}
	}
	fp(out, "\t\t\tsaved, err := r.BeginLengthDelim()\n")
	fp(out, "\t\t\tif err != nil { return err }\n")
	fp(out, "\t\t\tn, err := r.ReadLength()\n")
	fp(out, "\t\t\tif err != nil { return err }\n")
	// Always re-shape the slice to len=n, even when n==0: the encoder
	// always writes a count, so a wire-empty slice must replace any
	// prior content the receiver was carrying. nil[:0] is valid Go and
	// cap(nil)>=0, so no special case for n==0 is needed.
	if isStructElem {
		// Slice-of-struct: when the existing capacity is too small we
		// drop the old backing array via gsbm.MakeSlice. Each element of
		// that array carries its own sidecar entry (recorded by a prior
		// UnmarshalGSBM into this same field); without an explicit forget
		// pass the entries orphan in the package-level sidecar once the
		// old array becomes unreachable, defeating the "bounded by pool
		// size" guarantee under repeated high-water decodes.
		fp(out, "\t\t\tif cap(%s) >= n {\n", expr)
		fp(out, "\t\t\t\t%s = %s[:n]\n", expr, expr)
		fp(out, "\t\t\t} else {\n")
		fp(out, "\t\t\t\told := %s[:cap(%s)]\n", expr, expr)
		fp(out, "\t\t\t\tfor i := range old { old[i].ForgetPresenceTree() }\n")
		fp(out, "\t\t\t\t%s = gsbm.MakeSlice[%s](r, n)\n", expr, elemTypeStr)
		fp(out, "\t\t\t}\n")
	} else {
		fp(out, "\t\t\tif cap(%s) >= n { %s = %s[:n] } else { %s = gsbm.MakeSlice[%s](r, n) }\n",
			expr, expr, expr, expr, elemTypeStr)
	}
	fp(out, "\t\t\tif err := r.Err(); err != nil { return err }\n")
	fp(out, "\t\t\tfor i := 0; i < n; i++ {\n")
	if named, ok := elemT.(*types.Named); ok {
		if _, isStruct := named.Underlying().(*types.Struct); isStruct {
			fp(out, "\t\t\t\tinner, err := r.BeginLengthDelim()\n")
			fp(out, "\t\t\t\tif err != nil { return err }\n")
			// Reset rather than zero-assign: when DecodeInto reuses the
			// slice, the existing element may carry nested slice/map
			// capacity that Reset preserves but `T{}` would discard.
			fp(out, "\t\t\t\t%s[i].Reset()\n", expr)
			fp(out, "\t\t\t\tif err := %s[i].UnmarshalGSBM(r); err != nil { return err }\n", expr)
			fp(out, "\t\t\t\tif err := r.EndLengthDelim(inner); err != nil { return err }\n")
			fp(out, "\t\t\t}\n")
			fp(out, "\t\t\tif err := r.EndLengthDelim(saved); err != nil { return err }\n")
			return nil
		}
		// Named-not-struct: decode underlying primitive into a temp and
		// convert into the declared type before assignment.
		fp(out, "\t\t\t\tvar u %s\n", e.typeExpr(named.Underlying()))
		if err := e.emitPrimitiveDecodeAssign(out, "u", named.Underlying()); err != nil {
			return err
		}
		fp(out, "\t\t\t\t%s[i] = %s(u)\n", expr, e.typeExpr(named))
		fp(out, "\t\t\t}\n")
		fp(out, "\t\t\tif err := r.EndLengthDelim(saved); err != nil { return err }\n")
		return nil
	}
	// Primitive element.
	if err := e.emitPrimitiveDecodeAssign(out, fmt.Sprintf("%s[i]", expr), elemT); err != nil {
		return err
	}
	fp(out, "\t\t\t}\n")
	fp(out, "\t\t\tif err := r.EndLengthDelim(saved); err != nil { return err }\n")
	return nil
}

func (e *emitter) emitMapDecode(out io.Writer, expr string, t *types.Map) error {
	if !isPrimitiveKey(t.Key()) {
		return fmt.Errorf("map key must be primitive or string")
	}
	keyTypeStr := e.typeExpr(t.Key())
	valTypeStr := e.typeExpr(t.Elem())
	isStructValue := false
	if named, ok := t.Elem().(*types.Named); ok {
		if _, isStruct := named.Underlying().(*types.Struct); isStruct {
			isStructValue = true
		}
	}
	fp(out, "\t\t\tsaved, err := r.BeginLengthDelim()\n")
	fp(out, "\t\t\tif err != nil { return err }\n")
	fp(out, "\t\t\tn, err := r.ReadLength()\n")
	fp(out, "\t\t\tif err != nil { return err }\n")
	// Spec §3.3 last-wins: a fresh occurrence of a map field replaces the
	// entire field value, not merges into it. Without an explicit clear of
	// the receiver's existing entries (from a prior decode or duplicate-tag
	// occurrence in the same blob), keys not present in the new payload
	// would survive. For struct-valued maps the stale entries also keep
	// their descendants' sidecar presence alive; forget those first so the
	// clear() doesn't orphan them.
	if isStructValue {
		fp(out, "\t\t\tif len(%s) > 0 {\n", expr)
		fp(out, "\t\t\t\tfor _, vv := range %s { vv.ForgetPresenceTree() }\n", expr)
		fp(out, "\t\t\t\tclear(%s)\n", expr)
		fp(out, "\t\t\t}\n")
	} else {
		fp(out, "\t\t\tif len(%s) > 0 { clear(%s) }\n", expr, expr)
	}
	fp(out, "\t\t\tif n > 0 && %s == nil { %s = gsbm.MakeMap[%s, %s](r, n) }\n",
		expr, expr, keyTypeStr, valTypeStr)
	fp(out, "\t\t\tfor i := 0; i < n; i++ {\n")
	fp(out, "\t\t\t\tvar k %s\n", keyTypeStr)
	if err := e.emitPrimitiveDecodeAssign(out, "k", t.Key()); err != nil {
		return err
	}
	fp(out, "\t\t\t\tvar vv %s\n", valTypeStr)
	if named, ok := t.Elem().(*types.Named); ok {
		if _, isStruct := named.Underlying().(*types.Struct); isStruct {
			fp(out, "\t\t\t\tinner, err := r.BeginLengthDelim()\n")
			fp(out, "\t\t\t\tif err != nil { return err }\n")
			fp(out, "\t\t\t\tif err := vv.UnmarshalGSBM(r); err != nil { return err }\n")
			fp(out, "\t\t\t\tif err := r.EndLengthDelim(inner); err != nil { return err }\n")
		} else {
			// Named-not-struct (e.g. type MyID string) decodes via the
			// underlying primitive and converts into the declared type.
			fp(out, "\t\t\t\t{\n")
			fp(out, "\t\t\t\t\tvar tmp %s\n", e.typeExpr(named.Underlying()))
			if err := e.emitPrimitiveDecodeAssign(out, "tmp", named.Underlying()); err != nil {
				return err
			}
			fp(out, "\t\t\t\t\tvv = %s(tmp)\n", e.typeExpr(named))
			fp(out, "\t\t\t\t}\n")
		}
	} else {
		if err := e.emitPrimitiveDecodeAssign(out, "vv", t.Elem()); err != nil {
			return err
		}
	}
	if isStructValue {
		// Spec §3.3 last-wins for map keys: a repeated key in this same
		// payload replaces the prior entry. The existing m[k] holds a
		// copy whose heap-backed descendants (pointers, slices, maps)
		// are about to become unreachable — forget their sidecar entries
		// before the assignment drops them, otherwise duplicate-key
		// payloads leak ~entries-per-descendant per repeat.
		fp(out, "\t\t\t\tif existing, ok := %s[k]; ok { existing.ForgetPresenceTree() }\n", expr)
	}
	fp(out, "\t\t\t\t%s[k] = vv\n", expr)
	if isStructValue {
		// vv lives only for this iteration but its UnmarshalGSBM left
		// sidecar entries at &vv and at the addresses of any embedded
		// value-struct sub-fields. The map's copy lands at a different
		// memory location, so callers can never observe those entries
		// — they would orphan once vv goes out of scope. We evict only
		// the temp-local entries; heap-backed descendants (pointers,
		// slices, maps) survive the m[k]=vv copy via shared pointers,
		// so a recursive ForgetPresenceTree would drop entries the
		// caller can still query through `m[k].Ptr` or
		// `items := m[k].Items; items[i]`.
		fp(out, "\t\t\t\tvv.ForgetValuePresenceTree()\n")
	}
	fp(out, "\t\t\t}\n")
	fp(out, "\t\t\tif err := r.EndLengthDelim(saved); err != nil { return err }\n")
	return nil
}

// isBuiltinPrimitive is the runtime-side mirror of
// gsbmschema.IsBuiltinPrimitive: types whose zero value can be elided by
// the presence-byte. The schema layer answers based on the rendered type
// name; here we ask the typechecked type directly.
func isBuiltinPrimitive(t types.Type) bool {
	switch tt := t.(type) {
	case *types.Basic:
		switch tt.Kind() {
		case types.Bool, types.String,
			types.Int, types.Int8, types.Int16, types.Int32, types.Int64,
			types.Uint, types.Uint8, types.Uint16, types.Uint32, types.Uint64, types.Uintptr,
			types.Float32, types.Float64:
			return true
		}
	case *types.Slice:
		// []byte qualifies; other slice element types don't.
		return isByteType(tt.Elem())
	}
	return false
}

func isPrimitiveKey(t types.Type) bool {
	b, ok := t.(*types.Basic)
	if !ok {
		return false
	}
	switch b.Kind() {
	case types.Bool, types.String,
		types.Int, types.Int8, types.Int16, types.Int32, types.Int64,
		types.Uint, types.Uint8, types.Uint16, types.Uint32, types.Uint64, types.Uintptr:
		return true
	}
	return false
}

// zeroValue returns a Go-source expression for the zero value of t. Used
// for the present-and-zero case in optional builtin fields.
func zeroValue(t types.Type) string {
	if isByteType(elemTypeOfSlice(t)) {
		return "nil"
	}
	if b, ok := t.(*types.Basic); ok {
		switch b.Kind() {
		case types.String:
			return `""`
		case types.Bool:
			return "false"
		case types.Float32, types.Float64:
			return "0"
		default:
			return "0"
		}
	}
	return "nil"
}

func elemTypeOfSlice(t types.Type) types.Type {
	if s, ok := t.(*types.Slice); ok {
		return s.Elem()
	}
	return nil
}
