package odmschema

import (
	"fmt"
	"strings"
)

// primitiveKinds is the set of types acceptable as a map key, per spec
// §5.3 ("MUST be a primitive type or string"). We accept Go integer and
// boolean kinds (rendered as `int64`, `uint32`, `bool`, …) plus `string`.
var primitiveKinds = map[string]bool{
	"bool":    true,
	"string":  true,
	"int":     true,
	"int8":    true,
	"int16":   true,
	"int32":   true,
	"int64":   true,
	"uint":    true,
	"uint8":   true,
	"uint16":  true,
	"uint32":  true,
	"uint64":  true,
	"uintptr": true,
	"byte":    true,
	"rune":    true,
}

// builtinPrimitives are the types eligible for zero-elision per spec
// §5.1: int*/uint*/float*/bool/string/[]byte. The presence-byte rule is
// enforced at codegen / runtime; this set lets the validator surface the
// fact in the snapshot so reviewers can spot mistakes.
var builtinPrimitives = func() map[string]bool {
	m := map[string]bool{
		"float32": true,
		"float64": true,
	}
	for k := range primitiveKinds {
		m[k] = true
	}
	return m
}()

// IsBuiltinPrimitive reports whether a field's declared type qualifies
// for presence-byte zero-elision.
func IsBuiltinPrimitive(typeName string) bool {
	if typeName == "[]byte" || typeName == "[]uint8" {
		return true
	}
	return builtinPrimitives[typeName]
}

// Validate runs every rule in §3.2 / §5 / §7 of the schema policy
// against s. ps may be nil; when supplied it is used to flag closure
// types whose package is outside the input set.
func Validate(s *Schema, ps *PackageSet) []Issue {
	var issues []Issue
	allowed := map[string]bool{}
	if ps != nil {
		for _, p := range ps.Packages {
			allowed[p.Path] = true
		}
	}
	byKey := map[string]*StructDecl{}
	for _, sd := range s.Structs {
		byKey[refKey(sd.Type)] = sd
	}
	for _, sd := range s.Structs {
		issues = append(issues, validateStruct(sd, allowed, len(allowed) > 0)...)
	}
	issues = append(issues, validateNoCycles(s, byKey)...)
	return issues
}

func validateStruct(sd *StructDecl, allowed map[string]bool, checkAllowed bool) []Issue {
	var issues []Issue
	if checkAllowed && sd.Type.PkgPath != "" && !allowed[sd.Type.PkgPath] {
		issues = append(issues, Issue{
			Code: "type/external",
			Message: fmt.Sprintf(
				"%s is declared in package %q which is outside the schema input — mark the referencing field //odm:opaque or include the package",
				sd.Type.Name, sd.Type.PkgPath),
		})
	}
	if sd.Opaque {
		// Opaque structs skip every other rule by design.
		return issues
	}

	// Tag uniqueness within the struct, plus reserved-tag honoring, plus
	// map-key validation, plus deprecated/cycle-break interactions.
	seenTag := map[uint32]string{}
	reservedSet := map[uint32]bool{}
	for _, t := range sd.Reserved {
		reservedSet[t] = true
	}
	for _, fd := range sd.Fields {
		if fd.Tag == 0 {
			issues = append(issues, Issue{
				Code:    "tag/zero",
				Message: fmt.Sprintf("%s.%s: tag 0 is reserved", sd.Type.Name, fd.Name),
			})
		}
		if other, dup := seenTag[fd.Tag]; dup {
			issues = append(issues, Issue{
				Code: "tag/duplicate",
				Message: fmt.Sprintf("%s: tag %d used by both %s and %s",
					sd.Type.Name, fd.Tag, other, fd.Name),
			})
		}
		seenTag[fd.Tag] = fd.Name
		if reservedSet[fd.Tag] {
			issues = append(issues, Issue{
				Code: "tag/reserved",
				Message: fmt.Sprintf("%s.%s: tag %d is in the struct's reserved set",
					sd.Type.Name, fd.Name, fd.Tag),
			})
		}
		if fd.MapKey != "" && !primitiveKinds[fd.MapKey] {
			issues = append(issues, Issue{
				Code: "map/bad-key",
				Message: fmt.Sprintf("%s.%s: map key %q must be a primitive or string",
					sd.Type.Name, fd.Name, fd.MapKey),
			})
		}
	}
	return issues
}

