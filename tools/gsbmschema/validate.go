package gsbmschema

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
				"%s is declared in package %q which is outside the schema input — include the package in the schema input, skip the referencing field with bin:\"-\", or wrap the referencing struct with //gsbm:opaque",
				sd.Type.Name, sd.Type.PkgPath),
		})
	}
	if sd.Opaque {
		// Opaque structs skip every other rule by design — including the
		// generic check below. A handwritten `func (b *Box[T]) MarshalGSBM`
		// instantiates per type-arg, so the parent's generated call to
		// Box[int].MarshalGSBM resolves at compile time without codegen.
		return issues
	}
	// Generic origins and their instantiations cannot be codegen'd: Go
	// does not permit a method body that varies per type argument, so
	// neither Box nor Box[int] gets a generated MarshalGSBM. A non-generic
	// parent referencing Box[int] would compile-fail at the call site,
	// since Box[int].MarshalGSBM does not exist. Reject any non-opaque
	// struct in the closure with type parameters so the failure surfaces
	// here, not at `go build` of the generated code.
	if len(sd.Generic) > 0 {
		issues = append(issues, Issue{
			Code: "type/generic",
			Message: fmt.Sprintf(
				"%s is generic (type parameters %v) — gsbm codegen does not support generic types; mark the type //gsbm:opaque with handwritten Marshal/Unmarshal/Reset, replace the field type with a non-generic struct, or skip the field with bin:\"-\"",
				refKey(sd.Type), sd.Generic),
		})
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
		// `custom=Foo` is plumbed through schema/classifier/hash so that
		// when codegen learns to dispatch on it, the append-only policy can
		// already guard wire-shape transitions (add → warning,
		// remove/swap → breaking). The codegen does NOT consult Custom
		// today, so accepting a `custom=` annotation would silently change
		// schemaHint and review labels with zero wire-format effect. Reject at
		// validate time until codegen support lands.
		if fd.Custom != "" {
			issues = append(issues, Issue{
				Code: "field/custom-not-supported",
				Message: fmt.Sprintf(
					"%s.%s: `bin:\"%d,custom=%s\"` — custom marshaler dispatch is not yet implemented in codegen; remove the annotation",
					sd.Type.Name, fd.Name, fd.Tag, fd.Custom),
			})
		}
		// Optional fields (`*T`) must wrap a primitive, []byte, or named
		// type. `*[]T` (non-byte), `*map[K]V`, and `*[N]T` are rejected
		// because the codegen has no decode path for them — the encoder
		// would emit wire data the decoder cannot read. Use the value
		// form (`[]T`, `map[K]V`, `[N]T`) instead, which has natural
		// nil/empty semantics.
		if fd.Optional && fd.Type != "[]byte" && fd.Type != "[]uint8" {
			if strings.HasPrefix(fd.Type, "[") || strings.HasPrefix(fd.Type, "map[") {
				issues = append(issues, Issue{
					Code: "field/optional-composite",
					Message: fmt.Sprintf(
						"%s.%s: optional %q is not supported — drop the pointer and use the value form",
						sd.Type.Name, fd.Name, "*"+fd.Type),
				})
			}
		}
	}
	return issues
}

// validateNoCycles walks the struct graph from each root looking for a
// path that returns to a struct already on the path. A cycle is an error
// unless it is broken by a field carrying //gsbm:cycle_break_via_id (or
// the equivalent `bin:"N,id_ref"` tag option). When a cycle is found the
// diagnostic names the shortest-tag field along the cycle as the
// recommended break candidate, plus any cycle fields whose names match
// the conventional break-point heuristic (Previous/Parent/Ref) so the
// author sees an actionable suggestion alongside the cycle path.
func validateNoCycles(s *Schema, byKey map[string]*StructDecl) []Issue {
	var issues []Issue
	state := map[string]int{} // 0=unseen, 1=on-stack, 2=done
	var dfs func(key string, pathNodes []string, pathEdges []*FieldDecl) []Issue
	dfs = func(key string, pathNodes []string, pathEdges []*FieldDecl) []Issue {
		var found []Issue
		sd := byKey[key]
		if sd == nil {
			return nil
		}
		state[key] = 1
		pathNodes = append(pathNodes, key)
		for _, fd := range sd.Fields {
			if fd.CycleBreak {
				continue
			}
			next := referencedKeys(fd, byKey)
			for _, n := range next {
				switch state[n] {
				case 0:
					found = append(found, dfs(n, pathNodes, append(pathEdges, fd))...)
				case 1:
					idx := -1
					for i, p := range pathNodes {
						if p == n {
							idx = i
							break
						}
					}
					if idx < 0 {
						continue
					}
					cNodes := append([]string{}, pathNodes[idx:]...)
					cEdges := append([]*FieldDecl{}, pathEdges[idx:]...)
					cEdges = append(cEdges, fd)
					found = append(found, Issue{
						Code:    "type/cycle",
						Message: formatCycleDiagnostic(cNodes, cEdges),
					})
				}
			}
		}
		state[key] = 2
		return found
	}
	for _, root := range s.Roots {
		issues = append(issues, dfs(refKey(root), nil, nil)...)
	}
	return issues
}

