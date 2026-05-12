package gsbmcodegen_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.flaticols.dev/gsbm/tools/gsbmcodegen"
	"go.flaticols.dev/gsbm/tools/gsbmschema"
)

// fixtureDir returns the absolute path of tools/gsbmcodegen/fixtures/sample
// based on the test source location, so the test runs regardless of cwd.
func fixtureDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "fixtures", "sample")
}

// loadFixture wraps gsbmschema.LoadFromDirs with a t.Fatalf on error.
// LoadFromDirs already skips _test.go and _gsbm{,_arena}.go siblings.
func loadFixture(t *testing.T, dir string) *gsbmschema.PackageSet {
	t.Helper()
	ps, err := gsbmschema.LoadFromDirs([]string{dir})
	if err != nil {
		t.Fatalf("LoadFromDirs(%s): %v", dir, err)
	}
	return ps
}

// TestGoldenSample asserts every committed <type>_gsbm.go is byte-identical
// to what Generate produces from the live fixture. This is the contract
// that lets us hand-edit the fixture and regenerate without surprises.
func TestGoldenSample(t *testing.T) {
	dir := fixtureDir(t)
	ps := loadFixture(t, dir)
	res := gsbmschema.Analyze(ps)
	if len(res.Issues) > 0 {
		t.Fatalf("schema issues: %s", gsbmschema.FormatIssues(res.Issues))
	}
	files, err := gsbmcodegen.Generate(ps, res.Schema)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no files generated")
	}
	for _, gf := range files {
		want, err := os.ReadFile(gf.Path)
		if err != nil {
			t.Errorf("%s: missing committed golden — write the file then re-run: %v", gf.Path, err)
			t.Logf("--- generated %s ---\n%s", gf.Path, gf.Contents)
			continue
		}
		if string(want) != string(gf.Contents) {
			t.Errorf("%s: drifted from golden", gf.Path)
			t.Logf("--- generated ---\n%s", gf.Contents)
			t.Logf("--- golden ---\n%s", want)
		}
	}
}

// TestGoldenSampleArena asserts every committed <root>_gsbm_arena.go is
// byte-identical to what GenerateArena produces. The arena generator
// emits per-root helpers only — non-root structs (Customer, Item, Total)
// must NOT appear in the output.
func TestGoldenSampleArena(t *testing.T) {
	dir := fixtureDir(t)
	ps := loadFixture(t, dir)
	res := gsbmschema.Analyze(ps)
	if len(res.Issues) > 0 {
		t.Fatalf("schema issues: %s", gsbmschema.FormatIssues(res.Issues))
	}
	files, err := gsbmcodegen.GenerateArena(ps, res.Schema)
	if err != nil {
		t.Fatalf("generate arena: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no arena files generated")
	}
	// Order and Renamed are the //gsbm:root types in the fixture.
	wantRoots := map[string]bool{"Order": true, "Renamed": true}
	if len(files) != len(wantRoots) {
		t.Errorf("expected %d arena files (one per //gsbm:root); got %d", len(wantRoots), len(files))
	}
	for _, gf := range files {
		if !wantRoots[gf.TypeName] {
			t.Errorf("unexpected arena file for non-root %s", gf.TypeName)
		}
		delete(wantRoots, gf.TypeName)
	}
	for missing := range wantRoots {
		t.Errorf("missing arena file for root %s", missing)
	}
	for _, gf := range files {
		want, err := os.ReadFile(gf.Path)
		if err != nil {
			t.Errorf("%s: missing committed golden — write the file then re-run: %v", gf.Path, err)
			t.Logf("--- generated %s ---\n%s", gf.Path, gf.Contents)
			continue
		}
		if string(want) != string(gf.Contents) {
			t.Errorf("%s: drifted from golden", gf.Path)
			t.Logf("--- generated ---\n%s", gf.Contents)
			t.Logf("--- golden ---\n%s", want)
		}
	}
}

// TestGenerateRejectsGenerics — Generate must hard-error when a generic
// origin or instantiation reaches it. Skipping with a warning is unsafe:
// a non-generic parent referencing the generic instantiation would still
// emit and call a non-existent MarshalGSBM method, producing a file that
// fails `go build`. This is defense-in-depth for callers that bypass
// gsbmschema.Validate (which surfaces type/generic earlier).
func TestGenerateRejectsGenerics(t *testing.T) {
	src := `package p

type Box[T any] struct {
	Value T ` + "`bin:\"1\"`" + `
}

//gsbm:root
type Root struct {
	B Box[int] ` + "`bin:\"1\"`" + `
}
`
	ps, err := gsbmschema.ParseSource("p", []string{src})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := gsbmschema.Discover(ps)
	schema, _ := gsbmschema.BuildSchema(ps, roots)
	if _, err := gsbmcodegen.Generate(ps, schema); err == nil {
		t.Fatal("expected Generate to error on generic type, got nil")
	} else if !strings.Contains(err.Error(), "generic") {
		t.Fatalf("expected error mentioning 'generic', got %v", err)
	}
	if _, err := gsbmcodegen.GenerateArena(ps, schema); err == nil {
		t.Fatal("expected GenerateArena to error on generic type, got nil")
	} else if !strings.Contains(err.Error(), "generic") {
		t.Fatalf("expected arena error mentioning 'generic', got %v", err)
	}
}

