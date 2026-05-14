// Package common (under pkg/a) is half of the import-collision fixture:
// it declares its Go package name as `common`, identical to the sibling
// at ../../b/common. The fixture's root type (importcollision.Record)
// references Value from both packages, forcing the codegen to emit two
// distinct import aliases for the same package name.
package common

// Value is a small struct with two primitive fields. The codegen must
// generate marshal/unmarshal methods on this type within its own package
// (no import-alias work needed inside this file); the alias work happens
// in the parent importcollision package, which imports both commons.
type Value struct {
	Label string `bin:"1"`
	Count int64  `bin:"2"`
}
