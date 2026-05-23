package gsbmschema

import (
	"fmt"
	"go/ast"
	"reflect"
	"strconv"
	"strings"
)

// FieldTag is the parsed form of a `bin:"…"` struct-tag value.
type FieldTag struct {
	// Skip is true for `bin:"-"` — the field is intentionally excluded
	// from the closure (handwritten or never-encoded state).
	Skip bool
	// Tag is the numeric field tag in [1, 2^29-1]. Zero is invalid.
	Tag uint32
	// Deprecated is true for `bin:"N,deprecated"` — the field is kept
	// for read compatibility but never written by the encoder.
	Deprecated bool
	// CompatWrite is true for `bin:"N,deprecated,compat_write"` — the
	// encoder MUST still emit the field during the rollback window so a
	// rollback to old code does not see business data disappear. Only
	// valid in combination with Deprecated.
	CompatWrite bool
	// Custom is the optional `,custom=Foo` component, naming a custom
	// marshaler. Carried through for the classifier's warning bucket.
	Custom string
	// CycleBreakViaID is true for `bin:"N,id_ref"` — the new preferred
	// alias of the legacy //gsbm:cycle_break_via_id comment marker. Both
	// forms set the same FieldDecl.CycleBreak flag downstream so the
	// codegen and validator behavior is identical.
	CycleBreakViaID bool
	// WireOverride is the optional `,type=<width>` component carrying the
	// per-field wire-range contract. Legal width lexemes are the eight
	// signed/unsigned integer widths {int8, int16, int32, int64, uint8,
	// uint16, uint32, uint64}; empty means default (platform-sized Go
	// `int`/`uint`/`uintptr` → 32-bit-bounded varint, fixed-width Go ints
	// → own width). Compatibility with the field's Go type is enforced by
	// gsbmschema.WireOverrideCompat at validate time: signs must match,
	// the override must not exceed the Go type's width (so `int32
	// type=int64` is rejected as redundant), and float/string fields
	// reject the option entirely. Mismatches surface as
	// `tag/type-width-mismatch` Issues.
	WireOverride string
	// Set distinguishes "no bin tag at all" from "bin:\"-\"".
	Set bool
}

// ParseFieldTag parses the value of the `bin` struct tag.
//
//	bin:"5"                  → Tag=5
//	bin:"5,deprecated"       → Tag=5, Deprecated=true
//	bin:"5,custom=PriceCodec"→ Tag=5, Custom="PriceCodec"
//	bin:"5,id_ref"           → Tag=5, CycleBreakViaID=true
//	bin:"5,type=int64"       → Tag=5, WireOverride="int64"
//	bin:"-"                  → Skip=true
//	(no tag)                 → Set=false
func ParseFieldTag(tag reflect.StructTag) (FieldTag, error) {
	raw, ok := tag.Lookup("bin")
	if !ok {
		return FieldTag{}, nil
	}
	ft := FieldTag{Set: true}
	if raw == "-" {
		ft.Skip = true
		return ft, nil
	}
	parts := strings.Split(raw, ",")
	if len(parts) == 0 || parts[0] == "" {
		return ft, fmt.Errorf("bin tag is empty")
	}
	n, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return ft, fmt.Errorf("bin tag %q: %w", parts[0], err)
	}
	if n == 0 {
		return ft, fmt.Errorf("bin tag 0 is reserved")
	}
	if n > (1<<29)-1 {
		return ft, fmt.Errorf("bin tag %d exceeds 2^29-1", n)
	}
	ft.Tag = uint32(n)
	for _, p := range parts[1:] {
		switch {
		case p == "deprecated":
			if ft.Deprecated {
				return ft, fmt.Errorf("bin tag option %q repeated", p)
			}
			ft.Deprecated = true
		case p == "compat_write":
			if ft.CompatWrite {
				return ft, fmt.Errorf("bin tag option %q repeated", p)
			}
			ft.CompatWrite = true
		case p == "id_ref":
			if ft.CycleBreakViaID {
				return ft, fmt.Errorf("bin tag option %q repeated", p)
			}
			ft.CycleBreakViaID = true
		case strings.HasPrefix(p, "custom="):
			if ft.Custom != "" {
				return ft, fmt.Errorf("bin tag option %q: custom marshaler already set to %q", p, ft.Custom)
			}
			ft.Custom = strings.TrimPrefix(p, "custom=")
			if ft.Custom == "" {
				return ft, fmt.Errorf("bin tag option %q: custom marshaler name is empty", p)
			}
		case strings.HasPrefix(p, "type="):
			if ft.WireOverride != "" {
				return ft, fmt.Errorf("bin tag option %q: wire-type override already set to %q", p, ft.WireOverride)
			}
			width := strings.TrimPrefix(p, "type=")
			switch width {
			case "":
				return ft, fmt.Errorf("bin tag option %q: wire-type width is empty", p)
			case "int8", "int16", "int32", "int64",
				"uint8", "uint16", "uint32", "uint64":
				ft.WireOverride = width
			default:
				return ft, fmt.Errorf("bin tag option %q: wire-type width %q not recognized (legal values: int8, int16, int32, int64, uint8, uint16, uint32, uint64)", p, width)
			}
		default:
			return ft, fmt.Errorf("bin tag option %q not recognized", p)
		}
	}
	if ft.CompatWrite && !ft.Deprecated {
		return ft, fmt.Errorf("bin tag option \"compat_write\" requires \"deprecated\"")
	}
	// id_ref and custom= are mutually exclusive: id_ref encodes a leaf
	// reference to the target's bin:"1" field, while custom=Name routes the
	// whole field through a user-supplied codec. Allowing both silently
	// produces a snapshot that records the field as both cycle-break and
	// custom (hash/classifier disagreement) while codegen emits only the
	// id_ref path; reject the combination at parse time so the user picks
	// one shape explicitly.
	if ft.CycleBreakViaID && ft.Custom != "" {
		return ft, fmt.Errorf("bin tag options \"id_ref\" and \"custom=%s\" are mutually exclusive", ft.Custom)
	}
	// type= overrides the int wire shape; custom=Name routes the whole field
	// through a user-supplied codec that picks its own wire shape. Combining
	// the two leaves the override unenforceable (the custom codec runs, the
	// override is silently ignored). Reject at parse time so the user picks
	// one shape explicitly.
	if ft.WireOverride != "" && ft.Custom != "" {
		return ft, fmt.Errorf("bin tag options \"type=%s\" and \"custom=%s\" are mutually exclusive", ft.WireOverride, ft.Custom)
	}
	// type= widens a leaf int's wire range; id_ref encodes a leaf reference
	// to the target's bin:"1" field on a pointer-to-struct target. The two
	// describe incompatible wire shapes for the same field, and id_ref's
	// pointer-to-struct requirement makes the basic-int gate downstream
	// reject the combination with a confusing tag/type-width-mismatch
	// diagnostic blaming the Go type. Reject at parse time so the error
	// names the real conflict.
	if ft.WireOverride != "" && ft.CycleBreakViaID {
		return ft, fmt.Errorf("bin tag options \"type=%s\" and \"id_ref\" are mutually exclusive", ft.WireOverride)
	}
	return ft, nil
}

