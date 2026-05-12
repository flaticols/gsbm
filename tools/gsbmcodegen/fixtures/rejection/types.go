// Package rejection is a negative-path fixture: WithBareEmbed contains an
// anonymous embed of a non-struct named type, which the schema validator
// MUST reject with Issue.Code == "field/anonymous-non-struct" (only struct
// embeds flatten per issue #11). It is paired with WithNamedEmbed, which
// uses named composition over a sibling struct type and MUST NOT raise any
// anonymous-embed issue — pinning the rule as non-struct-only, not
// embed-only.
//
// No goldens are committed for this package: WithBareEmbed intentionally
// fails schema validation, so codegen never runs against it. The package
// compiles as plain Go so the rejection_test.go file in the same package
// can hand its directory to gsbmschema.LoadFromDirs.
package rejection

type Inner struct {
	Value int64 `bin:"1"`
}

// Bare is a named primitive — embedding it anonymously is unsupported.
type Bare string

//gsbm:root
type WithBareEmbed struct {
	Bare
	ID string `bin:"1"`
}

//gsbm:root
type WithNamedEmbed struct {
	ID    string `bin:"1"`
	Embed Inner  `bin:"2"`
}
