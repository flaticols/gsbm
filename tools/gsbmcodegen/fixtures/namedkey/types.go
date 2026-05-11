// Package namedkey is a fixture exercising map[K]V where K is a named
// type whose underlying is a supported primitive map-key kind (spec
// §5.3 — string, bool, signed/unsigned int, uintptr). Resolves issue #7:
// callers can now use `map[Code]int64` instead of dropping to
// `map[string]int64` to satisfy the validator.
//
// Wire bytes are byte-identical to the same map keyed by the underlying
// builtin, so existing on-disk blobs migrate without rewriting. The
// committed *_gsbm.go siblings are byte-for-byte reproducible from these
// declarations via gsbmcodegen.Generate.
package namedkey

type (
	Code     string
	Flag     bool
	Severity int8
	Bucket   uint16
)

//gsbm:root
type Counts struct {
	ByCode     map[Code]int64     `bin:"1"`
	BySeverity map[Severity]int64 `bin:"2"`
	ByBucket   map[Bucket]int64   `bin:"3"`
	ByFlag     map[Flag]int64     `bin:"4"`
}
