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
	// WireOverride is the optional `,type=int32|int64` component. Only
	// the literal values "int32" and "int64" are legal; empty means the
	// default mapping (Go int → int32-bounded varint). The override is
	// only meaningful on Go `int` fields — the schema validator rejects
	// it on any other Go type with a `tag/type-width-mismatch` Issue.
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
			case "int32", "int64":
				ft.WireOverride = width
			default:
				return ft, fmt.Errorf("bin tag option %q: wire-type width %q not recognized (legal values: int32, int64)", p, width)
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
	return ft, nil
}

// markers is the parsed form of an //gsbm:* comment block above a
// declaration (struct type or field).
type markers struct {
	root            bool
	opaque          bool
	cycleBreakViaID bool
	trackPresence   bool
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
