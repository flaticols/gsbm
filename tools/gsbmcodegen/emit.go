package gsbmcodegen

import (
	"fmt"
	"go/types"
	"io"
	"sort"
	"strings"

	"go.flaticols.dev/gsbm/storage/gsbm"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/codecs"
	"go.flaticols.dev/gsbm/tools/gsbmschema"
)

// pickByteSliceLocal chooses a local-variable name for a ReadBytes() result
// that won't shadow the named type referenced on the next line. typeExpr
// is either a bare identifier ("Name") or a qualified one ("pkg.Name").
// Two shadow risks: a same-package named type whose name equals the
// preferred local (e.g. `type raw []byte` ⇒ generated `raw(append(... raw))`
// would resolve `raw` to the local var, not the type), or a cross-package
// alias whose prefix collides (e.g. import aliased `raw` ⇒ `raw.ID(...)`
// would resolve to the local). Falling back to a `_` suffix is enough —
// neither risk can collide with more than one identifier per call site.
func pickByteSliceLocal(typeExpr string) string {
	const preferred = "raw"
	prefix := typeExpr
	if dot := strings.IndexByte(typeExpr, '.'); dot > 0 {
		prefix = typeExpr[:dot]
	}
	if prefix == preferred {
		return preferred + "_"
	}
	return preferred
}

// pickConvertLocal picks a local-variable name for a primitive decode
// temp that gets converted to a named type on the next line. Same shadow
// shape as pickByteSliceLocal: the named type's expression can be bare
// ("Name") or qualified ("pkg.Name"); a bare match or a matching package
// prefix collides with the preferred local, so fall back to `_` suffix.
// Without this, generated code like `var u string; tmp := u(u)` (or
// `tmp := u.ID(u)` for a cross-package alias named `u`) resolves the
// outer `u` to the local var instead of the type, failing to compile.
func pickConvertLocal(preferred, typeExpr string) string {
	prefix := typeExpr
	if dot := strings.IndexByte(typeExpr, '.'); dot > 0 {
		prefix = typeExpr[:dot]
	}
	if prefix == preferred {
		return preferred + "_"
	}
	return preferred
}

// pickPresenceLocals chooses names for the BeginLengthDelim marker and the
// ReadPresenceByte result inside an optional-decode block, avoiding shadow
// of any type expression that will be referenced inside the same scope.
// Risk is the same shape as pickByteSliceLocal: a same-package named type
// (e.g. `type saved []byte`) or a cross-package alias (`saved.Blob`) whose
// prefix matches "saved" or "state". On collision, fall back to a `_`
// suffix; one suffix variant is enough because each typeExpr can only
// match one of the two preferred names.
func pickPresenceLocals(typeExprs ...string) (savedLocal, stateLocal string) {
	savedLocal = "saved"
	stateLocal = "state"
	for _, te := range typeExprs {
		if te == "" {
			continue
		}
		prefix := te
		if dot := strings.IndexByte(te, '.'); dot > 0 {
			prefix = te[:dot]
		}
		if prefix == "saved" {
			savedLocal = "saved_"
		}
		if prefix == "state" {
			stateLocal = "state_"
		}
	}
	return savedLocal, stateLocal
}

// pickPointerSliceLocals mirrors pickPresenceLocals for the per-element
// BeginLengthDelim marker (`inner`) and the ReadPresenceByte result
// (`state`) emitted inside the slice-of-pointer decode loop. The pointee
// type expression is referenced inside that same scope (`&pointee{}`), so
// a same-package type named `inner`/`state` or an import aliased to one of
// those names would shadow it; fall back to a `_` suffix on collision.
func pickPointerSliceLocals(typeExprs ...string) (innerLocal, stateLocal string) {
	innerLocal = "inner"
	stateLocal = "state"
	for _, te := range typeExprs {
		if te == "" {
			continue
		}
		prefix := te
		if dot := strings.IndexByte(te, '.'); dot > 0 {
			prefix = te[:dot]
		}
		if prefix == "inner" {
			innerLocal = "inner_"
		}
		if prefix == "state" {
			stateLocal = "state_"
		}
	}
	return innerLocal, stateLocal
}

// fp wraps fmt.Fprintf, dropping the result. Codegen writes to an
// in-memory buffer that does not surface I/O errors at this layer; the
// outer caller checks io.Writer state. Wrapping the call here keeps the
// emitter sites free of `_, _ =` noise.
func fp(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// nm suffixes a local-variable name with the nesting depth so recursive
// composite codegen (`map[K][]V`, `[]map[K]V`, `map[K]map[K2]V`) doesn't
// shadow outer-scope locals it still needs to reference. At depth 0 the
// bare name is returned, keeping byte-for-byte output for non-nested
// fixtures (existing goldens stay identical).
func nm(base string, depth int) string {
	if depth == 0 {
		return base
	}
	return fmt.Sprintf("%s_%d", base, depth)
}

// fieldEntry pairs a Schema FieldDecl with the corresponding *types.Var so
// the emitter has both the schema metadata (tag, deprecation) and the Go
// type (for codegen of value-level access).
//
// For fields promoted from anonymous embeds (FlattenedFrom != ""), accessPath
// is the explicit dotted Go expression `v.<Embed>.<Field>` (or deeper for
// multi-level embeds) and embedSegments captures the chain so the encoder
// can wrap pointer-embed fields in nil-checks and the decoder can lazily
// allocate. For direct fields, accessPath is simply `v.<Name>` and
// embedSegments is nil — preserving identical generated output for fixtures
// that don't use embedding.
type fieldEntry struct {
	decl          *gsbmschema.FieldDecl
	gov           *types.Var
	accessPath    string
	embedSegments []embedSegment
}

// embedSegment records one hop along an anonymous embed chain. name is the
// field name in the parent struct (which equals the embedded type's name in
// Go), typeExpr is the Go source expression for the embedded type used when
// allocating a pointer-embed on decode, and isPointer is true when the embed
// is `*Embed` rather than `Embed`.
type embedSegment struct {
	name      string
	typeExpr  string
	isPointer bool
}

// writableFields returns the (schema, *types.Var) pairs of fields the
// codegen needs to encode/decode. Skipped (`bin:"-"`) fields are already
// absent from the schema. Deprecated fields stay in the schema for
// read-compat; the encoder normally MUST NOT emit them, but a deprecated
// field carrying `compat_write` is still written during the rollback
// window so a rollback to old code can still see the field's value. The
// per-field `Deprecated`/`CompatWrite` flags gate the encode side; the
// decode side is identical for both.
//
// Fields promoted from anonymous embeds (FlattenedFrom != "") are resolved
// by walking the embed chain to find the inner *types.Var and to record the
// chain segments so the encoder/decoder can use the explicit dotted path
// `v.<Embed>.<Field>`. The plan calls for the explicit path (rather than
// Go's field promotion) so multi-level embeds with shadowed names compile
// unambiguously.
func (e *emitter) writableFields(str *types.Struct, sd *gsbmschema.StructDecl) []fieldEntry {
	byName := map[string]*types.Var{}
	for f := range str.Fields() {
		byName[f.Name()] = f
	}
	out := make([]fieldEntry, 0, len(sd.Fields))
	for _, fd := range sd.Fields {
		if fd.FlattenedFrom != "" {
			gov, segments, ok := e.resolveEmbedChain(str, fd.FlattenedFrom, fd.Name)
			if !ok {
				continue
			}
			ap := "v"
			for _, s := range segments {
				ap += "." + s.name
			}
			ap += "." + fd.Name
			out = append(out, fieldEntry{decl: fd, gov: gov, accessPath: ap, embedSegments: segments})
			continue
		}
		v, ok := byName[fd.Name]
		if !ok {
			continue
		}
		out = append(out, fieldEntry{decl: fd, gov: v, accessPath: "v." + fd.Name})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].decl.Tag < out[j].decl.Tag })
	return out
}

// resolveEmbedChain walks str through the anonymous embeds named in chain
// (a "."-joined list of embedded type names, mirroring FieldDecl.FlattenedFrom)
// and returns the *types.Var of the leaf field plus per-hop segment metadata.
// Returns (nil, nil, false) if any hop cannot be resolved — that signals a
// validator/codegen mismatch; the field is dropped silently rather than
// crashing the emitter (the validator already gated the schema).
func (e *emitter) resolveEmbedChain(str *types.Struct, chain, leaf string) (*types.Var, []embedSegment, bool) {
	cur := str
	parts := strings.Split(chain, ".")
	segments := make([]embedSegment, 0, len(parts))
	for _, want := range parts {
		var matched *types.Var
		var matchedNamed *types.Named
		var matchedIsPtr bool
		for i := 0; i < cur.NumFields(); i++ {
			f := cur.Field(i)
			if !f.Anonymous() {
				continue
			}
			t := f.Type()
			isPtr := false
			if ptr, ok := t.(*types.Pointer); ok {
				isPtr = true
				t = ptr.Elem()
			}
			named, ok := t.(*types.Named)
			if !ok {
				continue
			}
			if named.Obj().Name() != want {
				continue
			}
			matched = f
			matchedNamed = named
			matchedIsPtr = isPtr
			break
		}
		if matched == nil || matchedNamed == nil {
			return nil, nil, false
		}
		innerStr, ok := matchedNamed.Underlying().(*types.Struct)
		if !ok {
			return nil, nil, false
		}
		segments = append(segments, embedSegment{
			name:      matched.Name(),
			typeExpr:  e.typeExpr(matchedNamed),
			isPointer: matchedIsPtr,
		})
		cur = innerStr
	}
	for i := 0; i < cur.NumFields(); i++ {
		f := cur.Field(i)
		if f.Name() == leaf {
			return f, segments, true
		}
	}
	return nil, nil, false
}

