// Package rejection is a negative-path fixture: WithEmbed contains an
// anonymous (embedded) Go field which the schema validator MUST reject
// with Issue.Code == "field/anonymous". It is paired with WithNamedEmbed,
// which uses named composition over the same Inner type and MUST NOT
// raise any anonymous-field issue — pinning the rule as anonymous-only,
// not composition-only.
//
// No goldens are committed for this package: it intentionally fails
// schema validation, so codegen never runs against it. The package
// compiles as plain Go so the rejection_test.go file in the same package
// can hand its directory to gsbmschema.LoadFromDirs.
package rejection

type Inner struct {
	Value int64 `bin:"1"`
}

//gsbm:root
type WithEmbed struct {
	Inner
	ID string `bin:"1"`
}

//gsbm:root
type WithNamedEmbed struct {
	ID    string `bin:"1"`
	Embed Inner  `bin:"2"`
}
