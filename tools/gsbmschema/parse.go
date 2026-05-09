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
	// Custom is the optional `,custom=Foo` component, naming a custom
	// marshaler. Carried through for the classifier's warning bucket.
	Custom string
	// Set distinguishes "no bin tag at all" from "bin:\"-\"".
	Set bool
}

// ParseFieldTag parses the value of the `bin` struct tag.
//
//	bin:"5"                  → Tag=5
//	bin:"5,deprecated"       → Tag=5, Deprecated=true
//	bin:"5,custom=PriceCodec"→ Tag=5, Custom="PriceCodec"
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
			ft.Deprecated = true
		case strings.HasPrefix(p, "custom="):
			ft.Custom = strings.TrimPrefix(p, "custom=")
			if ft.Custom == "" {
				return ft, fmt.Errorf("bin tag option %q: custom marshaler name is empty", p)
			}
		default:
			return ft, fmt.Errorf("bin tag option %q not recognized", p)
		}
	}
	return ft, nil
}

// markers is the parsed form of an //gsbm:* comment block above a
// declaration (struct type or field).
type markers struct {
	root            bool
	opaque          bool
	cycleBreakViaID bool
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
