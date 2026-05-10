package gsbmschema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// MarshalJSON serializes a Schema to the canonical machine-readable JSON
// snapshot. Two-space indentation; deterministic key order is provided
// by Go's encoding/json (struct fields are emitted in declaration
// order, which is the order in types.go).
func MarshalJSON(s *Schema) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// MarshalYAML emits the human-review surface. The schema shape is fixed
// and shallow enough to hand-roll a small, deterministic emitter rather
// than pull in an external dependency. Output mirrors the JSON document
// 1:1 in field order.
func MarshalYAML(s *Schema) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "fmtVer: %d\n", s.FmtVer)
	fmt.Fprintf(&b, "schemaHint: %d\n", s.SchemaHint)
	b.WriteString("roots:\n")
	for _, r := range s.Roots {
		fmt.Fprintf(&b, "  - %s\n", yamlTypeRef(r))
	}
	b.WriteString("structs:\n")
	for _, sd := range s.Structs {
		fmt.Fprintf(&b, "  - type: %s\n", yamlTypeRef(sd.Type))
		if sd.Opaque {
			b.WriteString("    opaque: true\n")
		}
		if sd.AllowBreaking != "" {
			fmt.Fprintf(&b, "    allowBreaking: %s\n", yamlString(sd.AllowBreaking))
		}
		if len(sd.Generic) > 0 {
			fmt.Fprintf(&b, "    generic: [%s]\n", strings.Join(sd.Generic, ", "))
		}
		if len(sd.Reserved) > 0 {
			b.WriteString("    reserved: [")
			for i, t := range sd.Reserved {
				if i > 0 {
					b.WriteString(", ")
				}
				fmt.Fprintf(&b, "%d", t)
			}
			b.WriteString("]\n")
		}
		if len(sd.Fields) == 0 {
			continue
		}
		b.WriteString("    fields:\n")
		for _, fd := range sd.Fields {
			fmt.Fprintf(&b, "      - { tag: %d, name: %s, type: %s, wire: %s",
				fd.Tag, fd.Name, yamlString(fd.Type), fd.Wire)
			if fd.Optional {
				b.WriteString(", optional: true")
			}
			if fd.Deprecated {
				b.WriteString(", deprecated: true")
			}
			if fd.CompatWrite {
				b.WriteString(", compatWrite: true")
			}
			if fd.CycleBreak {
				b.WriteString(", cycleBreak: true")
			}
			if fd.MapKey != "" {
				fmt.Fprintf(&b, ", mapKey: %s, mapValue: %s",
					yamlString(fd.MapKey), yamlString(fd.MapValue))
			} else if fd.Elem != "" {
				fmt.Fprintf(&b, ", elem: %s", yamlString(fd.Elem))
			}
			if fd.Custom != "" {
				fmt.Fprintf(&b, ", custom: %s", yamlString(fd.Custom))
			}
			b.WriteString(" }\n")
		}
	}
	return []byte(b.String())
}

// yamlTypeRef formats a TypeRef as a single-line yaml string. Generic
// instantiations are rendered as `pkg.Name[arg1, arg2]`. Quoting is
// applied once at the outermost level so nested arguments don't pick up
// stacked escapes.
func yamlTypeRef(r TypeRef) string {
	return yamlString(typeRefString(r))
}

// typeRefString produces the unquoted dotted form of a TypeRef. Non-named
// type arguments arrive with an empty PkgPath (their Name already holds
// the type's full string form), so we skip the dot prefix in that case.
func typeRefString(r TypeRef) string {
	var base string
	if r.PkgPath != "" {
		base = r.PkgPath + "." + r.Name
	} else {
		base = r.Name
	}
	if len(r.TypeArgs) == 0 {
		return base
	}
	parts := make([]string, len(r.TypeArgs))
	for i, a := range r.TypeArgs {
		parts[i] = typeRefString(a)
	}
	return fmt.Sprintf("%s[%s]", base, strings.Join(parts, ", "))
}

// yamlString quotes s if it contains characters that would confuse the
// (small) yaml parsers used downstream — colons, commas, brackets, or
// leading/trailing whitespace. The chosen quoting is JSON-style double
// quotes, which is valid YAML.
func yamlString(s string) string {
	if s == "" {
		return `""`
	}
	if needsQuote(s) {
		j, _ := json.Marshal(s)
		return string(j)
	}
	return s
}

func needsQuote(s string) bool {
	if s != strings.TrimSpace(s) {
		return true
	}
	for _, r := range s {
		switch r {
		case ':', ',', '[', ']', '{', '}', '#', '&', '*', '!', '|', '>', '\'', '"', '%', '@', '`', '\n', '\t':
			return true
		}
	}
	return false
}
