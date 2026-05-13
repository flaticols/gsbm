// Package common (under pkg/b) is the second half of the import-collision
// fixture. Its declared Go package name is `common` — the same as
// ../../a/common — so the parent importcollision package's generated
// imports must distinguish them with collision-aware aliases.
package common

// Value here intentionally has different fields than the sibling
// pkg/a/common.Value so a swapped alias in the generated code would
// fail to compile (wrong field names) rather than silently round-trip.
type Value struct {
	Token string  `bin:"1"`
	Score float64 `bin:"2"`
}
