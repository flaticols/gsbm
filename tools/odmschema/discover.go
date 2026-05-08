package odmschema

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

// Discover walks ps for //odm:root markers and returns the discovered
// roots in a deterministic (PkgPath, Name) order. A type is a root iff
// its doc comment carries //odm:root; the type itself MUST be a struct.
func Discover(ps *PackageSet) ([]*types.Named, []Issue) {
	var roots []*types.Named
	var issues []Issue
	for _, p := range ps.Packages {
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
							Message: fmt.Sprintf("//odm:root on %s but type is not named", ts.Name.Name),
						})
						continue
					}
					if _, ok := named.Underlying().(*types.Struct); !ok {
						issues = append(issues, Issue{
							Pos:     ps.Fset.Position(ts.Pos()).String(),
							Code:    "root/not-struct",
							Message: fmt.Sprintf("//odm:root on %s but underlying type is not a struct", ts.Name.Name),
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
	// the Name slot — fine for fingerprinting since instantiations of
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
		sortReserved(sd.Reserved)
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

func refKey(r TypeRef) string {
	if len(r.TypeArgs) == 0 {
		return r.PkgPath + "." + r.Name
	}
	var b strings.Builder
	b.WriteString(r.PkgPath)
	b.WriteByte('.')
	b.WriteString(r.Name)
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

	for i := 0; i < str.NumFields(); i++ {
		f := str.Field(i)
		if f.Anonymous() {
			b.issues = append(b.issues, Issue{
				Pos:     b.ps.Fset.Position(f.Pos()).String(),
				Code:    "field/anonymous",
				Message: fmt.Sprintf("anonymous (embedded) fields are not supported in odm schema: %s", f.Name()),
			})
			continue
		}
		ft, err := ParseFieldTag(reflect.StructTag(str.Tag(i)))
		if err != nil {
			b.issues = append(b.issues, Issue{
				Pos:     b.ps.Fset.Position(f.Pos()).String(),
				Code:    "tag/parse",
				Message: fmt.Sprintf("%s.%s: %s", n.Obj().Name(), f.Name(), err),
			})
			continue
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
		if !ft.Set {
			b.issues = append(b.issues, Issue{
				Pos:     b.ps.Fset.Position(f.Pos()).String(),
				Code:    "tag/missing",
				Message: fmt.Sprintf("%s.%s has no `bin` tag (use `bin:\"-\"` to skip)", n.Obj().Name(), f.Name()),
			})
			continue
		}
		if ft.Skip {
			continue
		}
		fd := &FieldDecl{
			Name:       f.Name(),
			Tag:        ft.Tag,
			Deprecated: ft.Deprecated,
			CycleBreak: fm.cycleBreakViaID,
		}
		b.fillTypeShape(fd, f.Type())
		sd.Fields = append(sd.Fields, fd)
	}
}

// fillTypeShape sets Type / Wire / Optional / MapKey / MapValue / Elem
// on fd according to f's Go type. Named struct types referenced from
// the field are enqueued for further flattening; primitives terminate.
func (b *builder) fillTypeShape(fd *FieldDecl, t types.Type) {
	if ptr, ok := t.(*types.Pointer); ok {
		fd.Optional = true
		t = ptr.Elem()
	}
	fd.Type = b.shapeOf(t, fd, true)
	fd.Wire = wireFor(t)
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
		}
		return refKey(refOf(tt))
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
		return WireLengthDelim
	case *types.Slice, *types.Array, *types.Map:
		return WireLengthDelim
	case *types.Pointer:
		return wireFor(tt.Elem())
	default:
		return WireLengthDelim
	}
}

func (b *builder) findPackage(p *types.Package) *Package {
	if p == nil {
		return nil
	}
	for _, pp := range b.ps.Packages {
		if pp.Pkg == p {
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

func sortReserved(r []uint32) {
	slices.Sort(r)
}
