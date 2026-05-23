package gsbmschema

import (
	"fmt"
	"go/types"
	"strings"
)

// WireOverrideCompat is the single source of truth for the `bin:"N,type=W"`
// wire-width override contract. Given a field's Go type and an override
// width lexeme (`""` for no override, or one of `int8/16/32/64`,
// `uint8/16/32/64`), it reports the effective wire shape and whether the
// pair is legal.
//
// Contract (mirrors docs/spec.md §5 and the contract table in the
// 20260523-issue-50 plan):
//
//   - `int`            -> default 32-bit signed wire; legal overrides: int32, int64
//   - `uint`/`uintptr` -> default 32-bit unsigned wire; legal overrides: uint32, uint64
//   - `int8/16/32/64`  -> default own-width signed wire; legal overrides: signed widths <= own
//   - `uint8/16/32/64` -> default own-width unsigned wire; legal overrides: unsigned widths <= own
//   - named alias of any integer basic -> behaves as its underlying basic
//   - identity override (e.g. `int32 type=int32`) -> accepted as a documentation marker
//   - any int + cross-sign override (e.g. `int32 type=uint32`) -> REJECT
//   - fixed-width int + wider override (e.g. `int32 type=int64`) -> REJECT (redundant)
//   - non-integer types (string, float, struct, slice, map, ...) -> REJECT
//
// On success, returns (bits, signed, true, "") describing the effective
// wire width: when override == "", bits is the Go-type's default wire
// width; when override != "", bits is the override's width. `signed` is
// the wire sign, which always matches the Go type's sign (cross-sign
// overrides are rejected up front).
//
// On failure, returns (0, false, false, reason) where reason is a stable,
// user-facing explanation used in the `tag/type-width-mismatch` Issue.
//
// Walks *types.Named to its underlying basic so named integer aliases
// (`type UserID int64`) share the rules of their underlying kind.
func WireOverrideCompat(t types.Type, override string) (bits int, signed bool, ok bool, reason string) {
	if t == nil {
		return 0, false, false, "nil Go type"
	}
	// Walk through named-type aliases (e.g. `type UserID int64`) to the
	// underlying basic. The contract table applies to the basic kind;
	// the user-facing reason still refers to the original named type so
	// error messages name the symbol the user wrote.
	displayName := t.String()
	underlying := t.Underlying()
	basic, isBasic := underlying.(*types.Basic)
	if !isBasic {
		if override == "" {
			return 0, false, false, fmt.Sprintf("type %s is not an integer", displayName)
		}
		return 0, false, false, fmt.Sprintf("type= override %q is only valid on integer Go types (got %s)", override, displayName)
	}

	goBits, goSigned, isInt := basicIntShape(basic.Kind())
	if !isInt {
		if override == "" {
			return 0, false, false, fmt.Sprintf("type %s is not an integer", displayName)
		}
		return 0, false, false, fmt.Sprintf("type= override %q is only valid on integer Go types (got %s)", override, displayName)
	}

	// Platform-sized kinds (int, uint, uintptr) default to a portable
	// 32-bit wire width when no override is set. The override widens
	// (or identifies) that default to whichever the user declares.
	platformSized := basic.Kind() == types.Int || basic.Kind() == types.Uint || basic.Kind() == types.Uintptr

	if override == "" {
		if platformSized {
			return 32, goSigned, true, ""
		}
		return goBits, goSigned, true, ""
	}

	overrideBits, overrideSigned, overrideKnown := parseOverrideWidth(override)
	if !overrideKnown {
		// Defense in depth: the parser already rejects unknown widths, but
		// callers can construct FieldDecl programmatically.
		return 0, false, false, fmt.Sprintf("type= override %q is not a recognized width (legal: int8, int16, int32, int64, uint8, uint16, uint32, uint64)", override)
	}

	if overrideSigned != goSigned {
		return 0, false, false, fmt.Sprintf("type= override %q is %s but Go type %s is %s", override, signWord(overrideSigned), displayName, signWord(goSigned))
	}

	if platformSized {
		// `int`/`uint`/`uintptr` opt in to a specific portable width.
		// Both 32-bit (identity with the default) and 64-bit (widening
		// the default) are legal; narrower overrides like `int type=int8`
		// don't change the wire shape vs. an honest `int8` field and add
		// no contract value, so reject them.
		if overrideBits < 32 {
			return 0, false, false, fmt.Sprintf("type= override %q on platform-sized Go type %s would narrow below the default 32-bit wire width — declare the field as %s instead", override, displayName, narrowerKindName(overrideSigned, overrideBits))
		}
		return overrideBits, overrideSigned, true, ""
	}

	// Fixed-width signed/unsigned Go type: identity and narrowing are
	// legal; widening past the Go-type's own bound is redundant (the Go
	// type already restricts the value tighter than the override would)
	// and would muddy the field/wire-intent-changed classifier code.
	if overrideBits > goBits {
		return 0, false, false, fmt.Sprintf("type= override %q is wider than Go type %s — the override would be redundant", override, displayName)
	}

	return overrideBits, overrideSigned, true, ""
}