// validateNoCycles walks the struct graph from each root looking for a
// path that returns to a struct already on the path. A cycle is an error
// unless it is broken by a field carrying //odm:cycle_break_via_id.
func validateNoCycles(s *Schema, byKey map[string]*StructDecl) []Issue {
	var issues []Issue
	state := map[string]int{} // 0=unseen, 1=on-stack, 2=done
	var dfs func(key string, stack []string) []Issue
	dfs = func(key string, stack []string) []Issue {
		var found []Issue
		sd := byKey[key]
		if sd == nil {
			return nil
		}
		state[key] = 1
		for _, fd := range sd.Fields {
			if fd.CycleBreak {
				continue
			}
			next := referencedKeys(fd, byKey)
			for _, n := range next {
				switch state[n] {
				case 0:
					found = append(found, dfs(n, append(stack, key))...)
				case 1:
					path := append([]string{}, stack...)
					path = append(path, key, n)
					found = append(found, Issue{
						Code: "type/cycle",
						Message: fmt.Sprintf("cycle in closure: %v (break with //odm:cycle_break_via_id on a field along the cycle)",
							path),
					})
				}
			}
		}
		state[key] = 2
		return found
	}
	for _, root := range s.Roots {
		issues = append(issues, dfs(refKey(root), nil)...)
	}
	return issues
}

// referencedKeys returns the keys of any structs reachable from a single
// field declaration. The field's shape strings (Type, Elem, MapValue) are
// parsed recursively so that cycles routed through arbitrary compositions
// of `*`, `[]`, `[N]`, and `map[K]V` are all surfaced — earlier versions
// only stripped outer prefixes and missed shapes like `[]map[K]Foo` or
// `map[K]map[K2]Foo`.
func referencedKeys(fd *FieldDecl, byKey map[string]*StructDecl) []string {
	var out []string
	for _, s := range [...]string{fd.Type, fd.Elem, fd.MapValue} {
		out = collectRefs(s, byKey, out)
	}
	return out
}

// collectRefs walks a shape string and appends every leaf identifier that
// appears in byKey. Shape grammar (produced by builder.shapeOf):
//
//	shape := '*' shape
//	       | '[' (digits)? ']' shape
//	       | 'map[' shape ']' shape
//	       | identifier
//
// Identifiers may themselves contain `[...]` for generic instantiations
// (e.g. `pkg.List[pkg.Item]`) — those are resolved by checking byKey on
// the whole leaf rather than splitting on the first `]`.
func collectRefs(shape string, byKey map[string]*StructDecl, out []string) []string {
	for len(shape) > 0 && shape[0] == '*' {
		shape = shape[1:]
	}
	if shape == "" {
		return out
	}
	if strings.HasPrefix(shape, "map[") {
		// Find the `]` that closes the map's key bracket, accounting for
		// nested brackets in the key (e.g. generic instantiations).
		end := matchBracket(shape, 3)
		if end < 0 {
			return out
		}
		key := shape[4:end]
		val := shape[end+1:]
		out = collectRefs(key, byKey, out)
		out = collectRefs(val, byKey, out)
		return out
	}
	if shape[0] == '[' {
		// `[]X` or `[N]X` — split at the first `]` (digits between `[`
		// and `]` never contain another `[`, so Cut is sufficient).
		_, rest, ok := strings.Cut(shape, "]")
		if !ok {
			return out
		}
		return collectRefs(rest, byKey, out)
	}
	if _, ok := byKey[shape]; ok {
		out = append(out, shape)
	}
	return out
}

// matchBracket returns the index of the `]` that matches the `[` at
// position openIdx, or -1 if the brackets are unbalanced.
func matchBracket(s string, openIdx int) int {
	depth := 1
	for i := openIdx + 1; i < len(s); i++ {
		switch s[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}
