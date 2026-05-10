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

// TestRejectionAnonymousField pins the validator's anonymous-field rule
// end-to-end: loading the rejection fixture must surface at least one
// Issue with Code == "field/anonymous" whose Message names the embedded
// type "Inner". This proves discover.go:278 is wired through Analyze and
// FormatIssues — not just unit-tested in isolation.
func TestRejectionAnonymousField(t *testing.T) {
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
		if iss.Code == "field/anonymous" && strings.Contains(iss.Message, "Inner") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected field/anonymous issue mentioning Inner, got:\n%s",
			gsbmschema.FormatIssues(res.Issues))
	}
}

// TestRejectionNamedCompositionAccepted pins the complement: the sibling
// WithNamedEmbed root uses named composition (Embed Inner `bin:"2"`)
// over the same Inner type the rejected struct embeds anonymously. The
// validator must NOT raise a field/anonymous issue against it, and
// WithNamedEmbed must surface in the schema with the Embed field at tag
// 2. This proves the rule is anonymous-only, not composition-only.
func TestRejectionNamedCompositionAccepted(t *testing.T) {
	ps, err := gsbmschema.LoadFromDirs([]string{fixtureDir(t)})
	if err != nil {
		t.Fatalf("LoadFromDirs: %v", err)
	}
	res := gsbmschema.Analyze(ps)

	// Exactly one field/anonymous issue is expected — the one from
	// WithEmbed.Inner. If WithNamedEmbed contributed one, this count
	// would be 2.
	var anonCount int
	for _, iss := range res.Issues {
		if iss.Code == "field/anonymous" {
			anonCount++
		}
	}
	if anonCount != 1 {
		t.Fatalf("field/anonymous issues: got %d, want 1 (only WithEmbed should contribute); issues:\n%s",
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
