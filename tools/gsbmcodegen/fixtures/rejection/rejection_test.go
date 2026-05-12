package rejection_test

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.flaticols.dev/gsbm/tools/gsbmschema"
)

// fixtureDir returns the absolute path to the rejection fixture
// directory. The schema loader walks a directory's Go files; pinning
// the path off runtime.Caller keeps the test independent of cwd.
func fixtureDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(thisFile)
}

// TestRejectionAnonymousNonStruct pins the validator's anonymous-embed
// rule end-to-end: loading the rejection fixture must surface at least
// one Issue with Code == "field/anonymous-non-struct" whose Message names
// the embedded type "Bare". Anonymous embeds of struct types are now
// flattened (issue #11); only non-struct anonymous embeds remain rejected.
func TestRejectionAnonymousNonStruct(t *testing.T) {
	ps, err := gsbmschema.LoadFromDirs([]string{fixtureDir(t)})
	if err != nil {
		t.Fatalf("LoadFromDirs: %v", err)
	}
	res := gsbmschema.Analyze(ps)
	if len(res.Issues) == 0 {
		t.Fatalf("expected at least one issue, got 0")
	}
	var found bool
	for _, iss := range res.Issues {
		if iss.Code == "field/anonymous-non-struct" && strings.Contains(iss.Message, "Bare") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected field/anonymous-non-struct issue mentioning Bare, got:\n%s",
			gsbmschema.FormatIssues(res.Issues))
	}
}

// TestRejectionNamedCompositionAccepted pins the complement: the sibling
// WithNamedEmbed root uses named composition (Embed Inner `bin:"2"`) over
// the Inner struct type. The validator must NOT raise any anonymous-embed
// issue against it, and WithNamedEmbed must surface in the schema with the
// Embed field at tag 2. This proves the new rule is non-struct-only — not
// composition-only and not anonymous-only.
func TestRejectionNamedCompositionAccepted(t *testing.T) {
	ps, err := gsbmschema.LoadFromDirs([]string{fixtureDir(t)})
	if err != nil {
		t.Fatalf("LoadFromDirs: %v", err)
	}
	res := gsbmschema.Analyze(ps)

	// Exactly one field/anonymous-non-struct issue is expected — the one
	// from WithBareEmbed.Bare. If WithNamedEmbed contributed one, this
	// count would be 2.
	var anonCount int
	for _, iss := range res.Issues {
		if iss.Code == "field/anonymous-non-struct" {
			anonCount++
		}
	}
	if anonCount != 1 {
		t.Fatalf("field/anonymous-non-struct issues: got %d, want 1 (only WithBareEmbed should contribute); issues:\n%s",
			anonCount, gsbmschema.FormatIssues(res.Issues))
	}

	var withNamedEmbed *gsbmschema.StructDecl
	for _, sd := range res.Schema.Structs {
		if sd.Type.Name == "WithNamedEmbed" {
			withNamedEmbed = sd
			break
		}
	}
	if withNamedEmbed == nil {
		t.Fatalf("WithNamedEmbed missing from schema; structs: %v", res.Schema.Structs)
	}

	var embedAtTag2 bool
	for _, f := range withNamedEmbed.Fields {
		if f.Tag == 2 && f.Name == "Embed" {
			embedAtTag2 = true
			break
		}
	}
	if !embedAtTag2 {
		t.Fatalf("WithNamedEmbed.Embed at tag 2 not in schema fields: %+v", withNamedEmbed.Fields)
	}
}
