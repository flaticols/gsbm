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

// NestedBatch is the sibling root for issue #58 — the "many parallel
// nested length-delimited regions per item" shape. Each NestedItem
// carries three parallel nested slices (Legs, Prices, Tags) rather than
// the serial Item→Line→Tax chain of [Batch]. The Cartesian product of
// Item count × parallel-slice count is exactly the workload where
// (*Writer).BeginLengthDelim's recordedRegions append shows up as the
// top allocator on the streaming-compressed encode path, which this
// fixture is designed to expose.
//
//gsbm:root
type NestedBatch struct {
	Items []NestedItem `bin:"1"`
}

// NestedItem carries three parallel nested slices. Each Item produces
// 3 outer length-delim regions plus one inner length-delim per element
// across all three slices — the shape that makes recordedRegions
// allocation pressure dominate.
type NestedItem struct {
	ID     string  `bin:"1"`
	Legs   []Leg   `bin:"2"`
	Prices []Price `bin:"3"`
	Tags   []Tag   `bin:"4"`
}

// Leg is a small primitive-heavy nested struct. Origin/Dest draw from
// the airport pool so the wire bytes carry the same heavy short-string
// repetition the rest of this fixture relies on.
type Leg struct {
	Origin string `bin:"1"`
	Dest   string `bin:"2"`
}

// Price is a small nested struct pairing a Money figure with a label
// drawn from a small pool — keeps the same repetition profile.
type Price struct {
	Amount Money  `bin:"1"`
	Label  string `bin:"2"`
}

// Tag is a small nested struct. Code repeats from a fixed pool; Rate
// is a numeric leaf so the encoded size per Tag is small and the
// per-region overhead dominates.
type Tag struct {
	Code string `bin:"1"`
	Rate int32  `bin:"2"`
}
