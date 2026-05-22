package gsbmschema

import (
	"fmt"
	"go/types"
	"reflect"
)

// LookupIDRefField locates the `bin:"1"` field of the named struct that
// ptr points at, returning that field's *types.Var, Go type, and the
// parsed `type=int32|int64` wire-width override (empty when the target
// field omits the override). The ID field's type must be an integer,
// string, or []byte (a named type wrapping one of those is also
// accepted); other kinds are rejected because the spec's ID-reference
// wire shape (spec §5.7) only enumerates those variants.
//
// Callers must ensure ptr.Elem() is a named struct (discover enforces
// `tag/bad-id-ref` for non-pointer-to-struct id_ref fields). The helper
// is shared between discover (which uses it to validate cross-package
// access, set the resolved wire type on the field's snapshot, and fold
// the target's width override into the snapshot identity so opaque-
// target flips surface in CI) and codegen (which uses it to emit the
// value-access expression and to thread the width override down to the
// primitive emitter so a referencing field honors the target's widened
// wire range).
func LookupIDRefField(ptr *types.Pointer) (*types.Var, types.Type, string, error) {
	named, ok := ptr.Elem().(*types.Named)
	if !ok {
		return nil, nil, "", fmt.Errorf("idref/missing-id-tag: id_ref target is not a named struct")
	}
	str, ok := named.Underlying().(*types.Struct)
	if !ok {
		return nil, nil, "", fmt.Errorf("idref/missing-id-tag: id_ref target %s underlying is not a struct", named.Obj().Name())
	}
	// Keep scanning past parse errors on unrelated fields and only
	// surface one when no valid bin:"1" field is found. Returning on
	// the first parse error would (a) reject opaque targets whose
	// malformed tag lives on a non-ID field even though the real ID
	// field is fine, and (b) duplicate the `tag/parse` issue that
	// discover already records for non-opaque targets. We still need
	// to flag the malformed tag if it might have been the intended
	// bin:"1" field — i.e., when no other valid tag-1 exists — because
	// for opaque targets discover does not walk into the struct.
	var firstParseErr error
	for i := 0; i < str.NumFields(); i++ {
		f := str.Field(i)
		ft, err := ParseFieldTag(reflect.StructTag(str.Tag(i)))
		if err != nil {
			if firstParseErr == nil {
				firstParseErr = fmt.Errorf("idref/missing-id-tag: id_ref target %s.%s has malformed bin tag: %w", named.Obj().Name(), f.Name(), err)
			}
			continue
		}
		if !ft.Set || ft.Skip {
			continue
		}
		if ft.Tag != 1 {
			continue
		}
		if !isIDRefFieldType(f.Type()) {
			return nil, nil, "", fmt.Errorf("idref/missing-id-tag: id_ref target %s.%s at bin:\"1\" must be an integer, string, or []byte (got %s)", named.Obj().Name(), f.Name(), f.Type().String())
		}
		return f, f.Type(), ft.WireOverride, nil
	}
	if firstParseErr != nil {
		return nil, nil, "", firstParseErr
	}
	return nil, nil, "", fmt.Errorf("idref/missing-id-tag: id_ref target %s has no bin:\"1\" field", named.Obj().Name())
}

func isIDRefFieldType(t types.Type) bool {
	switch tt := t.(type) {
	case *types.Basic:
		return isBasicIDRefKind(tt)
	case *types.Named:
		return isIDRefFieldType(tt.Underlying())
	case *types.Slice:
		elem, ok := tt.Elem().(*types.Basic)
		return ok && (elem.Kind() == types.Byte || elem.Kind() == types.Uint8)
	}
	return false
}

func isBasicIDRefKind(b *types.Basic) bool {
	switch b.Kind() {
	case types.Int, types.Int8, types.Int16, types.Int32, types.Int64,
		types.Uint, types.Uint8, types.Uint16, types.Uint32, types.Uint64,
		types.Uintptr, types.String:
		return true
	}
	return false
}
