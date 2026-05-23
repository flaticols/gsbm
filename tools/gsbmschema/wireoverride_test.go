package gsbmschema

import (
	"go/types"
	"strings"
	"testing"
)

// TestWireOverrideCompat exercises the contract table from the
// 20260523-issue-50 plan. The helper is the single source of truth used
// by discover (validation) and emit (effective wire width); these tests
// pin the behavior both callers depend on.
func TestWireOverrideCompat(t *testing.T) {
	// Build a small package with one named-alias declaration so we can
	// exercise the *types.Named -> underlying basic walk without parsing
	// real source. `type UserID int64` is the canonical case from the
	// plan's contract table.
	userID := types.NewNamed(
		types.NewTypeName(0, types.NewPackage("p", "p"), "UserID", nil),
		types.Typ[types.Int64],
		nil,
	)
	// And a named-int kind for the int-aliasing case (`type Quantity int`).
	quantity := types.NewNamed(
		types.NewTypeName(0, types.NewPackage("p", "p"), "Quantity", nil),
		types.Typ[types.Int],
		nil,
	)
	// A non-integer named type (`type Title string`) — must still reject.
	titleStr := types.NewNamed(
		types.NewTypeName(0, types.NewPackage("p", "p"), "Title", nil),
		types.Typ[types.String],
		nil,
	)
	// A struct underlying — must reject for any override.
	structT := types.NewStruct(nil, nil)
	namedStruct := types.NewNamed(
		types.NewTypeName(0, types.NewPackage("p", "p"), "Foo", nil),
		structT,
		nil,
	)

	type legalCase struct {
		name           string
		t              types.Type
		override       string
		wantBits       int
		wantSigned     bool
	}
	legal := []legalCase{
		// Defaults — no override.
		{"int-default", types.Typ[types.Int], "", 32, true},
		{"uint-default", types.Typ[types.Uint], "", 32, false},
		{"uintptr-default", types.Typ[types.Uintptr], "", 32, false},
		{"int8-default", types.Typ[types.Int8], "", 8, true},
		{"int16-default", types.Typ[types.Int16], "", 16, true},
		{"int32-default", types.Typ[types.Int32], "", 32, true},
		{"int64-default", types.Typ[types.Int64], "", 64, true},
		{"uint8-default", types.Typ[types.Uint8], "", 8, false},
		{"uint16-default", types.Typ[types.Uint16], "", 16, false},
		{"uint32-default", types.Typ[types.Uint32], "", 32, false},
		{"uint64-default", types.Typ[types.Uint64], "", 64, false},

		// Platform-sized identity + widening (the PR #51 cases plus the
		// symmetric uint fix).
		{"int-type=int32", types.Typ[types.Int], "int32", 32, true},
		{"int-type=int64", types.Typ[types.Int], "int64", 64, true},
		{"uint-type=uint32", types.Typ[types.Uint], "uint32", 32, false},
		{"uint-type=uint64", types.Typ[types.Uint], "uint64", 64, false},
		{"uintptr-type=uint32", types.Typ[types.Uintptr], "uint32", 32, false},
		{"uintptr-type=uint64", types.Typ[types.Uintptr], "uint64", 64, false},

		// Fixed-width narrowing.
		{"int64-type=int32", types.Typ[types.Int64], "int32", 32, true},
		{"int64-type=int16", types.Typ[types.Int64], "int16", 16, true},
		{"int64-type=int8", types.Typ[types.Int64], "int8", 8, true},
		{"int32-type=int16", types.Typ[types.Int32], "int16", 16, true},
		{"int32-type=int8", types.Typ[types.Int32], "int8", 8, true},
		{"int16-type=int8", types.Typ[types.Int16], "int8", 8, true},
		{"uint64-type=uint32", types.Typ[types.Uint64], "uint32", 32, false},
		{"uint64-type=uint16", types.Typ[types.Uint64], "uint16", 16, false},
		{"uint64-type=uint8", types.Typ[types.Uint64], "uint8", 8, false},
		{"uint32-type=uint16", types.Typ[types.Uint32], "uint16", 16, false},
		{"uint32-type=uint8", types.Typ[types.Uint32], "uint8", 8, false},
		{"uint16-type=uint8", types.Typ[types.Uint16], "uint8", 8, false},

		// Identity on fixed-width — documentation marker, byte-identical
		// to the un-annotated field.
		{"int8-identity", types.Typ[types.Int8], "int8", 8, true},
		{"int16-identity", types.Typ[types.Int16], "int16", 16, true},
		{"int32-identity", types.Typ[types.Int32], "int32", 32, true},
		{"int64-identity", types.Typ[types.Int64], "int64", 64, true},
		{"uint8-identity", types.Typ[types.Uint8], "uint8", 8, false},
		{"uint16-identity", types.Typ[types.Uint16], "uint16", 16, false},
		{"uint32-identity", types.Typ[types.Uint32], "uint32", 32, false},
		{"uint64-identity", types.Typ[types.Uint64], "uint64", 64, false},

		// Named alias resolves through the underlying basic.
		{"UserID-default", userID, "", 64, true},
		{"UserID-type=int32", userID, "int32", 32, true},
		{"UserID-type=int64-identity", userID, "int64", 64, true},
		{"Quantity-default", quantity, "", 32, true},
		{"Quantity-type=int64", quantity, "int64", 64, true},
		{"Quantity-type=int32-identity", quantity, "int32", 32, true},
	}

	for _, c := range legal {
		t.Run("legal/"+c.name, func(t *testing.T) {
			bits, signed, ok, reason := WireOverrideCompat(c.t, c.override)
			if !ok {
				t.Fatalf("expected legal, got ok=false reason=%q", reason)
			}
			if bits != c.wantBits || signed != c.wantSigned {
				t.Fatalf("got (bits=%d, signed=%v), want (bits=%d, signed=%v)", bits, signed, c.wantBits, c.wantSigned)
			}
			if reason != "" {
				t.Fatalf("legal case has reason=%q, want empty", reason)
			}
		})
	}

	type illegalCase struct {
		name      string
		t         types.Type
		override  string
		reasonHas string
	}
	illegal := []illegalCase{
		// Redundant widening on fixed-width kinds — the Go type already
		// bounds tighter than the override, so the override emits dead
		// code and creates two ways to spell the same wire shape.
		{"int32-type=int64-redundant", types.Typ[types.Int32], "int64", "wider than"},
		{"int16-type=int64-redundant", types.Typ[types.Int16], "int64", "wider than"},
		{"int16-type=int32-redundant", types.Typ[types.Int16], "int32", "wider than"},
		{"int8-type=int16-redundant", types.Typ[types.Int8], "int16", "wider than"},
		{"uint32-type=uint64-redundant", types.Typ[types.Uint32], "uint64", "wider than"},
		{"uint16-type=uint64-redundant", types.Typ[types.Uint16], "uint64", "wider than"},
		{"uint8-type=uint16-redundant", types.Typ[types.Uint8], "uint16", "wider than"},

		// Cross-sign: int+uint or uint+int override always rejects, even
		// at identity widths.
		{"int32-type=uint32-crosssign", types.Typ[types.Int32], "uint32", "unsigned but Go type"},
		{"int64-type=uint64-crosssign", types.Typ[types.Int64], "uint64", "unsigned but Go type"},
		{"uint32-type=int32-crosssign", types.Typ[types.Uint32], "int32", "signed but Go type"},
		{"uint64-type=int64-crosssign", types.Typ[types.Uint64], "int64", "signed but Go type"},
		{"int-type=uint32-crosssign", types.Typ[types.Int], "uint32", "unsigned but Go type"},
		{"int-type=uint64-crosssign", types.Typ[types.Int], "uint64", "unsigned but Go type"},
		{"uint-type=int32-crosssign", types.Typ[types.Uint], "int32", "signed but Go type"},
		{"uint-type=int64-crosssign", types.Typ[types.Uint], "int64", "signed but Go type"},

		// Platform-sized narrowing below 32 bits — the user should just
		// declare the field as the narrower fixed-width kind.
		{"int-type=int8-narrow", types.Typ[types.Int], "int8", "narrow below the default"},
		{"int-type=int16-narrow", types.Typ[types.Int], "int16", "narrow below the default"},
		{"uint-type=uint8-narrow", types.Typ[types.Uint], "uint8", "narrow below the default"},
		{"uint-type=uint16-narrow", types.Typ[types.Uint], "uint16", "narrow below the default"},
		{"uintptr-type=uint8-narrow", types.Typ[types.Uintptr], "uint8", "narrow below the default"},

		// Non-integer Go types with an override — string, float, struct,
		// named-string-alias all reject.
		{"string-type=int32", types.Typ[types.String], "int32", "only valid on integer"},
		{"float32-type=int32", types.Typ[types.Float32], "int32", "only valid on integer"},
		{"float64-type=int64", types.Typ[types.Float64], "int64", "only valid on integer"},
		{"bool-type=int8", types.Typ[types.Bool], "int8", "only valid on integer"},
		{"NamedString-type=int32", titleStr, "int32", "only valid on integer"},
		{"NamedStruct-type=int32", namedStruct, "int32", "only valid on integer"},

		// Defensive: unknown override lexeme on an integer kind (parser
		// guards against this, but callers can construct FieldDecl
		// programmatically).
		{"int-type=int24-unknown", types.Typ[types.Int], "int24", "not a recognized width"},
		{"int32-type=mystery-unknown", types.Typ[types.Int32], "mystery", "not a recognized width"},
	}

	for _, c := range illegal {
		t.Run("illegal/"+c.name, func(t *testing.T) {
			bits, signed, ok, reason := WireOverrideCompat(c.t, c.override)
			if ok {
				t.Fatalf("expected illegal, got ok=true bits=%d signed=%v", bits, signed)
			}
			if reason == "" {
				t.Fatalf("illegal case returned empty reason")
			}
			if !strings.Contains(reason, c.reasonHas) {
				t.Fatalf("reason %q does not contain %q", reason, c.reasonHas)
			}
		})
	}

	// Nil Go type is rejected with a stable reason — defensive coverage
	// since downstream callers always pass f.Type(), but the helper is
	// public and callable from tests.
	t.Run("nil-type", func(t *testing.T) {
		_, _, ok, reason := WireOverrideCompat(nil, "")
		if ok {
			t.Fatalf("expected ok=false for nil type")
		}
		if !strings.Contains(reason, "nil") {
			t.Fatalf("reason %q does not mention nil", reason)
		}
	})
}
