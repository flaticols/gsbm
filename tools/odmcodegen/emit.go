package odmcodegen

import (
	"fmt"
	"go/types"
	"io"
	"sort"

	"github.com/flaticols/gsbm/tools/odmschema"
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
	decl *odmschema.FieldDecl
	gov  *types.Var
}

// activeFields returns the (schema, *types.Var) pairs of fields the
// codegen actually needs to encode/decode. Skipped (`bin:"-"`) fields are
// already absent from the schema; deprecated fields stay in the schema
// for read-compat but MUST NOT be encoded.
func activeFields(str *types.Struct, sd *odmschema.StructDecl) []fieldEntry {
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
		return "odm.WireLengthDelim"
	}
	switch tt := t.(type) {
	case *types.Basic:
		switch tt.Kind() {
		case types.Float32:
			return "odm.WireFixed32"
		case types.Float64:
			return "odm.WireFixed64"
		case types.String:
			return "odm.WireLengthDelim"
		}
		return "odm.WireVarint"
	case *types.Named:
		return "odm.WireLengthDelim"
	case *types.Slice, *types.Array, *types.Map:
		return "odm.WireLengthDelim"
	}
	return "odm.WireLengthDelim"
}

// emitReset writes `func (v *T) Reset()`. The body is capacity-preserving:
// slices truncate to length 0 (cap retained for the next decode pass);
// maps go through clear (Go 1.21+) so backing buckets stay; pointers go
// to nil; required nested structs recurse via their own Reset; primitive
// fields are zeroed so a tag missing from the next blob lands as zero.
//
// Order: inner Reset before outer truncation, per the plan, so the inner
// struct sees a fully-formed receiver before the slice header collapses.
func (e *emitter) emitReset(out io.Writer, named *types.Named, str *types.Struct, sd *odmschema.StructDecl) error {
	name := named.Obj().Name()
	fp(out, "func (v *%s) Reset() {\n", name)
	for _, f := range activeFields(str, sd) {
		expr := "v." + f.decl.Name
		if err := e.emitFieldReset(out, expr, f.gov.Type()); err != nil {
			return fmt.Errorf("%s.%s: %w", name, f.decl.Name, err)
		}
	}
	fp(out, "}\n")
	return nil
}