// TestGenerateAllowsOpaqueGenerics — Generate and GenerateArena must NOT
// reject a generic struct that is marked //gsbm:opaque. Opaque generics
// are skipped (handwritten Marshal/Unmarshal/Reset), and Go's
// per-instantiation generic methods make Box[int].MarshalGSBM resolve at
// the parent's call site, so the parent's generated code still compiles.
func TestGenerateAllowsOpaqueGenerics(t *testing.T) {
	src := `package p

//gsbm:opaque
type Box[T any] struct {
	Value T
}

//gsbm:root
type Root struct {
	B Box[int] ` + "`bin:\"1\"`" + `
}
`
	ps, err := gsbmschema.ParseSource("p", []string{src})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := gsbmschema.Discover(ps)
	schema, _ := gsbmschema.BuildSchema(ps, roots)
	if _, err := gsbmcodegen.Generate(ps, schema); err != nil {
		t.Fatalf("Generate must accept opaque generics, got %v", err)
	}
	if _, err := gsbmcodegen.GenerateArena(ps, schema); err != nil {
		t.Fatalf("GenerateArena must accept opaque generics, got %v", err)
	}
}

// TestGenerateRejectsIndirectOpaqueGenerics — defense-in-depth for callers
// that bypass gsbmschema.Analyze. The opaque-generic exemption only covers
// the direct-value-field case (`B Box[int]`); indirect uses (`*Box[int]`,
// `[]Box[int]`, `map[K]Box[int]`) flow through typeExpr which strips type
// arguments. Generate and GenerateArena must hard-fail before emitting
// uncompilable output.
func TestGenerateRejectsIndirectOpaqueGenerics(t *testing.T) {
	cases := []struct {
		name  string
		field string
	}{
		{"pointer", "B *Box[int]"},
		{"slice", "B []Box[int]"},
		{"map", "B map[string]Box[int]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := `package p

//gsbm:opaque
type Box[T any] struct {
	Value T
}

//gsbm:root
type Root struct {
	` + tc.field + " `bin:\"1\"`" + `
}
`
			ps, err := gsbmschema.ParseSource("p", []string{src})
			if err != nil {
				t.Fatal(err)
			}
			roots, _ := gsbmschema.Discover(ps)
			schema, _ := gsbmschema.BuildSchema(ps, roots)
			if _, err := gsbmcodegen.Generate(ps, schema); err == nil {
				t.Fatalf("Generate must reject indirect opaque-generic %q", tc.field)
			} else if !strings.Contains(err.Error(), "generic") {
				t.Fatalf("expected error mentioning 'generic', got %v", err)
			}
			if _, err := gsbmcodegen.GenerateArena(ps, schema); err == nil {
				t.Fatalf("GenerateArena must reject indirect opaque-generic %q", tc.field)
			} else if !strings.Contains(err.Error(), "generic") {
				t.Fatalf("expected arena error mentioning 'generic', got %v", err)
			}
		})
	}
}

// TestGenerateArenaRejectsOpaqueGenericRoot — even when an opaque generic
// has handwritten methods, the arena emit body writes
// `gsbmarena.AllocStruct[Root](a)` which needs a concrete type name. A
// generic root would render as `AllocStruct[Box]` and fail to compile.
func TestGenerateArenaRejectsOpaqueGenericRoot(t *testing.T) {
	src := `package p

//gsbm:root
//gsbm:opaque
type Box[T any] struct {
	Value T
}
`
	ps, err := gsbmschema.ParseSource("p", []string{src})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := gsbmschema.Discover(ps)
	schema, _ := gsbmschema.BuildSchema(ps, roots)
	if _, err := gsbmcodegen.GenerateArena(ps, schema); err == nil {
		t.Fatal("expected GenerateArena to error on opaque generic root")
	} else if !strings.Contains(err.Error(), "generic") {
		t.Fatalf("expected error mentioning 'generic', got %v", err)
	}
}

