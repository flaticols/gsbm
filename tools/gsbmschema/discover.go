package gsbmschema

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"reflect"
	"slices"
	"sort"
	"strings"
)

// MaxNestingDepth caps how many composite layers (slice/map) may stack
// inside one field. Three levels is the practical ceiling for real
// storage payloads — `map[K]map[K2][]V` is at the cap, anything deeper
// gets the `type/nesting-too-deep` diagnostic. The wire format itself
// has no depth limit; the cap is a codegen guard that keeps generated
// switch-on-type predictable and protects against pathological schemas.
const MaxNestingDepth = 3

// Issue describes one validation or discovery problem. Position is the
// best-effort source location; in tests against synthesized sources it
// may be the FileSet's <input>:line:col form.
type Issue struct {
	Pos     string
	Code    string // short stable code (e.g. "tag/missing", "type/cycle")
	Message string
}

func (i Issue) Error() string {
	if i.Pos == "" {
		return fmt.Sprintf("%s: %s", i.Code, i.Message)
	}
	return fmt.Sprintf("%s: %s: %s", i.Pos, i.Code, i.Message)
}

// Discover walks ps for //gsbm:root markers and returns the discovered
// roots in a deterministic (PkgPath, Name) order. A type is a root iff
// its doc comment carries //gsbm:root; the type itself MUST be a struct.
//
// Only packages with TopLevel=true are scanned for roots. Same-module
// dependency packages enter PackageSet so their AST is available for
// marker lookup and validation, but they cannot contribute roots — adding
// //gsbm:root to a dependency must not silently widen the schema for an
// unrelated caller's `lint ./pkg` invocation.
func Discover(ps *PackageSet) ([]*types.Named, []Issue) {
	var roots []*types.Named
	var issues []Issue
	for _, p := range ps.Packages {
		if !p.TopLevel {
			continue
		}
		for _, f := range p.Files {
			for _, decl := range f.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.TYPE {
					continue
				}
				for _, spec := range gd.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					doc := ts.Doc
					if doc == nil && len(gd.Specs) == 1 {
						doc = gd.Doc
					}
					m, err := parseMarkers(doc)
					if err != nil {
						issues = append(issues, Issue{
							Pos:     ps.Fset.Position(ts.Pos()).String(),
							Code:    "marker/parse",
							Message: err.Error(),
						})
						continue
					}
					if !m.root {
						continue
					}
					obj := p.Pkg.Scope().Lookup(ts.Name.Name)
					if obj == nil {
						continue
					}
					tn, ok := obj.(*types.TypeName)
					if !ok {
						continue
					}
					named, ok := tn.Type().(*types.Named)
					if !ok {
						issues = append(issues, Issue{
							Pos:     ps.Fset.Position(ts.Pos()).String(),
							Code:    "root/not-named",
							Message: fmt.Sprintf("//gsbm:root on %s but type is not named", ts.Name.Name),
						})
						continue
					}
					if _, ok := named.Underlying().(*types.Struct); !ok {
						issues = append(issues, Issue{
							Pos:     ps.Fset.Position(ts.Pos()).String(),
							Code:    "root/not-struct",
							Message: fmt.Sprintf("//gsbm:root on %s but underlying type is not a struct", ts.Name.Name),
						})
						continue
					}
					roots = append(roots, named)
				}
			}
		}
	}
	sort.SliceStable(roots, func(i, j int) bool {
		a, b := refOf(roots[i]), refOf(roots[j])
		if a.PkgPath != b.PkgPath {
			return a.PkgPath < b.PkgPath
		}
		return a.Name < b.Name
	})
	return roots, issues
}

// refOf renders a *types.Named (possibly an instantiation) as a TypeRef.
func refOf(n *types.Named) TypeRef {
	r := TypeRef{Name: n.Obj().Name()}
	if n.Obj().Pkg() != nil {
		r.PkgPath = n.Obj().Pkg().Path()
	}
	if ta := n.TypeArgs(); ta != nil && ta.Len() > 0 {
		for t := range ta.Types() {
			r.TypeArgs = append(r.TypeArgs, refOfType(t))
		}
	}
	return r
}

func refOfType(t types.Type) TypeRef {
	if n, ok := t.(*types.Named); ok {
		return refOf(n)
	}
	// For non-named arguments we render the type's string form into
	// the Name slot — fine for hashing since instantiations of
	// generics over basic types are rare in this domain.
	return TypeRef{Name: t.String()}
}

// BuildSchema starts from roots and walks each named struct's fields,
// pulling the transitive closure of struct types into the schema. It
// returns the schema (with stable ordering) and any issues encountered
// during traversal. Validation rules are applied separately by Validate.
//
// Generic instantiations (e.g. List[Segment]) are recorded as their own
// StructDecl, keyed by (origin, args), so List[Segment] and List[Leg]
// don't collapse into one entry.
func BuildSchema(ps *PackageSet, roots []*types.Named) (*Schema, []Issue) {
	b := &builder{
		ps:     ps,
		seen:   map[string]*StructDecl{},
		queue:  nil,
		issues: nil,
	}
	for _, r := range roots {
		b.enqueue(r)
	}
	// BFS over the closure. Queue holds named types still to flatten;
	// flatten produces *StructDecl that may enqueue more types.
	for len(b.queue) > 0 {
		head := b.queue[0]
		b.queue = b.queue[1:]
		b.flatten(head)
	}
	rootRefs := make([]TypeRef, 0, len(roots))
	for _, r := range roots {
		rootRefs = append(rootRefs, refOf(r))
	}
	structs := make([]*StructDecl, 0, len(b.seen))
	for _, sd := range b.seen {
		structs = append(structs, sd)
	}
	sortStructs(structs)
	for _, sd := range structs {
		sortFields(sd.Fields)
		sd.Reserved = sortReserved(sd.Reserved)
	}
	s := &Schema{
		FmtVer:  FmtVer,
		Roots:   rootRefs,
		Structs: structs,
	}
	return s, b.issues
}

type builder struct {
	ps     *PackageSet
	seen   map[string]*StructDecl
	queue  []*types.Named
	issues []Issue
}

// keyOf produces the de-duplication key for a named type (with
// instantiation args). Identical instantiations share one StructDecl.
func keyOf(n *types.Named) string {
	r := refOf(n)
	return refKey(r)
}

// refKey produces the canonical dotted form of a TypeRef for hashing,
// dedup keys, and field-type strings. Non-named type arguments arrive
// with an empty PkgPath (their Name already holds the type's full string
// form, e.g. "int"), so we skip the dot prefix in that case to avoid
// emitting malformed `.int` segments.
func refKey(r TypeRef) string {
	var base string
	if r.PkgPath != "" {
		base = r.PkgPath + "." + r.Name
	} else {
		base = r.Name
	}
	if len(r.TypeArgs) == 0 {
		return base
	}
	var b strings.Builder
	b.WriteString(base)
	b.WriteByte('[')
	for i, a := range r.TypeArgs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(refKey(a))
	}
	b.WriteByte(']')
	return b.String()
}

func (b *builder) enqueue(n *types.Named) {
	k := keyOf(n)
	if _, seen := b.seen[k]; seen {
		return
	}
	// Reserve a placeholder so cycles terminate.
	b.seen[k] = nil
	b.queue = append(b.queue, n)
}

