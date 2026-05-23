// Package repeatednested is a benchmark fixture for the compression win
// (issue #53) on payloads with many repeated short strings — the
// canonical shape where structured binary formats fall behind a
// compressed protobuf baseline.
//
// The shape — 100+ nested records sharing small string pools (airport
// codes, currency codes, tax codes) — is the case where every literal
// short string is reencoded into the wire and a general compressor like
// zstd has a lot to fold out. The fixture exists so the compression
// benchmarks have a public reproducible payload to measure against.
//
// MakeBatch produces a deterministic Batch with configurable cardinality
// so encode/decode benchmarks can sweep over payload size and observe
// the compression ratio scale with N.
//
// The committed *_gsbm.go siblings are byte-for-byte reproducible from
// these declarations via gsbmcodegen.Generate (see regen_golden_test.go).
package repeatednested

// Batch is the root encoded type. A flat repeated list of Item is the
// only field; the repetition load is what makes the fixture stress
// compression rather than envelope overhead.
//
//gsbm:root
type Batch struct {
	Items []Item `bin:"1"`
}

// Item is one record. Origin/Dest/Currency are short strings drawn from
// small pools so the same byte sequences appear over and over across the
// batch — the property zstd exploits.
type Item struct {
	ID       string `bin:"1"`
	Origin   string `bin:"2"`
	Dest     string `bin:"3"`
	Currency string `bin:"4"`
	Lines    []Line `bin:"5"`
}

// Line is one nested row per Item. Code repeats from a small pool;
// Amount is a numeric leaf; Taxes is a further repeated nested list.
type Line struct {
	Code   string `bin:"1"`
	Amount Money  `bin:"2"`
	Taxes  []Tax  `bin:"3"`
}

// Money is a minimal decimal leaf — fixed-point Units scaled by Scale,
// plus a Currency string drawn from the same small pool the parent uses.
// Kept dependency-free (no import of internal/bench/money) so this
// fixture stands alone.
type Money struct {
	Units    int64  `bin:"1"`
	Scale    int32  `bin:"2"`
	Currency string `bin:"3"`
}

// Tax is one nested tax row. Code repeats from a small pool; Amount is
// the per-tax money figure.
type Tax struct {
	Code   string `bin:"1"`
	Amount Money  `bin:"2"`
}