// basicIntShape returns the default wire shape for a Go basic kind, and
// whether the kind is an integer at all. Platform-sized kinds (int, uint,
// uintptr) return their machine width here; callers that want the wire
// default must apply the portable-32-bit rule themselves.
func basicIntShape(kind types.BasicKind) (bits int, signed, ok bool) {
	switch kind {
	case types.Int:
		// Machine-width signed. Wire default is 32-bit (the caller maps).
		return 64, true, true
	case types.Int8:
		return 8, true, true
	case types.Int16:
		return 16, true, true
	case types.Int32:
		return 32, true, true
	case types.Int64:
		return 64, true, true
	case types.Uint, types.Uintptr:
		return 64, false, true
	case types.Uint8:
		return 8, false, true
	case types.Uint16:
		return 16, false, true
	case types.Uint32:
		return 32, false, true
	case types.Uint64:
		return 64, false, true
	default:
		return 0, false, false
	}
}

// parseOverrideWidth decodes one of the legal `type=` width lexemes into
// (bits, signed). Returns ok=false for anything else.
func parseOverrideWidth(w string) (bits int, signed, ok bool) {
	switch w {
	case "int8":
		return 8, true, true
	case "int16":
		return 16, true, true
	case "int32":
		return 32, true, true
	case "int64":
		return 64, true, true
	case "uint8":
		return 8, false, true
	case "uint16":
		return 16, false, true
	case "uint32":
		return 32, false, true
	case "uint64":
		return 64, false, true
	default:
		return 0, false, false
	}
}

func signWord(signed bool) string {
	if signed {
		return "signed"
	}
	return "unsigned"
}

// effectiveWireWidthFromSnapshot returns the (bits, signed) wire shape
// implied by a FieldDecl's snapshot strings — `fd.Type` and
// `fd.WireOverride`. This is the snapshot-side analogue of
// WireOverrideCompat: the classifier compares two FieldDecls without
// access to *types.Type, so it derives effective widths from the
// recorded strings instead.
//
// When override is set, the override's width and sign win directly.
// When override is empty, the default is derived from `fdType`:
//   - platform-sized `int`/`uint`/`uintptr` → 32 bits (portable default,
//     matching WireOverrideCompat)
//   - fixed-width `int8/16/32/64`, `uint8/16/32/64` → own width
//   - named alias rendered as `pkg.Name(underlying)` (per shapeOf) →
//     recurse on the underlying basic
//
// Returns ok=false for non-integer or unrecognized type strings; the
// classifier skips the width-transition branch in that case rather than
// guessing.
func effectiveWireWidthFromSnapshot(fdType, override string) (bits int, signed, ok bool) {
	if override != "" {
		return parseOverrideWidth(override)
	}
	return defaultIntWidthFromTypeString(fdType)
}

// defaultIntWidthFromTypeString maps the snapshot string form of an
// integer Go type to its default wire width. Mirrors basicIntShape's
// kind→shape table plus the portable-32-bit rule for platform-sized
// kinds, and handles the `pkg.Name(underlying)` rendering shapeOf uses
// for named non-struct types.
func defaultIntWidthFromTypeString(fdType string) (bits int, signed, ok bool) {
	// id_ref snapshot shape is `<base>/id:<id shape>` (discover.go appends
	// the resolved ID type's shape so opaque-target wire-class flips
	// surface in CI). The classifier compares two id_ref FieldDecls'
	// WireOverride strings — when one side carries no override, derive
	// the default width from the id-side shape suffix, not the base type
	// (which is *pkg.Target and unparseable here).
	if i := strings.LastIndex(fdType, "/id:"); i >= 0 {
		return defaultIntWidthFromTypeString(fdType[i+len("/id:"):])
	}
	switch fdType {
	case "int":
		return 32, true, true
	case "int8":
		return 8, true, true
	case "int16":
		return 16, true, true
	case "int32":
		return 32, true, true
	case "int64":
		return 64, true, true
	case "uint", "uintptr":
		return 32, false, true
	case "uint8":
		return 8, false, true
	case "uint16":
		return 16, false, true
	case "uint32":
		return 32, false, true
	case "uint64":
		return 64, false, true
	}
	// Named non-struct alias: `pkg.Name(underlying)` per shapeOf. Recurse
	// on the inner basic so `type UserID int64` resolves like int64.
	if lp := strings.LastIndexByte(fdType, '('); lp >= 0 && strings.HasSuffix(fdType, ")") {
		inner := fdType[lp+1 : len(fdType)-1]
		return defaultIntWidthFromTypeString(inner)
	}
	return 0, false, false
}

// narrowerKindName picks the Go-level kind name a user should declare
// when they tried to narrow a platform-sized int below the portable wire
// default. Used only to render a helpful reason string.
func narrowerKindName(signed bool, bits int) string {
	if signed {
		switch bits {
		case 8:
			return "int8"
		case 16:
			return "int16"
		}
	} else {
		switch bits {
		case 8:
			return "uint8"
		case 16:
			return "uint16"
		}
	}
	return ""
}