// flatten extracts a StructDecl from a named struct type and enqueues
// any further named struct types it references. Map keys, slice elems,
// and pointers are unwrapped to find the underlying named struct (if
// any) without ascending into Go primitives.
func (b *builder) flatten(n *types.Named) {
	k := keyOf(n)
	str, ok := n.Underlying().(*types.Struct)
	if !ok {
		// Non-struct named types are never standalone in the schema;
		// drop the placeholder so downstream code doesn't see a nil.
		delete(b.seen, k)
		return
	}
	pkg := b.findPackage(n.Obj().Pkg())
	var (
		doc       *ast.CommentGroup
		astStruct *ast.StructType
	)
	if pkg != nil {
		doc, astStruct, _ = pkg.findStructDoc(n.Obj().Name())
	}
	tm, err := parseMarkers(doc)
	if err != nil {
		b.issues = append(b.issues, Issue{
			Pos:     b.ps.Fset.Position(n.Obj().Pos()).String(),
			Code:    "marker/parse",
			Message: err.Error(),
		})
	}

	sd := &StructDecl{
		Type:          refOf(n),
		Reserved:      append([]uint32(nil), tm.reserved...),
		Opaque:        tm.opaque,
		TrackPresence: tm.trackPresence,
		BorrowStrings: tm.borrowStrings,
		AllowBreaking: tm.allowBreaking,
	}
	if tparams := n.Origin().TypeParams(); tparams != nil && tparams.Len() > 0 {
		for tp := range tparams.TypeParams() {
			sd.Generic = append(sd.Generic, tp.Obj().Name())
		}
	}
	b.seen[k] = sd

	if tm.opaque {
		// Opaque structs are not walked into; they appear in the schema
		// only as a placeholder so the classifier still notices when an
		// opaque marker is added or removed.
		return
	}

	var fields []collectedField
	b.collectStructFields(n, str, astStruct, nil, false, nil, &fields, sd)
	for _, c := range fields {
		sd.Fields = append(sd.Fields, c.fd)
	}
	b.checkFlattenedTagCollisions(n, fields, sd)
}

// collectedField carries a flattened FieldDecl alongside the source *types.Var
// it was built from, so the collision check can report the embedded
// declaration's position rather than the outer struct's.
type collectedField struct {
	fd  *FieldDecl
	src *types.Var
}

// collectStructFields walks str's fields and appends each tagged field as
// a *FieldDecl into out. Anonymous embedded struct fields are flattened
// recursively: the embed's tagged fields are promoted into the outer
// struct's field list with FlattenedFrom recording the chain of embedded
// type names (e.g. "Base" for a direct embed, "Outer.Base" for a two-level
// embed). chain is the dot-joined embedded-type prefix accumulated so far;
// pointerEmbed is true once any segment of the chain is a pointer-to-struct.
// visited tracks the set of *types.Named already traversed in the current
// embed chain so mutually-recursive pointer embeds (legal Go) cannot drive
// the validator into unbounded recursion.
//
// Anonymous embeds whose type is not a named struct (e.g. embedding a
// named primitive) are rejected with `field/anonymous-non-struct`.
func (b *builder) collectStructFields(
	owner *types.Named,
	str *types.Struct,
	astStruct *ast.StructType,
	chain []string,
	pointerEmbed bool,
	visited map[*types.Named]bool,
	out *[]collectedField,
	sd *StructDecl,
) {
	for i := 0; i < str.NumFields(); i++ {
		f := str.Field(i)
		if f.Anonymous() {
			// findFieldDoc cannot retrieve comments for anonymous embeds
			// (they have no Names), but the Go parser still attaches the
			// leading //gsbm:* directives to the *ast.Field. Look them up
			// explicitly so a misplaced //gsbm:track-presence on an embed
			// is rejected with the same diagnostic as on a named field
			// instead of silently degrading to the default (no
			// FieldPresent state after decode).
			if adoc, ok := findAnonymousFieldDoc(astStruct, f.Name()); ok {
				am, mErr := parseMarkers(adoc)
				if mErr != nil {
					b.issues = append(b.issues, Issue{
						Pos:     b.ps.Fset.Position(f.Pos()).String(),
						Code:    "marker/parse",
						Message: mErr.Error(),
					})
				}
				// parseMarkers returns a partially populated markers struct
				// even on error, so still reject a misplaced //gsbm:track-presence
				// when other directives in the same block failed to parse —
				// otherwise an unknown sibling directive would silently mask
				// the misplaced-marker diagnostic.
				if am.trackPresence {
					b.issues = append(b.issues, Issue{
						Pos:     b.ps.Fset.Position(f.Pos()).String(),
						Code:    "marker/track-presence-misplaced",
						Message: fmt.Sprintf("%s.%s: //gsbm:track-presence must be on the struct doc-comment, not an embedded field", owner.Obj().Name(), f.Name()),
					})
					continue
				}
				if am.borrowStrings {
					b.issues = append(b.issues, Issue{
						Pos:     b.ps.Fset.Position(f.Pos()).String(),
						Code:    "marker/borrow-strings-misplaced",
						Message: fmt.Sprintf("%s.%s: //gsbm:borrow-strings must be on the struct doc-comment, not an embedded field", owner.Obj().Name(), f.Name()),
					})
					continue
				}
			}
			b.flattenAnonymous(owner, f, chain, pointerEmbed, visited, out, sd)
			continue
		}
		fd := b.buildFieldDecl(owner, f, str.Tag(i), astStruct)
		if fd == nil {
			continue
		}
		if len(chain) > 0 {
			fd.FlattenedFrom = strings.Join(chain, ".")
			fd.FlattenedFromPointer = pointerEmbed
			// Cross-package flatten guard: codegen emits the explicit
			// `v.<Embed>.<Field>` access path from the outer's package.
			// If the promoted leaf field — or its named type — is
			// unexported in a foreign package, the generated file will
			// not compile. Record a diagnostic up front instead of
			// letting the user discover this at build time. Same-package
			// unexported fields stay legal because they compile fine in
			// the owning package; only the cross-package case is broken.
			b.checkFlattenedFieldAccessible(owner, f, chain)
		}
		*out = append(*out, collectedField{fd: fd, src: f})
	}
}

