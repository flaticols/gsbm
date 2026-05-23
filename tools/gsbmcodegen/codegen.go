// Package gsbmcodegen emits MarshalGSBM / UnmarshalGSBM companion files for
// the type closure discovered by tools/gsbmschema. The generator consumes
// the typechecked PackageSet plus the validated Schema and produces one
// generated Go file per non-generic struct in the closure, named
// <lowercase_typename>_gsbm.go and placed next to the handwritten source
// file. Generated code uses only storage/gsbm primitives — no reflection,
// all tag values inlined as integer literals.
//
// Scope notes:
//   - Generic origin types (those with type parameters) are skipped here.
//     Go does not allow a generic method body to dispatch on its type
//     parameter, so per-instantiation free functions would be required.
//     Tracked as a follow-up; no generics appear in the M3 closure tested.
//   - Opaque structs (//gsbm:opaque) are skipped — the schema flags them
//     so the codegen leaves their (re)marshaling to handwritten code.
//   - External types (declared in packages outside the input set) are
//     skipped because we cannot place generated files into foreign trees.
package gsbmcodegen

import (
	"bytes"
	"fmt"
	"go/format"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.flaticols.dev/gsbm/storage/gsbm"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/codecs"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/codecs/builtins"
	"go.flaticols.dev/gsbm/tools/gsbmschema"
)

// genWarnOut is where generation-time warnings are written. Tests can
// redirect this to a buffer to assert the codegen warns when a struct's
// declared tags exceed gsbm.MaxTrackedTag (the bitmap cap silently no-ops
// for higher tags, so the warning is the user's only signal).
var genWarnOut io.Writer = os.Stderr

// GeneratedFile is one emitted source file. The Path is absolute (taken
// from the source file's filesystem location) so callers can pass the
// list straight to a file writer or compare against committed goldens.
type GeneratedFile struct {
	Path     string
	Package  string
	TypeName string
	Contents []byte
}

// Generate runs the full codegen pass: discover writable named structs in
// the closure, find each one's source location and import path, and emit
// a companion file per type. The result is sorted by Path for stable
// ordering across runs.
//
// Custom codecs (`bin:"N,custom=Name"`) are resolved against the default
// built-in registry (see builtins.NewBuiltinRegistry). Callers that need
// project-specific codecs registered should use GenerateWithCodecs.
func Generate(ps *gsbmschema.PackageSet, schema *gsbmschema.Schema) ([]GeneratedFile, error) {
	return GenerateWithCodecs(ps, schema, builtins.NewBuiltinRegistry())
}