// pointerEmbedGuard returns a Go boolean expression that is true when every
// pointer embed in segments is non-nil — the nil-check the encoder needs
// before touching v.<chain>.<field>. recv is the receiver expression
// (typically "v"). Empty result means no pointer embeds, no guard needed.
//
// Multi-pointer-embed example: segments = [{Mid, *Mid}, {Inner, *Inner}]
// yields `v.Mid != nil && v.Mid.Inner != nil`. A value hop sandwiched
// between pointer hops still uses the cumulative path correctly because
// we accumulate the access prefix as we walk.
func pointerEmbedGuard(segments []embedSegment, recv string) string {
	var parts []string
	prefix := recv
	for _, s := range segments {
		prefix += "." + s.name
		if s.isPointer {
			parts = append(parts, prefix+" != nil")
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " && ")
}

// pointerEmbedAllocs returns the lazy-allocation snippets the decoder
// prepends inside a tag case for a flattened field. Each pointer hop in
// segments emits one `if v.<chain> == nil { v.<chain> = &Type{} }` line so
// the decode target exists by the time we reach the leaf access path.
// recv is the receiver expression (typically "v"). Empty result means no
// pointer embeds — no allocation needed.
func pointerEmbedAllocs(segments []embedSegment, recv string) []string {
	var lines []string
	prefix := recv
	for _, s := range segments {
		prefix += "." + s.name
		if s.isPointer {
			lines = append(lines, fmt.Sprintf("if %s == nil { %s = &%s{} }", prefix, prefix, s.typeExpr))
		}
	}
	return lines
}

// wireType returns the wire type to put in the field key for f. Optional
// fields always use WireLengthDelim so older readers can SkipField past
// an unknown nullable tag without misinterpreting the presence byte.
//
// id_ref fields are the one exception: their wire type matches the
// referenced struct's bin:"1" field directly because the payload is the
// ID value itself (a leaf scalar), not a length-delim-wrapped optional.
// The presence/absence of the entire field is signalled by tag omission,
// not by a presence byte. Switching this wire type is what makes the
// classifier flag id_ref toggles as wire-affecting (Task 4).
//
// Custom-codec (`bin:"N,custom=Name"`) fields delegate to the codec's
// declared wire type. Optional + custom keeps the LENGTH_DELIM envelope
// for the same reason normal optionals do (SkipField safety on an unknown
// nullable tag).
func (e *emitter) wireType(f fieldEntry) (string, error) {
	t := f.gov.Type()
	if f.decl.CycleBreak {
		ptr, _ := t.(*types.Pointer)
		_, idType, err := idRefTargetField(ptr)
		if err != nil {
			// Caller (emitFile) surfaces this; fall through to LengthDelim
			// for the wire-type literal so a later error wins over a panic.
			return "gsbm.WireLengthDelim", nil
		}
		return wireTypeForValue(idType), nil
	}
	if _, ok := t.(*types.Pointer); ok {
		return "gsbm.WireLengthDelim", nil
	}
	if f.decl.Custom != "" {
		decl, ok := e.lookupCodec(f.decl.Custom)
		if !ok {
			return "", codecs.UnregisteredError(f.decl.Custom, e.codecNames())
		}
		ident := codecs.WireIdent(decl.WireType)
		if ident == "" {
			return "", fmt.Errorf("codec %q: invalid WireType %q", decl.Name, decl.WireType)
		}
		return "gsbm." + ident, nil
	}
	return wireTypeForValue(t), nil
}

// lookupCodec resolves a codec name through the emitter's registry. A nil
// registry yields (CodecDecl{}, false) — callers translate that to a
// codec/unregistered diagnostic via codecs.UnregisteredError.
func (e *emitter) lookupCodec(name string) (codecs.CodecDecl, bool) {
	if e.reg == nil {
		return codecs.CodecDecl{}, false
	}
	return e.reg.Lookup(name)
}

// resolveCodec looks up a codec by name and validates that the field's Go
// type matches the codec's declared GoType. Returns codec/unregistered
// when the name is unknown and codec/type-mismatch when the registered
// CodecDecl.GoType differs from the field's underlying type (pointer
// wrapper stripped). The GoType check is skipped when the codec declares
// an empty GoType — the field is documented as informational and the
// emitter treats absent GoType as "trust the call site / let go build
// catch a mismatch."
//
// Type aliases (`type T = X`) are unwrapped via types.Unalias before the
// string comparison: the alias and its target are identical types in Go,
// so the generated codec calls compile fine — rejecting them here would
// be a false-positive. Distinct named types like `type T X` are NOT
// unwrapped (Unalias is a no-op on them) and remain correctly rejected.
func (e *emitter) resolveCodec(codecName string, t types.Type) (codecs.CodecDecl, error) {
	decl, ok := e.lookupCodec(codecName)
	if !ok {
		return decl, codecs.UnregisteredError(codecName, e.codecNames())
	}
	if decl.GoType == "" {
		return decl, nil
	}
	elem := t
	if ptr, ok := elem.(*types.Pointer); ok {
		elem = ptr.Elem()
	}
	if got := types.Unalias(elem).String(); got != decl.GoType {
		return decl, fmt.Errorf("codec/type-mismatch: codec %q expects Go type %q, field has type %q", codecName, decl.GoType, got)
	}
	return decl, nil
}

func (e *emitter) codecNames() []string {
	if e.reg == nil {
		return nil
	}
	return e.reg.Names()
}

// codecCallExpr renders the qualified call expression for a codec function
// (encode or decode), adding the codec package to the import set. The
// returned expression is either bare (`EncodeFoo`) when the codec lives in
// the package being generated, or qualified (`builtins.EncodeFoo`) when it
// lives elsewhere.
func (e *emitter) codecCallExpr(decl codecs.CodecDecl, fnName string) string {
	if decl.PkgImport == "" || decl.PkgImport == e.pkg.Path() {
		return fnName
	}
	// addImport never returns "" here: the same-package short-circuit above
	// is its only "" path.
	return e.addImport(decl.PkgImport) + "." + fnName
}

// idRefTargetField locates the bin:"1" field of the named struct pointed
// to by ptr, returning its field name and Go type. Delegates to the
// shared gsbmschema lookup so discover and codegen agree on the rules.
func idRefTargetField(ptr *types.Pointer) (string, types.Type, error) {
	f, t, err := gsbmschema.LookupIDRefField(ptr)
	if err != nil {
		return "", nil, err
	}
	return f.Name(), t, nil
}

// wireTypeForValue returns the wire type for a non-pointer value type.
// Named-not-struct types unwrap to their underlying primitive — the field
// key MUST match the actual body encoding, otherwise SkipField on an
// unknown tag desyncs the parser past it.
func wireTypeForValue(t types.Type) string {
	switch tt := t.(type) {
	case *types.Basic:
		switch tt.Kind() {
		case types.Float32:
			return "gsbm.WireFixed32"
		case types.Float64:
			return "gsbm.WireFixed64"
		case types.String:
			return "gsbm.WireLengthDelim"
		}
		return "gsbm.WireVarint"
	case *types.Named:
		if _, ok := tt.Underlying().(*types.Struct); ok {
			return "gsbm.WireLengthDelim"
		}
		return wireTypeForValue(tt.Underlying())
	case *types.Slice, *types.Array, *types.Map:
		return "gsbm.WireLengthDelim"
	}
	return "gsbm.WireLengthDelim"
}

// presenceFieldName is the conventional field name the user must add to a
// //gsbm:track-presence struct. It carries the bitmap of decoded tags so
// post-decode `FieldPresent(tag)` can read directly off the receiver rather
// than going through the global sidecar. The field MUST carry `bin:"-"` so
// the schema discoverer skips it, and its type MUST be `[K]uint64` with K
// large enough to cover the struct's max in-range tag.
const presenceFieldName = "gsbmPresent"

// findTrackPresenceField returns the size K of the user-declared
// `gsbmPresent [K]uint64` field on a //gsbm:track-presence struct, or an
// error explaining what the user must add if the field is missing or has
// the wrong type. minK is the minimum K required to cover every in-range
// tag on the struct (computed by the caller from the max field tag). The
// field is found by scanning str directly because writableFields filters
// `bin:"-"` fields out of the schema-driven list — gsbmPresent has no bin
// tag and would otherwise be invisible to the emitter.
func findTrackPresenceField(named *types.Named, str *types.Struct, minK int) (k int, err error) {
	name := named.Obj().Name()
	var fv *types.Var
	for f := range str.Fields() {
		if f.Name() == presenceFieldName {
			fv = f
			break
		}
	}
	if fv == nil {
		return 0, fmt.Errorf(
			"%s: //gsbm:track-presence requires a `%s [%d]uint64 \"bin:\\\"-\\\"\"` field on the struct (add it manually so the marker can store presence bits)",
			name, presenceFieldName, minK)
	}
	arr, ok := fv.Type().(*types.Array)
	if !ok {
		return 0, fmt.Errorf(
			"%s.%s: //gsbm:track-presence requires field type `[%d]uint64`, got %s",
			name, presenceFieldName, minK, fv.Type().String())
	}
	elem, ok := arr.Elem().(*types.Basic)
	if !ok || elem.Kind() != types.Uint64 {
		return 0, fmt.Errorf(
			"%s.%s: //gsbm:track-presence requires field type `[%d]uint64`, got [%d]%s",
			name, presenceFieldName, minK, arr.Len(), arr.Elem().String())
	}
	k = int(arr.Len())
	if k < minK {
		return 0, fmt.Errorf(
			"%s.%s: //gsbm:track-presence field size [%d]uint64 is too small for max tag (need at least [%d]uint64)",
			name, presenceFieldName, k, minK)
	}
	return k, nil
}

// emitReset writes `func (v *T) Reset()`. The body is capacity-preserving:
// slices truncate to length 0 (cap retained for the next decode pass);
// maps go through clear (Go 1.21+) so backing buckets stay; pointers go
// to nil; required nested structs recurse via their own Reset; primitive
// fields are zeroed so a tag missing from the next blob lands as zero.
// The trailing gsbm.ClearPresence drops the sidecar bitmap so callers
// re-querying FieldPresent after Reset see no stale presence bits. For
// //gsbm:track-presence types the user's `gsbmPresent` array is zeroed
// instead — the sidecar is not consulted on the opt-in path.
//
// Order: inner Reset before outer truncation, per the plan, so the inner
// struct sees a fully-formed receiver before the slice header collapses.
func (e *emitter) emitReset(out io.Writer, named *types.Named, str *types.Struct, sd *gsbmschema.StructDecl) error {
	name := named.Obj().Name()
	fp(out, "func (v *%s) Reset() {\n", name)
	// Pointer embeds are reset by dropping the pointee outright: the
	// embedded type itself isn't in the schema (the validator flattens it
	// away), so it has no generated Reset method to call. We scan every
	// flattened field's chain for pointer segments (including those nested
	// inside value embeds, e.g. `Outer{Mid}` over `Mid{*Base}`) and emit
	// `v.<path-to-pointer-hop> = nil` once per unique hop. Each flattened
	// field whose chain crosses any pointer hop is then skipped in the
	// per-field reset below — the nil pointer covers it, and dereferencing
	// through a nil hop would panic.
	//
	// Only the OUTERMOST pointer hop on each chain is recorded. Niling the
	// outer hop drops every deeper hop with it; emitting a nested hop
	// after its ancestor is nil would dereference nil and panic. The
	// outer hop may itself already be nil on entry (Reset on a
	// partially-populated receiver), which makes "deepest-first" niling
	// equally unsafe — only ancestor suppression is correct.
	fields := e.writableFields(str, sd)
	ptrPrefixes := map[string]bool{}
	var orderedPtrPrefixes []string
	fieldHasPtrHop := make([]bool, len(fields))
	for i, f := range fields {
		prefix := "v"
		outermost := ""
		for _, s := range f.embedSegments {
			prefix += "." + s.name
			if s.isPointer {
				fieldHasPtrHop[i] = true
				if outermost == "" {
					outermost = prefix
				}
			}
		}
		if outermost != "" && !ptrPrefixes[outermost] {
			ptrPrefixes[outermost] = true
			orderedPtrPrefixes = append(orderedPtrPrefixes, outermost)
		}
	}
	for _, p := range orderedPtrPrefixes {
		fp(out, "\t%s = nil\n", p)
	}
	for i, f := range fields {
		if fieldHasPtrHop[i] {
			continue
		}
		expr := f.accessPath
		if f.decl.Custom != "" {
			if _, isPtr := f.gov.Type().(*types.Pointer); isPtr {
				fp(out, "\t%s = nil\n", expr)
			} else {
				fp(out, "\t%s = *new(%s)\n", expr, e.typeExpr(f.gov.Type()))
			}
			continue
		}
		if err := e.emitFieldReset(out, expr, f.gov.Type()); err != nil {
			return fmt.Errorf("%s.%s: %w", name, f.decl.Name, err)
		}
	}
	if sd.TrackPresence {
		// Compute minimum required size from the schema's max tag (same rule
		// emitUnmarshal uses). The user's declared K may be larger; zero the
		// whole array so a re-decode sees a clean bitmap. findTrackPresenceField
		// already ran during emitUnmarshal — by the time emitReset is called
		// the field exists with the correct shape, so the lookup here cannot
		// fail. Falling back to MaxTrackedTag bits would over-allocate the
		// reset width.
		minK := 1
		var maxTag uint32
		for _, f := range fields {
			if f.decl.Tag > maxTag && f.decl.Tag <= gsbm.MaxTrackedTag {
				maxTag = f.decl.Tag
			}
		}
		if maxTag > 0 {
			minK = int((maxTag + 63) / 64)
		}
		k, err := findTrackPresenceField(named, str, minK)
		if err != nil {
			return err
		}
		fp(out, "\tv.%s = [%d]uint64{}\n", presenceFieldName, k)
	} else {
		fp(out, "\tgsbm.ClearPresence(v)\n")
	}
	fp(out, "}\n")
	return nil
}

// emitFieldPresent writes `func (v *T) FieldPresent(tag uint32) bool`.
// The default body delegates to the package-level sidecar; the sidecar
// returns false for any tag that was never marked, including unknown tags
// and tags above gsbm.MaxTrackedTag. For //gsbm:track-presence types the
// body reads directly from the user-declared `v.gsbmPresent` array — no
// sidecar lookup, no synchronization — so post-decode presence queries
// have no allocation or map cost.
func (e *emitter) emitFieldPresent(out io.Writer, named *types.Named, str *types.Struct, sd *gsbmschema.StructDecl) error {
	name := named.Obj().Name()
	if sd != nil && sd.TrackPresence {
		var maxTag uint32
		for _, fd := range sd.Fields {
			if fd.Tag > maxTag && fd.Tag <= gsbm.MaxTrackedTag {
				maxTag = fd.Tag
			}
		}
		minK := 1
		if maxTag > 0 {
			minK = int((maxTag + 63) / 64)
		}
		k, err := findTrackPresenceField(named, str, minK)
		if err != nil {
			return err
		}
		cap := uint32(k) * 64
		fp(out, "func (v *%s) FieldPresent(tag uint32) bool {\n", name)
		fp(out, "\tif tag == 0 || tag > %d { return false }\n", cap)
		fp(out, "\tidx := (tag - 1) >> 6\n")
		fp(out, "\tbit := (tag - 1) & 63\n")
		fp(out, "\treturn v.%s[idx]&(1<<bit) != 0\n", presenceFieldName)
		fp(out, "}\n")
		return nil
	}
	fp(out, "func (v *%s) FieldPresent(tag uint32) bool {\n", name)
	fp(out, "\treturn gsbm.IsPresent(v, tag)\n")
	fp(out, "}\n")
	return nil
}

func (e *emitter) emitFieldReset(out io.Writer, expr string, t types.Type) error {
	switch tt := t.(type) {
	case *types.Pointer:
		// Drop the pointee; a pooled root keeps the parent struct, not its
		// nullable children, since the decoder always allocates fresh.
		fp(out, "\t%s = nil\n", expr)
		return nil
	case *types.Basic:
		fp(out, "\t%s = %s\n", expr, primitiveZero(tt))
		return nil
	case *types.Named:
		if _, ok := tt.Underlying().(*types.Struct); ok {
			// Recurse into the nested struct's generated Reset.
			fp(out, "\t%s.Reset()\n", expr)
			return nil
		}
		// Named-byte-slice (e.g. `type ID []byte`) — preserve capacity
		// the same way the plain `[]byte` branch does; slicing a named
		// slice type returns the same named type so the assignment is
		// type-clean.
		if s, ok := tt.Underlying().(*types.Slice); ok && isByteType(s.Elem()) {
			fp(out, "\t%s = %s[:0]\n", expr, expr)
			return nil
		}
		// Named slice alias over a non-byte element (`type ItemList []Item`,
		// `type ItemPtrList []*Item`): mirror the value-form *types.Slice
		// reset so capacity is preserved across re-decodes. Struct elements
		// recurse into their generated Reset. Pointer and nested-composite
		// (slice / map) elements get cleared first so cap-retained values
		// are eligible for GC; otherwise a pooled DecodeInto cycle would
		// keep every previously-decoded inner map / slice / pointee
		// reachable through the backing array.
		if s, ok := tt.Underlying().(*types.Slice); ok {
			switch s.Elem().(type) {
			case *types.Pointer, *types.Slice, *types.Map:
				fp(out, "\tclear(%s)\n", expr)
				fp(out, "\t%s = %s[:0]\n", expr, expr)
				return nil
			}
			if named, ok := s.Elem().(*types.Named); ok {
				if _, isStruct := named.Underlying().(*types.Struct); isStruct {
					fp(out, "\tfor i := range %s { %s[i].Reset() }\n", expr, expr)
				}
			}
			fp(out, "\t%s = %s[:0]\n", expr, expr)
			return nil
		}
		// Named-not-struct (e.g. type Label string) — zero via the
		// underlying primitive's literal, converted to the named type so
		// the assignment is type-clean.
		under, ok := tt.Underlying().(*types.Basic)
		if !ok {
			fp(out, "\t%s = %s{}\n", expr, e.typeExpr(tt))
			return nil
		}
		fp(out, "\t%s = %s(%s)\n", expr, e.typeExpr(tt), primitiveZero(under))
		return nil
	case *types.Slice:
		if isByteType(tt.Elem()) {
			fp(out, "\t%s = %s[:0]\n", expr, expr)
			return nil
		}
		// Pointer- and nested-composite-element slices need their slots
		// cleared before truncation so cap-retained values are eligible
		// for GC. Without this, a pooled DecodeInto cycle that decodes a
		// large `[]*T` / `[]map[K]V` / `[][]V` then a smaller one would
		// keep the previous pointees / inner maps / inner slices
		// reachable through the backing array indefinitely.
		switch tt.Elem().(type) {
		case *types.Pointer, *types.Slice, *types.Map:
			fp(out, "\tclear(%s)\n", expr)
			fp(out, "\t%s = %s[:0]\n", expr, expr)
			return nil
		}
		// For slice-of-struct, recurse into each element's Reset before
		// collapsing the slice. The elements stay allocated within cap.
		if named, ok := tt.Elem().(*types.Named); ok {
			if _, isStruct := named.Underlying().(*types.Struct); isStruct {
				fp(out, "\tfor i := range %s { %s[i].Reset() }\n", expr, expr)
			}
		}
		fp(out, "\t%s = %s[:0]\n", expr, expr)
		return nil
	case *types.Map:
		// clear preserves the map's bucket allocation; the decoder will
		// repopulate.
		fp(out, "\tclear(%s)\n", expr)
		return nil
	case *types.Array:
		// Fixed-size arrays: zero in place by assigning a zero value.
		fp(out, "\t%s = [%d]%s{}\n", expr, tt.Len(), e.typeExpr(tt.Elem()))
		return nil
	}
	return fmt.Errorf("unsupported reset type %T", t)
}

func primitiveZero(b *types.Basic) string {
	switch b.Kind() {
	case types.Bool:
		return "false"
	case types.String:
		return `""`
	case types.Float32, types.Float64:
		return "0"
	default:
		return "0"
	}
}

// emitMarshal writes `func (v *T) MarshalGSBM(w *gsbm.Writer) error { ... }`.
func (e *emitter) emitMarshal(out io.Writer, named *types.Named, str *types.Struct, sd *gsbmschema.StructDecl) error {
	name := named.Obj().Name()
	e.currentStructFQN = structFQN(named)
	defer func() { e.currentStructFQN = "" }()
	fp(out, "func (v *%s) MarshalGSBM(w *gsbm.Writer) error {\n", name)
	for _, f := range e.writableFields(str, sd) {
		if f.decl.Deprecated && !f.decl.CompatWrite {
			// Deprecated fields are read-only; never emit on the wire.
			fp(out, "\t// tag %d %s: deprecated, not written\n", f.decl.Tag, f.decl.Name)
			continue
		}
		if f.decl.CompatWrite {
			fp(out, "\t// tag %d %s: deprecated, compat_write (rollback window)\n", f.decl.Tag, f.decl.Name)
		} else {
			fp(out, "\t// tag %d %s\n", f.decl.Tag, f.decl.Name)
		}
		if err := e.emitFieldEncode(out, f); err != nil {
			return fmt.Errorf("%s.%s: %w", name, f.decl.Name, err)
		}
	}
	fp(out, "\treturn w.Err()\n}\n")
	return nil
}

// emitUnmarshal writes `func (v *T) UnmarshalGSBM(r *gsbm.Reader) error`.
// gsbm.ClearPresence is emitted at the top so a re-decode into the same
// receiver starts with an empty sidecar presence mask (for legacy callers
// that still query via gsbm.IsPresent). After each known-tag case
// successfully decodes its value, a local bitmap bit is set on `present`
// instead of routing through the global sidecar — this kills the per-tag
// MarkPresent allocations that dominate decode profiles on nested graphs.
// In default mode `present` dies with the call: gsbm.IsPresent(v, tag)
// returns false post-decode, matching the documented contract. The default
// branch (unknown tag) does not touch the bitmap — only declared tags are
// tracked.
func (e *emitter) emitUnmarshal(out io.Writer, named *types.Named, str *types.Struct, sd *gsbmschema.StructDecl) error {
	name := named.Obj().Name()
	fields := e.writableFields(str, sd)
	// N is sized from the largest in-range tag (>MaxTrackedTag is silently
	// untracked, mirroring the sidecar's cap). Floor at 1 so the `var present`
	// declaration is always syntactically valid even for zero-field structs.
	var maxTrackedTag uint32
	for _, f := range fields {
		if f.decl.Tag > maxTrackedTag && f.decl.Tag <= gsbm.MaxTrackedTag {
			maxTrackedTag = f.decl.Tag
		}
	}
	n := int((maxTrackedTag + 63) / 64)
	if n < 1 {
		n = 1
	}
	// For //gsbm:track-presence types the bitmap lives on the receiver
	// (v.gsbmPresent), not on a stack-local that dies with the call. Bind
	// `present` to the receiver field via an addressable alias so the case
	// bodies below stay identical (`present[idx] |= 1 << bit`) between the
	// default and tracked paths — only the declaration differs. The user
	// must have declared the field; findTrackPresenceField surfaces a clear
	// error if not.
	tracked := sd != nil && sd.TrackPresence
	fp(out, "func (v *%s) UnmarshalGSBM(r *gsbm.Reader) error {\n", name)
	if tracked {
		// findTrackPresenceField returns the user's declared K; use it (not
		// the computed minimum n) when zero-initializing so the literal's
		// type matches the field's type. The user is free to declare a K
		// larger than strictly necessary; the emitted reset literal must
		// match that declaration exactly to compile.
		k, err := findTrackPresenceField(named, str, n)
		if err != nil {
			return err
		}
		// Zero the receiver bitmap before decode so a re-decode into the same
		// receiver starts with no stale presence bits, matching the default
		// path's gsbm.ClearPresence semantics.
		fp(out, "\tv.%s = [%d]uint64{}\n", presenceFieldName, k)
		fp(out, "\tpresent := &v.%s\n", presenceFieldName)
	} else {
		fp(out, "\tvar present [%d]uint64\n", n)
		fp(out, "\tgsbm.ClearPresence(v)\n")
	}
	fp(out, "\tfor r.HasMore() {\n")
	fp(out, "\t\ttag, wt, err := r.ReadTag()\n")
	fp(out, "\t\tif err != nil { return err }\n")
	fp(out, "\t\tswitch tag {\n")
	for _, f := range fields {
		fp(out, "\t\tcase %d:\n", f.decl.Tag)
		// Validate the on-wire wire type matches what the schema says this
		// tag carries. The spec (§3.2) forbids skipping past a known tag
		// with the wrong wire type — mismatch is corruption, not a future
		// schema. SkipField on a known-tag mismatch can desync the parser
		// (e.g., wt=VARINT on a slice tag would consume a varint then walk
		// off into the body).
		wt, err := e.wireType(f)
		if err != nil {
			return fmt.Errorf("%s.%s: %w", name, f.decl.Name, err)
		}
		fp(out, "\t\t\tif wt != %s { return gsbm.ErrWrongWireType }\n", wt)
		if err := e.emitFieldDecode(out, f); err != nil {
			return fmt.Errorf("%s.%s: %w", name, f.decl.Name, err)
		}
		if f.decl.Tag > 0 && f.decl.Tag <= gsbm.MaxTrackedTag {
			idx := (f.decl.Tag - 1) / 64
			bit := (f.decl.Tag - 1) % 64
			fp(out, "\t\t\tpresent[%d] |= 1 << %d\n", idx, bit)
		}
	}
	fp(out, "\t\tdefault:\n")
	fp(out, "\t\t\tif err := r.SkipField(wt); err != nil { return err }\n")
	fp(out, "\t\t}\n") // switch
	fp(out, "\t}\n")   // for
	// Silence unused-var when the struct has zero fields (or every field is
	// above MaxTrackedTag, both of which produce a switch body that never
	// touches the bitmap). In all other cases the |= writes count as a use,
	// but the underscore assignment is free.
	fp(out, "\t_ = present\n")
	fp(out, "\treturn r.Err()\n}\n")
	return nil
}

// emitFieldEncode emits the encode body for one field, key first.
func (e *emitter) emitFieldEncode(out io.Writer, f fieldEntry) error {
	tag := f.decl.Tag
	wt, err := e.wireType(f)
	if err != nil {
		return err
	}
	expr := f.accessPath
	t := f.gov.Type()

	// Pointer-embed: if any embed in the chain is a pointer, wrap the entire
	// encode body (including WriteTag) in a nil-check so a nil-Base produces
	// no orphan tag keys on the wire. Per-field guards are simple but
	// repetitive; the alternative — grouping fields by embed and emitting one
	// wrapper around the group — would tangle MarshalGSBM's straight-line
	// shape and gain nothing on correctness.
	if guard := pointerEmbedGuard(f.embedSegments, "v"); guard != "" {
		fp(out, "\tif %s {\n", guard)
		defer fp(out, "\t}\n")
	}

	if f.decl.CycleBreak {
		ptr, _ := t.(*types.Pointer)
		idName, idType, err := idRefTargetField(ptr)
		if err != nil {
			return err
		}
		// id_ref omits the field entirely when nil — there is no presence
		// byte. The decoder restores nil simply by not entering the case
		// branch. When non-nil, the value-payload is the referenced
		// struct's bin:"1" field encoded as a leaf scalar, with the field
		// key carrying that scalar's natural wire type (string→LengthDelim,
		// int→Varint). The caller is responsible for hydrating other
		// fields after decode; v1 ships ID-only.
		fp(out, "\tif %s != nil {\n", expr)
		fp(out, "\t\tw.WriteTag(%d, %s)\n", tag, wt)
		if err := e.emitValueEncode(out, expr+"."+idName, idType, true, 0); err != nil {
			return err
		}
		fp(out, "\t}\n")
		return nil
	}
	if f.decl.Custom != "" {
		return e.emitCustomCodecEncode(out, tag, wt, expr, t, f.decl.Custom)
	}
	if ptr, ok := t.(*types.Pointer); ok {
		return e.emitOptionalEncode(out, tag, wt, expr, ptr.Elem())
	}
	fp(out, "\tw.WriteTag(%d, %s)\n", tag, wt)
	return e.emitValueEncode(out, expr, t, true, 0)
}

// emitCustomCodecEncode renders the encode side for a `bin:"N,custom=Name"`
// field. The schema validator has already short-circuited normal type
// traversal for this field; here we resolve the codec name to a CodecDecl
// and emit a direct call to the encode function.
//
// For value-typed fields we emit `WriteTag(tag, <codec wire type>)` then
// `codecs.<EncodeFn>(w, v.Field)`. For pointer-typed (`*T`) fields we
// preserve the spec §5.1 envelope: outer key carries LENGTH_DELIM, body
// is a presence byte followed (on PresenceNonZero) by the codec output.
// PresenceZero is forbidden for non-builtin payloads per the spec — only
// Nil / NonZero appear on the wire.
func (e *emitter) emitCustomCodecEncode(out io.Writer, tag uint32, wt, expr string, t types.Type, codecName string) error {
	decl, err := e.resolveCodec(codecName, t)
	if err != nil {
		return err
	}
	switch decl.Kind() {
	case codecs.CodecKindAnalytic:
		call := e.codecCallExpr(decl, decl.EncodeFn)
		if _, ok := t.(*types.Pointer); ok {
			fp(out, "\tw.WriteTag(%d, %s)\n", tag, wt)
			fp(out, "\t{\n")
			fp(out, "\t\tm := w.BeginLengthDelim()\n")
			fp(out, "\t\tif %s == nil {\n", expr)
			fp(out, "\t\t\tw.WritePresenceNil()\n")
			fp(out, "\t\t} else {\n")
			fp(out, "\t\t\tw.WritePresenceNonZero()\n")
			fp(out, "\t\t\tif err := %s(w, *%s); err != nil { return err }\n", call, expr)
			fp(out, "\t\t}\n")
			fp(out, "\t\tw.EndLengthDelim(m)\n")
			fp(out, "\t}\n")
			return nil
		}
		fp(out, "\tw.WriteTag(%d, %s)\n", tag, wt)
		fp(out, "\tif err := %s(w, %s); err != nil { return err }\n", call, expr)
		return nil
	case codecs.CodecKindMaterializing:
		call := e.codecCallExpr(decl, decl.EmitFn)
		cs := e.callsiteFor(tag)
		if _, ok := t.(*types.Pointer); ok {
			fp(out, "\tw.WriteTag(%d, %s)\n", tag, wt)
			fp(out, "\t{\n")
			fp(out, "\t\tm := w.BeginLengthDelim()\n")
			fp(out, "\t\tif %s == nil {\n", expr)
			fp(out, "\t\t\tw.WritePresenceNil()\n")
			fp(out, "\t\t} else {\n")
			fp(out, "\t\t\tw.WritePresenceNonZero()\n")
			fp(out, "\t\t\tif err := %s(w, *%s, %s); err != nil { return err }\n", call, expr, cs)
			fp(out, "\t\t}\n")
			fp(out, "\t\tw.EndLengthDelim(m)\n")
			fp(out, "\t}\n")
			return nil
		}
		fp(out, "\tw.WriteTag(%d, %s)\n", tag, wt)
		fp(out, "\tif err := %s(w, %s, %s); err != nil { return err }\n", call, expr, cs)
		return nil
	default:
		return fmt.Errorf("codec %q: unrecognized codec kind (registry returned a decl with neither analytic (SizeFn+EncodeFn) nor materializing (EmitFn) shape — this is an emitter/registry contract bug)", codecName)
	}
}

// emitOptionalEncode wraps the field in the standard optional layout:
// WireLengthDelim outer key, length-delim body containing presence byte
// and (when present) the value. WireLengthDelim is required so older
// readers can safely SkipField past an unknown optional tag.
func (e *emitter) emitOptionalEncode(out io.Writer, tag uint32, wt, expr string, elem types.Type) error {
	fp(out, "\tw.WriteTag(%d, %s)\n", tag, wt)
	fp(out, "\t{\n")
	fp(out, "\t\tm := w.BeginLengthDelim()\n")
	// *[]byte takes the builtin-primitive path (zero-elide eligible per
	// spec §5.1) but routes through WriteBytes — emitPrimitiveEncode does
	// not handle slices, and the zero check is len-based since []byte
	// equality with nil only matches a nil-slice, not an empty one.
	if s, ok := elem.(*types.Slice); ok && isByteType(s.Elem()) {
		fp(out, "\t\tswitch {\n")
		fp(out, "\t\tcase %s == nil:\n", expr)
		fp(out, "\t\t\tw.WritePresenceNil()\n")
		fp(out, "\t\tcase len(*%s) == 0:\n", expr)
		fp(out, "\t\t\tw.WritePresenceZero()\n")
		fp(out, "\t\tdefault:\n")
		fp(out, "\t\t\tw.WritePresenceNonZero()\n")
		fp(out, "\t\t\tw.WriteBytes(*%s)\n", expr)
		fp(out, "\t\t}\n")
		fp(out, "\t\tw.EndLengthDelim(m)\n")
		fp(out, "\t}\n")
		return nil
	}
	if isBuiltinPrimitive(elem) {
		zeroExpr := zeroValue(elem)
		fp(out, "\t\tswitch {\n")
		fp(out, "\t\tcase %s == nil:\n", expr)
		fp(out, "\t\t\tw.WritePresenceNil()\n")
		fp(out, "\t\tcase *%s == %s:\n", expr, zeroExpr)
		fp(out, "\t\t\tw.WritePresenceZero()\n")
		fp(out, "\t\tdefault:\n")
		fp(out, "\t\t\tw.WritePresenceNonZero()\n")
		if err := e.emitPrimitiveEncode(out, "*"+expr, elem); err != nil {
			return err
		}
		fp(out, "\t\t}\n")
	} else {
		// Non-builtin optional: zero-elide is forbidden by the spec, so
		// only Nil / NonZero are emitted on the wire.
		fp(out, "\t\tif %s == nil {\n", expr)
		fp(out, "\t\t\tw.WritePresenceNil()\n")
		fp(out, "\t\t} else {\n")
		fp(out, "\t\t\tw.WritePresenceNonZero()\n")
		// Inline the named-struct body directly inside the outer length-
		// delim — no second wrapper. The decoder uses HasMore against the
		// outer bound to read fields to the end.
		switch et := elem.(type) {
		case *types.Named:
			if _, ok := et.Underlying().(*types.Struct); ok {
				fp(out, "\t\t\tif err := %s.MarshalGSBM(w); err != nil { return err }\n", expr)
			} else {
				if err := e.emitValueEncode(out, "*"+expr, elem, false, 0); err != nil {
					return err
				}
			}
		default:
			if err := e.emitValueEncode(out, "*"+expr, elem, false, 0); err != nil {
				return err
			}
		}
		fp(out, "\t\t}\n")
	}
	fp(out, "\t\tw.EndLengthDelim(m)\n")
	fp(out, "\t}\n")
	return nil
}

// emitValueEncode emits encoder code for a value of type t accessed via
// expr. emitsKey indicates the key was already written by the caller.
// depth is the composite nesting depth (0 = top-level field, >0 = inside
// a slice element or map value); recursing emitters suffix per-level
// locals so the nested code doesn't shadow names the outer scope still
// references (e.g. `slice[i]` referencing an outer `i` while a nested
// slice runs its own loop).
func (e *emitter) emitValueEncode(out io.Writer, expr string, t types.Type, _ bool, depth int) error {
	switch tt := t.(type) {
	case *types.Basic:
		return e.emitPrimitiveEncode(out, expr, tt)
	case *types.Named:
		if _, ok := tt.Underlying().(*types.Struct); ok {
			// Named-struct length-delim is its own `{ }` scope, so `m`
			// shadows safely at any depth — no need to suffix and risk
			// drifting golden output for fields whose value is a named
			// struct nested inside an outer map/slice.
			fp(out, "\t{\n")
			fp(out, "\t\tm := w.BeginLengthDelim()\n")
			fp(out, "\t\tif err := %s.MarshalGSBM(w); err != nil { return err }\n", expr)
			fp(out, "\t\tw.EndLengthDelim(m)\n")
			fp(out, "\t}\n")
			return nil
		}
		// Named slice alias (`type ItemList []Item`, `type ItemPtrList []*Item`):
		// the wire bytes match the underlying slice form, so route the encode
		// through emitSliceEncode with the underlying slice. The alias-typed
		// expr is left as-is because `len`, `range`, and `expr[i]` all work
		// uniformly on named slice types.
		if s, ok := tt.Underlying().(*types.Slice); ok && !isByteType(s.Elem()) {
			return e.emitSliceEncode(out, expr, s, depth)
		}
		// Defined-but-not-struct named type (e.g. type ID string): unwrap
		// to its underlying primitive for encoding.
		return e.emitValueEncode(out, fmt.Sprintf("(%s)(%s)", e.typeExpr(tt.Underlying()), expr), tt.Underlying(), false, depth)
	case *types.Slice:
		if isByteType(tt.Elem()) {
			fp(out, "\tw.WriteBytes(%s)\n", expr)
			return nil
		}
		return e.emitSliceEncode(out, expr, tt, depth)
	case *types.Map:
		return e.emitMapEncode(out, expr, tt, depth)
	}
	return fmt.Errorf("unsupported encode type %T", t)
}

func (e *emitter) emitPrimitiveEncode(out io.Writer, expr string, t types.Type) error {
	b, ok := t.(*types.Basic)
	if !ok {
		return fmt.Errorf("emitPrimitiveEncode: %T not a basic type", t)
	}
	switch b.Kind() {
	case types.Bool:
		fp(out, "\tw.WriteBool(%s)\n", expr)
	case types.String:
		fp(out, "\tw.WriteString(%s)\n", expr)
	case types.Int8, types.Int16, types.Int32, types.Int64:
		fp(out, "\tw.WriteVarint(int64(%s))\n", expr)
	case types.Int:
		// `int` is platform-sized. Bound by 32-bit range on encode so blobs
		// are portable to a 32-bit reader (which the decoder also enforces).
		fp(out, "\tif int64(%s) < math.MinInt32 || int64(%s) > math.MaxInt32 { return gsbm.ErrIntegerOverflow }\n", expr, expr)
		fp(out, "\tw.WriteVarint(int64(%s))\n", expr)
		e.addImport("math")
	case types.Uint8, types.Uint16, types.Uint32, types.Uint64:
		fp(out, "\tw.WriteUvarint(uint64(%s))\n", expr)
	case types.Uint, types.Uintptr:
		// Platform-sized: bound to 32-bit so the wire is portable.
		fp(out, "\tif uint64(%s) > math.MaxUint32 { return gsbm.ErrIntegerOverflow }\n", expr)
		fp(out, "\tw.WriteUvarint(uint64(%s))\n", expr)
		e.addImport("math")
	case types.Float32:
		fp(out, "\tw.WriteFloat32(%s)\n", expr)
	case types.Float64:
		fp(out, "\tw.WriteFloat64(%s)\n", expr)
	default:
		return fmt.Errorf("unsupported basic kind %v", b.Kind())
	}
	return nil
}

func (e *emitter) emitSliceEncode(out io.Writer, expr string, t *types.Slice, depth int) error {
	elemT := t.Elem()
	marker := nm("m", depth)
	idx := nm("i", depth)
	inner := nm("inner", depth)
	fp(out, "\t{\n")
	fp(out, "\t\t%s := w.BeginLengthDelim()\n", marker)
	fp(out, "\t\tw.WriteUvarint(uint64(len(%s)))\n", expr)
	fp(out, "\t\tfor %s := range %s {\n", idx, expr)
	elemExpr := fmt.Sprintf("%s[%s]", expr, idx)
	// Slice-of-pointer-to-named-struct: each element is encoded per spec
	// §5.1 as a length-delim envelope carrying a presence byte and (when
	// non-nil) the element body. Zero-elide is forbidden for non-builtin
	// payloads, so only PresenceNil / PresenceNonZero appear on the wire.
	if ptr, ok := elemT.(*types.Pointer); ok {
		if named, ok := ptr.Elem().(*types.Named); ok {
			if _, isStruct := named.Underlying().(*types.Struct); isStruct {
				fp(out, "\t\t\t%s := w.BeginLengthDelim()\n", inner)
				fp(out, "\t\t\tif %s == nil {\n", elemExpr)
				fp(out, "\t\t\t\tw.WritePresenceNil()\n")
				fp(out, "\t\t\t} else {\n")
				fp(out, "\t\t\t\tw.WritePresenceNonZero()\n")
				fp(out, "\t\t\t\tif err := %s.MarshalGSBM(w); err != nil { return err }\n", elemExpr)
				fp(out, "\t\t\t}\n")
				fp(out, "\t\t\tw.EndLengthDelim(%s)\n", inner)
				fp(out, "\t\t}\n")
				fp(out, "\t\tw.EndLengthDelim(%s)\n", marker)
				fp(out, "\t}\n")
				return nil
			}
		}
	}
	if named, ok := elemT.(*types.Named); ok {
		if _, ok := named.Underlying().(*types.Struct); ok {
			fp(out, "\t\t\t%s := w.BeginLengthDelim()\n", inner)
			fp(out, "\t\t\tif err := %s.MarshalGSBM(w); err != nil { return err }\n", elemExpr)
			fp(out, "\t\t\tw.EndLengthDelim(%s)\n", inner)
			fp(out, "\t\t}\n")
			fp(out, "\t\tw.EndLengthDelim(%s)\n", marker)
			fp(out, "\t}\n")
			return nil
		}
	}
	if err := e.emitValueEncode(out, elemExpr, elemT, false, depth+1); err != nil {
		return err
	}
	fp(out, "\t\t}\n")
	fp(out, "\t\tw.EndLengthDelim(%s)\n", marker)
	fp(out, "\t}\n")
	return nil
}

func (e *emitter) emitMapEncode(out io.Writer, expr string, t *types.Map, depth int) error {
	if !isPrimitiveKey(t.Key()) {
		return fmt.Errorf("map key must be primitive or string")
	}
	keyExpr := e.typeExpr(t.Key())
	marker := nm("m", depth)
	keysVar := nm("keys", depth)
	kVar := nm("k", depth)
	vvVar := nm("vv", depth)
	fp(out, "\t{\n")
	fp(out, "\t\t%s := w.BeginLengthDelim()\n", marker)
	fp(out, "\t\tw.WriteUvarint(uint64(len(%s)))\n", expr)
	// Sort keys for deterministic output. Same logical map => same bytes,
	// so callers may take a stable hash of the encoded blob (audit, dedup,
	// content-addressed checkpointing).
	fp(out, "\t\t%s := make([]%s, 0, len(%s))\n", keysVar, keyExpr, expr)
	fp(out, "\t\tfor %s := range %s { %s = append(%s, %s) }\n", kVar, expr, keysVar, keysVar, kVar)
	if err := emitKeySort(out, t.Key(), keysVar); err != nil {
		return err
	}
	e.addImport("sort")
	fp(out, "\t\tfor _, %s := range %s {\n", kVar, keysVar)
	fp(out, "\t\t\t%s := %s[%s]\n", vvVar, expr, kVar)
	// Named primitive keys cast through their underlying so WriteString /
	// WriteBool accept them and the wire bytes match a builtin-keyed map
	// of the same underlying primitive byte-for-byte (spec §5.3).
	keyT := t.Key()
	keyExprStr := kVar
	if named, ok := keyT.(*types.Named); ok {
		keyT = named.Underlying()
		keyExprStr = fmt.Sprintf("(%s)(%s)", e.typeExpr(keyT), kVar)
	}
	if err := e.emitPrimitiveEncode(out, keyExprStr, keyT); err != nil {
		return err
	}
	if err := e.emitValueEncode(out, vvVar, t.Elem(), false, depth+1); err != nil {
		return err
	}
	fp(out, "\t\t}\n")
	fp(out, "\t\tw.EndLengthDelim(%s)\n", marker)
	fp(out, "\t}\n")
	return nil
}

// emitKeySort emits a sort.Slice call on `keys` using the natural ordering
// of the key's underlying primitive. Bool maps are sorted false→true.
// For named string and integer kinds the `<` operator compares directly
// (yielding untyped bool), so the closure body is identical to the builtin
// case; sort.Strings would refuse a []NamedString though, so the string
// branch always uses the closure form for named keys. Named bool is the
// odd one out: `!Flag && Flag` has type Flag, not bool, so the comparator
// must cast both operands through `bool(...)` before returning.
func emitKeySort(out io.Writer, t types.Type, keysVar string) error {
	b, named := basicForKey(t)
	if b == nil {
		return fmt.Errorf("map key must be a basic type or a named type whose underlying is basic")
	}
	switch b.Kind() {
	case types.Bool:
		// `!Flag && Flag` evaluates to type Flag for a named-bool key, but
		// sort.Slice wants a plain bool. Cast through the underlying when
		// the slice element is named.
		if named {
			fp(out, "\t\tsort.Slice(%s, func(i, j int) bool { return !bool(%s[i]) && bool(%s[j]) })\n", keysVar, keysVar, keysVar)
		} else {
			fp(out, "\t\tsort.Slice(%s, func(i, j int) bool { return !%s[i] && %s[j] })\n", keysVar, keysVar, keysVar)
		}
	case types.String:
		if named {
			fp(out, "\t\tsort.Slice(%s, func(i, j int) bool { return %s[i] < %s[j] })\n", keysVar, keysVar, keysVar)
		} else {
			fp(out, "\t\tsort.Strings(%s)\n", keysVar)
		}
	case types.Int, types.Int8, types.Int16, types.Int32, types.Int64,
		types.Uint, types.Uint8, types.Uint16, types.Uint32, types.Uint64, types.Uintptr:
		fp(out, "\t\tsort.Slice(%s, func(i, j int) bool { return %s[i] < %s[j] })\n", keysVar, keysVar, keysVar)
	default:
		return fmt.Errorf("unsortable map key kind %v", b.Kind())
	}
	return nil
}

// basicForKey unwraps a named type once to expose the underlying *types.Basic
// that map-key codegen dispatches on. The second return reports whether the
// caller was given a *types.Named (used by the string branch of emitKeySort
// to pick the comparator-based sort).
func basicForKey(t types.Type) (*types.Basic, bool) {
	if n, ok := t.(*types.Named); ok {
		b, ok := n.Underlying().(*types.Basic)
		if !ok {
			return nil, false
		}
		return b, true
	}
	b, ok := t.(*types.Basic)
	if !ok {
		return nil, false
	}
	return b, false
}

// emitFieldDecode emits the decode case body for one field.
func (e *emitter) emitFieldDecode(out io.Writer, f fieldEntry) error {
	t := f.gov.Type()
	expr := f.accessPath
	// Pointer-embed: lazily allocate each pointer hop in the chain so the
	// decode target exists before the leaf assignment. Allocation happens
	// inside the tag case; the encoder's nil-Base skip ensures we only land
	// here when a flattened tag actually appeared on the wire, so a present
	// tag-of-Base unambiguously signals that the embed should materialize
	// even if all of its other fields are zero-valued.
	for _, line := range pointerEmbedAllocs(f.embedSegments, "v") {
		fp(out, "\t\t\t%s\n", line)
	}
	if f.decl.CycleBreak {
		ptr, _ := t.(*types.Pointer)
		idName, idType, err := idRefTargetField(ptr)
		if err != nil {
			return err
		}
		// Allocate a fresh target struct and decode only the ID field.
		// All other fields stay at their type's zero value; the caller
		// hydrates them after decode if needed. The wire-type check is
		// already emitted by the outer switch (against wireType(f),
		// which for id_ref equals the ID field's natural wire type).
		fp(out, "\t\t\t%s = &%s{}\n", expr, e.typeExpr(ptr.Elem()))
		return e.emitValueDecode(out, expr+"."+idName, idType, 0)
	}
	if f.decl.Custom != "" {
		return e.emitCustomCodecDecode(out, expr, t, f.decl.Custom)
	}
	if ptr, ok := t.(*types.Pointer); ok {
		return e.emitOptionalDecode(out, expr, ptr.Elem())
	}
	return e.emitValueDecode(out, expr, t, 0)
}

// emitCustomCodecDecode renders the decode side for a custom-codec field.
// Value case: a direct call to the codec's decode function. Pointer case:
// open the length-delim envelope, read the presence byte (Nil / NonZero —
// PresenceZero is rejected for non-builtin payloads), and on NonZero
// allocate a fresh pointee and run the codec on it.
//
// The outer wire-type check (against the codec's declared wire type for
// value fields, or LENGTH_DELIM for pointer fields) is already emitted by
// emitUnmarshal before this body runs.
func (e *emitter) emitCustomCodecDecode(out io.Writer, expr string, t types.Type, codecName string) error {
	decl, err := e.resolveCodec(codecName, t)
	if err != nil {
		return err
	}
	call := e.codecCallExpr(decl, decl.DecodeFn)
	if ptr, ok := t.(*types.Pointer); ok {
		elemTypeExpr := e.typeExpr(ptr.Elem())
		// pickPresenceLocals avoids shadowing a type expression whose
		// package alias is `saved` or `state` (e.g. a codec whose import
		// path is `.../state`).
		savedLocal, stateLocal := pickPresenceLocals(elemTypeExpr)
		fp(out, "\t\t\t%s, err := r.BeginLengthDelim()\n", savedLocal)
		fp(out, "\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t%s, err := r.ReadPresenceByte(false)\n", stateLocal)
		fp(out, "\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\tswitch %s {\n", stateLocal)
		fp(out, "\t\t\tcase gsbm.PresenceNil:\n")
		fp(out, "\t\t\t\t%s = nil\n", expr)
		fp(out, "\t\t\tcase gsbm.PresenceNonZero:\n")
		fp(out, "\t\t\t\tvar tmp %s\n", elemTypeExpr)
		fp(out, "\t\t\t\tif err := %s(r, &tmp); err != nil { return err }\n", call)
		fp(out, "\t\t\t\t%s = &tmp\n", expr)
		fp(out, "\t\t\t}\n")
		fp(out, "\t\t\tif err := r.EndLengthDelim(%s); err != nil { return err }\n", savedLocal)
		return nil
	}
	fp(out, "\t\t\tif err := %s(r, &%s); err != nil { return err }\n", call, expr)
	return nil
}

func (e *emitter) emitOptionalDecode(out io.Writer, expr string, elem types.Type) error {
	// *[]byte is a builtin-primitive optional but the present-non-zero
	// branch reads via r.ReadBytes rather than the basic-type primitive
	// decoder; emit the full switch inline so we don't fall through to
	// emitPrimitiveDecodeAssign below (which rejects *types.Slice).
	if s, ok := elem.(*types.Slice); ok && isByteType(s.Elem()) {
		fp(out, "\t\t\tsaved, err := r.BeginLengthDelim()\n")
		fp(out, "\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\tstate, err := r.ReadPresenceByte(true)\n")
		fp(out, "\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\tswitch state {\n")
		fp(out, "\t\t\tcase gsbm.PresenceNil:\n")
		fp(out, "\t\t\t\t%s = nil\n", expr)
		fp(out, "\t\t\tcase gsbm.PresenceZero:\n")
		fp(out, "\t\t\t\tz := []byte{}\n")
		fp(out, "\t\t\t\t%s = &z\n", expr)
		fp(out, "\t\t\tcase gsbm.PresenceNonZero:\n")
		fp(out, "\t\t\t\tb, err := r.ReadBytes()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		// ReadBytes aliases the input buffer; copy so the decoded
		// *[]byte owns its bytes (heap-mode contract: callers may reuse
		// or mutate the source slice after decode). Mirrors the value
		// []byte path's append(dst[:0], b...).
		fp(out, "\t\t\t\tcp := append([]byte(nil), b...)\n")
		fp(out, "\t\t\t\t%s = &cp\n", expr)
		fp(out, "\t\t\t}\n")
		fp(out, "\t\t\tif err := r.EndLengthDelim(saved); err != nil { return err }\n")
		return nil
	}
	// The length-delim marker and presence-byte locals must not shadow any
	// type expression referenced inside this scope. A user-declared type or
	// import alias named `saved` or `state` would otherwise make the
	// subsequent `var z TYPE` / `TYPE{}` / `TYPE(...)` resolve to the local
	// value instead of the type. Pre-compute the type expressions used
	// downstream and pick non-colliding local names.
	elemTypeExpr := e.typeExpr(elem)
	var underlyingTypeExpr string
	if named, ok := elem.(*types.Named); ok {
		underlyingTypeExpr = e.typeExpr(named.Underlying())
	}
	savedLocal, stateLocal := pickPresenceLocals(elemTypeExpr, underlyingTypeExpr)
	fp(out, "\t\t\t%s, err := r.BeginLengthDelim()\n", savedLocal)
	fp(out, "\t\t\tif err != nil { return err }\n")
	allow := isBuiltinPrimitive(elem)
	fp(out, "\t\t\t%s, err := r.ReadPresenceByte(%v)\n", stateLocal, allow)
	fp(out, "\t\t\tif err != nil { return err }\n")
	fp(out, "\t\t\tswitch %s {\n", stateLocal)
	fp(out, "\t\t\tcase gsbm.PresenceNil:\n")
	fp(out, "\t\t\t\t%s = nil\n", expr)
	if allow {
		// Use `var z TYPE` rather than `z := zeroValue(...)`: an untyped
		// numeric literal `0` would resolve to `int`, leaving `&z` as `*int`
		// and breaking the assignment to a typed field like `*int64`.
		fp(out, "\t\t\tcase gsbm.PresenceZero:\n")
		fp(out, "\t\t\t\tvar z %s\n", elemTypeExpr)
		fp(out, "\t\t\t\t%s = &z\n", expr)
	}
	fp(out, "\t\t\tcase gsbm.PresenceNonZero:\n")
	if named, ok := elem.(*types.Named); ok {
		if _, isStruct := named.Underlying().(*types.Struct); isStruct {
			fp(out, "\t\t\t\t%s = &%s{}\n", expr, elemTypeExpr)
			fp(out, "\t\t\t\tif err := %s.UnmarshalGSBM(r); err != nil { return err }\n", expr)
			fp(out, "\t\t\t}\n")
			fp(out, "\t\t\tif err := r.EndLengthDelim(%s); err != nil { return err }\n", savedLocal)
			return nil
		}
		// Named-byte-slice (e.g. `*ID` where `type ID []byte`): read raw
		// bytes, copy off r's buffer, convert to the named type and take
		// its address. Mirrors emitValueDecode; without this branch the
		// named-not-struct path below routes a *types.Slice into
		// emitPrimitiveDecodeAssign and codegen errors. The local-variable
		// name is picked dynamically so it doesn't shadow the package
		// alias of the named type (e.g. cross-package import aliased as
		// `raw` would collide with a hard-coded `raw` local).
		if s, isSlice := named.Underlying().(*types.Slice); isSlice && isByteType(s.Elem()) {
			local := pickByteSliceLocal(elemTypeExpr)
			fp(out, "\t\t\t\t%s, err := r.ReadBytes()\n", local)
			fp(out, "\t\t\t\tif err != nil { return err }\n")
			fp(out, "\t\t\t\ttmp := %s(append([]byte(nil), %s...))\n", elemTypeExpr, local)
			fp(out, "\t\t\t\t%s = &tmp\n", expr)
			fp(out, "\t\t\t}\n")
			fp(out, "\t\t\tif err := r.EndLengthDelim(%s); err != nil { return err }\n", savedLocal)
			return nil
		}
		// Named-not-struct: decode underlying primitive into a temp, then
		// convert and take its address. The local-variable name is picked
		// dynamically so it doesn't shadow the named type referenced on
		// the conversion line (e.g. `type u string` or a cross-package
		// import aliased as `u` would otherwise make `tmp := u(u)` /
		// `tmp := u.ID(u)` resolve `u` to the local var).
		uLocal := pickConvertLocal("u", elemTypeExpr)
		fp(out, "\t\t\t\tvar %s %s\n", uLocal, underlyingTypeExpr)
		if err := e.emitPrimitiveDecodeAssign(out, uLocal, named.Underlying()); err != nil {
			return err
		}
		fp(out, "\t\t\t\ttmp := %s(%s)\n", elemTypeExpr, uLocal)
		fp(out, "\t\t\t\t%s = &tmp\n", expr)
		fp(out, "\t\t\t}\n")
		fp(out, "\t\t\tif err := r.EndLengthDelim(%s); err != nil { return err }\n", savedLocal)
		return nil
	}
	// Builtin primitive present-non-zero: decode into a temporary, then
	// take its address.
	fp(out, "\t\t\t\tvar tmp %s\n", elemTypeExpr)
	if err := e.emitPrimitiveDecodeAssign(out, "tmp", elem); err != nil {
		return err
	}
	fp(out, "\t\t\t\t%s = &tmp\n", expr)
	fp(out, "\t\t\t}\n")
	fp(out, "\t\t\tif err := r.EndLengthDelim(%s); err != nil { return err }\n", savedLocal)
	return nil
}

func (e *emitter) emitValueDecode(out io.Writer, expr string, t types.Type, depth int) error {
	switch tt := t.(type) {
	case *types.Basic:
		return e.emitPrimitiveDecodeAssign(out, expr, tt)
	case *types.Named:
		if _, ok := tt.Underlying().(*types.Struct); ok {
			// emitValueDecode is only called at depth 0 for a top-level
			// named-struct field (nested struct values inside maps/slices
			// have their own inline emit in emitMapDecode/emitSliceDecode),
			// so `saved` is unambiguous and we keep the literal name to
			// preserve goldens.
			fp(out, "\t\t\tsaved, err := r.BeginLengthDelim()\n")
			fp(out, "\t\t\tif err != nil { return err }\n")
			fp(out, "\t\t\tif err := %s.UnmarshalGSBM(r); err != nil { return err }\n", expr)
			fp(out, "\t\t\tif err := r.EndLengthDelim(saved); err != nil { return err }\n")
			return nil
		}
		// Named slice alias over a non-byte element (`type ItemList []Item`,
		// `type ItemPtrList []*Item`): the wire bytes are identical to the
		// underlying slice, so route the decode through emitSliceDecode with
		// the underlying. Assigning gsbm.MakeSlice[E](r, n) ([]E, unnamed)
		// to a field of named slice type is legal Go because the underlying
		// types match and one side is unnamed; reslicing a named slice
		// yields the same named type.
		if s, isSlice := tt.Underlying().(*types.Slice); isSlice && !isByteType(s.Elem()) {
			return e.emitSliceDecode(out, expr, s, depth)
		}
		// Named-byte-slice (e.g. `type ID []byte`): read raw bytes, copy
		// to detach from r's buffer (ReadBytes aliases), then convert to
		// the named type. Without this branch, the named-not-struct path
		// below would route into emitPrimitiveDecodeAssign with a
		// *types.Slice and fail at codegen time. The local-variable name
		// is picked dynamically so it doesn't shadow the package alias of
		// the named type (typeExpr can emit `pkg.Name`, and a cross-
		// package import aliased to whatever name we hard-coded here
		// would otherwise produce uncompilable code).
		if s, isSlice := tt.Underlying().(*types.Slice); isSlice && isByteType(s.Elem()) {
			local := pickByteSliceLocal(e.typeExpr(tt))
			fp(out, "\t\t\t%s, err := r.ReadBytes()\n", local)
			fp(out, "\t\t\tif err != nil { return err }\n")
			// append copies r's aliased bytes into expr's backing array
			// (allocating if needed), so the result detaches from r and
			// reuses the existing capacity when expr already had one —
			// mirroring the plain []byte branch below. Slicing and
			// appending preserve the named slice type, so no explicit
			// conversion is needed.
			fp(out, "\t\t\t%s = append(%s[:0], %s...)\n", expr, expr, local)
			return nil
		}
		// Named-not-struct: decode underlying primitive then convert. The
		// local-variable name is picked dynamically so it doesn't shadow
		// the named type referenced on the conversion line (e.g.
		// `type tmp string` or a cross-package import aliased as `tmp`
		// would otherwise make `expr = tmp(tmp)` / `expr = tmp.ID(tmp)`
		// resolve `tmp` to the local var).
		ttExpr := e.typeExpr(tt)
		tmpLocal := pickConvertLocal("tmp", ttExpr)
		fp(out, "\t\t\tvar %s %s\n", tmpLocal, e.typeExpr(tt.Underlying()))
		if err := e.emitPrimitiveDecodeAssign(out, tmpLocal, tt.Underlying()); err != nil {
			return err
		}
		fp(out, "\t\t\t%s = %s(%s)\n", expr, ttExpr, tmpLocal)
		return nil
	case *types.Slice:
		if isByteType(tt.Elem()) {
			fp(out, "\t\t\tb, err := r.ReadBytes()\n")
			fp(out, "\t\t\tif err != nil { return err }\n")
			fp(out, "\t\t\t%s = append(%s[:0], b...)\n", expr, expr)
			return nil
		}
		return e.emitSliceDecode(out, expr, tt, depth)
	case *types.Map:
		return e.emitMapDecode(out, expr, tt, depth)
	}
	return fmt.Errorf("unsupported decode type %T", t)
}

func (e *emitter) emitPrimitiveDecodeAssign(out io.Writer, lhs string, t types.Type) error {
	b, ok := t.(*types.Basic)
	if !ok {
		return fmt.Errorf("emitPrimitiveDecodeAssign: %T not a basic type", t)
	}
	// Wrap each primitive decode in its own block so multiple back-to-back
	// reads in the same enclosing scope (e.g. map key + map value) don't
	// shadow each other on `x, err :=`.
	fp(out, "\t\t\t{\n")
	switch b.Kind() {
	case types.Bool:
		fp(out, "\t\t\t\tx, err := r.ReadBool()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\t%s = x\n", lhs)
	case types.String:
		fp(out, "\t\t\t\tx, err := r.ReadString()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\t%s = x\n", lhs)
	case types.Int8:
		fp(out, "\t\t\t\tx, err := r.ReadVarint()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\tif x < math.MinInt8 || x > math.MaxInt8 { return gsbm.ErrIntegerOverflow }\n")
		fp(out, "\t\t\t\t%s = int8(x)\n", lhs)
		e.addImport("math")
	case types.Int16:
		fp(out, "\t\t\t\tx, err := r.ReadVarint()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\tif x < math.MinInt16 || x > math.MaxInt16 { return gsbm.ErrIntegerOverflow }\n")
		fp(out, "\t\t\t\t%s = int16(x)\n", lhs)
		e.addImport("math")
	case types.Int32:
		fp(out, "\t\t\t\tx, err := r.ReadVarint()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\tif x < math.MinInt32 || x > math.MaxInt32 { return gsbm.ErrIntegerOverflow }\n")
		fp(out, "\t\t\t\t%s = int32(x)\n", lhs)
		e.addImport("math")
	case types.Int:
		// `int` is platform-sized (32 or 64). Bound by 32-bit range so the
		// blob round-trips between platforms; a 32-bit reader cannot accept
		// a 64-bit-only value anyway.
		fp(out, "\t\t\t\tx, err := r.ReadVarint()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\tif x < math.MinInt32 || x > math.MaxInt32 { return gsbm.ErrIntegerOverflow }\n")
		fp(out, "\t\t\t\t%s = int(x)\n", lhs)
		e.addImport("math")
	case types.Int64:
		fp(out, "\t\t\t\tx, err := r.ReadVarint()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\t%s = int64(x)\n", lhs)
	case types.Uint8:
		fp(out, "\t\t\t\tx, err := r.ReadUvarint()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\tif x > math.MaxUint8 { return gsbm.ErrIntegerOverflow }\n")
		fp(out, "\t\t\t\t%s = uint8(x)\n", lhs)
		e.addImport("math")
	case types.Uint16:
		fp(out, "\t\t\t\tx, err := r.ReadUvarint()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\tif x > math.MaxUint16 { return gsbm.ErrIntegerOverflow }\n")
		fp(out, "\t\t\t\t%s = uint16(x)\n", lhs)
		e.addImport("math")
	case types.Uint32:
		fp(out, "\t\t\t\tx, err := r.ReadUvarint()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\tif x > math.MaxUint32 { return gsbm.ErrIntegerOverflow }\n")
		fp(out, "\t\t\t\t%s = uint32(x)\n", lhs)
		e.addImport("math")
	case types.Uint:
		fp(out, "\t\t\t\tx, err := r.ReadUvarint()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\tif x > math.MaxUint32 { return gsbm.ErrIntegerOverflow }\n")
		fp(out, "\t\t\t\t%s = uint(x)\n", lhs)
		e.addImport("math")
	case types.Uint64:
		fp(out, "\t\t\t\tx, err := r.ReadUvarint()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\t%s = x\n", lhs)
	case types.Uintptr:
		// Platform-sized: bound to 32-bit so the wire is portable to a
		// 32-bit reader. Without this a 64-bit value silently truncates.
		fp(out, "\t\t\t\tx, err := r.ReadUvarint()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\tif x > math.MaxUint32 { return gsbm.ErrIntegerOverflow }\n")
		fp(out, "\t\t\t\t%s = uintptr(x)\n", lhs)
		e.addImport("math")
	case types.Float32:
		fp(out, "\t\t\t\tx, err := r.ReadFloat32()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\t%s = x\n", lhs)
	case types.Float64:
		fp(out, "\t\t\t\tx, err := r.ReadFloat64()\n")
		fp(out, "\t\t\t\tif err != nil { return err }\n")
		fp(out, "\t\t\t\t%s = x\n", lhs)
	default:
		return fmt.Errorf("unsupported decode kind %v", b.Kind())
	}
	fp(out, "\t\t\t}\n")
	return nil
}

func (e *emitter) emitSliceDecode(out io.Writer, expr string, t *types.Slice, depth int) error {
	elemT := t.Elem()
	elemTypeStr := e.typeExpr(elemT)
	saved := nm("saved", depth)
	nVar := nm("n", depth)
	idx := nm("i", depth)
	innerVar := nm("inner", depth)
	fp(out, "\t\t\t%s, err := r.BeginLengthDelim()\n", saved)
	fp(out, "\t\t\tif err != nil { return err }\n")
	fp(out, "\t\t\t%s, err := r.ReadLength()\n", nVar)
	fp(out, "\t\t\tif err != nil { return err }\n")
	fp(out, "\t\t\tif %s > 0 {\n", nVar)
	fp(out, "\t\t\t\tif cap(%s) >= %s { %s = %s[:%s] } else { %s = gsbm.MakeSlice[%s](r, %s) }\n",
		expr, nVar, expr, expr, nVar, expr, elemTypeStr, nVar)
	fp(out, "\t\t\t\tif err := r.Err(); err != nil { return err }\n")
	fp(out, "\t\t\t}\n")
	fp(out, "\t\t\tfor %s := 0; %s < %s; %s++ {\n", idx, idx, nVar, idx)
	// Slice-of-pointer-to-named-struct: each element is a length-delim
	// envelope carrying a presence byte. PresenceNil → store nil;
	// PresenceNonZero → allocate fresh and decode. Zero-elide is rejected
	// (non-builtin payload, per spec §5.1).
	if ptr, ok := elemT.(*types.Pointer); ok {
		if named, ok := ptr.Elem().(*types.Named); ok {
			if _, isStruct := named.Underlying().(*types.Struct); isStruct {
				pointee := e.typeExpr(named)
				var innerLocal, stateLocal string
				if depth == 0 {
					innerLocal, stateLocal = pickPointerSliceLocals(pointee)
				} else {
					innerLocal, stateLocal = innerVar, nm("state", depth)
				}
				fp(out, "\t\t\t\t%s, err := r.BeginLengthDelim()\n", innerLocal)
				fp(out, "\t\t\t\tif err != nil { return err }\n")
				fp(out, "\t\t\t\t%s, err := r.ReadPresenceByte(false)\n", stateLocal)
				fp(out, "\t\t\t\tif err != nil { return err }\n")
				fp(out, "\t\t\t\tswitch %s {\n", stateLocal)
				fp(out, "\t\t\t\tcase gsbm.PresenceNil:\n")
				fp(out, "\t\t\t\t\t%s[%s] = nil\n", expr, idx)
				fp(out, "\t\t\t\tcase gsbm.PresenceNonZero:\n")
				// Reuse the existing element pointer when the slot is
				// non-nil so a DecodeInto cap-reuse path preserves any
				// nested slice/map capacity the previous element held —
				// mirrors the value-element Reset() rationale below.
				fp(out, "\t\t\t\t\tif %s[%s] == nil { %s[%s] = &%s{} } else { %s[%s].Reset() }\n",
					expr, idx, expr, idx, pointee, expr, idx)
				fp(out, "\t\t\t\t\tif err := %s[%s].UnmarshalGSBM(r); err != nil { return err }\n", expr, idx)
				fp(out, "\t\t\t\t}\n")
				fp(out, "\t\t\t\tif err := r.EndLengthDelim(%s); err != nil { return err }\n", innerLocal)
				fp(out, "\t\t\t}\n")
				fp(out, "\t\t\tif err := r.EndLengthDelim(%s); err != nil { return err }\n", saved)
				return nil
			}
		}
	}
	if named, ok := elemT.(*types.Named); ok {
		if _, isStruct := named.Underlying().(*types.Struct); isStruct {
			fp(out, "\t\t\t\t%s, err := r.BeginLengthDelim()\n", innerVar)
			fp(out, "\t\t\t\tif err != nil { return err }\n")
			// Reset rather than zero-assign: when DecodeInto reuses the
			// slice, the existing element may carry nested slice/map
			// capacity that Reset preserves but `T{}` would discard.
			fp(out, "\t\t\t\t%s[%s].Reset()\n", expr, idx)
			fp(out, "\t\t\t\tif err := %s[%s].UnmarshalGSBM(r); err != nil { return err }\n", expr, idx)
			fp(out, "\t\t\t\tif err := r.EndLengthDelim(%s); err != nil { return err }\n", innerVar)
			fp(out, "\t\t\t}\n")
			fp(out, "\t\t\tif err := r.EndLengthDelim(%s); err != nil { return err }\n", saved)
			return nil
		}
		// Named-not-struct: decode underlying primitive into a temp and
		// convert into the declared type before assignment.
		fp(out, "\t\t\t\tvar u %s\n", e.typeExpr(named.Underlying()))
		if err := e.emitPrimitiveDecodeAssign(out, "u", named.Underlying()); err != nil {
			return err
		}
		fp(out, "\t\t\t\t%s[%s] = %s(u)\n", expr, idx, e.typeExpr(named))
		fp(out, "\t\t\t}\n")
		fp(out, "\t\t\tif err := r.EndLengthDelim(%s); err != nil { return err }\n", saved)
		return nil
	}
	// Nested composite element (slice or map): recurse into emitValueDecode
	// at depth+1 so inner per-level locals get unique names and the inner
	// code can reference outer's index via the elem expr without shadowing.
	// []byte elements are routed through emitValueDecode too — it has a
	// dedicated ReadBytes path that the primitive-decode fall-through below
	// would otherwise crash on (it expects *types.Basic, not *types.Slice).
	if _, isSlice := elemT.(*types.Slice); isSlice {
		elemExpr := fmt.Sprintf("%s[%s]", expr, idx)
		// Cap-reused slot may hold a stale slice from a previous decode;
		// the inner emitValueDecode for non-byte slices reuses cap via
		// `expr[:n]`, but for byte slices it does `append(expr[:0], b...)`,
		// so either way truncating to [:0] up front is correct and keeps
		// inner capacity available for reuse.
		if !isByteType(elemT.(*types.Slice).Elem()) {
			fp(out, "\t\t\t\t%s = %s[:0]\n", elemExpr, elemExpr)
		}
		if err := e.emitValueDecode(out, elemExpr, elemT, depth+1); err != nil {
			return err
		}
		fp(out, "\t\t\t}\n")
		fp(out, "\t\t\tif err := r.EndLengthDelim(%s); err != nil { return err }\n", saved)
		return nil
	}
	if _, isMap := elemT.(*types.Map); isMap {
		elemExpr := fmt.Sprintf("%s[%s]", expr, idx)
		// Cap-reused slot may hold a stale map from a previous decode;
		// clear it so re-decode sees an empty map. `clear` on a nil map
		// is a no-op (Go 1.21+) so the fresh-receiver path is unaffected.
		fp(out, "\t\t\t\tclear(%s)\n", elemExpr)
		if err := e.emitValueDecode(out, elemExpr, elemT, depth+1); err != nil {
			return err
		}
		fp(out, "\t\t\t}\n")
		fp(out, "\t\t\tif err := r.EndLengthDelim(%s); err != nil { return err }\n", saved)
		return nil
	}
	// Primitive element.
	if err := e.emitPrimitiveDecodeAssign(out, fmt.Sprintf("%s[%s]", expr, idx), elemT); err != nil {
		return err
	}
	fp(out, "\t\t\t}\n")
	fp(out, "\t\t\tif err := r.EndLengthDelim(%s); err != nil { return err }\n", saved)
	return nil
}

func (e *emitter) emitMapDecode(out io.Writer, expr string, t *types.Map, depth int) error {
	if !isPrimitiveKey(t.Key()) {
		return fmt.Errorf("map key must be primitive or string")
	}
	keyTypeStr := e.typeExpr(t.Key())
	valTypeStr := e.typeExpr(t.Elem())
	saved := nm("saved", depth)
	nVar := nm("n", depth)
	idx := nm("i", depth)
	kVar := nm("k", depth)
	vvVar := nm("vv", depth)
	innerVar := nm("inner", depth)
	fp(out, "\t\t\t%s, err := r.BeginLengthDelim()\n", saved)
	fp(out, "\t\t\tif err != nil { return err }\n")
	fp(out, "\t\t\t%s, err := r.ReadLength()\n", nVar)
	fp(out, "\t\t\tif err != nil { return err }\n")
	fp(out, "\t\t\tif %s > 0 && %s == nil { %s = gsbm.MakeMap[%s, %s](r, %s) }\n",
		nVar, expr, expr, keyTypeStr, valTypeStr, nVar)
	fp(out, "\t\t\tfor %s := 0; %s < %s; %s++ {\n", idx, idx, nVar, idx)
	fp(out, "\t\t\t\tvar %s %s\n", kVar, keyTypeStr)
	// Named primitive keys: decode the underlying primitive into a
	// temporary and convert into the declared type before assigning.
	// Without the cast the assignment fails to compile (`k int8 = int64`
	// etc.), and routing the named key directly into the primitive
	// decoder would error on the *types.Named.
	if _, ok := t.Key().(*types.Named); ok {
		// Pick a non-colliding local: a same-package named key type
		// itself named `tmp`, or a cross-package import aliased as
		// `tmp`, would make `k = tmp(tmp)` / `k = tmp.ID(tmp)` resolve
		// `tmp` to the local var instead of the type. Same shape as the
		// named-not-struct path in emitPrimitiveDecodeAssign.
		tmpLocal := pickConvertLocal("tmp", keyTypeStr)
		fp(out, "\t\t\t\t{\n")
		fp(out, "\t\t\t\t\tvar %s %s\n", tmpLocal, e.typeExpr(t.Key().Underlying()))
		if err := e.emitPrimitiveDecodeAssign(out, tmpLocal, t.Key().Underlying()); err != nil {
			return err
		}
		fp(out, "\t\t\t\t\t%s = %s(%s)\n", kVar, keyTypeStr, tmpLocal)
		fp(out, "\t\t\t\t}\n")
	} else {
		if err := e.emitPrimitiveDecodeAssign(out, kVar, t.Key()); err != nil {
			return err
		}
	}
	fp(out, "\t\t\t\tvar %s %s\n", vvVar, valTypeStr)
	if named, ok := t.Elem().(*types.Named); ok {
		if _, isStruct := named.Underlying().(*types.Struct); isStruct {
			fp(out, "\t\t\t\t%s, err := r.BeginLengthDelim()\n", innerVar)
			fp(out, "\t\t\t\tif err != nil { return err }\n")
			fp(out, "\t\t\t\tif err := %s.UnmarshalGSBM(r); err != nil { return err }\n", vvVar)
			fp(out, "\t\t\t\tif err := r.EndLengthDelim(%s); err != nil { return err }\n", innerVar)
		} else {
			// Named-not-struct (e.g. type MyID string) decodes via the
			// underlying primitive and converts into the declared type.
			fp(out, "\t\t\t\t{\n")
			fp(out, "\t\t\t\t\tvar tmp %s\n", e.typeExpr(named.Underlying()))
			if err := e.emitPrimitiveDecodeAssign(out, "tmp", named.Underlying()); err != nil {
				return err
			}
			fp(out, "\t\t\t\t\t%s = %s(tmp)\n", vvVar, e.typeExpr(named))
			fp(out, "\t\t\t\t}\n")
		}
	} else if _, isSlice := t.Elem().(*types.Slice); isSlice {
		// Nested slice value (including []byte): recurse at depth+1 so
		// inner locals don't clash with this map's `k`, `vv`, etc., and
		// the inner code can still write into this map's value local
		// (`vv`). emitValueDecode dispatches `[]byte` to ReadBytes; any
		// other slice element falls through to emitSliceDecode.
		if err := e.emitValueDecode(out, vvVar, t.Elem(), depth+1); err != nil {
			return err
		}
	} else if _, isMap := t.Elem().(*types.Map); isMap {
		// Nested map value: same recursion shape as nested slice above.
		if err := e.emitValueDecode(out, vvVar, t.Elem(), depth+1); err != nil {
			return err
		}
	} else {
		if err := e.emitPrimitiveDecodeAssign(out, vvVar, t.Elem()); err != nil {
			return err
		}
	}
	fp(out, "\t\t\t\t%s[%s] = %s\n", expr, kVar, vvVar)
	fp(out, "\t\t\t}\n")
	fp(out, "\t\t\tif err := r.EndLengthDelim(%s); err != nil { return err }\n", saved)
	return nil
}

// isBuiltinPrimitive is the runtime-side mirror of
// gsbmschema.IsBuiltinPrimitive: types whose zero value can be elided by
// the presence-byte. The schema layer answers based on the rendered type
// name; here we ask the typechecked type directly.
func isBuiltinPrimitive(t types.Type) bool {
	switch tt := t.(type) {
	case *types.Basic:
		switch tt.Kind() {
		case types.Bool, types.String,
			types.Int, types.Int8, types.Int16, types.Int32, types.Int64,
			types.Uint, types.Uint8, types.Uint16, types.Uint32, types.Uint64, types.Uintptr,
			types.Float32, types.Float64:
			return true
		}
	case *types.Slice:
		// []byte qualifies; other slice element types don't.
		return isByteType(tt.Elem())
	}
	return false
}

// isPrimitiveKey reports whether t is acceptable as a map-key per spec
// §5.3. A *types.Named is accepted iff its underlying is itself a
// permitted *types.Basic kind (so `type Code string` and `type Bucket
// uint16` qualify, but `type Rate float64` does not).
func isPrimitiveKey(t types.Type) bool {
	if n, ok := t.(*types.Named); ok {
		t = n.Underlying()
	}
	b, ok := t.(*types.Basic)
	if !ok {
		return false
	}
	switch b.Kind() {
	case types.Bool, types.String,
		types.Int, types.Int8, types.Int16, types.Int32, types.Int64,
		types.Uint, types.Uint8, types.Uint16, types.Uint32, types.Uint64, types.Uintptr:
		return true
	}
	return false
}

// zeroValue returns a Go-source expression for the zero value of t. Used
// for the present-and-zero case in optional builtin fields.
func zeroValue(t types.Type) string {
	if isByteType(elemTypeOfSlice(t)) {
		return "nil"
	}
	if b, ok := t.(*types.Basic); ok {
		switch b.Kind() {
		case types.String:
			return `""`
		case types.Bool:
			return "false"
		case types.Float32, types.Float64:
			return "0"
		default:
			return "0"
		}
	}
	return "nil"
}

func elemTypeOfSlice(t types.Type) types.Type {
	if s, ok := t.(*types.Slice); ok {
		return s.Elem()
	}
	return nil
}

// emitSize writes `func (v *T) SizeGSBM() int { ... }`. The body walks
// the same writableFields list as emitMarshal in the same order and
// accumulates a byte counter `n` so the invariant
// `v.SizeGSBM() == len(body produced by v.MarshalGSBM())` holds for every
// generated value.
func (e *emitter) emitSize(out io.Writer, named *types.Named, str *types.Struct, sd *gsbmschema.StructDecl) error {
	name := named.Obj().Name()
	e.currentStructFQN = structFQN(named)
	defer func() { e.currentStructFQN = "" }()
	fp(out, "func (v *%s) SizeGSBM() int {\n", name)
	fp(out, "\tvar n int\n")
	for _, f := range e.writableFields(str, sd) {
		if f.decl.Deprecated && !f.decl.CompatWrite {
			fp(out, "\t// tag %d %s: deprecated, not written\n", f.decl.Tag, f.decl.Name)
			continue
		}
		if f.decl.CompatWrite {
			fp(out, "\t// tag %d %s: deprecated, compat_write (rollback window)\n", f.decl.Tag, f.decl.Name)
		} else {
			fp(out, "\t// tag %d %s\n", f.decl.Tag, f.decl.Name)
		}
		if err := e.emitFieldSize(out, f); err != nil {
			return fmt.Errorf("%s.%s: %w", name, f.decl.Name, err)
		}
	}
	fp(out, "\treturn n\n}\n")
	return nil
}

// emitFieldSize emits sizing code for one field, accumulating into the
// outer `n` counter. Layout mirrors emitFieldEncode: pointer-embed guard,
// id_ref short-circuit, custom-codec / pointer / value dispatch.
func (e *emitter) emitFieldSize(out io.Writer, f fieldEntry) error {
	tag := f.decl.Tag
	wt, err := e.wireType(f)
	if err != nil {
		return err
	}
	expr := f.accessPath
	t := f.gov.Type()

	if guard := pointerEmbedGuard(f.embedSegments, "v"); guard != "" {
		fp(out, "\tif %s {\n", guard)
		defer fp(out, "\t}\n")
	}

	if f.decl.CycleBreak {
		ptr, _ := t.(*types.Pointer)
		idName, idType, err := idRefTargetField(ptr)
		if err != nil {
			return err
		}
		fp(out, "\tif %s != nil {\n", expr)
		fp(out, "\t\tn += gsbm.SizeTag(%d, %s)\n", tag, wt)
		if err := e.emitValueSize(out, expr+"."+idName, idType, "n", 0); err != nil {
			return err
		}
		fp(out, "\t}\n")
		return nil
	}
	if f.decl.Custom != "" {
		return e.emitCustomCodecSize(out, tag, wt, expr, t, f.decl.Custom)
	}
	if ptr, ok := t.(*types.Pointer); ok {
		return e.emitOptionalSize(out, tag, wt, expr, ptr.Elem())
	}
	fp(out, "\tn += gsbm.SizeTag(%d, %s)\n", tag, wt)
	return e.emitValueSize(out, expr, t, "n", 0)
}

// emitCustomCodecSize emits the size accumulation for a custom-codec
// field. Value case: `SizeTag(tag, wt) + codec.SizeFn(v.Field)`. Pointer
// case: the spec §5.1 nullable envelope around the codec — outer tag +
// length-delim envelope containing a presence byte and (on NonZero) the
// codec payload. The shape is symmetric with emitCustomCodecEncode so
// SizeGSBM and MarshalGSBM walk the same bytes by construction.
func (e *emitter) emitCustomCodecSize(out io.Writer, tag uint32, wt, expr string, t types.Type, codecName string) error {
	decl, err := e.resolveCodec(codecName, t)
	if err != nil {
		return err
	}
	switch decl.Kind() {
	case codecs.CodecKindAnalytic:
		sizeCall := e.codecCallExpr(decl, decl.SizeFn)
		if _, ok := t.(*types.Pointer); ok {
			fp(out, "\tn += gsbm.SizeTag(%d, %s)\n", tag, wt)
			fp(out, "\tif %s == nil {\n", expr)
			fp(out, "\t\tn += gsbm.SizeLengthDelim(1)\n")
			fp(out, "\t} else {\n")
			fp(out, "\t\tn += gsbm.SizeLengthDelim(1 + %s(*%s))\n", sizeCall, expr)
			fp(out, "\t}\n")
			return nil
		}
		fp(out, "\tn += gsbm.SizeTag(%d, %s) + %s(%s)\n", tag, wt, sizeCall, expr)
		return nil
	case codecs.CodecKindMaterializing:
		// SizeGSBM has no Writer to thread the scratch cache through, so a
		// standalone SizeGSBM call materializes the codec body via a fresh
		// CountingWriter — the EmitFn writes against it in size-mode and the
		// accumulated count gives the exact body length. The scratch lives
		// and dies with `cw`, so users who call SizeGSBM alone pay the
		// double-materialization cost (documented). gsbm.Marshal (Task 5)
		// threads one Writer through both passes and gets the cache benefit.
		call := e.codecCallExpr(decl, decl.EmitFn)
		cs := e.callsiteFor(tag)
		if _, ok := t.(*types.Pointer); ok {
			fp(out, "\tn += gsbm.SizeTag(%d, %s)\n", tag, wt)
			fp(out, "\tif %s == nil {\n", expr)
			fp(out, "\t\tn += gsbm.SizeLengthDelim(1)\n")
			fp(out, "\t} else {\n")
			fp(out, "\t\tcw := gsbm.NewCountingWriter()\n")
			fp(out, "\t\t_ = %s(cw, *%s, %s)\n", call, expr, cs)
			fp(out, "\t\tn += gsbm.SizeLengthDelim(1 + cw.Size())\n")
			fp(out, "\t}\n")
			return nil
		}
		fp(out, "\t{\n")
		fp(out, "\t\tcw := gsbm.NewCountingWriter()\n")
		fp(out, "\t\t_ = %s(cw, %s, %s)\n", call, expr, cs)
		fp(out, "\t\tn += gsbm.SizeTag(%d, %s) + cw.Size()\n", tag, wt)
		fp(out, "\t}\n")
		return nil
	default:
		return fmt.Errorf("codec %q: unrecognized codec kind (registry returned a decl with neither analytic (SizeFn+EncodeFn) nor materializing (EmitFn) shape — this is an emitter/registry contract bug)", codecName)
	}
}

// emitOptionalSize mirrors emitOptionalEncode: a length-delim envelope
// carrying a presence byte (1 byte) plus, for present non-zero, the
// value's body. The marshal switch has three branches (nil/zero/nonzero)
// for zero-elide-eligible types; size collapses to two because both
// PresenceNil and PresenceZero are one byte.
func (e *emitter) emitOptionalSize(out io.Writer, tag uint32, wt, expr string, elem types.Type) error {
	fp(out, "\tn += gsbm.SizeTag(%d, %s)\n", tag, wt)
	// *[]byte: zero-elide on len == 0; otherwise SizeBytes(*expr).
	if s, ok := elem.(*types.Slice); ok && isByteType(s.Elem()) {
		fp(out, "\tif %s == nil || len(*%s) == 0 {\n", expr, expr)
		fp(out, "\t\tn += gsbm.SizeLengthDelim(1)\n")
		fp(out, "\t} else {\n")
		fp(out, "\t\tn += gsbm.SizeLengthDelim(1 + gsbm.SizeBytes(*%s))\n", expr)
		fp(out, "\t}\n")
		return nil
	}
	if isBuiltinPrimitive(elem) {
		zeroExpr := zeroValue(elem)
		fp(out, "\tif %s == nil || *%s == %s {\n", expr, expr, zeroExpr)
		fp(out, "\t\tn += gsbm.SizeLengthDelim(1)\n")
		fp(out, "\t} else {\n")
		fp(out, "\t\tn += gsbm.SizeLengthDelim(1 + ")
		if err := e.emitPrimitiveSizeExpr(out, "*"+expr, elem); err != nil {
			return err
		}
		fp(out, ")\n")
		fp(out, "\t}\n")
		return nil
	}
	// Non-builtin optional: no zero-elide — Nil contributes 1, NonZero
	// contributes 1 + body. Named-struct body comes from its SizeGSBM;
	// named-with-primitive-underlying unwraps; composite shapes (slice,
	// map, named slice/map) delegate to emitValueSize so the byte count
	// matches the bytes emitValueEncode produces.
	fp(out, "\tif %s == nil {\n", expr)
	fp(out, "\t\tn += gsbm.SizeLengthDelim(1)\n")
	fp(out, "\t} else {\n")
	if named, ok := elem.(*types.Named); ok {
		under := named.Underlying()
		if _, isStruct := under.(*types.Struct); isStruct {
			fp(out, "\t\tn += gsbm.SizeLengthDelim(1 + %s.SizeGSBM())\n", expr)
		} else if s, isSlice := under.(*types.Slice); isSlice && isByteType(s.Elem()) {
			fp(out, "\t\tn += gsbm.SizeLengthDelim(1 + gsbm.SizeBytes([]byte(*%s)))\n", expr)
		} else if _, isBasic := under.(*types.Basic); isBasic {
			fp(out, "\t\tn += gsbm.SizeLengthDelim(1 + ")
			if err := e.emitPrimitiveSizeExpr(out, fmt.Sprintf("(%s)(*%s)", e.typeExpr(under), expr), under); err != nil {
				return err
			}
			fp(out, ")\n")
		} else {
			fp(out, "\t\tinner := 0\n")
			if err := e.emitValueSize(out, "*"+expr, elem, "inner", 0); err != nil {
				return err
			}
			fp(out, "\t\tn += gsbm.SizeLengthDelim(1 + inner)\n")
		}
	} else if _, isBasic := elem.(*types.Basic); isBasic {
		fp(out, "\t\tn += gsbm.SizeLengthDelim(1 + ")
		if err := e.emitPrimitiveSizeExpr(out, "*"+expr, elem); err != nil {
			return err
		}
		fp(out, ")\n")
	} else {
		fp(out, "\t\tinner := 0\n")
		if err := e.emitValueSize(out, "*"+expr, elem, "inner", 0); err != nil {
			return err
		}
		fp(out, "\t\tn += gsbm.SizeLengthDelim(1 + inner)\n")
	}
	fp(out, "\t}\n")
	return nil
}

// emitValueSize emits sizing code for a value of type t accessed via expr,
// adding the resulting size to accVar. depth is the composite-nesting
// depth so nested slice/map sizing can name locals uniquely.
func (e *emitter) emitValueSize(out io.Writer, expr string, t types.Type, accVar string, depth int) error {
	switch tt := t.(type) {
	case *types.Basic:
		fp(out, "\t%s += ", accVar)
		if err := e.emitPrimitiveSizeExpr(out, expr, tt); err != nil {
			return err
		}
		fp(out, "\n")
		return nil
	case *types.Named:
		if _, ok := tt.Underlying().(*types.Struct); ok {
			fp(out, "\t%s += gsbm.SizeLengthDelim(%s.SizeGSBM())\n", accVar, expr)
			return nil
		}
		if s, ok := tt.Underlying().(*types.Slice); ok && !isByteType(s.Elem()) {
			return e.emitSliceSize(out, expr, s, accVar, depth)
		}
		// Named-not-struct (incl. named []byte): unwrap to underlying.
		under := tt.Underlying()
		castExpr := fmt.Sprintf("(%s)(%s)", e.typeExpr(under), expr)
		return e.emitValueSize(out, castExpr, under, accVar, depth)
	case *types.Slice:
		if isByteType(tt.Elem()) {
			fp(out, "\t%s += gsbm.SizeBytes(%s)\n", accVar, expr)
			return nil
		}
		return e.emitSliceSize(out, expr, tt, accVar, depth)
	case *types.Map:
		return e.emitMapSize(out, expr, tt, accVar, depth)
	}
	return fmt.Errorf("unsupported size type %T", t)
}

// emitPrimitiveSizeExpr writes a size-of-primitive expression (no
// surrounding "n += ...\n") to out. The expression has no side effects
// and can be embedded inside `n += SizeLengthDelim(1 + <here>)` or
// similar arithmetic.
func (e *emitter) emitPrimitiveSizeExpr(out io.Writer, expr string, t types.Type) error {
	b, ok := t.(*types.Basic)
	if !ok {
		return fmt.Errorf("emitPrimitiveSizeExpr: %T not a basic type", t)
	}
	switch b.Kind() {
	case types.Bool:
		fp(out, "gsbm.SizeBool()")
	case types.String:
		fp(out, "gsbm.SizeString(%s)", expr)
	case types.Int8, types.Int16, types.Int32, types.Int64, types.Int:
		fp(out, "gsbm.SizeVarint(int64(%s))", expr)
	case types.Uint8, types.Uint16, types.Uint32, types.Uint64, types.Uint, types.Uintptr:
		fp(out, "gsbm.SizeUvarint(uint64(%s))", expr)
	case types.Float32:
		fp(out, "gsbm.SizeFixed32()")
	case types.Float64:
		fp(out, "gsbm.SizeFixed64()")
	default:
		return fmt.Errorf("unsupported size kind %v", b.Kind())
	}
	return nil
}

// emitSliceSize emits a length-delim envelope sized as
// `SizeLengthDelim(SizeUvarint(len) + sum(per-element sizes))`. The body
// is summed into a depth-unique local so nested slices/maps don't shadow.
func (e *emitter) emitSliceSize(out io.Writer, expr string, t *types.Slice, accVar string, depth int) error {
	elemT := t.Elem()
	body := nm("body", depth)
	idx := nm("i", depth)
	fp(out, "\t{\n")
	fp(out, "\t\t%s := gsbm.SizeUvarint(uint64(len(%s)))\n", body, expr)
	fp(out, "\t\tfor %s := range %s {\n", idx, expr)
	elemExpr := fmt.Sprintf("%s[%s]", expr, idx)
	// Slice-of-pointer-to-named-struct: per-spec §5.1 each element is a
	// length-delim envelope carrying a presence byte plus (NonZero) the
	// element body.
	if ptr, ok := elemT.(*types.Pointer); ok {
		if named, ok := ptr.Elem().(*types.Named); ok {
			if _, isStruct := named.Underlying().(*types.Struct); isStruct {
				fp(out, "\t\t\tif %s == nil {\n", elemExpr)
				fp(out, "\t\t\t\t%s += gsbm.SizeLengthDelim(1)\n", body)
				fp(out, "\t\t\t} else {\n")
				fp(out, "\t\t\t\t%s += gsbm.SizeLengthDelim(1 + %s.SizeGSBM())\n", body, elemExpr)
				fp(out, "\t\t\t}\n")
				fp(out, "\t\t}\n")
				fp(out, "\t\t%s += gsbm.SizeLengthDelim(%s)\n", accVar, body)
				fp(out, "\t}\n")
				return nil
			}
		}
	}
	if named, ok := elemT.(*types.Named); ok {
		if _, isStruct := named.Underlying().(*types.Struct); isStruct {
			fp(out, "\t\t\t%s += gsbm.SizeLengthDelim(%s.SizeGSBM())\n", body, elemExpr)
			fp(out, "\t\t}\n")
			fp(out, "\t\t%s += gsbm.SizeLengthDelim(%s)\n", accVar, body)
			fp(out, "\t}\n")
			return nil
		}
	}
	// Primitive or nested composite element: recurse.
	if err := e.emitValueSize(out, elemExpr, elemT, body, depth+1); err != nil {
		return err
	}
	fp(out, "\t\t}\n")
	fp(out, "\t\t%s += gsbm.SizeLengthDelim(%s)\n", accVar, body)
	fp(out, "\t}\n")
	return nil
}

// emitMapSize sums each (key, value) pair's size into a depth-unique
// local. Iteration order doesn't matter — only the total byte count does
// — so we range over the map directly without sorting. When the key or
// value sizing collapses to a constant (bool kinds → SizeBool()), the
// corresponding range variable is blanked so the Go compiler doesn't
// flag it as declared-and-not-used.
func (e *emitter) emitMapSize(out io.Writer, expr string, t *types.Map, accVar string, depth int) error {
	if !isPrimitiveKey(t.Key()) {
		return fmt.Errorf("map key must be primitive or string")
	}
	body := nm("body", depth)
	kVar := nm("k", depth)
	vvVar := nm("vv", depth)

	keyName := kVar
	if !sizeExprReferencesArg(t.Key()) {
		keyName = "_"
	}
	vvName := vvVar
	if !sizeExprReferencesArg(t.Elem()) {
		vvName = "_"
	}

	fp(out, "\t{\n")
	fp(out, "\t\t%s := gsbm.SizeUvarint(uint64(len(%s)))\n", body, expr)
	fp(out, "\t\tfor %s, %s := range %s {\n", keyName, vvName, expr)
	keyT := t.Key()
	keyExprStr := kVar
	if named, ok := keyT.(*types.Named); ok {
		keyT = named.Underlying()
		keyExprStr = fmt.Sprintf("(%s)(%s)", e.typeExpr(keyT), kVar)
	}
	fp(out, "\t\t\t%s += ", body)
	if err := e.emitPrimitiveSizeExpr(out, keyExprStr, keyT); err != nil {
		return err
	}
	fp(out, "\n")
	if err := e.emitValueSize(out, vvVar, t.Elem(), body, depth+1); err != nil {
		return err
	}
	fp(out, "\t\t}\n")
	fp(out, "\t\t%s += gsbm.SizeLengthDelim(%s)\n", accVar, body)
	fp(out, "\t}\n")
	return nil
}

// sizeExprReferencesArg reports whether the size expression emitted for
// t references its argument expression. Only bool kinds (basic or named)
// compile to SizeBool(), which takes no argument; all other primitives,
// slices, maps, and struct types reference their value.
func sizeExprReferencesArg(t types.Type) bool {
	if n, ok := t.(*types.Named); ok {
		// Named-struct values call SizeGSBM() on the receiver — referenced.
		if _, isStruct := n.Underlying().(*types.Struct); isStruct {
			return true
		}
		return sizeExprReferencesArg(n.Underlying())
	}
	if b, ok := t.(*types.Basic); ok {
		return b.Kind() != types.Bool
	}
	return true
}