// TestGenerateEmitsPresenceTracking asserts every generated file emits
// the local-bitmap presence-tracking shape: a `var present [N]uint64`
// declaration at the top of UnmarshalGSBM, at least one `present[i] |=`
// write on a known-tag case, and a FieldPresent method routed through
// the legacy gsbm.IsPresent sidecar surface (which returns false for
// default-mode receivers, per the new contract). The fixture has at
// least one Marshal-only-required-primitive struct (Customer / Item /
// Total / Renamed) plus the heterogeneous Order, so asserting on every
// emitted file gives broad coverage.
func TestGenerateEmitsPresenceTracking(t *testing.T) {
	dir := fixtureDir(t)
	ps := loadFixture(t, dir)
	res := gsbmschema.Analyze(ps)
	if len(res.Issues) > 0 {
		t.Fatalf("schema issues: %s", gsbmschema.FormatIssues(res.Issues))
	}
	files, err := gsbmcodegen.Generate(ps, res.Schema)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no files generated")
	}
	for _, gf := range files {
		body := string(gf.Contents)
		if !strings.Contains(body, "var present [") {
			t.Errorf("%s: missing local `var present [N]uint64` declaration", gf.Path)
		}
		if !strings.Contains(body, "present[") || !strings.Contains(body, "] |= 1 <<") {
			t.Errorf("%s: missing `present[i] |= 1 << bit` bitmap write", gf.Path)
		}
		if strings.Contains(body, "gsbm.MarkPresent(v,") {
			t.Errorf("%s: generated code must not call gsbm.MarkPresent (replaced by local bitmap)", gf.Path)
		}
		wantSig := "func (v *" + gf.TypeName + ") FieldPresent(tag uint32) bool"
		if !strings.Contains(body, wantSig) {
			t.Errorf("%s: missing FieldPresent method signature %q", gf.Path, wantSig)
		}
		if !strings.Contains(body, "return gsbm.IsPresent(v, tag)") {
			t.Errorf("%s: FieldPresent body does not delegate to gsbm.IsPresent", gf.Path)
		}
	}
}

// TestWarnIfMaxTagExceeded asserts the codegen emits a warning when a
// struct declares a tag higher than gsbm.MaxTrackedTag, and stays silent
// for tags at or below the cap. The warning is the user's only signal —
// the runtime sidecar silently no-ops on out-of-range tags — so a
// regression that drops it would be invisible without this guard.
func TestWarnIfMaxTagExceeded(t *testing.T) {
	cases := []struct {
		name    string
		tag     uint32
		warned  bool
		message string // substring expected when warned == true
	}{
		{name: "below cap", tag: 1, warned: false},
		{name: "at cap", tag: 1024, warned: false},
		{name: "above cap", tag: 1025, warned: true, message: "exceeds gsbm.MaxTrackedTag"},
		{name: "far above cap", tag: 9999, warned: true, message: "9999"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf strings.Builder
			restore := gsbmcodegen.SetWarnOut(&buf)
			t.Cleanup(func() { gsbmcodegen.SetWarnOut(restore) })

			sd := &gsbmschema.StructDecl{
				Type:   gsbmschema.TypeRef{PkgPath: "example.com/p", Name: "Big"},
				Fields: []*gsbmschema.FieldDecl{{Name: "F", Tag: tc.tag, Type: "string", Wire: gsbmschema.WireLengthDelim}},
			}
			gsbmcodegen.WarnIfMaxTagExceeded(sd)

			got := buf.String()
			if tc.warned {
				if got == "" {
					t.Fatalf("expected a warning for tag %d, got nothing", tc.tag)
				}
				if !strings.Contains(got, tc.message) {
					t.Fatalf("warning %q does not contain %q", got, tc.message)
				}
				if !strings.Contains(got, "Big") {
					t.Fatalf("warning %q does not name the offending struct", got)
				}
			} else {
				if got != "" {
					t.Fatalf("expected no warning for tag %d, got %q", tc.tag, got)
				}
			}
		})
	}
}

// TestGenerateIDRefRejectsMissingIDTag — codegen must surface a stable
// idref/missing-id-tag error when an id_ref field points at a struct
// that has no bin:"1" field. The schema validator accepts the cycle
// (the marker is present), but codegen cannot pick an ID field to
// encode, so it must hard-fail before emitting partial output.
func TestGenerateIDRefRejectsMissingIDTag(t *testing.T) {
	src := `package p

//gsbm:root
type Node struct {
	Name string ` + "`bin:\"2\"`" + `
	Next *Node  ` + "`bin:\"3,id_ref\"`" + `
}
`
	ps, err := gsbmschema.ParseSource("p", []string{src})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := gsbmschema.Discover(ps)
	schema, _ := gsbmschema.BuildSchema(ps, roots)
	if _, err := gsbmcodegen.Generate(ps, schema); err == nil {
		t.Fatal("expected Generate to error on missing bin:\"1\" id field, got nil")
	} else if !strings.Contains(err.Error(), "idref/missing-id-tag") {
		t.Fatalf("expected error code 'idref/missing-id-tag', got %v", err)
	}
}

