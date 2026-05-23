package gsbmcodegen_test

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"go.flaticols.dev/gsbm/tools/gsbmcodegen"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/codecs"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/codecs/builtins"
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

func TestGenerateBorrowStringsOptIn(t *testing.T) {
	t.Run("unmarked stays copying", func(t *testing.T) {
		src := `package p

//gsbm:root
type Plain struct {
	ID string ` + "`bin:\"1\"`" + `
}
`
		ps, err := gsbmschema.ParseSource("p", []string{src})
		if err != nil {
			t.Fatal(err)
		}
		res := gsbmschema.Analyze(ps)
		if len(res.Issues) > 0 {
			t.Fatalf("schema issues: %s", gsbmschema.FormatIssues(res.Issues))
		}
		files, err := gsbmcodegen.Generate(ps, res.Schema)
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if len(files) != 1 {
			t.Fatalf("got %d files, want 1", len(files))
		}
		body := string(files[0].Contents)
		if !strings.Contains(body, "x, err := r.ReadString()") {
			t.Fatalf("unmarked string decode no longer uses ReadString:\n%s", body)
		}
		if strings.Contains(body, `"unsafe"`) || strings.Contains(body, "unsafe.String") || strings.Contains(body, "r.ReadBytes()") {
			t.Fatalf("unmarked decoder emitted borrow path:\n%s", body)
		}
	})

	t.Run("marked borrows heap strings with allocator fallback", func(t *testing.T) {
		src := `package p

type Label string

//gsbm:root
//gsbm:borrow-strings
type Borrow struct {
	ID     string            ` + "`bin:\"1\"`" + `
	Note   *string           ` + "`bin:\"2\"`" + `
	Labels []Label           ` + "`bin:\"3\"`" + `
	Tags   map[string]string ` + "`bin:\"4\"`" + `
}
`
		ps, err := gsbmschema.ParseSource("p", []string{src})
		if err != nil {
			t.Fatal(err)
		}
		res := gsbmschema.Analyze(ps)
		if len(res.Issues) > 0 {
			t.Fatalf("schema issues: %s", gsbmschema.FormatIssues(res.Issues))
		}
		files, err := gsbmcodegen.Generate(ps, res.Schema)
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if len(files) != 1 {
			t.Fatalf("got %d files, want 1", len(files))
		}
		body := string(files[0].Contents)
		for _, want := range []string{
			`"unsafe"`,
			"b, err := r.ReadBytes()",
			"if r.Allocator() != nil",
			"r.AcquireString(b)",
			"unsafe.String(&b[0], len(b))",
			"clear(v.Labels)",
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("marked decoder missing %q:\n%s", want, body)
			}
		}
		if strings.Contains(body, "x, err := r.ReadString()") {
			t.Fatalf("marked decoder still emits ReadString copy path:\n%s", body)
		}
	})
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
	// addImport reserves the bare `raw` name (it's an emitter-introduced
	// local) so the user's `package raw` import gets a disambiguated
	// alias and the local can keep its preferred name without shadowing
	// anything. Verify the disambiguation walked the path segments and
	// produced `projraw` rather than falling through to a numeric suffix.
	if !strings.Contains(rootBody, "projraw \"example.com/proj/raw\"") {
		t.Errorf("expected `raw` package to alias as `projraw` (disambiguation against reserved emitter local), got body:\n%s", rootBody)
	}
	// With the alias renamed, every selector that targets the user's
	// package must now be qualified with `projraw`, not the old `raw`.
	// Use the word-boundary regex form because `projraw.Blob` contains
	// the substring `raw.Blob`.
	if regexp.MustCompile(`\braw\.(ID|Blob)`).MatchString(rootBody) {
		t.Errorf("decoder still references unqualified `raw.ID`/`raw.Blob` after rename to `projraw`:\n%s", rootBody)
	}
	if !strings.Contains(rootBody, "raw, err := r.ReadBytes()") {
		t.Errorf("expected decoder to keep `raw` as the ReadBytes local once the import is renamed, got body:\n%s", rootBody)
	}
	if !strings.Contains(rootBody, "v.ID = append(v.ID[:0], raw...)") {
		t.Errorf("expected value branch to append via `raw...`, got body:\n%s", rootBody)
	}
	if !strings.Contains(rootBody, "tmp := projraw.Blob(append([]byte(nil), raw...))") {
		t.Errorf("expected optional branch to convert via `projraw.Blob(...)`, got body:\n%s", rootBody)
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
	When string ` + "`bin:\"1,custom=Time\"`" + `
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
	When Timestamp ` + "`bin:\"1,custom=Time\"`" + `
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

// TestGenerateTrackPresenceRequiresField — //gsbm:track-presence is an
// explicit opt-in to store presence bits on the receiver, and the storage
// is a user-declared `gsbmPresent [K]uint64 \"bin:\\\"-\\\"\"` field. If the
// user adds the marker but forgets the field, codegen MUST error with an
// actionable message so the user discovers the missing piece at lint /
// generate time rather than at build time of the emitted file.
func TestGenerateTrackPresenceRequiresField(t *testing.T) {
	t.Run("missing field errors", func(t *testing.T) {
		src := `package p

//gsbm:root
//gsbm:track-presence
type Offer struct {
	ID string ` + "`bin:\"1\"`" + `
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
			t.Fatal("expected Generate to error when //gsbm:track-presence struct lacks gsbmPresent field")
		}
		if !strings.Contains(err.Error(), "gsbmPresent") {
			t.Fatalf("error should reference the required gsbmPresent field, got %v", err)
		}
	})

	t.Run("field too small errors", func(t *testing.T) {
		// Max tag 65 needs [2]uint64 — a [1]uint64 declaration is rejected.
		src := `package p

//gsbm:root
//gsbm:track-presence
type Offer struct {
	gsbmPresent [1]uint64 ` + "`bin:\"-\"`" + `
	ID    string ` + "`bin:\"1\"`" + `
	HiTag string ` + "`bin:\"65\"`" + `
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
			t.Fatal("expected Generate to error when gsbmPresent is too small for max tag")
		}
		if !strings.Contains(err.Error(), "too small") {
			t.Fatalf("error should mention the size mismatch, got %v", err)
		}
	})

	t.Run("wrong element type errors", func(t *testing.T) {
		src := `package p

//gsbm:root
//gsbm:track-presence
type Offer struct {
	gsbmPresent [1]uint32 ` + "`bin:\"-\"`" + `
	ID string ` + "`bin:\"1\"`" + `
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
			t.Fatal("expected Generate to error when gsbmPresent has non-uint64 element type")
		}
		if !strings.Contains(err.Error(), "uint64") {
			t.Fatalf("error should mention uint64 requirement, got %v", err)
		}
	})

	t.Run("accepts larger-than-needed declaration", func(t *testing.T) {
		// User declares [16]uint64 even though [1]uint64 would suffice;
		// codegen accepts it and uses the user's K. The emitted reset
		// literal `v.gsbmPresent = [16]uint64{}` must match the field's
		// type — using the computed minimum n=1 here would emit a
		// `[1]uint64{}` literal that fails to compile as a [16]uint64
		// assignment. Catch a regression by scanning the emitted source
		// for both the [16]uint64{} reset literals and the absence of
		// a smaller-sized literal.
		src := `package p

//gsbm:root
//gsbm:track-presence
type Offer struct {
	gsbmPresent [16]uint64 ` + "`bin:\"-\"`" + `
	ID string ` + "`bin:\"1\"`" + `
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
			t.Fatalf("Generate should accept oversized gsbmPresent declaration, got %v", err)
		}
		if len(files) != 1 {
			t.Fatalf("expected 1 generated file, got %d", len(files))
		}
		body := string(files[0].Contents)
		if !strings.Contains(body, "v.gsbmPresent = [16]uint64{}") {
			t.Errorf("emitted reset literal must match the field's declared K; expected `v.gsbmPresent = [16]uint64{}` in:\n%s", body)
		}
		if strings.Contains(body, "v.gsbmPresent = [1]uint64{}") {
			t.Errorf("emitted reset literal must NOT use the computed minimum K when the user declared [16]uint64; found [1]uint64{} in:\n%s", body)
		}
	})
}

// TestEmitMaterializingCodec drives the customcodec fixture through
// GenerateWithCodecs with a materializing-shape DecimalString decl (EmitFn
// instead of SizeFn/EncodeFn). The generated output is inspected in-memory
// only — committed goldens stay analytic until Task 4 migrates the fixture
// — so this exercises the new Kind() branch without touching disk.
func TestEmitMaterializingCodec(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Join(filepath.Dir(thisFile), "fixtures", "customcodec")
	ps, err := gsbmschema.LoadFromDirs([]string{dir})
	if err != nil {
		t.Fatalf("LoadFromDirs(%s): %v", dir, err)
	}
	res := gsbmschema.Analyze(ps)
	if len(res.Issues) > 0 {
		t.Fatalf("schema issues: %s", gsbmschema.FormatIssues(res.Issues))
	}
	reg := builtins.NewBuiltinRegistry()
	if err := reg.Register(codecs.CodecDecl{
		Name:      "DecimalString",
		GoType:    "go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/customcodec.DecimalAmount",
		WireType:  codecs.WireLengthDelim,
		EmitFn:    "EmitDecimalAmount",
		DecodeFn:  "DecodeDecimalAmount",
		PkgImport: "go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/customcodec",
	}); err != nil {
		t.Fatalf("register materializing DecimalString: %v", err)
	}
	if err := reg.Register(codecs.CodecDecl{
		Name:      "DecimalAppend",
		GoType:    "go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/customcodec.DecimalAmount",
		WireType:  codecs.WireLengthDelim,
		EmitFn:    "EmitDecimalAmountAppend",
		DecodeFn:  "DecodeDecimalAmountAppend",
		PkgImport: "go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/customcodec",
	}); err != nil {
		t.Fatalf("register materializing DecimalAppend: %v", err)
	}
	if err := reg.Register(codecs.CodecDecl{
		Name:      "StreamingJSON",
		GoType:    "go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/customcodec.LargePayload",
		WireType:  codecs.WireLengthDelim,
		StreamFn:  "StreamLargePayload",
		DecodeFn:  "DecodeLargePayload",
		PkgImport: "go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/customcodec",
	}); err != nil {
		t.Fatalf("register streaming StreamingJSON: %v", err)
	}
	if err := reg.Register(codecs.CodecDecl{
		Name:      "DecimalBinary",
		GoType:    "go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/customcodec.DecimalAmount",
		WireType:  codecs.WireLengthDelim,
		EncodeFn:  "EncodeDecimalAmountBinary",
		DecodeFn:  "DecodeDecimalAmountBinary",
		SizeFn:    "SizeDecimalAmountBinary",
		PkgImport: "go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/customcodec",
	}); err != nil {
		t.Fatalf("register analytic DecimalBinary: %v", err)
	}
	files, err := gsbmcodegen.GenerateWithCodecs(ps, res.Schema, reg)
	if err != nil {
		t.Fatalf("GenerateWithCodecs: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no files generated")
	}
	var body string
	for _, gf := range files {
		if strings.HasSuffix(gf.Path, "record_gsbm.go") {
			body = string(gf.Contents)
			break
		}
	}
	if body == "" {
		t.Fatal("record_gsbm.go not found in generated files")
	}
	// callsite constant exists at file scope.
	if !strings.Contains(body, "const (") || !strings.Contains(body, "csRecord_2") {
		t.Errorf("expected callsite constant block with csRecord_2 in generated body:\n%s", body)
	}
	// MarshalGSBM emits EmitDecimalAmount(w, v.Amount, csRecord_2).
	if !strings.Contains(body, "EmitDecimalAmount(w, v.Amount, csRecord_2)") {
		t.Errorf("expected EmitFn call in MarshalGSBM:\n%s", body)
	}
	// SizeGSBM is a one-line delegation to MarshalGSBM against a
	// size-mode Writer (Task 5 collapse); the materializing codec's
	// EmitFn is invoked through MarshalGSBM, not via a per-field
	// CountingWriter inside SizeGSBM.
	if !strings.Contains(body, "cw := gsbm.NewCountingWriter()") {
		t.Errorf("expected SizeGSBM to delegate via CountingWriter:\n%s", body)
	}
	if !strings.Contains(body, "_ = v.MarshalGSBM(cw)") {
		t.Errorf("expected SizeGSBM to call MarshalGSBM against CountingWriter:\n%s", body)
	}
	// Analytic Time field (tag 1) keeps its EncodeFn shape in
	// MarshalGSBM — the Kind() branch must not bleed materializing
	// emission into analytic codecs. Time is LENGTH_DELIM-shaped so
	// codegen also emits the size-prefix line.
	if !strings.Contains(body, "builtins.EncodeTime(w, v.CreatedAt)") {
		t.Errorf("analytic Time encode path drifted:\n%s", body)
	}
	if !strings.Contains(body, "w.WriteUvarint(uint64(builtins.SizeTime(v.CreatedAt)))") {
		t.Errorf("analytic LENGTH_DELIM Time codec missing size-prefix emission:\n%s", body)
	}
}

// TestEmitMaterializingCodecPointer asserts the pointer-wrapped variant of
// the materializing-codec branch in emit.go renders the spec §5.1 nullable
// envelope around the EmitFn call: outer WriteTag(LengthDelim) +
// BeginLengthDelim, presence byte (Nil / NonZero), the EmitFn call with the
// pointee dereferenced and a callsite id, and EndLengthDelim. The customcodec
// fixture only carries a value-typed Amount field, so this synthetic schema
// is the only coverage for the `*T custom=...` materializing path.
func TestEmitMaterializingCodecPointer(t *testing.T) {
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

type Amount struct {
	V string
}

func (a Amount) String() string { return a.V }

//gsbm:root
type Root struct {
	Opt *Amount ` + "`bin:\"1,custom=DecimalString\"`" + `
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
		t.Fatalf("schema issues: %s", gsbmschema.FormatIssues(res.Issues))
	}
	reg := builtins.NewBuiltinRegistry()
	if err := reg.Register(codecs.CodecDecl{
		Name:      "DecimalString",
		GoType:    "example.com/proj/pkg.Amount",
		WireType:  codecs.WireLengthDelim,
		EmitFn:    "EmitAmount",
		DecodeFn:  "DecodeAmount",
		PkgImport: "example.com/proj/pkg",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	files, err := gsbmcodegen.GenerateWithCodecs(ps, res.Schema, reg)
	if err != nil {
		t.Fatalf("GenerateWithCodecs: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no files generated")
	}
	body := string(files[0].Contents)
	for _, want := range []string{
		"w.WriteTag(1, gsbm.WireLengthDelim)",
		"m := w.BeginLengthDelim()",
		"if v.Opt == nil",
		"w.WritePresenceNil()",
		"w.WritePresenceNonZero()",
		"EmitAmount(w, *v.Opt, csRoot_1)",
		"w.EndLengthDelim(m)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("pointer materializing codec rendering missing %q in:\n%s", want, body)
		}
	}
}

// TestEmitStreamingCodec asserts the streaming-codec branch in
// emit.go renders a tag + StreamFn(w, v.Field) call with no callsite
// argument, no scratch-cache key, and no callsite-constant declaration.
// The streaming kind's defining property is that the body is materialized
// per pass (not cached between size and write), so codegen must not
// thread a callsite id through StreamFn.
func TestEmitStreamingCodec(t *testing.T) {
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

type Payload struct {
	V string
}

//gsbm:root
type Root struct {
	P Payload ` + "`bin:\"1,custom=StreamingJSON\"`" + `
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
		t.Fatalf("schema issues: %s", gsbmschema.FormatIssues(res.Issues))
	}
	reg := builtins.NewBuiltinRegistry()
	if err := reg.Register(codecs.CodecDecl{
		Name:      "StreamingJSON",
		GoType:    "example.com/proj/pkg.Payload",
		WireType:  codecs.WireLengthDelim,
		StreamFn:  "StreamPayload",
		DecodeFn:  "DecodePayload",
		PkgImport: "example.com/proj/pkg",
	}); err != nil {
		t.Fatalf("register streaming StreamingJSON: %v", err)
	}
	files, err := gsbmcodegen.GenerateWithCodecs(ps, res.Schema, reg)
	if err != nil {
		t.Fatalf("GenerateWithCodecs: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no files generated")
	}
	body := string(files[0].Contents)
	for _, want := range []string{
		"w.WriteTag(1, gsbm.WireLengthDelim)",
		"StreamPayload(w, v.P)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("streaming codec rendering missing %q in:\n%s", want, body)
		}
	}
	// Streaming codecs never carry a callsite argument — the cache they
	// would key into is intentionally not used. Confirm no callsite
	// constant was registered for this field and the StreamFn call shape
	// has only two arguments (w, v.P).
	if strings.Contains(body, "csRoot_1") {
		t.Errorf("streaming codec must not emit callsite constant csRoot_1:\n%s", body)
	}
	if strings.Contains(body, "StreamPayload(w, v.P, ") {
		t.Errorf("streaming codec must not pass a callsite argument to StreamFn:\n%s", body)
	}
	// The Writer is mode-aware, so the same call shape works in both
	// passes. SizeGSBM delegates to MarshalGSBM via CountingWriter; the
	// streaming call is therefore reached identically in size-mode and
	// write-mode without a per-pass branch.
	if !strings.Contains(body, "cw := gsbm.NewCountingWriter()") ||
		!strings.Contains(body, "_ = v.MarshalGSBM(cw)") {
		t.Errorf("expected SizeGSBM to delegate via CountingWriter for streaming codecs:\n%s", body)
	}
}

// TestEmitStreamingCodecPointer asserts the pointer-wrapped variant of
// the streaming-codec branch renders the spec §5.1 nullable envelope:
// outer WriteTag(LengthDelim) + BeginLengthDelim, presence byte (Nil /
// NonZero), and on NonZero a StreamFn(w, *expr) call with the pointee
// dereferenced and NO callsite argument.
func TestEmitStreamingCodecPointer(t *testing.T) {
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

type Payload struct {
	V string
}

//gsbm:root
type Root struct {
	Opt *Payload ` + "`bin:\"1,custom=StreamingJSON\"`" + `
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
		t.Fatalf("schema issues: %s", gsbmschema.FormatIssues(res.Issues))
	}
	reg := builtins.NewBuiltinRegistry()
	if err := reg.Register(codecs.CodecDecl{
		Name:      "StreamingJSON",
		GoType:    "example.com/proj/pkg.Payload",
		WireType:  codecs.WireLengthDelim,
		StreamFn:  "StreamPayload",
		DecodeFn:  "DecodePayload",
		PkgImport: "example.com/proj/pkg",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	files, err := gsbmcodegen.GenerateWithCodecs(ps, res.Schema, reg)
	if err != nil {
		t.Fatalf("GenerateWithCodecs: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no files generated")
	}
	body := string(files[0].Contents)
	for _, want := range []string{
		"w.WriteTag(1, gsbm.WireLengthDelim)",
		"m := w.BeginLengthDelim()",
		"if v.Opt == nil",
		"w.WritePresenceNil()",
		"w.WritePresenceNonZero()",
		"StreamPayload(w, *v.Opt)",
		"w.EndLengthDelim(m)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("pointer streaming codec rendering missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, "csRoot_1") {
		t.Errorf("streaming codec must not emit callsite constant csRoot_1:\n%s", body)
	}
	if strings.Contains(body, "StreamPayload(w, *v.Opt, ") {
		t.Errorf("streaming codec must not pass a callsite argument to StreamFn:\n%s", body)
	}
}

// TestCallsiteConstantsStable asserts the callsite constant emitted for a
// (struct, tag) pair is the same value across re-runs and the same value
// in both SizeGSBM and MarshalGSBM. Stability across runs is what makes
// the scratch cache lookups deterministic; same-value-in-both-passes is
// what threads the cache through gsbm.Marshal's size→write hand-off.
func TestCallsiteConstantsStable(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Join(filepath.Dir(thisFile), "fixtures", "customcodec")
	ps, err := gsbmschema.LoadFromDirs([]string{dir})
	if err != nil {
		t.Fatalf("LoadFromDirs: %v", err)
	}
	res := gsbmschema.Analyze(ps)
	reg := builtins.NewBuiltinRegistry()
	if err := reg.Register(codecs.CodecDecl{
		Name:      "DecimalString",
		GoType:    "go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/customcodec.DecimalAmount",
		WireType:  codecs.WireLengthDelim,
		EmitFn:    "EmitDecimalAmount",
		DecodeFn:  "DecodeDecimalAmount",
		PkgImport: "go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/customcodec",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := reg.Register(codecs.CodecDecl{
		Name:      "DecimalAppend",
		GoType:    "go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/customcodec.DecimalAmount",
		WireType:  codecs.WireLengthDelim,
		EmitFn:    "EmitDecimalAmountAppend",
		DecodeFn:  "DecodeDecimalAmountAppend",
		PkgImport: "go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/customcodec",
	}); err != nil {
		t.Fatalf("register DecimalAppend: %v", err)
	}
	if err := reg.Register(codecs.CodecDecl{
		Name:      "StreamingJSON",
		GoType:    "go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/customcodec.LargePayload",
		WireType:  codecs.WireLengthDelim,
		StreamFn:  "StreamLargePayload",
		DecodeFn:  "DecodeLargePayload",
		PkgImport: "go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/customcodec",
	}); err != nil {
		t.Fatalf("register StreamingJSON: %v", err)
	}
	if err := reg.Register(codecs.CodecDecl{
		Name:      "DecimalBinary",
		GoType:    "go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/customcodec.DecimalAmount",
		WireType:  codecs.WireLengthDelim,
		EncodeFn:  "EncodeDecimalAmountBinary",
		DecodeFn:  "DecodeDecimalAmountBinary",
		SizeFn:    "SizeDecimalAmountBinary",
		PkgImport: "go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/customcodec",
	}); err != nil {
		t.Fatalf("register DecimalBinary: %v", err)
	}
	first, err := gsbmcodegen.GenerateWithCodecs(ps, res.Schema, reg)
	if err != nil {
		t.Fatalf("first generate: %v", err)
	}
	second, err := gsbmcodegen.GenerateWithCodecs(ps, res.Schema, reg)
	if err != nil {
		t.Fatalf("second generate: %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("file count differs across runs: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if string(first[i].Contents) != string(second[i].Contents) {
			t.Errorf("file %s differs across runs", first[i].Path)
		}
	}
}

// TestGenerateIntWireOverrideInt64 — a Go `int` field tagged
// `bin:"N,type=int64"` must emit an encode that skips the int32 bounds
// check and a decode that accepts the full int64 range. Without the
// emit-side plumbing the override is silently ignored: the field keeps
// the default int32-bounded encode/decode and the user's intent is
// dropped.
func TestGenerateIntWireOverrideInt64(t *testing.T) {
	src := `package p

//gsbm:root
type Wide struct {
	Big int ` + "`bin:\"1,type=int64\"`" + `
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
	if !strings.Contains(body, "w.WriteVarint(int64(v.Big))") {
		t.Errorf("expected unbounded WriteVarint for type=int64 field, got body:\n%s", body)
	}
	// The int32-bound check must NOT appear on the Big field path.
	// Locate the encode case for tag 1 and confirm no MinInt32/MaxInt32
	// guard precedes the WriteVarint. The simplest robust check: the
	// emitted file should not contain "math.MinInt32" anywhere — the
	// only `int` field is the overridden one, so any int32-bound check
	// would point at a regression.
	if strings.Contains(body, "math.MinInt32") || strings.Contains(body, "math.MaxInt32") {
		t.Errorf("type=int64 override must skip the int32 bounds check, but the generated code still contains an int32 guard:\n%s", body)
	}
}

// TestGenerateIntWireOverrideInt32 — a Go `int` field tagged
// `bin:"N,type=int32"` is an explicit form of today's default. The
// emitted encode/decode must be byte-identical to the un-annotated `int`
// path: the int32 bounds check is present at both encode and decode.
func TestGenerateIntWireOverrideInt32(t *testing.T) {
	srcOverride := `package p

//gsbm:root
type Pinned struct {
	Small int ` + "`bin:\"1,type=int32\"`" + `
}
`
	srcDefault := `package p

//gsbm:root
type Pinned struct {
	Small int ` + "`bin:\"1\"`" + `
}
`
	gen := func(src string) string {
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
		return string(files[0].Contents)
	}
	override := gen(srcOverride)
	def := gen(srcDefault)
	if override != def {
		t.Errorf("type=int32 must emit byte-identical code to un-annotated int field\n--- override ---\n%s\n--- default ---\n%s", override, def)
	}
}

// TestGenerateIntWireOverrideDecodeInt64 — symmetric to the encode
// check: the decode for a `type=int64` field must call ReadVarint and
// assign without the int32 bounds check.
func TestGenerateIntWireOverrideDecodeInt64(t *testing.T) {
	src := `package p

//gsbm:root
type Wide struct {
	Big int ` + "`bin:\"1,type=int64\"`" + `
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
	if !strings.Contains(body, "v.Big = int(x)") {
		t.Errorf("expected decode to assign Big = int(x), got body:\n%s", body)
	}
	if strings.Contains(body, "math.MinInt32") || strings.Contains(body, "math.MaxInt32") {
		t.Errorf("type=int64 decode must skip the int32 bounds check, but the generated code still contains an int32 guard:\n%s", body)
	}
	// Platform-sized guard: math.MinInt/MaxInt fold to a no-op on 64-bit
	// and surface ErrIntegerOverflow on 32-bit, preventing silent
	// truncation of int64-range values into a 32-bit `int`.
	if !strings.Contains(body, "x < math.MinInt ||") || !strings.Contains(body, "x > math.MaxInt ") {
		t.Errorf("type=int64 decode must guard with platform-sized math.MinInt/math.MaxInt, got body:\n%s", body)
	}
}

// TestGenerateIDRefHonorsTargetIntWireOverride — an id_ref field whose
// target's bin:"1" Go `int` field carries `type=int64` MUST emit
// encode/decode that skip the int32 bounds check. The cycle-break leaf
// emitter previously routed through the generic emitValueEncode/
// emitValueDecode paths which always pass an empty width override, so
// the target's widened ID range was silently dropped at the referencing
// site even though the target's own MarshalGSBM honored it. The check
// here pins the fix: no math.MinInt32 / math.MaxInt32 guard appears
// anywhere in the referencing struct's generated body.
func TestGenerateIDRefHonorsTargetIntWireOverride(t *testing.T) {
	src := `package p

type Target struct {
	ID int ` + "`bin:\"1,type=int64\"`" + `
}

//gsbm:root
type Holder struct {
	Ref *Target ` + "`bin:\"2,id_ref\"`" + `
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
	// Locate the file for Holder — the Target file legitimately contains
	// no int32 guard (its own ID is type=int64), so scanning every file
	// would never catch a regression.
	var holderBody string
	for _, f := range files {
		body := string(f.Contents)
		if strings.Contains(body, "func (v *Holder) MarshalGSBM") {
			holderBody = body
			break
		}
	}
	if holderBody == "" {
		t.Fatal("Holder file not found in generator output")
	}
	if strings.Contains(holderBody, "math.MinInt32") || strings.Contains(holderBody, "math.MaxInt32") {
		t.Errorf("id_ref leaf must inherit target's type=int64 width, but Holder body still contains an int32 guard:\n%s", holderBody)
	}
	if !strings.Contains(holderBody, "w.WriteVarint(int64(v.Ref.ID))") {
		t.Errorf("expected unbounded WriteVarint(int64(v.Ref.ID)) on the id_ref encode path, got body:\n%s", holderBody)
	}
	if !strings.Contains(holderBody, "v.Ref.ID = int(x)") {
		t.Errorf("expected `v.Ref.ID = int(x)` assignment on the id_ref decode path, got body:\n%s", holderBody)
	}
}

// TestGenerateIDRefDefaultTargetKeepsInt32Guard — the symmetric
// regression check: an id_ref whose target's bin:"1" is an un-annotated
// Go `int` (default int32-bounded shape) MUST keep the bounds check on
// the referencing field. Without this, a refactor that always threads a
// non-empty override would silently widen unintended fields.
func TestGenerateIDRefDefaultTargetKeepsInt32Guard(t *testing.T) {
	src := `package p

type Target struct {
	ID int ` + "`bin:\"1\"`" + `
}

//gsbm:root
type Holder struct {
	Ref *Target ` + "`bin:\"2,id_ref\"`" + `
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
	var holderBody string
	for _, f := range files {
		body := string(f.Contents)
		if strings.Contains(body, "func (v *Holder) MarshalGSBM") {
			holderBody = body
			break
		}
	}
	if holderBody == "" {
		t.Fatal("Holder file not found in generator output")
	}
	if !strings.Contains(holderBody, "math.MinInt32") || !strings.Contains(holderBody, "math.MaxInt32") {
		t.Errorf("default-target id_ref must keep the int32 bounds check, got body:\n%s", holderBody)
	}
}

// TestGenerateIDRefNamedAliasTargetWithOverride — when an id_ref target's
// bin:"1" field uses a named integer alias (`type UserID int64`) with a
// `type=W` wire-width override, the referencing struct's encode/decode
// must unwrap the alias before calling the primitive emitter. Without
// the unwrap, the id_ref leaf path passes *types.Named to
// emitPrimitiveEncode/emitPrimitiveDecodeAssign which both require
// *types.Basic and fail with "not a basic type", crashing codegen for a
// schema validate accepts. Regression check for the gap between
// WireOverrideCompat's named-alias support and the id_ref leaf paths.
func TestGenerateIDRefNamedAliasTargetWithOverride(t *testing.T) {
	src := `package p

type UserID int64

type Target struct {
	ID UserID ` + "`bin:\"1,type=int32\"`" + `
}

//gsbm:root
type Holder struct {
	Ref *Target ` + "`bin:\"2,id_ref\"`" + `
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
	var holderBody string
	for _, f := range files {
		body := string(f.Contents)
		if strings.Contains(body, "func (v *Holder) MarshalGSBM") {
			holderBody = body
			break
		}
	}
	if holderBody == "" {
		t.Fatal("Holder file not found in generator output")
	}
	// Encode side: must apply the int32 bound on the named-alias value.
	if !strings.Contains(holderBody, "int64(v.Ref.ID) < math.MinInt32") {
		t.Errorf("id_ref named-alias encode must bound by MaxInt32, got body:\n%s", holderBody)
	}
	// Decode side: must read into a tmp of the underlying basic, then cast
	// back to the named alias on assign (typeExpr omits the package
	// qualifier for same-package types).
	if !strings.Contains(holderBody, "v.Ref.ID = UserID(") {
		t.Errorf("id_ref named-alias decode must cast back to the named alias, got body:\n%s", holderBody)
	}
}

// genIntFieldFile is a helper for the integer-emit refactor tests: parse
// a single-field root struct with the given field text and return the
// generated body. The field text is the part after the field name, e.g.
// `int \`bin:"1,type=int64"\``. Reduces boilerplate across the cases.
func genIntFieldFile(t *testing.T, fieldDecl string) string {
	t.Helper()
	src := "package p\n\n//gsbm:root\ntype Rec struct {\n\tF " + fieldDecl + "\n}\n"
	ps, err := gsbmschema.ParseSource("p", []string{src})
	if err != nil {
		t.Fatalf("ParseSource: %v", err)
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
	return string(files[0].Contents)
}

// TestGenerateUintDefaultEmitsInt32Guard pins the previously-bugged Go
// `uint` default path: encode and decode MUST bound to the 32-bit
// portable range. This is the symmetric counterpart to the existing
// `int` test — without the refactor, a `uint` value above MaxUint32
// silently violated the portable contract.
func TestGenerateUintDefaultEmitsInt32Guard(t *testing.T) {
	body := genIntFieldFile(t, "uint `bin:\"1\"`")
	if !strings.Contains(body, "math.MaxUint32") {
		t.Errorf("default uint must bound to MaxUint32, got body:\n%s", body)
	}
	if !strings.Contains(body, "w.WriteUvarint(uint64(v.F))") {
		t.Errorf("expected WriteUvarint(uint64(v.F)) on encode, got body:\n%s", body)
	}
	if !strings.Contains(body, "v.F = uint(x)") {
		t.Errorf("expected `v.F = uint(x)` on decode, got body:\n%s", body)
	}
}

// TestGenerateUintWireOverrideUint64 — the symmetric platform-width fix:
// a `uint` field tagged `type=uint64` MUST skip the MaxUint32 bound on
// encode and decode, exactly as `int type=int64` does on the signed side.
func TestGenerateUintWireOverrideUint64(t *testing.T) {
	body := genIntFieldFile(t, "uint `bin:\"1,type=uint64\"`")
	if strings.Contains(body, "math.MaxUint32") {
		t.Errorf("type=uint64 override must skip the MaxUint32 guard, got body:\n%s", body)
	}
	if !strings.Contains(body, "w.WriteUvarint(uint64(v.F))") {
		t.Errorf("expected unbounded WriteUvarint, got body:\n%s", body)
	}
	if !strings.Contains(body, "x > math.MaxUint ") {
		t.Errorf("uint type=uint64 decode must guard with platform-sized math.MaxUint to surface ErrIntegerOverflow on 32-bit hosts, got body:\n%s", body)
	}
	if !strings.Contains(body, "v.F = uint(x)") {
		t.Errorf("expected `v.F = uint(x)` on decode, got body:\n%s", body)
	}
}

// TestGenerateUintWireOverrideUint32 — explicit `type=uint32` on a Go
// `uint` field must emit byte-identical code to the un-annotated form,
// mirroring the signed `int type=int32` identity case.
func TestGenerateUintWireOverrideUint32(t *testing.T) {
	override := genIntFieldFile(t, "uint `bin:\"1,type=uint32\"`")
	def := genIntFieldFile(t, "uint `bin:\"1\"`")
	if override != def {
		t.Errorf("type=uint32 must emit byte-identical code to un-annotated uint field\n--- override ---\n%s\n--- default ---\n%s", override, def)
	}
}

// TestGenerateInt64NarrowingToInt16 — narrowing a fixed-width Go kind
// via `type=` was rejected by the validator before Task 3 / Task 4.
// Encode MUST bound by MaxInt16; decode MUST bound by MaxInt16 and
// assign back into the original int64 lhs. Without this, a 40000-value
// int64 field tagged `type=int16` would round-trip silently — defeating
// the narrowing contract.
func TestGenerateInt64NarrowingToInt16(t *testing.T) {
	body := genIntFieldFile(t, "int64 `bin:\"1,type=int16\"`")
	// Encode-side bound on the input value.
	if !strings.Contains(body, "int64(v.F) < math.MinInt16") || !strings.Contains(body, "int64(v.F) > math.MaxInt16") {
		t.Errorf("encode of int64 type=int16 must bound by MinInt16/MaxInt16, got body:\n%s", body)
	}
	if !strings.Contains(body, "w.WriteVarint(int64(v.F))") {
		t.Errorf("expected WriteVarint(int64(v.F)), got body:\n%s", body)
	}
	// Decode-side bound on the read varint.
	if !strings.Contains(body, "x < math.MinInt16") || !strings.Contains(body, "x > math.MaxInt16") {
		t.Errorf("decode of int64 type=int16 must bound by MinInt16/MaxInt16, got body:\n%s", body)
	}
	if !strings.Contains(body, "v.F = int64(x)") {
		t.Errorf("expected `v.F = int64(x)` on decode, got body:\n%s", body)
	}
}

// TestGenerateUint64NarrowingToUint8 — unsigned narrowing counterpart.
// Encode bounds by MaxUint8; decode bounds by MaxUint8 and (because of
// the uint64 byte-identity exception) assigns the bare reader result.
func TestGenerateUint64NarrowingToUint8(t *testing.T) {
	body := genIntFieldFile(t, "uint64 `bin:\"1,type=uint8\"`")
	if !strings.Contains(body, "uint64(v.F) > math.MaxUint8") {
		t.Errorf("encode of uint64 type=uint8 must bound by MaxUint8, got body:\n%s", body)
	}
	if !strings.Contains(body, "w.WriteUvarint(uint64(v.F))") {
		t.Errorf("expected WriteUvarint(uint64(v.F)), got body:\n%s", body)
	}
	if !strings.Contains(body, "x > math.MaxUint8") {
		t.Errorf("decode of uint64 type=uint8 must bound by MaxUint8, got body:\n%s", body)
	}
	if !strings.Contains(body, "v.F = x") {
		t.Errorf("uint64 decode preserves the bare `v.F = x` assignment (no explicit cast), got body:\n%s", body)
	}
}

// TestGenerateInt32IdentityIsByteIdentical — `int32 type=int32` is the
// documented identity case: it must emit exactly the same code as an
// un-annotated `int32` field. Any divergence muddies the
// field/wire-intent-changed classifier event.
func TestGenerateInt32IdentityIsByteIdentical(t *testing.T) {
	override := genIntFieldFile(t, "int32 `bin:\"1,type=int32\"`")
	def := genIntFieldFile(t, "int32 `bin:\"1\"`")
	if override != def {
		t.Errorf("int32 type=int32 must emit byte-identical code to un-annotated int32 field\n--- override ---\n%s\n--- default ---\n%s", override, def)
	}
}

// TestGenerateNamedAliasNarrowing — named integer aliases (`type UserID
// int64`) must take the same width-override path as their underlying
// kind. Without WireOverrideCompat walking through *types.Named, a
// UserID field with `type=int32` would be silently rejected or routed
// through an un-bounded emit.
func TestGenerateNamedAliasNarrowing(t *testing.T) {
	src := "package p\n\ntype UserID int64\n\n//gsbm:root\ntype Rec struct {\n\tID UserID `bin:\"1,type=int32\"`\n}\n"
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
	if !strings.Contains(body, "int64(v.ID) < math.MinInt32") || !strings.Contains(body, "int64(v.ID) > math.MaxInt32") {
		t.Errorf("named-alias UserID type=int32 must bound by MaxInt32 on encode, got body:\n%s", body)
	}
	if !strings.Contains(body, "x < math.MinInt32") || !strings.Contains(body, "x > math.MaxInt32") {
		t.Errorf("named-alias UserID type=int32 must bound by MaxInt32 on decode, got body:\n%s", body)
	}
}