// GenerateWithCodecs is Generate with an explicit codec registry. Use this
// entry point when the schema references custom codecs that live outside
// the built-in set (e.g. a project-bound DecimalString codec).
func GenerateWithCodecs(ps *gsbmschema.PackageSet, schema *gsbmschema.Schema, reg *codecs.Registry) ([]GeneratedFile, error) {
	if ps == nil || schema == nil {
		return nil, fmt.Errorf("gsbmcodegen: nil input")
	}
	if reg == nil {
		reg = codecs.NewRegistry()
	}
	allowed := map[string]bool{}
	for _, p := range ps.Packages {
		allowed[p.Path] = true
	}
	// Hard-fail before emitting any file if the schema contains a non-opaque
	// generic origin or instantiation. Skipping was unsafe: a non-generic
	// parent referencing the generic instantiation would still be emitted
	// and call a non-existent MarshalGSBM. Opaque generics are exempt only
	// in the direct-value-field shape (`B Box[int]`) — the parent emits
	// `B.MarshalGSBM(w)` which Go's per-instantiation generic methods
	// resolve at compile time. Indirect uses (`*Box[int]`, `[]Box[int]`,
	// `map[K]Box[int]`, named-with-non-struct underlying like `Label[int]`)
	// flow through emit.go's typeExpr which renders named types without
	// type arguments, producing invalid Go (`&Box{}`, `MakeSlice[Box]`,
	// `var vv Box`, `Label(tmp)`). gsbmschema/discover.go's checkSupportedType
	// surfaces type/generic for those; this upfront walk is defense-in-depth
	// for callers that bypass Analyze/Validate.
	for _, sd := range schema.Structs {
		if len(sd.Generic) > 0 && !sd.Opaque {
			return nil, fmt.Errorf("gsbmcodegen: %s.%s: generic types are not supported — mark //gsbm:opaque with handwritten Marshal/Unmarshal/Reset, or replace with a non-generic type", sd.Type.PkgPath, sd.Type.Name)
		}
	}
	for _, sd := range schema.Structs {
		if sd.Opaque {
			continue
		}
		if !allowed[sd.Type.PkgPath] {
			continue
		}
		named, _ := lookupNamed(ps, sd.Type)
		if named == nil {
			continue
		}
		str, _ := named.Underlying().(*types.Struct)
		if str == nil {
			continue
		}
		if err := rejectIndirectGenerics(sd.Type.Name, str); err != nil {
			return nil, fmt.Errorf("gsbmcodegen: %w", err)
		}
	}

	var files []GeneratedFile
	for _, sd := range schema.Structs {
		if sd.Opaque {
			continue
		}
		if !allowed[sd.Type.PkgPath] {
			continue
		}
		warnIfMaxTagExceeded(sd)
		named, pkg := lookupNamed(ps, sd.Type)
		if named == nil {
			continue
		}
		path := ps.Fset.Position(named.Obj().Pos()).Filename
		if path == "" {
			continue
		}
		dir := filepath.Dir(path)
		fname := strings.ToLower(sd.Type.Name) + "_gsbm.go"
		out, err := emitFile(pkg, named, sd, reg)
		if err != nil {
			return nil, fmt.Errorf("gsbmcodegen: %s: %w", sd.Type.Name, err)
		}
		files = append(files, GeneratedFile{
			Path:     filepath.Join(dir, fname),
			Package:  pkg.Name(),
			TypeName: sd.Type.Name,
			Contents: out,
		})
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func lookupNamed(ps *gsbmschema.PackageSet, ref gsbmschema.TypeRef) (*types.Named, *types.Package) {
	for _, p := range ps.Packages {
		if p.Path != ref.PkgPath {
			continue
		}
		obj := p.Pkg.Scope().Lookup(ref.Name)
		if obj == nil {
			return nil, nil
		}
		tn, ok := obj.(*types.TypeName)
		if !ok {
			return nil, nil
		}
		named, ok := tn.Type().(*types.Named)
		if !ok {
			return nil, nil
		}
		return named, p.Pkg
	}
	return nil, nil
}

// emitFile produces a complete formatted Go source file for one struct.
func emitFile(pkg *types.Package, named *types.Named, sd *gsbmschema.StructDecl, reg *codecs.Registry) ([]byte, error) {
	str, _ := named.Underlying().(*types.Struct)
	if str == nil {
		return nil, fmt.Errorf("type %s is not a struct", named.Obj().Name())
	}
	e := &emitter{pkg: pkg, imports: map[string]string{}, nameToPath: map[string]string{}, reg: reg, callsiteIdx: map[string]int{}}
	e.runtimeAlias = e.addImport("go.flaticols.dev/gsbm/storage/gsbm", "gsbm")

	// Emit method bodies into a side buffer; we'll prepend the header and
	// imports once we know which packages were referenced.
	var methods bytes.Buffer
	if err := e.emitSize(&methods, named, str, sd); err != nil {
		return nil, err
	}
	methods.WriteString("\n")
	if err := e.emitMarshal(&methods, named, str, sd); err != nil {
		return nil, err
	}
	methods.WriteString("\n")
	if err := e.emitUnmarshal(&methods, named, str, sd); err != nil {
		return nil, err
	}
	methods.WriteString("\n")
	if err := e.emitReset(&methods, named, str, sd); err != nil {
		return nil, err
	}
	methods.WriteString("\n")
	if err := e.emitFieldPresent(&methods, named, str, sd); err != nil {
		return nil, err
	}

	// Collect callsite constants discovered while emitting methods into a
	// preamble that precedes the method bodies. Empty for analytic-only
	// structs — emits nothing and leaves goldens untouched.
	var body bytes.Buffer
	if len(e.callsites) > 0 {
		body.WriteString("const (\n")
		for _, cs := range e.callsites {
			fmt.Fprintf(&body, "\t%s uint64 = 0x%x\n", cs.name, cs.value)
		}
		body.WriteString(")\n\n")
	}
	body.Write(methods.Bytes())

	var out bytes.Buffer
	out.WriteString("// Code generated by gsbmcodegen. DO NOT EDIT.\n\n")
	fmt.Fprintf(&out, "package %s\n\n", pkg.Name())
	if len(e.imports) > 0 {
		out.WriteString("import (\n")
		paths := make([]string, 0, len(e.imports))
		for p := range e.imports {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			alias := e.imports[p]
			if alias == "" || alias == pathToIdent(lastPathSegment(p)) {
				fmt.Fprintf(&out, "\t%q\n", p)
			} else {
				fmt.Fprintf(&out, "\t%s %q\n", alias, p)
			}
		}
		out.WriteString(")\n\n")
	}
	out.Write(body.Bytes())

	formatted, err := format.Source(out.Bytes())
	if err != nil {
		// Surface unformatted content for debugging.
		return out.Bytes(), fmt.Errorf("format: %w", err)
	}
	return formatted, nil
}

// emitter accumulates per-file state.
type emitter struct {
	pkg     *types.Package
	imports map[string]string // pkgPath → alias used in the import block
	// nameToPath is the reverse index of imports, keyed by alias, used to
	// detect collisions: if two distinct import paths share a candidate
	// alias (commonly because their packages declare the same name), the
	// second registration must disambiguate.
	nameToPath map[string]string
	// reg resolves custom-codec names (`bin:"N,custom=Name"`) at emit time.
	// It is set by emitFile and consulted from emit.go's encode/decode
	// dispatchers when FieldDecl.Custom is non-empty. May be nil in tests
	// that don't exercise custom codecs.
	reg *codecs.Registry
	// callsites collects materializing-codec callsite constants needed by
	// the emitted methods, deduplicated by name. emitFile renders them as
	// a file-level const block before the method bodies. Empty when the
	// struct uses only analytic codecs (the common case).
	callsites []callsiteEntry
	// callsiteIdx maps a callsite name to its index in callsites so the
	// same (struct, tag) pair returns the same constant when SizeGSBM and
	// MarshalGSBM both visit it during a single emitFile pass.
	callsiteIdx map[string]int
	// currentStructFQN is the fully-qualified `<pkg-path>.<struct-name>`
	// of the struct whose body is currently being emitted. Set by
	// emitMarshal/emitSize before they walk fields and cleared after.
	// Used by callsiteFor to derive a stable, globally-unique callsite id
	// for a materializing-codec call site.
	currentStructFQN string
	// runtimeAlias is the import alias for the gsbm runtime package
	// (`go.flaticols.dev/gsbm/storage/gsbm`), captured from addImport so
	// emitted references survive disambiguation when the user package
	// declares a top-level identifier named `gsbm`. Mirrors the math/sort
	// alias-threading pattern.
	runtimeAlias string
	// borrowStrings is true while emitting a struct whose schema carries
	// //gsbm:borrow-strings. Decode emitters consult it to replace heap
	// string copies with unsafe aliases into the caller-owned blob, while
	// keeping allocator-backed readers on the Allocator path.
	borrowStrings bool
}

// callsiteEntry is one materializing-codec callsite constant scheduled for
// emission at file scope. Value is FNV-1a of currentStructFQN.<tag>; the
// constant is rendered as `const <name> uint64 = 0x<value>` so the field
// emitters can pass it inline to EmitFn calls. Choosing a hash (rather than
// a per-file counter) guarantees uniqueness across files that share a
// gsbm.Marshal call via nested struct walks — collisions would corrupt the
// Writer's scratch cache by aliasing two distinct fields' materialization.
type callsiteEntry struct {
	name  string
	value uint64
}

// addImport registers path in the import set and returns the alias the
// caller should qualify exported names with. name is obj.Pkg().Name() when
// the caller has a *types.Package (typeExpr); for path-only callers (custom
// codec imports, "math", "sort", ...) it is "" and the alias is derived
// from the path's last identifier-safe segment.
//
// Collisions — two distinct paths whose packages share a name — are
// resolved deterministically: walk the new path's segments right-to-left,
// prepending each (identifier-safe) segment to the candidate until unique;
// if no segment-derived alias is free, append a numeric suffix.
//
// A candidate that names a Go keyword, predeclared identifier, or an
// identifier the emitter itself introduces as a local in generated bodies
// (see reservedAliases) is treated as already-taken so disambiguation
// kicks in. Without this, an external `package error` or `package m`
// would be aliased as `error`/`m` and shadow either the builtin (breaking
// emitted `error` return signatures, `make`/`len`/`new`/`clear` calls)
// or a generator-introduced local (e.g. `m := w.BeginLengthDelim()`
// followed by `make([]m.Value, ...)`).
func (e *emitter) addImport(path, name string) string {
	if path == e.pkg.Path() {
		return ""
	}
	if alias, ok := e.imports[path]; ok {
		return alias
	}
	candidate := name
	if candidate == "" || !isValidGoIdent(candidate) {
		candidate = pathToIdent(lastPathSegment(path))
	}
	if e.aliasUnavailable(candidate, path) {
		candidate = e.disambiguateAlias(candidate, path)
	}
	e.imports[path] = candidate
	e.nameToPath[candidate] = path
	return candidate
}

// aliasUnavailable reports whether candidate is already bound to a
// different import path or is reserved (Go keyword / predeclared
// identifier / emitter-local name / package-scope identifier in the
// package being generated). The last guard prevents an import alias
// from shadowing a local named type that typeExpr emits bare for
// same-package references (codegen.go:534) — e.g. an external
// `package label` cannot bind as `label` when the local package
// declares `type label string`, because the emitted `var x label`
// would resolve to the import (file scope) instead of the type
// (package scope) and fail to compile. Used by addImport and
// disambiguateAlias to share collision logic.
func (e *emitter) aliasUnavailable(candidate, path string) bool {
	if reservedAliases[candidate] {
		return true
	}
	if other, taken := e.nameToPath[candidate]; taken && other != path {
		return true
	}
	if e.pkg != nil && e.pkg.Scope().Lookup(candidate) != nil {
		return true
	}
	return false
}

// disambiguateAlias finds a unique alias for path by walking its segments
// right-to-left (excluding the last, which produced the already-taken
// candidate) and prepending each identifier-safe segment to candidate. If
// every prefix is still taken, falls back to candidate+2, candidate+3, …
func (e *emitter) disambiguateAlias(candidate, path string) string {
	segs := strings.Split(path, "/")
	for i := len(segs) - 2; i >= 0; i-- {
		seg := pathToIdent(segs[i])
		if seg == "" {
			continue
		}
		try := seg + candidate
		if !isValidGoIdent(try) {
			continue
		}
		if !e.aliasUnavailable(try, path) {
			return try
		}
	}
	for n := 2; ; n++ {
		try := fmt.Sprintf("%s%d", candidate, n)
		if !e.aliasUnavailable(try, path) {
			return try
		}
	}
}

// structFQN returns "<pkg-path>.<struct-name>" for n, the stable global
// identifier used to derive callsite ids. Pkg-path is empty for builtin
// types (never reached for gsbm:root structs but kept defensive).
func structFQN(n *types.Named) string {
	obj := n.Obj()
	if obj.Pkg() == nil {
		return obj.Name()
	}
	return obj.Pkg().Path() + "." + obj.Name()
}

// callsiteFor returns the constant name to pass to a materializing codec's
// EmitFn for the field at tag within e.currentStructFQN. The constant is
// registered (lazily) so the same name is reused if SizeGSBM and MarshalGSBM
// both reach the same field — both passes must hit the same scratch key.
//
// The value is FNV-1a of "<pkg-path>.<struct>.<tag>" as a uint64, which
// is stable across regenerations and unique with overwhelming probability
// across all files participating in one gsbm.Marshal call. A counter-based
// id would collide when one struct's MarshalGSBM walks into a nested
// struct's MarshalGSBM under the same Writer — both files restart their
// counter at zero and the cache would alias unrelated fields.
func (e *emitter) callsiteFor(tag uint32) string {
	if e.currentStructFQN == "" {
		// emitMarshal/emitSize set this before walking fields; if we get here
		// the emitter has a bug — surface a deterministic constant that will
		// fail loudly when callsites collide rather than silently aliasing.
		return fmt.Sprintf("csUnknown_%d", tag)
	}
	short := e.currentStructFQN
	if i := strings.LastIndex(short, "."); i >= 0 {
		short = short[i+1:]
	}
	name := fmt.Sprintf("cs%s_%d", short, tag)
	if _, ok := e.callsiteIdx[name]; ok {
		return name
	}
	const offset64 uint64 = 1469598103934665603
	const prime64 uint64 = 1099511628211
	key := fmt.Sprintf("%s.%d", e.currentStructFQN, tag)
	h := offset64
	for i := 0; i < len(key); i++ {
		h ^= uint64(key[i])
		h *= prime64
	}
	e.callsiteIdx[name] = len(e.callsites)
	e.callsites = append(e.callsites, callsiteEntry{name: name, value: h})
	return name
}

// lastPathSegment returns the substring after the final "/" in path, or
// path itself when path has no separator.
func lastPathSegment(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// pathToIdent converts a path segment into a Go-identifier-safe candidate
// for use as an import alias. Hyphens and other punctuation are dropped;
// a leading digit is prefixed with "pkg"; an empty result returns "pkg".
// Used when the real package name (obj.Pkg().Name()) is unavailable —
// typically for custom-codec imports that ship as a path string only.
func pathToIdent(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range s {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" {
		return "pkg"
	}
	if out[0] >= '0' && out[0] <= '9' {
		out = "pkg" + out
	}
	return out
}

// reservedAliases names that addImport must never bind as an import
// alias, because doing so would either be illegal Go (keywords) or
// shadow an identifier referenced by emitted code (predeclared
// identifiers used in generated bodies, plus the unsuffixed locals the
// per-depth emitters introduce at depth 0 via nm()). Each entry's
// rationale:
//
//   - Go keywords (case, type, …): syntactically can't be identifiers.
//   - Predeclared types/values/functions (error, len, make, new, clear,
//     append, …): emitters reference these unqualified — `func ... error`,
//     `*new(T)`, `make([]T, n)`, `clear(m)`, `append(…)`. An import alias
//     equal to any of them would shadow the builtin in the file scope.
//   - Emitter locals (m, saved, n, k, vv, inner, state, keys, i, x, u,
//     err, r, w, v, present, tag): introduced unsuffixed at depth 0 by
//     emit.go and visible to type expressions that follow them in the
//     same block. A future emitter that adds a new unsuffixed local
//     must extend this set — the contract is brittle; prefixing all
//     emitter locals (e.g. _m, _saved) would remove the contract but
//     is a larger refactor.
var reservedAliases = func() map[string]bool {
	names := []string{
		// keywords
		"break", "case", "chan", "const", "continue", "default", "defer",
		"else", "fallthrough", "for", "func", "go", "goto", "if", "import",
		"interface", "map", "package", "range", "return", "select", "struct",
		"switch", "type", "var",
		// predeclared types
		"bool", "byte", "complex64", "complex128", "error", "float32",
		"float64", "int", "int8", "int16", "int32", "int64", "rune", "string",
		"uint", "uint8", "uint16", "uint32", "uint64", "uintptr", "any",
		"comparable",
		// predeclared values
		"true", "false", "iota", "nil",
		// predeclared functions
		"append", "cap", "clear", "close", "complex", "copy", "delete",
		"imag", "len", "make", "max", "min", "new", "panic", "print",
		"println", "real", "recover",
		// emitter-introduced locals at depth 0 (see nm() call sites in
		// emit.go and unsuffixed identifiers baked into format strings).
		// Suffixed variants at deeper depths (m_1, saved_2, …) are not
		// reserved because they're improbable as real package names; if
		// such a collision ever surfaces, extend this list.
		"m", "saved", "n", "k", "vv", "inner", "state", "keys", "i", "x",
		"u", "err", "r", "w", "v", "present", "tag", "wt",
		"idx", "bit", "cp", "tmp", "z", "cw", "b", "raw",
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}()

// isValidGoIdent reports whether s is a syntactically valid Go identifier.
// Empty strings and identifiers starting with a digit are rejected; the
// rest of the characters must each be a letter, digit, or underscore.
func isValidGoIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			continue
		}
		if i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

// typeExpr returns the Go source expression for a type, qualifying named
// references against the current package and tracking imports.
func (e *emitter) typeExpr(t types.Type) string {
	switch tt := t.(type) {
	case *types.Basic:
		return tt.Name()
	case *types.Named:
		obj := tt.Obj()
		if obj.Pkg() == nil || obj.Pkg().Path() == e.pkg.Path() {
			return obj.Name()
		}
		alias := e.addImport(obj.Pkg().Path(), obj.Pkg().Name())
		return alias + "." + obj.Name()
	case *types.Pointer:
		return "*" + e.typeExpr(tt.Elem())
	case *types.Slice:
		if isByteType(tt.Elem()) {
			return "[]byte"
		}
		return "[]" + e.typeExpr(tt.Elem())
	case *types.Array:
		return fmt.Sprintf("[%d]%s", tt.Len(), e.typeExpr(tt.Elem()))
	case *types.Map:
		return fmt.Sprintf("map[%s]%s", e.typeExpr(tt.Key()), e.typeExpr(tt.Elem()))
	default:
		return tt.String()
	}
}

// rejectIndirectGenerics walks every field of str looking for a generic
// instantiation in a position the codegen cannot render. Direct value
// fields whose type is a generic struct (e.g. `B Box[int]`) are allowed —
// the parent emits `B.MarshalGSBM(w)` and Go resolves the per-instantiation
// method at compile time. Anything inside a pointer/slice/map/array, or a
// named type whose underlying is not a struct, would funnel through
// typeExpr which strips type arguments and produces invalid Go.
func rejectIndirectGenerics(owner string, str *types.Struct) error {
	for f := range str.Fields() {
		if err := walkRejectGeneric(f.Type(), 0, owner, f.Name()); err != nil {
			return err
		}
	}
	return nil
}

func walkRejectGeneric(t types.Type, depth int, owner, fname string) error {
	if named, ok := t.(*types.Named); ok {
		if ta := named.TypeArgs(); ta != nil && ta.Len() > 0 {
			_, isStruct := named.Underlying().(*types.Struct)
			if depth > 0 || !isStruct {
				return fmt.Errorf("%s.%s: generic instantiation %s is not supported in this position — only a direct value field of a struct compiles; pointer/slice/map/array elements and named-with-non-struct underlying produce invalid Go", owner, fname, t.String())
			}
		}
	}
	switch tt := t.(type) {
	case *types.Pointer:
		return walkRejectGeneric(tt.Elem(), depth+1, owner, fname)
	case *types.Slice:
		return walkRejectGeneric(tt.Elem(), depth+1, owner, fname)
	case *types.Array:
		return walkRejectGeneric(tt.Elem(), depth+1, owner, fname)
	case *types.Map:
		if err := walkRejectGeneric(tt.Key(), depth+1, owner, fname); err != nil {
			return err
		}
		return walkRejectGeneric(tt.Elem(), depth+1, owner, fname)
	}
	return nil
}

// warnIfMaxTagExceeded prints a warning when sd has at least one declared
// tag higher than gsbm.MaxTrackedTag. The runtime sidecar bitmap silently
// no-ops on out-of-range tags (FieldPresent returns false for them), so
// this is the only signal the user gets that those tags will not be
// observable through FieldPresent.
func warnIfMaxTagExceeded(sd *gsbmschema.StructDecl) {
	var maxTag uint32
	for _, fd := range sd.Fields {
		if fd.Tag > maxTag {
			maxTag = fd.Tag
		}
	}
	if maxTag > gsbm.MaxTrackedTag {
		_, _ = fmt.Fprintf(genWarnOut, "gsbmcodegen: %s.%s: declared tag %d exceeds gsbm.MaxTrackedTag (%d); FieldPresent will return false for tags above the cap\n",
			sd.Type.PkgPath, sd.Type.Name, maxTag, gsbm.MaxTrackedTag)
	}
}

func isByteType(t types.Type) bool {
	b, ok := t.(*types.Basic)
	if !ok {
		return false
	}
	return b.Kind() == types.Byte || b.Kind() == types.Uint8
}
