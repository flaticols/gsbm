package gsbmcodegen

import (
	"go/types"
	"testing"
)

// newTestEmitter constructs an emitter whose current package is at "self",
// with empty import state. Tests register external paths against it and
// observe the alias decisions.
func newTestEmitter() *emitter {
	pkg := types.NewPackage("self", "self")
	return &emitter{
		pkg:        pkg,
		imports:    map[string]string{},
		nameToPath: map[string]string{},
	}
}

func TestAddImport_FirstRegistrationUsesRealName(t *testing.T) {
	e := newTestEmitter()
	got := e.addImport("example.com/a/common", "common")
	if got != "common" {
		t.Fatalf("alias = %q, want %q", got, "common")
	}
	if e.imports["example.com/a/common"] != "common" {
		t.Fatalf("imports[%q] = %q, want %q", "example.com/a/common", e.imports["example.com/a/common"], "common")
	}
	if e.nameToPath["common"] != "example.com/a/common" {
		t.Fatalf("nameToPath[%q] = %q, want %q", "common", e.nameToPath["common"], "example.com/a/common")
	}
}

func TestAddImport_CollisionDisambiguates(t *testing.T) {
	e := newTestEmitter()
	first := e.addImport("example.com/a/common", "common")
	if first != "common" {
		t.Fatalf("first alias = %q, want %q", first, "common")
	}
	second := e.addImport("example.com/b/common", "common")
	if second != "bcommon" {
		t.Fatalf("second alias = %q, want %q", second, "bcommon")
	}
	if e.imports["example.com/a/common"] != "common" {
		t.Fatalf("first path's alias drifted: got %q", e.imports["example.com/a/common"])
	}
	if e.imports["example.com/b/common"] != "bcommon" {
		t.Fatalf("second path's alias = %q, want bcommon", e.imports["example.com/b/common"])
	}
}

func TestAddImport_IdempotentReRegistration(t *testing.T) {
	e := newTestEmitter()
	_ = e.addImport("example.com/a/common", "common")
	_ = e.addImport("example.com/b/common", "common")
	// Re-register the first path; must return the originally-assigned alias.
	again := e.addImport("example.com/a/common", "common")
	if again != "common" {
		t.Fatalf("re-register first path returned %q, want %q", again, "common")
	}
	// Re-register the second path; same expectation.
	againB := e.addImport("example.com/b/common", "common")
	if againB != "bcommon" {
		t.Fatalf("re-register second path returned %q, want %q", againB, "bcommon")
	}
}

func TestAddImport_HyphenPathUsesRealName(t *testing.T) {
	e := newTestEmitter()
	got := e.addImport("github.com/foo/bar-baz", "barbaz")
	if got != "barbaz" {
		t.Fatalf("alias = %q, want %q", got, "barbaz")
	}
}

func TestAddImport_VersionSuffixPathUsesRealName(t *testing.T) {
	e := newTestEmitter()
	got := e.addImport("github.com/foo/v2", "foo")
	if got != "foo" {
		t.Fatalf("alias = %q, want %q", got, "foo")
	}
}

func TestAddImport_EmptyNameFallsBackToPath(t *testing.T) {
	e := newTestEmitter()
	got := e.addImport("example.com/codecs/decimal", "")
	if got != "decimal" {
		t.Fatalf("alias = %q, want %q", got, "decimal")
	}
}

func TestAddImport_EmptyNameHyphenPathSanitized(t *testing.T) {
	e := newTestEmitter()
	got := e.addImport("github.com/foo/bar-baz", "")
	if got != "barbaz" {
		t.Fatalf("alias = %q, want %q", got, "barbaz")
	}
}

func TestAddImport_TripleCollisionFallsThroughSegments(t *testing.T) {
	e := newTestEmitter()
	a := e.addImport("example.com/a/common", "common")
	b := e.addImport("example.com/b/common", "common")
	c := e.addImport("example.com/c/common", "common")
	if a != "common" || b != "bcommon" || c != "ccommon" {
		t.Fatalf("aliases = %q,%q,%q; want common,bcommon,ccommon", a, b, c)
	}
}

func TestAddImport_NumericFallbackWhenSegmentsExhausted(t *testing.T) {
	e := newTestEmitter()
	// Single-segment paths with the same name and no further segments to
	// prepend — collisions must fall through to incrementing numeric suffixes.
	a := e.addImport("foo", "foo")
	b := e.addImport("bar", "foo")
	c := e.addImport("baz", "foo")
	if a != "foo" || b != "foo2" || c != "foo3" {
		t.Fatalf("aliases = %q,%q,%q; want foo,foo2,foo3", a, b, c)
	}
}