// TestGenerateIDRefRejectsNonPrimitiveIDField — an id_ref target whose
// bin:"1" field is not a primitive (e.g., a nested struct or slice) has
// no natural leaf-scalar wire encoding. Codegen must surface
// idref/missing-id-tag rather than emit code that would call a struct
// MarshalGSBM into the leaf-scalar slot.
func TestGenerateIDRefRejectsNonPrimitiveIDField(t *testing.T) {
	src := `package p

type Key struct {
	V string ` + "`bin:\"1\"`" + `
}

//gsbm:root
type Node struct {
	ID   Key   ` + "`bin:\"1\"`" + `
	Next *Node ` + "`bin:\"2,id_ref\"`" + `
}
`
	ps, err := gsbmschema.ParseSource("p", []string{src})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := gsbmschema.Discover(ps)
	schema, _ := gsbmschema.BuildSchema(ps, roots)
	if _, err := gsbmcodegen.Generate(ps, schema); err == nil {
		t.Fatal("expected Generate to error on non-primitive bin:\"1\" id field, got nil")
	} else if !strings.Contains(err.Error(), "idref/missing-id-tag") {
		t.Fatalf("expected error code 'idref/missing-id-tag', got %v", err)
	}
}

// TestGenerateIDRefIntID — an id_ref field whose target's bin:"1" is an
// integer must produce a field key with VARINT wire type and encode the
// integer ID directly. This is the cousin of the string-ID case in the
// cyclebreak fixture; both paths share the same wireTypeForValue lookup.
func TestGenerateIDRefIntID(t *testing.T) {
	src := `package p

//gsbm:root
type Node struct {
	ID   int64 ` + "`bin:\"1\"`" + `
	Next *Node ` + "`bin:\"2,id_ref\"`" + `
}
`
	ps, err := gsbmschema.ParseSource("p", []string{src})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := gsbmschema.Discover(ps)
	schema, _ := gsbmschema.BuildSchema(ps, roots)
	files, err := gsbmcodegen.Generate(ps, schema)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no files generated")
	}
	body := string(files[0].Contents)
	if !strings.Contains(body, "w.WriteTag(2, gsbm.WireVarint)") {
		t.Errorf("expected id_ref field key with WireVarint for int64 ID, got body:\n%s", body)
	}
	if !strings.Contains(body, "w.WriteVarint(int64(v.Next.ID))") {
		t.Errorf("expected encoder to emit Next.ID as varint, got body:\n%s", body)
	}
}

// TestGenerateIDRefNamedByteSlice — an id_ref target whose bin:"1" is a
// defined-but-not-struct named type wrapping []byte (e.g. `type ID
// []byte`) must produce code that compiles. The earlier decode path fell
// through to emitPrimitiveDecodeAssign with a *types.Slice and errored
// out at codegen time; the named-byte-slice branch in emitValueDecode
// reads raw bytes and converts to the named type.
func TestGenerateIDRefNamedByteSlice(t *testing.T) {
	src := `package p

type ID []byte

//gsbm:root
type Node struct {
	ID   ID    ` + "`bin:\"1\"`" + `
	Next *Node ` + "`bin:\"2,id_ref\"`" + `
}
`
	ps, err := gsbmschema.ParseSource("p", []string{src})
	if err != nil {
		t.Fatal(err)
	}
	// Exercise the full toolchain pipeline (Analyze, the same call
	// cmd/gsbmschema gen makes) so a regression where Analyze rejects
	// `type ID []byte` shows up here — not just at codegen time after
	// Discover/BuildSchema issues are discarded.
	res := gsbmschema.Analyze(ps)
	if len(res.Issues) > 0 {
		t.Fatalf("unexpected analyze issues: %s", gsbmschema.FormatIssues(res.Issues))
	}
	files, err := gsbmcodegen.Generate(ps, res.Schema)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no files generated")
	}
	body := string(files[0].Contents)
	// Encode side: unwrap to []byte then WriteBytes.
	if !strings.Contains(body, "w.WriteBytes(([]byte)(v.Next.ID))") {
		t.Errorf("expected encoder to emit Next.ID via WriteBytes with []byte conversion, got body:\n%s", body)
	}
	// Decode side: read raw bytes, copy into expr's backing array
	// (reusing capacity), preserving the named ID type.
	if !strings.Contains(body, "v.Next.ID = append(v.Next.ID[:0], raw...)") {
		t.Errorf("expected decoder to assign Next.ID via append(v.Next.ID[:0], raw...), got body:\n%s", body)
	}
}