// formatCycleDiagnostic renders a human-readable cycle path plus a
// suggested break candidate. cycleEdges[i] is the field on cycleNodes[i]
// that descends to cycleNodes[i+1] (wrapping at the end). The picked
// break candidate is the field with the lowest tag, with a tie broken in
// favor of a field whose name matches the Previous/Parent/Ref heuristic.
// Additional name-heuristic matches are listed as alternatives so the
// author sees the conventional anchor fields even when the primary
// suggestion is something else.
func formatCycleDiagnostic(cycleNodes []string, cycleEdges []*FieldDecl) string {
	var b strings.Builder
	b.WriteString("cycle in closure: ")
	for i, fd := range cycleEdges {
		if i > 0 {
			b.WriteString(" → ")
		}
		fmt.Fprintf(&b, "%s.%s", shortTypeName(cycleNodes[i]), fd.Name)
	}
	b.WriteString(" → ")
	b.WriteString(shortTypeName(cycleNodes[0]))

	bestIdx := 0
	for i, fd := range cycleEdges {
		switch {
		case fd.Tag < cycleEdges[bestIdx].Tag:
			bestIdx = i
		case fd.Tag == cycleEdges[bestIdx].Tag:
			if isCycleBreakName(fd.Name) && !isCycleBreakName(cycleEdges[bestIdx].Name) {
				bestIdx = i
			}
		}
	}
	best := cycleEdges[bestIdx]
	fmt.Fprintf(&b,
		"; suggested break: %s.%s (tag %d) — add the `id_ref` tag option (`bin:\"%d,id_ref\"`) or the //gsbm:cycle_break_via_id comment",
		shortTypeName(cycleNodes[bestIdx]), best.Name, best.Tag, best.Tag)

	type cand struct {
		owner, name string
		tag         uint32
	}
	var heur []cand
	seen := map[string]bool{}
	for i, fd := range cycleEdges {
		if i == bestIdx {
			continue
		}
		if !isCycleBreakName(fd.Name) {
			continue
		}
		owner := shortTypeName(cycleNodes[i])
		key := owner + "." + fd.Name
		if seen[key] {
			continue
		}
		seen[key] = true
		heur = append(heur, cand{owner, fd.Name, fd.Tag})
	}
	if len(heur) > 0 {
		b.WriteString("; name-heuristic candidates: ")
		for i, c := range heur {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s.%s (tag %d)", c.owner, c.name, c.tag)
		}
	}
	return b.String()
}

// isCycleBreakName reports whether a field name matches the conventional
// anchor-field heuristic for cycle breaks. "previous" and "parent" may
// appear anywhere in the name (PreviousID, ParentNode); "ref" is anchored
// to a suffix to avoid matching unrelated identifiers like Reference,
// Preference, or RefreshToken.
func isCycleBreakName(name string) bool {
	lower := strings.ToLower(name)
	if strings.Contains(lower, "previous") || strings.Contains(lower, "parent") {
		return true
	}
	return lower == "ref" || strings.HasSuffix(lower, "ref")
}

// shortTypeName trims the package-path prefix from a refKey so the
// diagnostic shows `Item.Previous` rather than `example.com/pkg.Item.Previous`.
// Generic instantiations keep their bracketed type-arg suffix, since the
// short form still reads sensibly (e.g. `List[Item]`).
func shortTypeName(key string) string {
	end := len(key)
	if i := strings.Index(key, "["); i >= 0 {
		end = i
	}
	if i := strings.LastIndex(key[:end], "."); i >= 0 {
		return key[i+1:]
	}
	return key
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