// TestAddImport_MixedSourceCollision covers the realistic codegen scenario
// where one import comes through typeExpr (real name) and the other through
// a path-only call site (custom codec). Both should disambiguate even though
// the second caller has no *types.Package.
func TestAddImport_MixedSourceCollision(t *testing.T) {
	e := newTestEmitter()
	a := e.addImport("example.com/a/common", "common")
	b := e.addImport("example.com/b/common", "")
	if a != "common" {
		t.Fatalf("first (named) alias = %q, want common", a)
	}
	if b != "bcommon" {
		t.Fatalf("second (path-only) alias = %q, want bcommon", b)
	}
}

// TestAddImport_StdlibDisplacementReturnsUsableAlias guards the contract
// emit.go relies on: every call site that needs the stdlib `math` or `sort`
// package must qualify references with the alias addImport returns, not the
// literal string "math"/"sort". If a user-typed field whose package declares
// `package math` is registered first, the stdlib lookup must still surface a
// unique alias the caller can interpolate into format strings.
func TestAddImport_StdlibDisplacementReturnsUsableAlias(t *testing.T) {
	e := newTestEmitter()
	user := e.addImport("example.com/x/math", "math")
	stdlib := e.addImport("math", "")
	if user != "math" {
		t.Fatalf("first claimant alias = %q, want %q", user, "math")
	}
	if stdlib == user {
		t.Fatalf("stdlib alias %q collides with user package %q — emit.go would generate ambiguous references", stdlib, user)
	}
	if e.imports["math"] != stdlib {
		t.Fatalf("imports[\"math\"] = %q, want %q", e.imports["math"], stdlib)
	}
}

func TestAddImport_SamePackageReturnsEmpty(t *testing.T) {
	e := newTestEmitter()
	got := e.addImport("self", "self")
	if got != "" {
		t.Fatalf("same-package alias = %q, want empty", got)
	}
	if _, registered := e.imports["self"]; registered {
		t.Fatalf("same-package path should not be registered")
	}
}

