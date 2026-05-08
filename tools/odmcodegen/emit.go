package odmcodegen

import (
	"fmt"
	"go/types"
	"io"
	"sort"

	"github.com/flaticols/gsbm/tools/odmschema"
)

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
	for i := 0; i < str.NumFields(); i++ {
		f := str.Field(i)
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

// emitMarshal writes `func (v *T) MarshalODM(w *odm.Writer) error { ... }`.
func (e *emitter) emitMarshal(out io.Writer, named *types.Named, str *types.Struct, sd *odmschema.StructDecl) error {
	name := named.Obj().Name()
	fmt.Fprintf(out, "func (v *%s) MarshalODM(w *odm.Writer) error {\n", name)
	for _, f := range activeFields(str, sd) {
		if f.decl.Deprecated {
			// Deprecated fields are read-only; never emit on the wire.
			fmt.Fprintf(out, "\t// tag %d %s: deprecated, not written\n", f.decl.Tag, f.decl.Name)
			continue
		}
		fmt.Fprintf(out, "\t// tag %d %s\n", f.decl.Tag, f.decl.Name)
		if err := e.emitFieldEncode(out, f); err != nil {
			return fmt.Errorf("%s.%s: %w", name, f.decl.Name, err)
		}
	}
	fmt.Fprintf(out, "\treturn w.Err()\n}\n")
	return nil
}

// emitUnmarshal writes `func (v *T) UnmarshalODM(r *odm.Reader) error`.
func (e *emitter) emitUnmarshal(out io.Writer, named *types.Named, str *types.Struct, sd *odmschema.StructDecl) error {
	name := named.Obj().Name()
	fmt.Fprintf(out, "func (v *%s) UnmarshalODM(r *odm.Reader) error {\n", name)
	fmt.Fprintf(out, "\tfor r.HasMore() {\n")
	fmt.Fprintf(out, "\t\ttag, wt, err := r.ReadTag()\n")
	fmt.Fprintf(out, "\t\tif err != nil { return err }\n")
	fmt.Fprintf(out, "\t\tswitch tag {\n")
	for _, f := range activeFields(str, sd) {
		fmt.Fprintf(out, "\t\tcase %d:\n", f.decl.Tag)
		if err := e.emitFieldDecode(out, f); err != nil {
			return fmt.Errorf("%s.%s: %w", name, f.decl.Name, err)
		}
	}
	fmt.Fprintf(out, "\t\tdefault:\n")
	fmt.Fprintf(out, "\t\t\tif err := r.SkipField(wt); err != nil { return err }\n")
	fmt.Fprintf(out, "\t\t}\n") // switch
	fmt.Fprintf(out, "\t}\n") // for
	fmt.Fprintf(out, "\treturn r.Err()\n}\n")
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
	fmt.Fprintf(out, "\tw.WriteTag(%d, %s)\n", tag, wt)
	return e.emitValueEncode(out, expr, t, true)
}

// emitOptionalEncode wraps the field in the standard optional layout:
// WireLengthDelim outer key, length-delim body containing presence byte
// and (when present) the value. WireLengthDelim is required so older
// readers can safely SkipField past an unknown optional tag.
func (e *emitter) emitOptionalEncode(out io.Writer, tag uint32, wt, expr string, elem types.Type) error {
	fmt.Fprintf(out, "\tw.WriteTag(%d, %s)\n", tag, wt)
	fmt.Fprintf(out, "\t{\n")
	fmt.Fprintf(out, "\t\tm := w.BeginLengthDelim()\n")
	if isBuiltinPrimitive(elem) {
		zeroExpr := zeroValue(elem)
		fmt.Fprintf(out, "\t\tswitch {\n")
		fmt.Fprintf(out, "\t\tcase %s == nil:\n", expr)
		fmt.Fprintf(out, "\t\t\tw.WritePresenceNil()\n")
		fmt.Fprintf(out, "\t\tcase *%s == %s:\n", expr, zeroExpr)
		fmt.Fprintf(out, "\t\t\tw.WritePresenceZero()\n")
		fmt.Fprintf(out, "\t\tdefault:\n")
		fmt.Fprintf(out, "\t\t\tw.WritePresenceNonZero()\n")
		if err := e.emitPrimitiveEncode(out, "*"+expr, elem); err != nil {
			return err
		}
		fmt.Fprintf(out, "\t\t}\n")
	} else {
		// Non-builtin optional: zero-elide is forbidden by the spec, so
		// only Nil / NonZero are emitted on the wire.
		fmt.Fprintf(out, "\t\tif %s == nil {\n", expr)
		fmt.Fprintf(out, "\t\t\tw.WritePresenceNil()\n")
		fmt.Fprintf(out, "\t\t} else {\n")
		fmt.Fprintf(out, "\t\t\tw.WritePresenceNonZero()\n")
		// Inline the named-struct body directly inside the outer length-
		// delim — no second wrapper. The decoder uses HasMore against the
		// outer bound to read fields to the end.
		switch et := elem.(type) {
		case *types.Named:
			if _, ok := et.Underlying().(*types.Struct); ok {
				fmt.Fprintf(out, "\t\t\tif err := %s.MarshalODM(w); err != nil { return err }\n", expr)
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
		fmt.Fprintf(out, "\t\t}\n")
	}
	fmt.Fprintf(out, "\t\tw.EndLengthDelim(m)\n")
	fmt.Fprintf(out, "\t}\n")
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
			fmt.Fprintf(out, "\t{\n")
			fmt.Fprintf(out, "\t\tm := w.BeginLengthDelim()\n")
			fmt.Fprintf(out, "\t\tif err := %s.MarshalODM(w); err != nil { return err }\n", expr)
			fmt.Fprintf(out, "\t\tw.EndLengthDelim(m)\n")
			fmt.Fprintf(out, "\t}\n")
			return nil
		}
		// Defined-but-not-struct named type (e.g. type ID string): unwrap
		// to its underlying primitive for encoding.
		return e.emitValueEncode(out, fmt.Sprintf("(%s)(%s)", e.typeExpr(tt.Underlying()), expr), tt.Underlying(), false)
	case *types.Slice:
		if isByteType(tt.Elem()) {
			fmt.Fprintf(out, "\tw.WriteBytes(%s)\n", expr)
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
		fmt.Fprintf(out, "\tw.WriteBool(%s)\n", expr)
	case types.String:
		fmt.Fprintf(out, "\tw.WriteString(%s)\n", expr)
	case types.Int, types.Int8, types.Int16, types.Int32, types.Int64:
		fmt.Fprintf(out, "\tw.WriteVarint(int64(%s))\n", expr)
	case types.Uint, types.Uint8, types.Uint16, types.Uint32, types.Uint64, types.Uintptr:
		fmt.Fprintf(out, "\tw.WriteUvarint(uint64(%s))\n", expr)
	case types.Float32:
		fmt.Fprintf(out, "\tw.WriteFloat32(%s)\n", expr)
	case types.Float64:
		fmt.Fprintf(out, "\tw.WriteFloat64(%s)\n", expr)
	default:
		return fmt.Errorf("unsupported basic kind %v", b.Kind())
	}
	return nil
}

func (e *emitter) emitSliceEncode(out io.Writer, expr string, t *types.Slice) error {
	elemT := t.Elem()
	fmt.Fprintf(out, "\t{\n")
	fmt.Fprintf(out, "\t\tm := w.BeginLengthDelim()\n")
	fmt.Fprintf(out, "\t\tw.WriteUvarint(uint64(len(%s)))\n", expr)
	fmt.Fprintf(out, "\t\tfor i := range %s {\n", expr)
	elemExpr := fmt.Sprintf("%s[i]", expr)
	if named, ok := elemT.(*types.Named); ok {
		if _, ok := named.Underlying().(*types.Struct); ok {
			fmt.Fprintf(out, "\t\t\tinner := w.BeginLengthDelim()\n")
			fmt.Fprintf(out, "\t\t\tif err := %s.MarshalODM(w); err != nil { return err }\n", elemExpr)
			fmt.Fprintf(out, "\t\t\tw.EndLengthDelim(inner)\n")
			fmt.Fprintf(out, "\t\t}\n")
			fmt.Fprintf(out, "\t\tw.EndLengthDelim(m)\n")
			fmt.Fprintf(out, "\t}\n")
			return nil
		}
	}
	if err := e.emitValueEncode(out, elemExpr, elemT, false); err != nil {
		return err
	}
	fmt.Fprintf(out, "\t\t}\n")
	fmt.Fprintf(out, "\t\tw.EndLengthDelim(m)\n")
	fmt.Fprintf(out, "\t}\n")
	return nil
}

func (e *emitter) emitMapEncode(out io.Writer, expr string, t *types.Map) error {
	if !isPrimitiveKey(t.Key()) {
		return fmt.Errorf("map key must be primitive or string")
	}
	fmt.Fprintf(out, "\t{\n")
	fmt.Fprintf(out, "\t\tm := w.BeginLengthDelim()\n")
	fmt.Fprintf(out, "\t\tw.WriteUvarint(uint64(len(%s)))\n", expr)
	fmt.Fprintf(out, "\t\tfor k, vv := range %s {\n", expr)
	if err := e.emitPrimitiveEncode(out, "k", t.Key()); err != nil {
		return err
	}
	if err := e.emitValueEncode(out, "vv", t.Elem(), false); err != nil {
		return err
	}
	fmt.Fprintf(out, "\t\t}\n")
	fmt.Fprintf(out, "\t\tw.EndLengthDelim(m)\n")
	fmt.Fprintf(out, "\t}\n")
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
	fmt.Fprintf(out, "\t\t\tsaved, err := r.BeginLengthDelim()\n")
	fmt.Fprintf(out, "\t\t\tif err != nil { return err }\n")
	allow := isBuiltinPrimitive(elem)
	fmt.Fprintf(out, "\t\t\tstate, err := r.ReadPresenceByte(%v)\n", allow)
	fmt.Fprintf(out, "\t\t\tif err != nil { return err }\n")
	fmt.Fprintf(out, "\t\t\tswitch state {\n")
	fmt.Fprintf(out, "\t\t\tcase odm.PresenceNil:\n")
	fmt.Fprintf(out, "\t\t\t\t%s = nil\n", expr)
	if allow {
		fmt.Fprintf(out, "\t\t\tcase odm.PresenceZero:\n")
		fmt.Fprintf(out, "\t\t\t\tz := %s\n", zeroValue(elem))
		fmt.Fprintf(out, "\t\t\t\t%s = &z\n", expr)
	}
	fmt.Fprintf(out, "\t\t\tcase odm.PresenceNonZero:\n")
	if named, ok := elem.(*types.Named); ok {
		if _, isStruct := named.Underlying().(*types.Struct); isStruct {
			fmt.Fprintf(out, "\t\t\t\t%s = &%s{}\n", expr, e.typeExpr(named))
			fmt.Fprintf(out, "\t\t\t\tif err := %s.UnmarshalODM(r); err != nil { return err }\n", expr)
			fmt.Fprintf(out, "\t\t\t}\n")
			fmt.Fprintf(out, "\t\t\tif err := r.EndLengthDelim(saved); err != nil { return err }\n")
			return nil
		}
	}
	// Builtin primitive present-non-zero: decode into a temporary, then
	// take its address.
	fmt.Fprintf(out, "\t\t\t\tvar tmp %s\n", e.typeExpr(elem))
	if err := e.emitPrimitiveDecodeAssign(out, "tmp", elem); err != nil {
		return err
	}
	fmt.Fprintf(out, "\t\t\t\t%s = &tmp\n", expr)
	fmt.Fprintf(out, "\t\t\t}\n")
	fmt.Fprintf(out, "\t\t\tif err := r.EndLengthDelim(saved); err != nil { return err }\n")
	return nil
}

func (e *emitter) emitValueDecode(out io.Writer, expr string, t types.Type) error {
	switch tt := t.(type) {
	case *types.Basic:
		return e.emitPrimitiveDecodeAssign(out, expr, tt)
	case *types.Named:
		if _, ok := tt.Underlying().(*types.Struct); ok {
			fmt.Fprintf(out, "\t\t\tsaved, err := r.BeginLengthDelim()\n")
			fmt.Fprintf(out, "\t\t\tif err != nil { return err }\n")
			fmt.Fprintf(out, "\t\t\tif err := %s.UnmarshalODM(r); err != nil { return err }\n", expr)
			fmt.Fprintf(out, "\t\t\tif err := r.EndLengthDelim(saved); err != nil { return err }\n")
			return nil
		}
		// Named-not-struct: decode underlying primitive then convert.
		fmt.Fprintf(out, "\t\t\tvar tmp %s\n", e.typeExpr(tt.Underlying()))
		if err := e.emitPrimitiveDecodeAssign(out, "tmp", tt.Underlying()); err != nil {
			return err
		}
		fmt.Fprintf(out, "\t\t\t%s = %s(tmp)\n", expr, e.typeExpr(tt))
		return nil
	case *types.Slice:
		if isByteType(tt.Elem()) {
			fmt.Fprintf(out, "\t\t\tb, err := r.ReadBytes()\n")
			fmt.Fprintf(out, "\t\t\tif err != nil { return err }\n")
			fmt.Fprintf(out, "\t\t\t%s = append(%s[:0], b...)\n", expr, expr)
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
	fmt.Fprintf(out, "\t\t\t{\n")
	switch b.Kind() {
	case types.Bool:
		fmt.Fprintf(out, "\t\t\t\tx, err := r.ReadBool()\n")
		fmt.Fprintf(out, "\t\t\t\tif err != nil { return err }\n")
		fmt.Fprintf(out, "\t\t\t\t%s = x\n", lhs)
	case types.String:
		fmt.Fprintf(out, "\t\t\t\tx, err := r.ReadString()\n")
		fmt.Fprintf(out, "\t\t\t\tif err != nil { return err }\n")
		fmt.Fprintf(out, "\t\t\t\t%s = x\n", lhs)
	case types.Int, types.Int8, types.Int16, types.Int32, types.Int64:
		fmt.Fprintf(out, "\t\t\t\tx, err := r.ReadVarint()\n")
		fmt.Fprintf(out, "\t\t\t\tif err != nil { return err }\n")
		fmt.Fprintf(out, "\t\t\t\t%s = %s(x)\n", lhs, b.Name())
	case types.Uint, types.Uint8, types.Uint16, types.Uint32, types.Uint64, types.Uintptr:
		fmt.Fprintf(out, "\t\t\t\tx, err := r.ReadUvarint()\n")
		fmt.Fprintf(out, "\t\t\t\tif err != nil { return err }\n")
		fmt.Fprintf(out, "\t\t\t\t%s = %s(x)\n", lhs, b.Name())
	case types.Float32:
		fmt.Fprintf(out, "\t\t\t\tx, err := r.ReadFloat32()\n")
		fmt.Fprintf(out, "\t\t\t\tif err != nil { return err }\n")
		fmt.Fprintf(out, "\t\t\t\t%s = x\n", lhs)
	case types.Float64:
		fmt.Fprintf(out, "\t\t\t\tx, err := r.ReadFloat64()\n")
		fmt.Fprintf(out, "\t\t\t\tif err != nil { return err }\n")
		fmt.Fprintf(out, "\t\t\t\t%s = x\n", lhs)
	default:
		return fmt.Errorf("unsupported decode kind %v", b.Kind())
	}
	fmt.Fprintf(out, "\t\t\t}\n")
	return nil
}

func (e *emitter) emitSliceDecode(out io.Writer, expr string, t *types.Slice) error {
	elemT := t.Elem()
	elemTypeStr := e.typeExpr(elemT)
	fmt.Fprintf(out, "\t\t\tsaved, err := r.BeginLengthDelim()\n")
	fmt.Fprintf(out, "\t\t\tif err != nil { return err }\n")
	fmt.Fprintf(out, "\t\t\tn, err := r.ReadLength()\n")
	fmt.Fprintf(out, "\t\t\tif err != nil { return err }\n")
	fmt.Fprintf(out, "\t\t\tif n > 0 {\n")
	fmt.Fprintf(out, "\t\t\t\tif cap(%s) >= n { %s = %s[:n] } else { %s = make([]%s, n) }\n",
		expr, expr, expr, expr, elemTypeStr)
	fmt.Fprintf(out, "\t\t\t}\n")
	fmt.Fprintf(out, "\t\t\tfor i := 0; i < n; i++ {\n")
	if named, ok := elemT.(*types.Named); ok {
		if _, isStruct := named.Underlying().(*types.Struct); isStruct {
			fmt.Fprintf(out, "\t\t\t\tinner, err := r.BeginLengthDelim()\n")
			fmt.Fprintf(out, "\t\t\t\tif err != nil { return err }\n")
			fmt.Fprintf(out, "\t\t\t\t%s[i] = %s{}\n", expr, e.typeExpr(named))
			fmt.Fprintf(out, "\t\t\t\tif err := %s[i].UnmarshalODM(r); err != nil { return err }\n", expr)
			fmt.Fprintf(out, "\t\t\t\tif err := r.EndLengthDelim(inner); err != nil { return err }\n")
			fmt.Fprintf(out, "\t\t\t}\n")
			fmt.Fprintf(out, "\t\t\tif err := r.EndLengthDelim(saved); err != nil { return err }\n")
			return nil
		}
	}
	// Primitive element.
	if err := e.emitPrimitiveDecodeAssign(out, fmt.Sprintf("%s[i]", expr), elemT); err != nil {
		return err
	}
	fmt.Fprintf(out, "\t\t\t}\n")
	fmt.Fprintf(out, "\t\t\tif err := r.EndLengthDelim(saved); err != nil { return err }\n")
	return nil
}

func (e *emitter) emitMapDecode(out io.Writer, expr string, t *types.Map) error {
	if !isPrimitiveKey(t.Key()) {
		return fmt.Errorf("map key must be primitive or string")
	}
	keyTypeStr := e.typeExpr(t.Key())
	valTypeStr := e.typeExpr(t.Elem())
	fmt.Fprintf(out, "\t\t\tsaved, err := r.BeginLengthDelim()\n")
	fmt.Fprintf(out, "\t\t\tif err != nil { return err }\n")
	fmt.Fprintf(out, "\t\t\tn, err := r.ReadLength()\n")
	fmt.Fprintf(out, "\t\t\tif err != nil { return err }\n")
	fmt.Fprintf(out, "\t\t\tif n > 0 && %s == nil { %s = make(map[%s]%s, n) }\n",
		expr, expr, keyTypeStr, valTypeStr)
	fmt.Fprintf(out, "\t\t\tfor i := 0; i < n; i++ {\n")
	fmt.Fprintf(out, "\t\t\t\tvar k %s\n", keyTypeStr)
	if err := e.emitPrimitiveDecodeAssign(out, "k", t.Key()); err != nil {
		return err
	}
	fmt.Fprintf(out, "\t\t\t\tvar vv %s\n", valTypeStr)
	if named, ok := t.Elem().(*types.Named); ok {
		if _, isStruct := named.Underlying().(*types.Struct); isStruct {
			fmt.Fprintf(out, "\t\t\t\tinner, err := r.BeginLengthDelim()\n")
			fmt.Fprintf(out, "\t\t\t\tif err != nil { return err }\n")
			fmt.Fprintf(out, "\t\t\t\tif err := vv.UnmarshalODM(r); err != nil { return err }\n")
			fmt.Fprintf(out, "\t\t\t\tif err := r.EndLengthDelim(inner); err != nil { return err }\n")
		} else {
			// Named-not-struct (e.g. type MyID string) decodes via the
			// underlying primitive and converts into the declared type.
			fmt.Fprintf(out, "\t\t\t\t{\n")
			fmt.Fprintf(out, "\t\t\t\t\tvar tmp %s\n", e.typeExpr(named.Underlying()))
			if err := e.emitPrimitiveDecodeAssign(out, "tmp", named.Underlying()); err != nil {
				return err
			}
			fmt.Fprintf(out, "\t\t\t\t\tvv = %s(tmp)\n", e.typeExpr(named))
			fmt.Fprintf(out, "\t\t\t\t}\n")
		}
	} else {
		if err := e.emitPrimitiveDecodeAssign(out, "vv", t.Elem()); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "\t\t\t\t%s[k] = vv\n", expr)
	fmt.Fprintf(out, "\t\t\t}\n")
	fmt.Fprintf(out, "\t\t\tif err := r.EndLengthDelim(saved); err != nil { return err }\n")
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