// TestGenerateOptionalNamedByteSlice — an optional field whose pointee is
// a named []byte (e.g. `*Blob` with `type Blob []byte`) must round-trip
// through codegen. Discover accepts the named-byte-slice underlying for
// the id_ref ID-field case, but the same relaxation also lets `*Blob`
// reach codegen; without the named-byte-slice branch in
// emitOptionalDecode the underlying *types.Slice routes into
// emitPrimitiveDecodeAssign and codegen errors at generation time.
func TestGenerateOptionalNamedByteSlice(t *testing.T) {
	src := `package p

type Blob []byte

//gsbm:root
type Root struct {
	Data *Blob ` + "`bin:\"1\"`" + `
}
`
	ps, err := gsbmschema.ParseSource("p", []string{src})
	if err != nil {
		t.Fatal(err)
	}
	res := gsbmschema.Analyze(ps)
	if len(res.Issues) > 0 {
		t.Fatalf("unexpected analyze issues: %s", gsbmschema.FormatIssues(res.Issues))
	}
	files, err := gsbmcodegen.Generate(ps, res.Schema)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no files generated")
	}
	body := string(files[0].Contents)
	// Decode side must produce a named-byte-slice address, not route
	// through emitPrimitiveDecodeAssign (which errors on *types.Slice).
	if !strings.Contains(body, "tmp := Blob(append([]byte(nil), raw...))") {
		t.Errorf("expected decoder to build *Blob via Blob(append([]byte(nil), raw...)), got body:\n%s", body)
	}
	if !strings.Contains(body, "v.Data = &tmp") {
		t.Errorf("expected decoder to assign v.Data = &tmp, got body:\n%s", body)
	}
}