func TestPathToIdent(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"common", "common"},
		{"bar-baz", "barbaz"},
		{"v2", "v2"},
		{"2foo", "pkg2foo"},
		{"", ""},
		{"---", "pkg"},
	}
	for _, tc := range tests {
		if got := pathToIdent(tc.in); got != tc.want {
			t.Errorf("pathToIdent(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestAddImport_ReservedPredeclaredDisambiguates guards against an
// external `package error` (or any predeclared identifier name) being
// bound as the alias `error`, which would shadow the builtin type in
// emitted `... error` return signatures. The disambiguation walk must
// pick a different alias derived from the path.
func TestAddImport_ReservedPredeclaredDisambiguates(t *testing.T) {
	cases := []struct {
		name string
		path string
		pkg  string
	}{
		{"error", "example.com/foo/error", "error"},
		{"len", "example.com/foo/len", "len"},
		{"make", "example.com/foo/make", "make"},
		{"new", "example.com/foo/new", "new"},
		{"clear", "example.com/foo/clear", "clear"},
		{"any", "example.com/foo/any", "any"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEmitter()
			got := e.addImport(tc.path, tc.pkg)
			if got == tc.pkg {
				t.Fatalf("alias = %q, must not equal predeclared identifier", got)
			}
			if !isValidGoIdent(got) {
				t.Fatalf("alias = %q, not a valid Go identifier", got)
			}
			if reservedAliases[got] {
				t.Fatalf("alias = %q, still reserved after disambiguation", got)
			}
		})
	}
}

// TestAddImport_ReservedEmitterLocalDisambiguates guards against an
// external `package m`, `package saved`, etc. being bound as that bare
// name; emit.go introduces unsuffixed locals with those names at depth
// 0 (e.g. `m := w.BeginLengthDelim()`) and a subsequent type expression
// using the package would be shadowed.
func TestAddImport_ReservedEmitterLocalDisambiguates(t *testing.T) {
	cases := []struct {
		name string
		path string
		pkg  string
	}{
		{"m", "example.com/foo/m", "m"},
		{"saved", "example.com/foo/saved", "saved"},
		{"tmp", "example.com/foo/tmp", "tmp"},
		{"n", "example.com/foo/n", "n"},
		{"present", "example.com/foo/present", "present"},
		{"wt", "example.com/foo/wt", "wt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEmitter()
			got := e.addImport(tc.path, tc.pkg)
			if got == tc.pkg {
				t.Fatalf("alias = %q, must not equal emitter-local name", got)
			}
			if !isValidGoIdent(got) {
				t.Fatalf("alias = %q, not a valid Go identifier", got)
			}
			if reservedAliases[got] {
				t.Fatalf("alias = %q, still reserved after disambiguation", got)
			}
		})
	}
}

// TestReservedAliases_NumericFallbackEscapesReservation pins that the
// numeric-suffix fallback never lands on another reserved name (no
// `error2`, `m2` collisions to worry about today, but the assertion
// keeps that invariant explicit).
func TestReservedAliases_NumericFallbackEscapesReservation(t *testing.T) {
	for name := range reservedAliases {
		if reservedAliases[name+"2"] {
			t.Fatalf("reserved alias %q has reserved numeric fallback %q2 — disambiguation could loop", name, name)
		}
	}
}

// TestAddImport_LocalPackageScopeDisambiguates guards against an import
// alias shadowing a package-scope identifier (type, func, var, const) in
// the package being generated. typeExpr emits bare names for same-package
// named types, so binding an import as `label` while the local package
// declares `type label string` would produce a generated file where
// `var x label` resolves to the import (file scope wins over package
// scope) and fails to compile.
func TestAddImport_LocalPackageScopeDisambiguates(t *testing.T) {
	pkg := types.NewPackage("self", "self")
	// Inject a package-scope type named "label".
	scope := pkg.Scope()
	labelObj := types.NewTypeName(0, pkg, "label", nil)
	types.NewNamed(labelObj, types.Typ[types.String], nil)
	scope.Insert(labelObj)

	e := &emitter{
		pkg:        pkg,
		imports:    map[string]string{},
		nameToPath: map[string]string{},
	}
	got := e.addImport("example.com/x/label", "label")
	if got == "label" {
		t.Fatalf("alias = %q, must not equal local type name", got)
	}
	if !isValidGoIdent(got) {
		t.Fatalf("alias = %q, not a valid Go identifier", got)
	}
}

// TestAddImport_RuntimeAliasSurvivesLocalGsbmIdent guards the contract that
// emitFile and emit.go's fp() format strings rely on: when the user package
// declares a top-level identifier named `gsbm`, the runtime import path
// `go.flaticols.dev/gsbm/storage/gsbm` must disambiguate to a different
// alias *and* every emitted reference must use that alias rather than the
// hardcoded literal `gsbm.`. The emitter records the returned alias as
// runtimeAlias so downstream emitters can interpolate it.
func TestAddImport_RuntimeAliasSurvivesLocalGsbmIdent(t *testing.T) {
	pkg := types.NewPackage("self", "self")
	// Inject a package-scope identifier named "gsbm" — e.g. a user-declared
	// type, var, or function in the package being generated. typeExpr's
	// bare-emission rule for same-package names means `gsbm` is already
	// resolvable at package scope; binding an import alias as `gsbm`
	// (file scope) would shadow it on the next line and force the
	// emitted generated file to fail compilation.
	scope := pkg.Scope()
	gsbmObj := types.NewTypeName(0, pkg, "gsbm", nil)
	types.NewNamed(gsbmObj, types.Typ[types.String], nil)
	scope.Insert(gsbmObj)

	e := &emitter{
		pkg:        pkg,
		imports:    map[string]string{},
		nameToPath: map[string]string{},
	}
	got := e.addImport("go.flaticols.dev/gsbm/storage/gsbm", "gsbm")
	if got == "gsbm" {
		t.Fatalf("runtime alias = %q, must not equal local package-scope identifier", got)
	}
	if !isValidGoIdent(got) {
		t.Fatalf("runtime alias = %q, not a valid Go identifier", got)
	}
}

func TestIsValidGoIdent(t *testing.T) {
	cases := map[string]bool{
		"foo":    true,
		"Foo":    true,
		"foo_2":  true,
		"_foo":   true,
		"":       false,
		"2foo":   false,
		"foo-x":  false,
		"foo.x":  false,
		"foo bar": false,
	}
	for in, want := range cases {
		if got := isValidGoIdent(in); got != want {
			t.Errorf("isValidGoIdent(%q) = %v, want %v", in, got, want)
		}
	}
}