func (e *emitter) emitFieldReset(out io.Writer, expr string, t types.Type) error {
	switch tt := t.(type) {
	case *types.Pointer:
		// Drop the pointee; a pooled root keeps the parent struct, not its
		// nullable children, since the decoder always allocates fresh.
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
		// clear preserves the map's bucket allocation; the decoder will
		// repopulate.
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

// emitMarshal writes `func (v *T) MarshalODM(w *odm.Writer) error { ... }`.
func (e *emitter) emitMarshal(out io.Writer, named *types.Named, str *types.Struct, sd *odmschema.StructDecl) error {
	name := named.Obj().Name()
	fp(out, "func (v *%s) MarshalODM(w *odm.Writer) error {\n", name)
	for _, f := range activeFields(str, sd) {
		if f.decl.Deprecated {
			// Deprecated fields are read-only; never emit on the wire.
			fp(out, "\t// tag %d %s: deprecated, not written\n", f.decl.Tag, f.decl.Name)
			continue
		}
		fp(out, "\t// tag %d %s\n", f.decl.Tag, f.decl.Name)
		if err := e.emitFieldEncode(out, f); err != nil {
			return fmt.Errorf("%s.%s: %w", name, f.decl.Name, err)
		}
	}
	fp(out, "\treturn w.Err()\n}\n")
	return nil
}

// emitUnmarshal writes `func (v *T) UnmarshalODM(r *odm.Reader) error`.
func (e *emitter) emitUnmarshal(out io.Writer, named *types.Named, str *types.Struct, sd *odmschema.StructDecl) error {
	name := named.Obj().Name()
	fp(out, "func (v *%s) UnmarshalODM(r *odm.Reader) error {\n", name)
	fp(out, "\tfor r.HasMore() {\n")
	fp(out, "\t\ttag, wt, err := r.ReadTag()\n")
	fp(out, "\t\tif err != nil { return err }\n")
	fp(out, "\t\tswitch tag {\n")
	for _, f := range activeFields(str, sd) {
		fp(out, "\t\tcase %d:\n", f.decl.Tag)
		if err := e.emitFieldDecode(out, f); err != nil {
			return fmt.Errorf("%s.%s: %w", name, f.decl.Name, err)
		}
	}
	fp(out, "\t\tdefault:\n")
	fp(out, "\t\t\tif err := r.SkipField(wt); err != nil { return err }\n")
	fp(out, "\t\t}\n") // switch
	fp(out, "\t}\n") // for
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
				fp(out, "\t\t\tif err := %s.MarshalODM(w); err != nil { return err }\n", expr)
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
			fp(out, "\t\tif err := %s.MarshalODM(w); err != nil { return err }\n", expr)
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
	case types.Int, types.Int8, types.Int16, types.Int32, types.Int64:
		fp(out, "\tw.WriteVarint(int64(%s))\n", expr)
	case types.Uint, types.Uint8, types.Uint16, types.Uint32, types.Uint64, types.Uintptr:
		fp(out, "\tw.WriteUvarint(uint64(%s))\n", expr)
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
			fp(out, "\t\t\tif err := %s.MarshalODM(w); err != nil { return err }\n", elemExpr)
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
	fp(out, "\t{\n")
	fp(out, "\t\tm := w.BeginLengthDelim()\n")
	fp(out, "\t\tw.WriteUvarint(uint64(len(%s)))\n", expr)
	fp(out, "\t\tfor k, vv := range %s {\n", expr)
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
	fp(out, "\t\t\tsaved, err := r.BeginLengthDelim()\n")
	fp(out, "\t\t\tif err != nil { return err }\n")
	allow := isBuiltinPrimitive(elem)
	fp(out, "\t\t\tstate, err := r.ReadPresenceByte(%v)\n", allow)
	fp(out, "\t\t\tif err != nil { return err }\n")
	fp(out, "\t\t\tswitch state {\n")
	fp(out, "\t\t\tcase odm.PresenceNil:\n")
	fp(out, "\t\t\t\t%s = nil\n", expr)
	if allow {
		fp(out, "\t\t\tcase odm.PresenceZero:\n")
		fp(out, "\t\t\t\tz := %s\n", zeroValue(elem))
		fp(out, "\t\t\t\t%s = &z\n", expr)
	}
	fp(out, "\t\t\tcase odm.PresenceNonZero:\n")
	if named, ok := elem.(*types.Named); ok {
		if _, isStruct := named.Underlying().(*types.Struct); isStruct {
			fp(out, "\t\t\t\t%s = &%s{}\n", expr, e.typeExpr(named))
			fp(out, "\t\t\t\tif err := %s.UnmarshalODM(r); err != nil { return err }\n", expr)
			fp(out, "\t\t\t}\n")
			fp(out, "\t\t\tif err := r.EndLengthDelim(saved); err != nil { return err }\n")
			return nil
		}
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
			fp(out, "\t\t\tif err := %s.UnmarshalODM(r); err != nil { return err }\n", expr)
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
	case types.Int, types.Int8, types.Int16, types.Int32, types.Int64:
		fp(out, "\t\t\t\tx, err := r.ReadVarint()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\t%s = %s(x)\n", lhs, b.Name())
	case types.Uint, types.Uint8, types.Uint16, types.Uint32, types.Uint64, types.Uintptr:
		fp(out, "\t\t\t\tx, err := r.ReadUvarint()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\t%s = %s(x)\n", lhs, b.Name())
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
	fp(out, "\t\t\tsaved, err := r.BeginLengthDelim()\n")
	fp(out, "\t\t\tif err != nil { return err }\n")
	fp(out, "\t\t\tn, err := r.ReadLength()\n")
	fp(out, "\t\t\tif err != nil { return err }\n")
	fp(out, "\t\t\tif n > 0 {\n")
	fp(out, "\t\t\t\tif cap(%s) >= n { %s = %s[:n] } else { %s = odm.MakeSlice[%s](r, n) }\n",
		expr, expr, expr, expr, elemTypeStr)
	fp(out, "\t\t\t}\n")
	fp(out, "\t\t\tfor i := 0; i < n; i++ {\n")
	if named, ok := elemT.(*types.Named); ok {
		if _, isStruct := named.Underlying().(*types.Struct); isStruct {
			fp(out, "\t\t\t\tinner, err := r.BeginLengthDelim()\n")
			fp(out, "\t\t\t\tif err != nil { return err }\n")
			// Reset rather than zero-assign: when DecodeInto reuses the
			// slice, the existing element may carry nested slice/map
			// capacity that Reset preserves but `T{}` would discard.
			fp(out, "\t\t\t\t%s[i].Reset()\n", expr)
			fp(out, "\t\t\t\tif err := %s[i].UnmarshalODM(r); err != nil { return err }\n", expr)
			fp(out, "\t\t\t\tif err := r.EndLengthDelim(inner); err != nil { return err }\n")
			fp(out, "\t\t\t}\n")
			fp(out, "\t\t\tif err := r.EndLengthDelim(saved); err != nil { return err }\n")
			return nil
		}
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
	fp(out, "\t\t\tsaved, err := r.BeginLengthDelim()\n")
	fp(out, "\t\t\tif err != nil { return err }\n")
	fp(out, "\t\t\tn, err := r.ReadLength()\n")
	fp(out, "\t\t\tif err != nil { return err }\n")
	fp(out, "\t\t\tif n > 0 && %s == nil { %s = odm.MakeMap[%s, %s](r, n) }\n",
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
			fp(out, "\t\t\t\tif err := vv.UnmarshalODM(r); err != nil { return err }\n")
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
	fp(out, "\t\t\t\t%s[k] = vv\n", expr)
	fp(out, "\t\t\t}\n")
	fp(out, "\t\t\tif err := r.EndLengthDelim(saved); err != nil { return err }\n")
	return nil
}

// isBuiltinPrimitive is the runtime-side mirror of
// odmschema.IsBuiltinPrimitive: types whose zero value can be elided by
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