// markers is the parsed form of an //gsbm:* comment block above a
// declaration (struct type or field).
type markers struct {
	root            bool
	opaque          bool
	cycleBreakViaID bool
	trackPresence   bool
	borrowStrings   bool
	reserved        []uint32
	allowBreaking   string // justification text after the directive
}

// parseMarkers walks a *ast.CommentGroup looking for //gsbm:* directives.
// Unknown //gsbm:* directives are reported as errors so misspellings don't
// silently degrade to "no marker".
func parseMarkers(cg *ast.CommentGroup) (markers, error) {
	var m markers
	if cg == nil {
		return m, nil
	}
	for _, c := range cg.List {
		// Strip the leading // or /* */ and any whitespace.
		line := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(c.Text, "//"), "/*"))
		line = strings.TrimSuffix(line, "*/")
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "gsbm:") {
			continue
		}
		body := strings.TrimPrefix(line, "gsbm:")
		// Split into directive name and optional argument tail.
		name, arg, _ := strings.Cut(body, " ")
		name = strings.TrimSpace(name)
		arg = strings.TrimSpace(arg)
		switch name {
		case "root":
			m.root = true
		case "opaque":
			m.opaque = true
		case "cycle_break_via_id":
			m.cycleBreakViaID = true
		case "reserved":
			tags, err := parseReservedList(arg)
			if err != nil {
				return m, fmt.Errorf("//gsbm:reserved: %w", err)
			}
			m.reserved = append(m.reserved, tags...)
		case "allow-breaking":
			if arg == "" {
				return m, fmt.Errorf("//gsbm:allow-breaking requires a justification")
			}
			m.allowBreaking = arg
		case "presence":
			// Reserved for a future opt-in toggle of decode-side
			// presence tracking. Accepted as a no-op so existing
			// handwritten schemas may start using the marker today;
			// the codegen ignores it.
		case "track-presence":
			// Opt the struct into stored decode-side presence: codegen
			// emits a hidden `gsbmPresent [N]uint64` field so post-decode
			// FieldPresent(tag) reflects which tags appeared on the wire.
			// Wire-format unchanged; rejected on //gsbm:opaque types.
			m.trackPresence = true
		case "borrow-strings":
			// Unsafe opt-in for generated heap decoders: string fields in
			// this struct may alias the caller-owned decode blob instead of
			// copying. Wire-format unchanged; rejected on //gsbm:opaque types
			// and when misplaced on fields.
			m.borrowStrings = true
		default:
			return m, fmt.Errorf("unknown //gsbm: directive %q", name)
		}
	}
	return m, nil
}

func parseReservedList(arg string) ([]uint32, error) {
	if arg == "" {
		return nil, fmt.Errorf("expected at least one tag")
	}
	parts := strings.Split(arg, ",")
	out := make([]uint32, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		n, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("tag %q: %w", p, err)
		}
		if n == 0 || n > (1<<29)-1 {
			return nil, fmt.Errorf("tag %d out of range", n)
		}
		out = append(out, uint32(n))
	}
	return out, nil
}