// TestGenerateNamedByteSliceCrossPackageAliasShadow — when the named
// []byte target lives in a sibling package whose default import alias
// collides with the local-variable name the decoder declares for the
// ReadBytes() result, codegen must rename the local rather than emit
// `raw, err := r.ReadBytes(); ...; raw.ID(...)` (which fails to compile
// because the local shadows the package and `raw.ID` resolves as a
// selector on []byte). Regression: an earlier rename from `b` to `raw`
// only moved the collision; the same shape recurs for any plausible
// short alias. Exercises both the optional and value named-byte-slice
// branches in emit.go.
func TestGenerateNamedByteSliceCrossPackageAliasShadow(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"),
		[]byte("module example.com/proj\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rawDir := filepath.Join(root, "raw")
	rootDir := filepath.Join(root, "root")
	for _, d := range []string{rawDir, rootDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Sibling package whose default import alias is "raw" — the same
	// identifier the decoder used to hard-code for its ReadBytes() local.
	rawSrc := []byte(`package raw

type ID []byte

type Blob []byte
`)
	if err := os.WriteFile(filepath.Join(rawDir, "raw.go"), rawSrc, 0o644); err != nil {
		t.Fatal(err)
	}
	// Root references *raw.ID via id_ref (exercises emitOptionalDecode's
	// named-byte-slice branch) and a value raw.ID field (exercises
	// emitValueDecode's). The id_ref target's bin:"1" must itself be a
	// raw.ID so Discover picks the named-byte-slice underlying.
	rootSrc := []byte(`package root

import "example.com/proj/raw"

//gsbm:root
type Node struct {
	ID   raw.ID    ` + "`bin:\"1\"`" + `
	Next *Node     ` + "`bin:\"2,id_ref\"`" + `
	Data *raw.Blob ` + "`bin:\"3\"`" + `
}
`)
	if err := os.WriteFile(filepath.Join(rootDir, "root.go"), rootSrc, 0o644); err != nil {
		t.Fatal(err)
	}
	ps, err := gsbmschema.LoadFromDirs([]string{rawDir, rootDir})
	if err != nil {
		t.Fatalf("LoadFromDirs: %v", err)
	}
	res := gsbmschema.Analyze(ps)
	if len(res.Issues) > 0 {
		t.Fatalf("unexpected analyze issues: %s", gsbmschema.FormatIssues(res.Issues))
	}
	files, err := gsbmcodegen.Generate(ps, res.Schema)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var rootBody string
	for _, gf := range files {
		if strings.Contains(gf.Path, filepath.Join("root", "")) || strings.HasSuffix(filepath.Dir(gf.Path), "root") {
			rootBody = string(gf.Contents)
			break
		}
	}
	if rootBody == "" {
		t.Fatalf("no generated file for root package; got %d files", len(files))
	}
	// The decoder MUST NOT declare `raw, err := r.ReadBytes()` because
	// `raw` is the import alias and the next line dereferences `raw.ID`
	// / `raw.Blob` for the type conversion.
	if strings.Contains(rootBody, "raw, err := r.ReadBytes()") {
		t.Errorf("decoder declares local `raw` that shadows the `raw` import alias:\n%s", rootBody)
	}
	// Positive checks: a non-conflicting local name is used in both
	// branches, and the type conversion reaches the qualified raw.ID /
	// raw.Blob types.
	if !strings.Contains(rootBody, "raw_, err := r.ReadBytes()") {
		t.Errorf("expected decoder to use a fallback local (raw_) instead of `raw`, got body:\n%s", rootBody)
	}
	if !strings.Contains(rootBody, "v.ID = append(v.ID[:0], raw_...)") {
		t.Errorf("expected value branch to reuse capacity via append(v.ID[:0], raw_...), got body:\n%s", rootBody)
	}
	if !strings.Contains(rootBody, "tmp := raw.Blob(append([]byte(nil), raw_...))") {
		t.Errorf("expected optional branch to convert via raw.Blob(...), got body:\n%s", rootBody)
	}
}

// TestGenerateNamedByteSliceSamePackageShadow guards against the local
// `raw` shadowing a same-package named []byte type whose Go name is also
// `raw`. emitOptionalDecode generates `tmp := raw(append(... raw...))`
// for an optional *raw field; if the local equals the type name the
// conversion resolves `raw` to the local var and the file fails to build.
func TestGenerateNamedByteSliceSamePackageShadow(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"),
		[]byte("module example.com/proj\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(root, "pkg")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := []byte(`package pkg

type raw []byte

//gsbm:root
type Root struct {
	Data *raw ` + "`bin:\"1\"`" + `
}
`)
	if err := os.WriteFile(filepath.Join(pkgDir, "pkg.go"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	ps, err := gsbmschema.LoadFromDirs([]string{pkgDir})
	if err != nil {
		t.Fatalf("LoadFromDirs: %v", err)
	}
	res := gsbmschema.Analyze(ps)
	if len(res.Issues) > 0 {
		t.Fatalf("unexpected analyze issues: %s", gsbmschema.FormatIssues(res.Issues))
	}
	files, err := gsbmcodegen.Generate(ps, res.Schema)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("expected generated files, got none")
	}
	body := string(files[0].Contents)
	// The decoder MUST NOT declare `raw, err := r.ReadBytes()` because the
	// next line conversion `raw(append(... raw...))` would resolve to the
	// local var instead of the type.
	if strings.Contains(body, "raw, err := r.ReadBytes()") {
		t.Errorf("decoder declares local `raw` that shadows same-package type `raw`:\n%s", body)
	}
	if !strings.Contains(body, "raw_, err := r.ReadBytes()") {
		t.Errorf("expected decoder to use fallback local (raw_), got body:\n%s", body)
	}
	if !strings.Contains(body, "tmp := raw(append([]byte(nil), raw_...))") {
		t.Errorf("expected optional branch to convert via raw(...), got body:\n%s", body)
	}
}

// TestGenerateOptionalNamedByteSliceSavedShadow guards against the
// `saved` and `state` length-delim/presence locals shadowing a same-
// package named []byte type whose Go name is `saved` or `state`.
// emitOptionalDecode generates `tmp := saved(append(...))` for an
// optional `*saved` field; if the surrounding `saved` local is in scope,
// the conversion resolves to the local value and the file fails to build.
func TestGenerateOptionalNamedByteSliceSavedShadow(t *testing.T) {
	for _, typeName := range []string{"saved", "state"} {
		t.Run(typeName, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "go.mod"),
				[]byte("module example.com/proj\n\ngo 1.26\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			pkgDir := filepath.Join(root, "pkg")
			if err := os.MkdirAll(pkgDir, 0o755); err != nil {
				t.Fatal(err)
			}
			src := []byte(`package pkg

type ` + typeName + ` []byte

//gsbm:root
type Root struct {
	Data *` + typeName + ` ` + "`bin:\"1\"`" + `
}
`)
			if err := os.WriteFile(filepath.Join(pkgDir, "pkg.go"), src, 0o644); err != nil {
				t.Fatal(err)
			}
			ps, err := gsbmschema.LoadFromDirs([]string{pkgDir})
			if err != nil {
				t.Fatalf("LoadFromDirs: %v", err)
			}
			res := gsbmschema.Analyze(ps)
			if len(res.Issues) > 0 {
				t.Fatalf("unexpected analyze issues: %s", gsbmschema.FormatIssues(res.Issues))
			}
			files, err := gsbmcodegen.Generate(ps, res.Schema)
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if len(files) == 0 {
				t.Fatalf("expected generated files, got none")
			}
			body := string(files[0].Contents)
			// The decoder MUST NOT declare a `saved`/`state` local that
			// shadows the same-named type used in the conversion below.
			if strings.Contains(body, typeName+", err := r.BeginLengthDelim()") {
				t.Errorf("decoder declares local `%s` that shadows same-package type `%s`:\n%s", typeName, typeName, body)
			}
			if strings.Contains(body, typeName+", err := r.ReadPresenceByte") {
				t.Errorf("decoder declares local `%s` that shadows same-package type `%s`:\n%s", typeName, typeName, body)
			}
			// The conversion line must reach the named type, not a local var.
			if !strings.Contains(body, "tmp := "+typeName+"(append([]byte(nil),") {
				t.Errorf("expected optional branch to convert via %s(...), got body:\n%s", typeName, body)
			}
		})
	}
}

// TestGenerateOptionalNamedPrimitiveShadow guards against the hard-coded
// `u` local in emitOptionalDecode's named-not-struct path. For a same-
// package `type u string` (or a cross-package alias whose package name
// is `u`), the generated `var u string; tmp := u(u)` would resolve the
// outer `u` to the local var instead of the type and fail to compile.
func TestGenerateOptionalNamedPrimitiveShadow(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"),
		[]byte("module example.com/proj\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(root, "pkg")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := []byte(`package pkg

type u string

//gsbm:root
type Root struct {
	Data *u ` + "`bin:\"1\"`" + `
}
`)
	if err := os.WriteFile(filepath.Join(pkgDir, "pkg.go"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	ps, err := gsbmschema.LoadFromDirs([]string{pkgDir})
	if err != nil {
		t.Fatalf("LoadFromDirs: %v", err)
	}
	res := gsbmschema.Analyze(ps)
	if len(res.Issues) > 0 {
		t.Fatalf("unexpected analyze issues: %s", gsbmschema.FormatIssues(res.Issues))
	}
	files, err := gsbmcodegen.Generate(ps, res.Schema)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("expected generated files, got none")
	}
	body := string(files[0].Contents)
	// The decoder MUST NOT declare `var u string` because the conversion
	// `tmp := u(u)` below would resolve `u` to the local var.
	if strings.Contains(body, "var u string") {
		t.Errorf("decoder declares local `u` that shadows same-package type `u`:\n%s", body)
	}
	if !strings.Contains(body, "var u_ string") {
		t.Errorf("expected decoder to use fallback local (u_), got body:\n%s", body)
	}
	if !strings.Contains(body, "tmp := u(u_)") {
		t.Errorf("expected optional branch to convert via u(u_), got body:\n%s", body)
	}
}

// TestGenerateValueNamedPrimitiveShadow guards against the hard-coded
// `tmp` local in emitValueDecode's named-not-struct path. For a same-
// package `type tmp string`, the generated `var tmp string; v.F = tmp(tmp)`
// resolves the outer `tmp` to the local var and fails to compile.
func TestGenerateValueNamedPrimitiveShadow(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"),
		[]byte("module example.com/proj\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(root, "pkg")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := []byte(`package pkg

type tmp string

//gsbm:root
type Root struct {
	Data tmp ` + "`bin:\"1\"`" + `
}
`)
	if err := os.WriteFile(filepath.Join(pkgDir, "pkg.go"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	ps, err := gsbmschema.LoadFromDirs([]string{pkgDir})
	if err != nil {
		t.Fatalf("LoadFromDirs: %v", err)
	}
	res := gsbmschema.Analyze(ps)
	if len(res.Issues) > 0 {
		t.Fatalf("unexpected analyze issues: %s", gsbmschema.FormatIssues(res.Issues))
	}
	files, err := gsbmcodegen.Generate(ps, res.Schema)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("expected generated files, got none")
	}
	body := string(files[0].Contents)
	if strings.Contains(body, "var tmp string") {
		t.Errorf("decoder declares local `tmp` that shadows same-package type `tmp`:\n%s", body)
	}
	if !strings.Contains(body, "var tmp_ string") {
		t.Errorf("expected decoder to use fallback local (tmp_), got body:\n%s", body)
	}
	if !strings.Contains(body, "= tmp(tmp_)") {
		t.Errorf("expected value branch to convert via tmp(tmp_), got body:\n%s", body)
	}
}

// TestGenerateSkipsExternalAndOpaque ensures the generator only produces
// files for in-set, non-opaque, non-generic structs.
func TestGenerateSkipsExternalAndOpaque(t *testing.T) {
	dir := fixtureDir(t)
	ps := loadFixture(t, dir)
	res := gsbmschema.Analyze(ps)
	files, err := gsbmcodegen.Generate(ps, res.Schema)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, gf := range files {
		if !strings.HasSuffix(gf.Path, "_gsbm.go") {
			t.Errorf("unexpected output filename: %s", gf.Path)
		}
		if !strings.Contains(string(gf.Contents), "Code generated by gsbmcodegen") {
			t.Errorf("%s: missing generated header", gf.Path)
		}
	}
}

// TestGenerateRejectsCustomCodecTypeMismatch — codegen must surface a
// stable codec/type-mismatch diagnostic when a field's Go type does not
// equal the CodecDecl.GoType of the codec referenced via `custom=Name`.
// Without this guard, the user hits a generic "cannot use X as Y" from
// `go build` of the generated file instead of a targeted error that
// names the codec and the expected type.
func TestGenerateRejectsCustomCodecTypeMismatch(t *testing.T) {
	src := `package p

//gsbm:root
type Root struct {
	When string ` + "`bin:\"1,custom=TimeUnixNano\"`" + `
}
`
	ps, err := gsbmschema.ParseSource("p", []string{src})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := gsbmschema.Discover(ps)
	schema, _ := gsbmschema.BuildSchema(ps, roots)
	_, err = gsbmcodegen.Generate(ps, schema)
	if err == nil {
		t.Fatal("expected Generate to error on custom-codec type mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "codec/type-mismatch") {
		t.Fatalf("expected error code 'codec/type-mismatch', got %v", err)
	}
	if !strings.Contains(err.Error(), "time.Time") {
		t.Fatalf("expected error to name the codec's expected Go type 'time.Time', got %v", err)
	}
	if !strings.Contains(err.Error(), "string") {
		t.Fatalf("expected error to name the actual field type 'string', got %v", err)
	}
}

// TestGenerateAcceptsCustomCodecOnTypeAlias — a Go type alias
// (`type Timestamp = time.Time`) is the SAME type as its target, so
// the generated codec calls compile fine. The codec/type-mismatch
// guard MUST unwrap aliases before string-comparing against
// CodecDecl.GoType, otherwise valid alias-based usages would surface
// a spurious mismatch (with go/types alias preservation enabled by
// default in Go 1.24+, an alias's String() prints the alias name).
func TestGenerateAcceptsCustomCodecOnTypeAlias(t *testing.T) {
	src := `package p

import "time"

type Timestamp = time.Time

//gsbm:root
type Root struct {
	When Timestamp ` + "`bin:\"1,custom=TimeUnixNano\"`" + `
}
`
	ps, err := gsbmschema.ParseSource("p", []string{src})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := gsbmschema.Discover(ps)
	schema, _ := gsbmschema.BuildSchema(ps, roots)
	if _, err := gsbmcodegen.Generate(ps, schema); err != nil {
		t.Fatalf("Generate must accept custom codec on type alias of codec's GoType, got %v", err)
	}
}

// TestGenerateNestedByteSlices — the validator accepts `[][]byte` and
// `map[K][]byte` (the inner `[]byte` is a supported leaf, not a generic
// nested composite), and Generate MUST emit codegen that compiles. The
// earlier slice/map-decode paths gated the nested-composite recursion on
// `!isByteType(elem)`, falling through to emitPrimitiveDecodeAssign on
// the `[]byte` element/value and erroring with `*types.Slice not a basic
// type`. Lock the behavior down with both Generate and GenerateArena.
func TestGenerateNestedByteSlices(t *testing.T) {
	src := `package p

//gsbm:root
type Root struct {
	Blobs [][]byte         ` + "`bin:\"1\"`" + `
	ByKey map[string][]byte ` + "`bin:\"2\"`" + `
}
`
	ps, err := gsbmschema.ParseSource("p", []string{src})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := gsbmschema.Discover(ps)
	schema, issues := gsbmschema.BuildSchema(ps, roots)
	if len(issues) != 0 {
		t.Fatalf("unexpected schema issues: %v", issues)
	}
	if _, err := gsbmcodegen.Generate(ps, schema); err != nil {
		t.Fatalf("Generate must accept nested []byte shapes, got %v", err)
	}
	if _, err := gsbmcodegen.GenerateArena(ps, schema); err != nil {
		t.Fatalf("GenerateArena must accept nested []byte shapes, got %v", err)
	}
}