// flattenAnonymous handles one anonymous embedded field: it resolves the
// embedded named struct type, recurses into its fields, and either appends
// flattened entries to out or records a diagnostic if the embed shape is
// unsupported (anonymous-non-struct, anonymous-generic, anonymous-opaque)
// or cyclic (field/embed-cycle). Reserved tags declared on the embedded
// type are unioned into sd so the outer struct's tag space inherits the
// append-only constraint.
func (b *builder) flattenAnonymous(
	owner *types.Named,
	f *types.Var,
	chain []string,
	pointerEmbed bool,
	visited map[*types.Named]bool,
	out *[]collectedField,
	sd *StructDecl,
) {
	pos := b.ps.Fset.Position(f.Pos()).String()
	t := f.Type()
	isPtr := false
	if ptr, ok := t.(*types.Pointer); ok {
		isPtr = true
		t = ptr.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok {
		// Go embeds must be named (or *Named); an unnamed embed is a
		// syntax error in Go itself, so this branch is defensive.
		b.issues = append(b.issues, Issue{
			Pos:     pos,
			Code:    "field/anonymous-non-struct",
			Message: fmt.Sprintf("%s: anonymous embed of %s — only named struct types can be embedded and flattened; replace with a named field", owner.Obj().Name(), t.String()),
		})
		return
	}
	innerStr, ok := named.Underlying().(*types.Struct)
	if !ok {
		b.issues = append(b.issues, Issue{
			Pos:     pos,
			Code:    "field/anonymous-non-struct",
			Message: fmt.Sprintf("%s: anonymous embed of %s whose underlying type %s is not a struct — only struct embeds flatten; replace with a named field (e.g. `Foo %s`)", owner.Obj().Name(), named.Obj().Name(), named.Underlying().String(), named.Obj().Name()),
		})
		return
	}
	// Generic instantiations (e.g. `type Outer struct { Box[int] }`) compile
	// in Go but codegen renders named types without type arguments, so the
	// promoted access path `v.Box.Field` plus reflective type literals
	// emitted by typeExpr would produce uncompilable Go. Reject up front so
	// the user gets a clear diagnostic instead of a broken build.
	if ta := named.TypeArgs(); ta != nil && ta.Len() > 0 {
		b.issues = append(b.issues, Issue{
			Pos:     pos,
			Code:    "field/anonymous-generic",
			Message: fmt.Sprintf("%s: anonymous embed of generic instantiation %s — codegen renders named types without type arguments and would emit invalid Go; replace with a named field (e.g. `B %s`) and mark the type //gsbm:opaque with a handwritten codec", owner.Obj().Name(), named.String(), named.String()),
		})
		return
	}
	if visited[named] {
		b.issues = append(b.issues, Issue{
			Pos:     pos,
			Code:    "field/embed-cycle",
			Message: fmt.Sprintf("%s: anonymous embed of %s forms a cycle through %s — break the cycle by replacing one embed with a named field", owner.Obj().Name(), named.Obj().Name(), strings.Join(append(chain, named.Obj().Name()), " → ")),
		})
		return
	}
	var (
		innerDoc *ast.CommentGroup
		innerAST *ast.StructType
	)
	if pkg := b.findPackage(named.Obj().Pkg()); pkg != nil {
		innerDoc, innerAST, _ = pkg.findStructDoc(named.Obj().Name())
	}
	innerMarkers, mErr := parseMarkers(innerDoc)
	if mErr != nil {
		b.issues = append(b.issues, Issue{
			Pos:     b.ps.Fset.Position(named.Obj().Pos()).String(),
			Code:    "marker/parse",
			Message: mErr.Error(),
		})
	}
	// Honoring //gsbm:opaque means the embedded type owns its wire image via
	// a handwritten Marshal/Unmarshal. Flattening would walk past those
	// methods, promote the inner fields, and emit inline encode/decode for
	// them — the user's opaque contract would be silently bypassed and the
	// wire format would diverge from what the handwritten codec produces.
	if innerMarkers.opaque {
		b.issues = append(b.issues, Issue{
			Pos:     pos,
			Code:    "field/anonymous-opaque",
			Message: fmt.Sprintf("%s: anonymous embed of opaque type %s — flattening would bypass the handwritten Marshal/Unmarshal on %s; replace with a named field (e.g. `B %s` with a `bin:\"N\"` tag) so the opaque codec is invoked", owner.Obj().Name(), named.Obj().Name(), named.Obj().Name(), named.Obj().Name()),
		})
		return
	}
	// Reserved tags on the embedded type extend into the outer struct's tag
	// space along with its fields. Without merging, an outer struct could
	// silently declare a new field on a tag that the embedded base reserved
	// (e.g. for a removed field), defeating the append-only guarantee.
	if len(innerMarkers.reserved) > 0 && sd != nil {
		sd.Reserved = append(sd.Reserved, innerMarkers.reserved...)
	}
	newChain := append(append([]string(nil), chain...), named.Obj().Name())
	// Cross-package gate for intermediate embed segments. Codegen emits the
	// full chain `v.<S1>.<S2>....<Leaf>`, so an unexported intermediate
	// segment in a foreign package would render uncompilable code in the
	// outer's package. Pointer-embed segments also surface their type name
	// via `&pkg.<S>{}` in the decoder's lazy-allocation snippet, doubling
	// the inaccessibility surface. Same-package unexported segments compile
	// fine; the gate fires only on cross-package unexported types. The
	// outermost owner here is what the codegen file's package will be, so
	// we measure exportedness against `owner`'s package, not the parent
	// embed's. The first segment is implicitly checked by the Go compiler
	// (you can't write `type Outer struct { foreignpkg.unexported }` in
	// the first place), but the same check still passes harmlessly for it.
	b.checkIntermediateEmbedAccessible(owner, f, named, newChain)
	if visited == nil {
		visited = map[*types.Named]bool{}
	}
	visited[named] = true
	b.collectStructFields(owner, innerStr, innerAST, newChain, pointerEmbed || isPtr, visited, out, sd)
	delete(visited, named)
}

// checkIntermediateEmbedAccessible flags an embed segment whose type is
// unexported in a foreign package. Codegen renders the segment's name in
// the access path (`v.<S>.<...>`) and, for pointer embeds, also in the
// decoder allocation snippet (`&pkg.<S>{}`), so either reference would
// fail to compile from the outer's package. For anonymous embeds in Go,
// the field name and the type name are identical, so checking the type's
// exportedness covers both the access-path and allocation cases.
func (b *builder) checkIntermediateEmbedAccessible(owner *types.Named, f *types.Var, named *types.Named, chain []string) bool {
	ownerPkg := ""
	if owner.Obj() != nil && owner.Obj().Pkg() != nil {
		ownerPkg = owner.Obj().Pkg().Path()
	}
	if named == nil || named.Obj() == nil {
		return true
	}
	typePkg := ""
	if named.Obj().Pkg() != nil {
		typePkg = named.Obj().Pkg().Path()
	}
	if typePkg == "" || typePkg == ownerPkg || named.Obj().Exported() {
		return true
	}
	pathStr := "v." + strings.Join(chain, ".")
	b.issues = append(b.issues, Issue{
		Pos:  b.ps.Fset.Position(f.Pos()).String(),
		Code: "field/anonymous-unexported-type",
		Message: fmt.Sprintf(
			"%s: intermediate embed segment %s is the unexported type %s in package %q; codegen would emit `%s` referencing an inaccessible name from the outer's package",
			owner.Obj().Name(), strings.Join(chain, "."), named.Obj().Name(), typePkg, pathStr),
	})
	return false
}

// checkFlattenedFieldAccessible rejects promoted fields that codegen cannot
// access from the outer struct's package. Codegen emits the explicit
// dotted path `v.<Embed>.<Field>` (see emit.go writableFields), so two
// things must hold for the generated file to compile in the outer's
// package:
//
//   - the leaf field's name must be exported when the field lives in a
//     different package than the outer struct (`field/anonymous-unexported-field`);
//   - every named type reachable inside the leaf field's type (the leaf
//     named type itself, the element of `*T`/`[]T`/`[N]T`/`chan T`, both
//     key and value of `map[K]V`) must be exported when it lives in a
//     different package than the outer struct (`field/anonymous-unexported-type`).
//     The codegen's `typeExpr` walker renders composite types verbatim
//     and the decoder side allocates leaf pointees by name (`&pkg.T{}`),
//     so a `*secret`/`[]secret`/`map[string]secret` leaf type would
//     produce uncompilable references even though `f.Type()` itself is
//     not a *types.Named.
//
// Same-package unexported fields compile fine, so the cross-package gate
// keeps this from over-firing on local embeds with unexported names.
// chain carries the type-name path used to construct the codegen access
// expression; it is included in the diagnostic so the user can map the
// problem back to the embed they wrote.
func (b *builder) checkFlattenedFieldAccessible(owner *types.Named, f *types.Var, chain []string) bool {
	ownerPkg := ""
	if owner.Obj() != nil && owner.Obj().Pkg() != nil {
		ownerPkg = owner.Obj().Pkg().Path()
	}
	fieldPkg := ""
	if f.Pkg() != nil {
		fieldPkg = f.Pkg().Path()
	}
	pathStr := "v." + strings.Join(chain, ".") + "." + f.Name()
	ok := true
	if fieldPkg != "" && fieldPkg != ownerPkg && !f.Exported() {
		b.issues = append(b.issues, Issue{
			Pos:  b.ps.Fset.Position(f.Pos()).String(),
			Code: "field/anonymous-unexported-field",
			Message: fmt.Sprintf(
				"%s: promoted field %s.%s is unexported but lives in a different package; codegen would emit `%s` which cannot compile across package boundaries",
				owner.Obj().Name(), strings.Join(chain, "."), f.Name(), pathStr),
		})
		ok = false
	}
	for _, named := range collectForeignUnexportedNamed(f.Type(), ownerPkg) {
		typePkg := ""
		if named.Obj() != nil && named.Obj().Pkg() != nil {
			typePkg = named.Obj().Pkg().Path()
		}
		typeName := ""
		if named.Obj() != nil {
			typeName = named.Obj().Name()
		}
		b.issues = append(b.issues, Issue{
			Pos:  b.ps.Fset.Position(f.Pos()).String(),
			Code: "field/anonymous-unexported-type",
			Message: fmt.Sprintf(
				"%s: promoted field %s.%s references unexported type %s in package %q via its declared type %s; codegen would emit an inaccessible reference from `%s`",
				owner.Obj().Name(), strings.Join(chain, "."), f.Name(), typeName, typePkg, f.Type().String(), pathStr),
		})
		ok = false
	}
	return ok
}

// collectForeignUnexportedNamed walks t and returns each *types.Named whose
// object is unexported and lives in a package other than ownerPkg. It
// descends through pointers, slices, arrays, maps, and channels (the
// composite forms codegen renders verbatim into the outer's package) and
// also through named-type instantiation arguments. For a *types.Named
// whose underlying is one of those composite forms (e.g.
// `type ExportedList []secret` in package b), codegen unwraps the
// underlying when emitting decode/encode (see emit.go ~1316/897), so the
// underlying element appears verbatim in the outer's package and we
// must descend into it. It deliberately does NOT descend into a named
// type's underlying struct or interface body: codegen never re-emits a
// named type's body in the outer's package; it only references the name.
// The visited set guards against cycles in the type graph (mutually
// recursive named types are legal Go).
func collectForeignUnexportedNamed(t types.Type, ownerPkg string) []*types.Named {
	var out []*types.Named
	visited := map[*types.Named]bool{}
	var walk func(types.Type)
	walk = func(t types.Type) {
		switch tt := t.(type) {
		case *types.Named:
			if visited[tt] {
				return
			}
			visited[tt] = true
			if tt.Obj() != nil {
				typePkg := ""
				if tt.Obj().Pkg() != nil {
					typePkg = tt.Obj().Pkg().Path()
				}
				if typePkg != "" && typePkg != ownerPkg && !tt.Obj().Exported() {
					out = append(out, tt)
				}
			}
			if ta := tt.TypeArgs(); ta != nil {
				for arg := range ta.Types() {
					walk(arg)
				}
			}
			// Named types whose underlying is a composite get unwrapped by
			// codegen at the use site, so any foreign+unexported name
			// reachable through the underlying body becomes a direct
			// textual reference in the outer's package. Struct/interface
			// underlyings are kept by name, so skip them.
			switch tt.Underlying().(type) {
			case *types.Slice, *types.Array, *types.Map, *types.Chan, *types.Pointer:
				walk(tt.Underlying())
			}
		case *types.Pointer:
			walk(tt.Elem())
		case *types.Slice:
			walk(tt.Elem())
		case *types.Array:
			walk(tt.Elem())
		case *types.Map:
			walk(tt.Key())
			walk(tt.Elem())
		case *types.Chan:
			walk(tt.Elem())
		}
	}
	walk(t)
	return out
}

// checkFlattenedTagCollisions inspects the collected field set for tag
// duplicates that involve at least one flattened field, and emits
// `field/tag-collision` with both field names and the embed boundary. The
// collided flattened entries are stripped from sd.Fields so the generic
// tag/duplicate check in validate.go does not double-fire on them.
func (b *builder) checkFlattenedTagCollisions(
	owner *types.Named,
	fields []collectedField,
	sd *StructDecl,
) {
	// Walk fields once, recording the first field at each tag. On a
	// duplicate involving any flattened side, emit `field/tag-collision`
	// and mark the colliding flattened entry for removal so it does not
	// propagate to validate.go's `tag/duplicate` pass.
	first := map[uint32]int{}
	drop := map[*FieldDecl]bool{}
	for i, c := range fields {
		if c.fd.Tag == 0 {
			continue
		}
		j, dup := first[c.fd.Tag]
		if !dup {
			first[c.fd.Tag] = i
			continue
		}
		prev := fields[j]
		// Only emit field/tag-collision when at least one side was
		// promoted from an embed. Pure direct-vs-direct duplicates are
		// caught by validate.go's tag/duplicate diagnostic.
		if prev.fd.FlattenedFrom == "" && c.fd.FlattenedFrom == "" {
			continue
		}
		b.issues = append(b.issues, Issue{
			Pos:     b.ps.Fset.Position(c.src.Pos()).String(),
			Code:    "field/tag-collision",
			Message: formatTagCollision(owner.Obj().Name(), prev.fd, c.fd),
		})
		// Drop the later flattened side from sd.Fields so the schema
		// remains internally consistent for downstream tooling.
		switch {
		case c.fd.FlattenedFrom != "":
			drop[c.fd] = true
		case prev.fd.FlattenedFrom != "":
			drop[prev.fd] = true
		}
	}
	if len(drop) == 0 {
		return
	}
	filtered := sd.Fields[:0]
	for _, fd := range sd.Fields {
		if drop[fd] {
			continue
		}
		filtered = append(filtered, fd)
	}
	sd.Fields = filtered
}

// formatTagCollision renders a `field/tag-collision` diagnostic naming
// both fields, their tag, and (for flattened sides) the embed chain that
// promoted them into the outer struct.
func formatTagCollision(ownerName string, a, b *FieldDecl) string {
	return fmt.Sprintf(
		"%s: tag %d used by both %s and %s",
		ownerName, a.Tag, describeFieldOrigin(a), describeFieldOrigin(b),
	)
}

func describeFieldOrigin(fd *FieldDecl) string {
	if fd.FlattenedFrom == "" {
		return fmt.Sprintf("direct field %s", fd.Name)
	}
	return fmt.Sprintf("field %s flattened from %s", fd.Name, fd.FlattenedFrom)
}

// buildFieldDecl translates one non-anonymous struct field into a
// *FieldDecl, applying tag parsing, marker parsing, cycle-break handling,
// custom-codec handling, and type-shape filling. It returns nil if the
// field is skipped (bin:"-", missing tag, or an issue was recorded).
func (b *builder) buildFieldDecl(n *types.Named, f *types.Var, rawTag string, astStruct *ast.StructType) *FieldDecl {
	ft, err := ParseFieldTag(reflect.StructTag(rawTag))
	if err != nil {
		b.issues = append(b.issues, Issue{
			Pos:     b.ps.Fset.Position(f.Pos()).String(),
			Code:    "tag/parse",
			Message: fmt.Sprintf("%s.%s: %s", n.Obj().Name(), f.Name(), err),
		})
		return nil
	}
	fdoc, _ := findFieldDoc(astStruct, f.Name())
	fm, err := parseMarkers(fdoc)
	if err != nil {
		b.issues = append(b.issues, Issue{
			Pos:     b.ps.Fset.Position(f.Pos()).String(),
			Code:    "marker/parse",
			Message: err.Error(),
		})
	}
	// //gsbm:track-presence is a struct-level opt-in for stored presence
	// bitmaps; misplacing it on a field would silently degrade to the
	// default (no FieldPresent state after decode), so reject it loudly
	// with a diagnostic that points the user at the struct doc-comment.
	if fm.trackPresence {
		b.issues = append(b.issues, Issue{
			Pos:     b.ps.Fset.Position(f.Pos()).String(),
			Code:    "marker/track-presence-misplaced",
			Message: fmt.Sprintf("%s.%s: //gsbm:track-presence must be on the struct doc-comment, not a field", n.Obj().Name(), f.Name()),
		})
		return nil
	}
	// //gsbm:borrow-strings is a struct-level unsafe lifetime opt-in. A
	// field-level spelling would look like per-field granularity, which v1
	// deliberately does not implement, so reject it loudly.
	if fm.borrowStrings {
		b.issues = append(b.issues, Issue{
			Pos:     b.ps.Fset.Position(f.Pos()).String(),
			Code:    "marker/borrow-strings-misplaced",
			Message: fmt.Sprintf("%s.%s: //gsbm:borrow-strings must be on the struct doc-comment, not a field", n.Obj().Name(), f.Name()),
		})
		return nil
	}
	if !ft.Set {
		b.issues = append(b.issues, Issue{
			Pos:     b.ps.Fset.Position(f.Pos()).String(),
			Code:    "tag/missing",
			Message: fmt.Sprintf("%s.%s has no `bin` tag (use `bin:\"-\"` to skip)", n.Obj().Name(), f.Name()),
		})
		return nil
	}
	if ft.Skip {
		return nil
	}
	// The pointer-to-struct shape check applies to both forms — the
	// tag option (`bin:"N,id_ref"`) and the legacy comment marker
	// (`//gsbm:cycle_break_via_id`). Without the comment-marker check
	// codegen later calls idRefTargetField(nil) and panics on nil
	// pointer deref of the target type.
	cycleBreak := fm.cycleBreakViaID || ft.CycleBreakViaID
	// id_ref and custom= are mutually exclusive in either spelling.
	// ParseFieldTag catches the tag-only form (`bin:"N,id_ref,custom=…"`),
	// but the legacy `//gsbm:cycle_break_via_id` marker lives on the
	// comment, not the tag, so we re-check here. Allowing both produces
	// a snapshot that records the field as both cycle-break and custom
	// (hash/classifier disagreement) while codegen emits only the
	// id_ref path (see emit.go: CycleBreak takes precedence over Custom).
	if fm.cycleBreakViaID && ft.Custom != "" {
		b.issues = append(b.issues, Issue{
			Pos:  b.ps.Fset.Position(f.Pos()).String(),
			Code: "tag/parse",
			Message: fmt.Sprintf(
				"%s.%s: //gsbm:cycle_break_via_id and `custom=%s` are mutually exclusive",
				n.Obj().Name(), f.Name(), ft.Custom),
		})
		return nil
	}
	// Symmetric to the parse-side `id_ref` + `type=` rejection: the legacy
	// //gsbm:cycle_break_via_id marker lives on the doc-comment, not in
	// the tag, so ParseFieldTag never sees the cycle-break flag and
	// cannot catch the combination there. Without this check the field
	// falls through to the basic-int gate below and gets blamed with
	// `tag/type-width-mismatch` for being a pointer-to-struct, hiding
	// the real conflict (id_ref encodes the target's bin:"1" leaf; the
	// width override widens that leaf's range — pick one shape).
	if fm.cycleBreakViaID && ft.WireOverride != "" {
		b.issues = append(b.issues, Issue{
			Pos:  b.ps.Fset.Position(f.Pos()).String(),
			Code: "tag/parse",
			Message: fmt.Sprintf(
				"%s.%s: //gsbm:cycle_break_via_id and `type=%s` are mutually exclusive (move the width override to the target's bin:\"1\" field)",
				n.Obj().Name(), f.Name(), ft.WireOverride),
		})
		return nil
	}
	if cycleBreak && !isPointerToStruct(f.Type()) {
		form := `bin:"` + fmt.Sprintf("%d", ft.Tag) + `,id_ref"`
		if !ft.CycleBreakViaID {
			form = "//gsbm:cycle_break_via_id"
		}
		b.issues = append(b.issues, Issue{
			Pos:  b.ps.Fset.Position(f.Pos()).String(),
			Code: "tag/bad-id-ref",
			Message: fmt.Sprintf(
				"%s.%s: `%s` — id_ref requires a pointer-to-struct field (got %s)",
				n.Obj().Name(), f.Name(), form, f.Type().String()),
		})
		return nil
	}
	// `type=` declares the wire range the field promises to hold; the
	// Go type bounds what it can hold. Sign and width must agree with
	// the field's Go type per the contract table (docs/spec.md §5 and
	// WireOverrideCompat). Reject cross-sign overrides, widening past a
	// fixed-width Go type, narrowing a platform-sized int below 32-bit,
	// and any override on non-integer types — with a stable Issue code
	// so the user sees the mismatch at schema validation rather than at
	// codegen / decode time. Named integer aliases (`type UserID int64`)
	// are walked to their underlying basic. Pointer-to-int (`*int`) is
	// rejected: the override widens the wire range, while optionality
	// is encoded via the presence envelope; mixing the two has no
	// defined wire shape.
	if ft.WireOverride != "" {
		if _, _, ok, reason := WireOverrideCompat(f.Type(), ft.WireOverride); !ok {
			b.issues = append(b.issues, Issue{
				Pos:  b.ps.Fset.Position(f.Pos()).String(),
				Code: "tag/type-width-mismatch",
				Message: fmt.Sprintf(
					"%s.%s: `bin:\"%d,type=%s\"` — %s",
					n.Obj().Name(), f.Name(), ft.Tag, ft.WireOverride, reason),
			})
			return nil
		}
	}
	fd := &FieldDecl{
		Name:         f.Name(),
		Tag:          ft.Tag,
		Deprecated:   ft.Deprecated,
		CompatWrite:  ft.CompatWrite,
		CycleBreak:   cycleBreak,
		Custom:       ft.Custom,
		WireOverride: ft.WireOverride,
	}
	if ft.Custom != "" {
		// Custom-codec fields opt out of normal schema traversal:
		// the wire shape is whatever the codec declares (filled in
		// by codegen from the codec registry), not what the field's
		// Go type implies. We therefore do NOT enqueue nested
		// struct types, do NOT recurse into private fields of
		// external types (so `time.Time`'s internal `wall/ext/loc`
		// never surface as `tag/missing`), and do NOT run
		// `checkSupportedType`. Pointer wrap is the one piece we
		// still observe — the spec §5.1 nullable envelope is
		// applied around the codec call.
		fieldType := f.Type()
		if ptr, ok := fieldType.(*types.Pointer); ok {
			fd.Optional = true
			fieldType = ptr.Elem()
		}
		// Reject composite underlying types (slice, map, array)
		// including named aliases like `type Times []time.Time`.
		// The codec's encode function takes a scalar; passing a
		// composite would miscompile the generated file. validate.go
		// catches literal `[]T`/`map[K]V` via fd.Type prefix, but a
		// named-alias composite renders as the qualified name and
		// slips past that check — check here where the underlying
		// kind is available.
		switch fieldType.Underlying().(type) {
		case *types.Slice, *types.Map, *types.Array:
			b.issues = append(b.issues, Issue{
				Pos:  b.ps.Fset.Position(f.Pos()).String(),
				Code: "field/custom-composite",
				Message: fmt.Sprintf(
					"%s.%s: custom codec %q applies to a single value of the codec's Go type — wrap the element type, not the composite (got %s)",
					n.Obj().Name(), f.Name(), ft.Custom, fieldType.String()),
			})
			return nil
		}
		fd.Type = fieldType.String()
		return fd
	}
	b.fillTypeShape(fd, f.Type())
	// For id_ref fields, the on-wire body is the target's bin:"1"
	// field encoded as a leaf scalar — not a length-delim struct
	// body. Resolve that ID field here so (a) we can flag bad
	// targets at discover time instead of waiting for codegen, and
	// (b) the snapshot's Wire reflects what's actually emitted. The
	// latter is what closes the opaque-target CI hole: changing an
	// opaque target's bin:"1" from string to int64 flips fd.Wire,
	// which the classifier already treats as field/wire-changed.
	if cycleBreak {
		b.resolveIDRefField(n, f, fd)
	}
	b.checkSupportedType(n, f, f.Type(), 0, 0)
	return fd
}

// checkSupportedType walks the field's Go type and records an issue for
// any kind the codegen cannot encode/decode. Per-spec §3.2 ("no unintended
// types in closure"), unsupported kinds must be rejected at validation
// time rather than at codegen time. Use //gsbm:opaque on the referencing
// struct to opt fields out of this check.
//
// depth counts every recursion (pointer, slice, map, named-alias) and
// gates the "only valid as a direct field" rejections (generic
// instantiation, named slice alias, nested pointer). compositeDepth
// counts ONLY composite layers (slice or map) and gates MaxNestingDepth;
// it stays at zero when descending through a named-non-struct or a
// pointer's pointee because those don't add a wire-format level.
func (b *builder) checkSupportedType(owner *types.Named, f *types.Var, t types.Type, depth, compositeDepth int) {
	pos := b.ps.Fset.Position(f.Pos()).String()
	// Generic instantiations (e.g. Box[int], Label[int]) are rejected
	// everywhere except the direct-value-field case where the underlying
	// type is a struct. The exempt shape is `B Box[int]`: codegen emits
	// `B.MarshalGSBM(w)` and never has to render the type expression, so
	// an opaque generic with handwritten methods works. Every other
	// position (pointer, slice elem, map value/key, non-struct alias) goes
	// through emit.go's typeExpr, which prints named types without type
	// arguments (codegen.go:typeExpr) and would emit invalid Go like
	// `&Box{}`, `MakeSlice[Box]`, `var vv Box`, or `Label(tmp)`.
	if named, ok := t.(*types.Named); ok {
		if ta := named.TypeArgs(); ta != nil && ta.Len() > 0 {
			_, isStruct := named.Underlying().(*types.Struct)
			if depth > 0 || !isStruct {
				b.issues = append(b.issues, Issue{
					Pos:     pos,
					Code:    "type/generic",
					Message: fmt.Sprintf("%s.%s: generic instantiation %s is not supported in this position — only a direct value field of a struct (e.g. `B Box[int]` with //gsbm:opaque on Box) compiles; pointer/slice/map/array elements and named-with-non-struct underlying produce invalid Go because the codegen renders named types without type arguments", owner.Obj().Name(), f.Name(), t.String()),
				})
				return
			}
		}
	}
	switch tt := t.(type) {
	case *types.Basic:
		if isSupportedBasicKind(tt.Kind()) {
			return
		}
		b.issues = append(b.issues, Issue{
			Pos:     pos,
			Code:    "type/unsupported",
			Message: fmt.Sprintf("%s.%s: basic type %s is not supported (only bool/string/int*/uint*/uintptr/float32/float64 are encodable)", owner.Obj().Name(), f.Name(), tt.String()),
		})
	case *types.Named:
		// Named struct: closure walk handled by enqueue elsewhere.
		// Named-not-struct: supported when the underlying type is a
		// supported basic kind, `[]byte` (the latter for ID fields like
		// `type ID []byte` referenced by id_ref), or a slice whose
		// element is itself a supported slice-element type (so named
		// aliases like `type ItemList []Item` and `type ItemPtrList
		// []*Item` round-trip with byte-identical wire to the underlying
		// slice). Named slice aliases are only supported as a direct
		// field — nested under a pointer or another composite they are
		// rejected, mirroring the optional-composite rule for `*[]T`.
		underlying := tt.Underlying()
		if _, isStruct := underlying.(*types.Struct); isStruct {
			return
		}
		if basic, isBasic := underlying.(*types.Basic); isBasic && isSupportedBasicKind(basic.Kind()) {
			return
		}
		if s, isSlice := underlying.(*types.Slice); isSlice {
			if isBasicByte(s.Elem()) {
				return
			}
			if depth > 0 {
				b.issues = append(b.issues, Issue{
					Pos:     pos,
					Code:    "type/unsupported",
					Message: fmt.Sprintf("%s.%s: named slice alias %s is not supported in this position — nested under a pointer or composite has no codegen path; use the value form directly", owner.Obj().Name(), f.Name(), tt.String()),
				})
				return
			}
			// Nested composite element (`type Variants []map[K]V`,
			// `type Matrix [][]V`): the emitter routes the alias through
			// emitSliceEncode → emitValueEncode which already handles
			// nested slice/map elements, so accept here and recurse so
			// the cap and leaf rules are enforced at depth.
			switch s.Elem().(type) {
			case *types.Slice, *types.Map:
				b.checkSupportedType(owner, f, s.Elem(), depth+1, compositeDepth+1)
				return
			}
			if !isSliceElementType(s.Elem()) {
				b.issues = append(b.issues, Issue{
					Pos:     pos,
					Code:    "type/unsupported",
					Message: fmt.Sprintf("%s.%s: named slice alias %s has unsupported element %s — only []byte, named structs, named-with-basic-underlying, pointers to named structs, basic primitives, or nested slice/map composites are valid slice elements", owner.Obj().Name(), f.Name(), tt.String(), s.Elem().String()),
				})
				return
			}
			elemT := s.Elem()
			if ptr, ok := elemT.(*types.Pointer); ok {
				elemT = ptr.Elem()
			}
			b.checkSupportedType(owner, f, elemT, depth+1, compositeDepth+1)
			return
		}
		b.issues = append(b.issues, Issue{
			Pos:     pos,
			Code:    "type/unsupported",
			Message: fmt.Sprintf("%s.%s: named type %s has unsupported underlying %s — only struct, basic primitive, []byte, or supported slice underlying are supported", owner.Obj().Name(), f.Name(), tt.String(), underlying.String()),
		})
	case *types.Pointer:
		if depth > 0 {
			b.issues = append(b.issues, Issue{
				Pos:     pos,
				Code:    "type/nested-pointer",
				Message: fmt.Sprintf("%s.%s: nested pointer types (%s) are not supported", owner.Obj().Name(), f.Name(), t.String()),
			})
			return
		}
		// One level of pointer = optional. Recurse into the pointee.
		// optional-composite is already caught by validateStruct, but we
		// still walk so deeper unsupported kinds inside the pointee surface.
		// Pointers do not contribute a composite layer themselves.
		b.checkSupportedType(owner, f, tt.Elem(), depth+1, compositeDepth)
	case *types.Slice:
		// Entering this slice contributes one composite level on the wire
		// (length-delim frame), regardless of whether the element forces
		// further codegen recursion. Check the cap first so `[]byte` ends
		// up counted the same as `[]int64`; otherwise `map[K]map[K2][]int`
		// (3 layers, accepted) and `map[K]map[K2][]byte` (3 layers, also
		// accepted) would diverge and chains like `[][][][]byte` would
		// silently slip past the cap.
		if compositeDepth+1 > MaxNestingDepth {
			b.issues = append(b.issues, Issue{
				Pos:     pos,
				Code:    "type/nesting-too-deep",
				Message: fmt.Sprintf("%s.%s: composite nesting depth exceeds MaxNestingDepth=%d at %s — introduce a named struct (or named slice alias) at an intermediate level to flatten the codegen", owner.Obj().Name(), f.Name(), MaxNestingDepth, f.Type().String()),
			})
			return
		}
		// `[]byte` is the only slice that doesn't recurse — element handling
		// in codegen short-circuits to ReadBytes/WriteBytes.
		if isBasicByte(tt.Elem()) {
			return
		}
		// Nested composite element (`[][]V`, `[]map[K]V`): recurse so the
		// inner shape is itself validated against the same rules.
		switch tt.Elem().(type) {
		case *types.Slice, *types.Map:
			b.checkSupportedType(owner, f, tt.Elem(), depth+1, compositeDepth+1)
			return
		}
		// Codegen's slice-decode path expects a leaf element type or a
		// pointer to a named struct (encoded per spec §5.1 with a per-
		// element presence-byte envelope). Other shapes (`[][N]T`,
		// `[]*int64`) have no decode path and would fail at gen time after
		// passing lint.
		if !isSliceElementType(tt.Elem()) {
			b.issues = append(b.issues, Issue{
				Pos:     pos,
				Code:    "type/unsupported",
				Message: fmt.Sprintf("%s.%s: slice element %s is not supported — only []byte, named structs, named-with-basic-underlying, pointers to named structs, basic primitives, or nested slice/map composites are valid slice elements", owner.Obj().Name(), f.Name(), tt.Elem().String()),
			})
			return
		}
		// Recurse via the unwrapped pointer pointee for `[]*T`, otherwise
		// the *types.Pointer case below would reject it as a nested pointer.
		elemT := tt.Elem()
		if ptr, ok := elemT.(*types.Pointer); ok {
			elemT = ptr.Elem()
		}
		b.checkSupportedType(owner, f, elemT, depth+1, compositeDepth+1)
	case *types.Map:
		// Key validity is checked separately in validateStruct via primitiveKinds.
		// Entering this map contributes one composite level.
		if compositeDepth+1 > MaxNestingDepth {
			b.issues = append(b.issues, Issue{
				Pos:     pos,
				Code:    "type/nesting-too-deep",
				Message: fmt.Sprintf("%s.%s: composite nesting depth exceeds MaxNestingDepth=%d at %s — introduce a named struct (or named slice alias) at an intermediate level to flatten the codegen", owner.Obj().Name(), f.Name(), MaxNestingDepth, f.Type().String()),
			})
			return
		}
		// Nested composite value (`map[K][]V`, `map[K]map[K2]V`): recurse.
		switch tt.Elem().(type) {
		case *types.Slice, *types.Map:
			b.checkSupportedType(owner, f, tt.Elem(), depth+1, compositeDepth+1)
			return
		}
		// Leaf value: must be a primitive, named-with-basic-underlying, or
		// named struct. Pointer-to-named-struct is intentionally NOT a
		// valid map value (no codegen path for the per-entry presence byte
		// the slice case relies on).
		if !isLeafElementType(tt.Elem()) {
			b.issues = append(b.issues, Issue{
				Pos:     pos,
				Code:    "type/unsupported",
				Message: fmt.Sprintf("%s.%s: map value %s is not supported — only named structs, named-with-basic-underlying, basic primitives, or nested slice/map composites are valid map values", owner.Obj().Name(), f.Name(), tt.Elem().String()),
			})
			return
		}
		b.checkSupportedType(owner, f, tt.Elem(), depth+1, compositeDepth+1)
	case *types.Array:
		b.issues = append(b.issues, Issue{
			Pos:     pos,
			Code:    "type/unsupported",
			Message: fmt.Sprintf("%s.%s: fixed-size array (%s) is not supported — use a slice instead", owner.Obj().Name(), f.Name(), t.String()),
		})
	case *types.Interface:
		b.issues = append(b.issues, Issue{
			Pos:     pos,
			Code:    "type/unsupported",
			Message: fmt.Sprintf("%s.%s: interface types (%s) are not supported in the schema closure — use a concrete type or //gsbm:opaque", owner.Obj().Name(), f.Name(), t.String()),
		})
	case *types.Chan, *types.Signature:
		b.issues = append(b.issues, Issue{
			Pos:     pos,
			Code:    "type/unsupported",
			Message: fmt.Sprintf("%s.%s: type %s is not encodable", owner.Obj().Name(), f.Name(), t.String()),
		})
	default:
		b.issues = append(b.issues, Issue{
			Pos:     pos,
			Code:    "type/unsupported",
			Message: fmt.Sprintf("%s.%s: unsupported type %s", owner.Obj().Name(), f.Name(), t.String()),
		})
	}
}

func isBasicByte(t types.Type) bool {
	if b, ok := t.(*types.Basic); ok {
		return b.Kind() == types.Uint8 || b.Kind() == types.Byte
	}
	return false
}

// isLeafElementType reports whether t can appear as the element of a slice
// or the value of a map without forcing codegen into nested-composite
// territory it cannot handle. Leaves are: supported basic primitives,
// named structs (codegen recurses into their MarshalGSBM/UnmarshalGSBM),
// and named types whose underlying is a supported basic primitive.
func isLeafElementType(t types.Type) bool {
	switch tt := t.(type) {
	case *types.Basic:
		return isSupportedBasicKind(tt.Kind())
	case *types.Named:
		underlying := tt.Underlying()
		if _, isStruct := underlying.(*types.Struct); isStruct {
			return true
		}
		if basic, isBasic := underlying.(*types.Basic); isBasic && isSupportedBasicKind(basic.Kind()) {
			return true
		}
		return false
	}
	return false
}

// isSliceElementType reports whether t is acceptable as the element of a
// slice (top-level field or named slice alias). The accepted shapes are
// every leaf element type plus `*T` where T is a named struct — slice-of-
// pointer-to-struct, per spec §5.1, encodes each element as a length-delim
// envelope with a presence byte so nil mid-slice round-trips. Pointer-to-
// primitive (`[]*int64`) and pointer-to-named-non-struct are rejected
// because neither has codegen support and neither is in the spec's
// optional-shape list for v1.
func isSliceElementType(t types.Type) bool {
	if isLeafElementType(t) {
		return true
	}
	ptr, ok := t.(*types.Pointer)
	if !ok {
		return false
	}
	named, ok := ptr.Elem().(*types.Named)
	if !ok {
		return false
	}
	_, isStruct := named.Underlying().(*types.Struct)
	return isStruct
}

// isSupportedBasicKind reports whether codegen has encoder/decoder cases
// for k. Complex, unsafe-pointer, untyped, and Invalid kinds are rejected.
func isSupportedBasicKind(k types.BasicKind) bool {
	switch k {
	case types.Bool, types.String,
		types.Int, types.Int8, types.Int16, types.Int32, types.Int64,
		types.Uint, types.Uint8, types.Uint16, types.Uint32, types.Uint64, types.Uintptr,
		types.Float32, types.Float64:
		return true
	}
	return false
}

// fillTypeShape sets Type / Wire / Optional / MapKey / MapValue / Elem
// on fd according to f's Go type. Named struct types referenced from
// the field are enqueued for further flattening; primitives terminate.
//
// A named slice alias used as the top-level field type is processed as
// if the field were declared with the alias's underlying slice type: the
// shape string (fd.Type), Elem, and Wire come from the slice form so the
// wire-bytes-relevant snapshot stays rename-stable. The alias identity
// is recorded separately in fd.AliasType so the classifier can flag a
// rename (same Underlying) as safe and a wire-affecting underlying
// change as breaking via the slice shape diff.
func (b *builder) fillTypeShape(fd *FieldDecl, t types.Type) {
	if ptr, ok := t.(*types.Pointer); ok {
		fd.Optional = true
		t = ptr.Elem()
	}
	if named, ok := t.(*types.Named); ok {
		if slice, isSlice := named.Underlying().(*types.Slice); isSlice && !isBasicByte(slice.Elem()) {
			fd.AliasType = aliasTypeRef(named, slice)
			t = slice
		}
	}
	fd.Type = b.shapeOf(t, fd, true)
	fd.Wire = wireFor(t)
}

// aliasTypeRef builds the structured TypeRef for a named slice alias.
// Name + PkgPath come from the alias itself; Underlying captures the
// slice element type so the classifier can compare it independently of
// the alias's identifier. Pointer-to-named elements are encoded by
// prefixing `*` to the element TypeRef's Name slot — TypeRef has no
// pointer flag, and equality-by-fields gives the right semantics.
func aliasTypeRef(n *types.Named, slice *types.Slice) *TypeRef {
	r := refOf(n)
	elem := sliceElemRef(slice.Elem())
	r.Underlying = &elem
	return &r
}

// sliceElemRef renders a slice element type as a TypeRef. Named elements
// pass through refOf; pointer-to-named elements record `*<pkg>.<Name>`
// in the Name slot so the resulting TypeRef is distinct from the same
// element without the pointer wrap. Non-named elements (primitives) use
// the type's string form in the Name slot — mirrors refOfType.
func sliceElemRef(t types.Type) TypeRef {
	if ptr, ok := t.(*types.Pointer); ok {
		named, ok := ptr.Elem().(*types.Named)
		if !ok {
			return TypeRef{Name: t.String()}
		}
		inner := refOf(named)
		return TypeRef{Name: "*" + refKey(inner)}
	}
	return refOfType(t)
}

// shapeOf produces a stable string form for a type and enqueues any
// named struct types it encounters. The first call is the field's top
// type; recursive calls (slice elem, map value) set fd.Elem / fd.MapValue.
func (b *builder) shapeOf(t types.Type, fd *FieldDecl, top bool) string {
	switch tt := t.(type) {
	case *types.Basic:
		return tt.Name()
	case *types.Named:
		if _, ok := tt.Underlying().(*types.Struct); ok {
			b.enqueue(tt)
			return refKey(refOf(tt))
		}
		// Non-struct named types: include the underlying shape so a change
		// like `type Quantity int64` → `type Quantity int32` is caught as
		// field/type-changed by the classifier. Without this, the wire-type
		// stays `varint` and the alias name is unchanged, so the diff would
		// silently report "safe" while old blobs fail with ErrIntegerOverflow
		// on decode.
		return refKey(refOf(tt)) + "(" + b.shapeOf(tt.Underlying(), fd, false) + ")"
	case *types.Slice:
		elem := b.shapeOf(tt.Elem(), fd, false)
		if top {
			fd.Elem = elem
		}
		return "[]" + elem
	case *types.Array:
		return fmt.Sprintf("[%d]%s", tt.Len(), b.shapeOf(tt.Elem(), fd, false))
	case *types.Map:
		k := b.shapeOf(tt.Key(), fd, false)
		v := b.shapeOf(tt.Elem(), fd, false)
		if top {
			fd.MapKey = k
			fd.MapValue = v
			// Record the underlying primitive's BasicKind name for a named
			// key so a future change to `Code`'s underlying (e.g. string →
			// int64) surfaces in the diff as a dedicated map-key change,
			// not just an opaque shape-string flip.
			if named, ok := tt.Key().(*types.Named); ok {
				if basic, ok := named.Underlying().(*types.Basic); ok {
					fd.MapKeyUnderlying = basic.Name()
				}
			}
		}
		return fmt.Sprintf("map[%s]%s", k, v)
	case *types.Pointer:
		return "*" + b.shapeOf(tt.Elem(), fd, false)
	case *types.Interface:
		if tt.Empty() {
			return "any"
		}
		return tt.String()
	default:
		return tt.String()
	}
}

// wireFor returns the spec §3.2 wire-type label for the top-level
// schema type of a field. Pointer fields are unwrapped first by the
// caller (Optional is set on the FieldDecl).
func wireFor(t types.Type) string {
	switch tt := t.(type) {
	case *types.Basic:
		switch tt.Kind() {
		case types.Float32:
			return WireFixed32
		case types.Float64:
			return WireFixed64
		case types.String:
			return WireLengthDelim
		default:
			return WireVarint
		}
	case *types.Named:
		// Named-but-not-struct unwraps to its underlying primitive's wire
		// type. The wire-type in the field key MUST match the actual
		// body encoding, otherwise SkipField on an unknown tag desyncs.
		if _, ok := tt.Underlying().(*types.Struct); ok {
			return WireLengthDelim
		}
		return wireFor(tt.Underlying())
	case *types.Slice, *types.Array, *types.Map:
		return WireLengthDelim
	case *types.Pointer:
		return wireFor(tt.Elem())
	default:
		return WireLengthDelim
	}
}

// findPackage maps a *types.Package back to the *Package whose AST we
// hold. Lookup is by import path, not pointer identity: the typechecker
// that built p may not be our own (e.g. importer.Default re-uses cached
// package instances), so pointer equality cannot be relied on. The path
// match is sufficient — input dirs are deduped by path in LoadFromDirs.
func (b *builder) findPackage(p *types.Package) *Package {
	if p == nil {
		return nil
	}
	path := p.Path()
	for _, pp := range b.ps.Packages {
		if pp.Path == path {
			return pp
		}
	}
	return nil
}

func sortStructs(s []*StructDecl) {
	sort.SliceStable(s, func(i, j int) bool {
		a, b := s[i].Type, s[j].Type
		if a.PkgPath != b.PkgPath {
			return a.PkgPath < b.PkgPath
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return refKey(a) < refKey(b)
	})
}

func sortFields(f []*FieldDecl) {
	sort.SliceStable(f, func(i, j int) bool { return f[i].Tag < f[j].Tag })
}

// isPointerToStruct reports whether t is `*T` where T is a named struct
// type, per spec §5.7. Used to gate the `id_ref` tag option: an
// ID-reference field must point at a named struct so the codegen has a
// stable target to read the designated ID field from. Anonymous structs
// are rejected because they have no name for the diagnostic, no stable
// identity across packages, and cannot themselves carry the `bin:"1"`
// convention reliably (no method set, no codegen output).
func isPointerToStruct(t types.Type) bool {
	ptr, ok := t.(*types.Pointer)
	if !ok {
		return false
	}
	named, ok := ptr.Elem().(*types.Named)
	if !ok {
		return false
	}
	_, ok = named.Underlying().(*types.Struct)
	return ok
}

// resolveIDRefField inspects the target of an id_ref field and (a) records
// any structural problem (missing bin:"1", unsupported ID-field type) as
// an Issue, (b) rejects cross-package references whose ID field is
// unexported — codegen reads the field as `v.Ref.<idName>` so an
// unexported foreign field would yield invalid Go, and (c) overwrites
// fd.Wire and appends the resolved ID's shape to fd.Type so
// snapshot/hash/diff reflect what the codegen actually emits — including
// same-wire-class flips like int32→int64 or string→[]byte that share a
// wire-type label and would otherwise be silent for opaque targets.
// Same-package unexported fields compile fine and are left alone.
func (b *builder) resolveIDRefField(owner *types.Named, f *types.Var, fd *FieldDecl) {
	ptr, ok := f.Type().(*types.Pointer)
	if !ok {
		return
	}
	idField, idType, idOverride, err := LookupIDRefField(ptr)
	if err != nil {
		b.issues = append(b.issues, Issue{
			Pos:     b.ps.Fset.Position(f.Pos()).String(),
			Code:    "idref/missing-id-tag",
			Message: fmt.Sprintf("%s.%s: %s", owner.Obj().Name(), f.Name(), err),
		})
		return
	}
	ownerPkg := pkgPath(owner)
	if !idField.Exported() {
		idPkg := pkgPath(ptr.Elem())
		if ownerPkg != idPkg {
			targetName := "<anonymous>"
			if n, ok := ptr.Elem().(*types.Named); ok {
				targetName = n.Obj().Name()
			}
			b.issues = append(b.issues, Issue{
				Pos:  b.ps.Fset.Position(f.Pos()).String(),
				Code: "idref/unexported-id-field",
				Message: fmt.Sprintf(
					"%s.%s: id_ref target %s.%s is unexported but lives in a different package; codegen would emit `v.%s.%s` which cannot compile across package boundaries",
					owner.Obj().Name(), f.Name(), targetName, idField.Name(), f.Name(), idField.Name()),
			})
			return
		}
	}
	// Cross-package compile check on the ID field's TYPE: codegen renders
	// a named ID type as `<alias>.<Name>` (e.g. for Reset zeroing or
	// id_ref decode conversion) when it's defined outside the consuming
	// package. An unexported name yields uncompilable Go even when the
	// field name itself is exported.
	if idNamed, ok := idType.(*types.Named); ok && idNamed.Obj() != nil && !idNamed.Obj().Exported() {
		typePkg := pkgPath(idNamed)
		if typePkg != "" && ownerPkg != typePkg {
			targetName := "<anonymous>"
			if n, ok := ptr.Elem().(*types.Named); ok {
				targetName = n.Obj().Name()
			}
			b.issues = append(b.issues, Issue{
				Pos:  b.ps.Fset.Position(f.Pos()).String(),
				Code: "idref/unexported-id-type",
				Message: fmt.Sprintf(
					"%s.%s: id_ref target %s.%s has type %s which is unexported in package %q; codegen would emit a `%s.%s(...)` conversion that cannot compile from a different package",
					owner.Obj().Name(), f.Name(), targetName, idField.Name(), idNamed.Obj().Name(), typePkg, typePkg, idNamed.Obj().Name()),
			})
			return
		}
	}
	fd.Wire = wireFor(idType)
	// Append the resolved ID type's shape to fd.Type. This is what closes
	// the same-wire-class hole: changing an opaque target's bin:"1" from
	// int32 to int64 (both varint) or from string to []byte (both
	// length-delim) doesn't move fd.Wire, but the appended id shape does
	// change, and the classifier's field/type-changed branch flags it.
	fd.Type = fd.Type + "/id:" + b.shapeOf(idType, fd, false)
	// Surface the target's `type=<width>` wire-width override on the
	// id_ref FieldDecl itself so the classifier's existing WireOverride
	// transition rules (widen=safe, narrow=breaking, identity-override↔
	// un-annotated=intent-only) apply directly across the full eight-width
	// integer contract. Encoding the override as a fd.Type
	// suffix would instead trip the unconditional field/type-changed
	// branch, falsely flagging widening and the byte-identical
	// int32↔un-annotated swap as breaking.
	//
	// Defense in depth for opaque targets: for a non-opaque target,
	// addField already validated the override against the per-field
	// contract via `tag/type-width-mismatch`. For an opaque target, that
	// field-level validation is skipped (discover doesn't walk into
	// opaque structs), so re-run the same compatibility check here — the
	// only place where an id_ref referencing an opaque target lands
	// before codegen — with the same Issue code so users see a schema
	// diagnostic rather than a late codegen panic.
	if idOverride != "" {
		if _, _, ok, reason := WireOverrideCompat(idType, idOverride); !ok {
			b.issues = append(b.issues, Issue{
				Pos:  b.ps.Fset.Position(f.Pos()).String(),
				Code: "tag/type-width-mismatch",
				Message: fmt.Sprintf(
					"%s.%s: id_ref target's bin:\"1\" carries `type=%s` — %s",
					owner.Obj().Name(), f.Name(), idOverride, reason),
			})
			return
		}
		fd.WireOverride = idOverride
	}
}

// pkgPath returns the import path of the package that defines t, or ""
// for types without a package (basics, unnamed composites). Used to
// compare ownership between a field's declaring struct and its id_ref
// target.
func pkgPath(t types.Type) string {
	named, ok := t.(*types.Named)
	if !ok || named.Obj() == nil || named.Obj().Pkg() == nil {
		return ""
	}
	return named.Obj().Pkg().Path()
}

// sortReserved sorts and deduplicates a struct's reserved-tag set in place.
// Duplicates can arrive from a careless author (`//gsbm:reserved 5,5,7`) or
// from multiple `//gsbm:reserved` lines on the same struct. Leaving them in
// place destabilises both the snapshot YAML and the canonical hash that
// feeds schemaHint, so two semantically-identical schemas would hash differently.
func sortReserved(r []uint32) []uint32 {
	slices.Sort(r)
	return slices.Compact(r)
}
