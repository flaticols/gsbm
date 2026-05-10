// Package bench contains test-only helpers used by the gsbm benchmark and
// fuzz suites. It is named under internal/ so it stays unimported by
// production code.
//
// The generators produce payloads in the 1-2 MiB range that target the
// Spanner offer-batch hot path. Same seed and target range yields
// byte-identical wire output across calls so benchmarks and fuzz seeds
// are reproducible.
package bench

import (
	"fmt"
	"math/rand/v2"

	"go.flaticols.dev/gsbm/storage/gsbm"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/graph"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/sample"
)

// Marshaler matches the MarshalGSBM signature emitted by gsbmcodegen on
// every root type. Defined locally so the bench package does not require
// adding a public interface to the gsbm runtime.
type Marshaler interface {
	MarshalGSBM(w *gsbm.Writer) error
}

// EncodedSize returns the on-the-wire byte count of v's body. The
// 8-byte blob header is not emitted; the size matches len(w.Bytes())
// after MarshalGSBM, which is also how the codegen-generated path is
// invoked from benchmarks.
func EncodedSize(v Marshaler) (int, error) {
	w := gsbm.NewWriter(nil)
	if err := v.MarshalGSBM(w); err != nil {
		return 0, err
	}
	if err := w.Err(); err != nil {
		return 0, err
	}
	return len(w.Bytes()), nil
}

// MakeLargeOrder returns a sample.Order whose body encodes to a length
// in [targetMin, targetMax]. The same (seed, targetMin, targetMax)
// triple yields byte-identical wire output across calls.
//
// The generator inflates the slice/map cardinalities of an Order
// (Items, Counts, Tags, QtyList, etc.) and fills variable strings with
// deterministic pseudo-random ASCII. It panics if the range is
// nonsensical (min > max, negative bounds) or unreachable.
func MakeLargeOrder(seed int64, targetMin, targetMax int) sample.Order {
	validateRange("MakeLargeOrder", targetMin, targetMax)
	build := func(n int) Marshaler {
		o := buildOrder(seed, n)
		return &o
	}
	n := fitScale(build, targetMin, targetMax)
	return buildOrder(seed, n)
}

// MakeLargeCatalog is the graph-fixture analogue of MakeLargeOrder. It
// scales Section count and per-Section Item count plus Tag count to
// land in the byte-size range. Section.Items[].Note (*string) and
// Tag.Weight (*int64) exercise the optional-in-slice / optional-in-map
// paths.
func MakeLargeCatalog(seed int64, targetMin, targetMax int) graph.Catalog {
	validateRange("MakeLargeCatalog", targetMin, targetMax)
	build := func(n int) Marshaler {
		c := buildCatalog(seed, n)
		return &c
	}
	n := fitScale(build, targetMin, targetMax)
	return buildCatalog(seed, n)
}

func validateRange(name string, lo, hi int) {
	if lo < 0 || hi < 0 || lo > hi {
		panic(fmt.Sprintf("bench.%s: invalid byte-size range [%d, %d]", name, lo, hi))
	}
	if hi-lo < 1024 {
		panic(fmt.Sprintf("bench.%s: range [%d, %d] is too narrow (need >= 1 KiB slack)", name, lo, hi))
	}
}

// fitScale finds the smallest scale factor n such that build(n)
// encodes to a body length in [targetMin, targetMax]. It expands
// exponentially until the floor is met, then binary-searches the
// last doubling window to find a satisfying n. Panics if no n
// in the searched window fits.
func fitScale(build func(int) Marshaler, targetMin, targetMax int) int {
	const maxScale = 1 << 22
	n := 16
	for {
		v := build(n)
		size, err := EncodedSize(v)
		if err != nil {
			panic(fmt.Sprintf("bench.fitScale: encode failed at n=%d: %v", n, err))
		}
		if size >= targetMin {
			if size <= targetMax {
				return n
			}
			break
		}
		if n >= maxScale {
			panic(fmt.Sprintf("bench.fitScale: target floor %d unreachable at n=%d (size %d)", targetMin, n, size))
		}
		n *= 2
	}
	// size > targetMax at the current n; shrink toward the prior step.
	lo, hi := n/2, n
	for lo < hi {
		mid := (lo + hi) / 2
		v := build(mid)
		size, _ := EncodedSize(v)
		if size < targetMin {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	v := build(lo)
	size, _ := EncodedSize(v)
	if size < targetMin || size > targetMax {
		panic(fmt.Sprintf("bench.fitScale: cannot fit [%d, %d]; nearest size %d at n=%d", targetMin, targetMax, size, lo))
	}
	return lo
}

const asciiAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_"

// randomString returns a deterministic ASCII string with a length in
// [minLen, maxLen]. minLen must be <= maxLen.
func randomString(r *rand.Rand, minLen, maxLen int) string {
	span := maxLen - minLen
	n := minLen
	if span > 0 {
		n += r.IntN(span + 1)
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = asciiAlphabet[r.IntN(len(asciiAlphabet))]
	}
	return string(b)
}

func newRand(seed int64, salt uint64) *rand.Rand {
	return rand.New(rand.NewPCG(uint64(seed), salt))
}

// buildOrder constructs a sample.Order whose collection cardinalities
// scale with n. n=0 yields the minimal seed (header fields populated,
// collections empty); larger n grows Items, Tags, Counts, QtyList,
// LabelList, and the Payload byte slice roughly linearly.
func buildOrder(seed int64, n int) sample.Order {
	r := newRand(seed, 0xC0FFEEC0FFEE)
	note := randomString(r, 8, 40)
	custName := randomString(r, 8, 32)
	custEmail := randomString(r, 12, 48)
	o := sample.Order{
		ID:       randomString(r, 8, 24),
		Quantity: int64(r.Uint64N(1<<32)) - (1 << 31),
		Price:    9.99,
		Active:   true,
		Note:     &note,
		Customer: &sample.Customer{Name: custName, Email: custEmail},
		Total:    sample.Total{Currency: "USD", Amount: 42.5},
		Qty:      sample.Quantity(int64(r.Uint64N(1 << 30))),
	}
	optQty := sample.Quantity(int64(r.Uint64N(1 << 30)))
	o.OptQty = &optQty
	optLabel := sample.Label(randomString(r, 4, 16))
	o.OptLabel = &optLabel

	if n <= 0 {
		return o
	}

	o.Items = make([]sample.Item, n)
	for i := range o.Items {
		o.Items[i] = sample.Item{
			SKU:   randomString(r, 8, 24),
			Count: int64(r.Uint64N(1<<32)) - (1 << 31),
		}
	}

	tagCount := n
	o.Tags = make(map[string]int64, tagCount)
	for i := range tagCount {
		k := fmt.Sprintf("tag-%07d-%s", i, randomString(r, 4, 12))
		o.Tags[k] = int64(r.Uint64N(1 << 32))
	}

	o.Counts = make([]int64, n)
	for i := range o.Counts {
		o.Counts[i] = int64(r.Uint64N(1<<32)) - (1 << 31)
	}

	aliasCount := n/4 + 1
	o.Aliases = make(map[string]sample.Label, aliasCount)
	for i := range aliasCount {
		k := fmt.Sprintf("alias-%07d-%s", i, randomString(r, 4, 12))
		o.Aliases[k] = sample.Label(randomString(r, 4, 16))
	}

	o.QtyList = make([]sample.Quantity, n)
	for i := range o.QtyList {
		o.QtyList[i] = sample.Quantity(int64(r.Uint64N(1 << 30)))
	}

	labelCount := n/4 + 1
	o.LabelList = make([]sample.Label, labelCount)
	for i := range o.LabelList {
		o.LabelList[i] = sample.Label(randomString(r, 4, 16))
	}

	payload := make([]byte, n*4)
	for i := range payload {
		payload[i] = byte(r.UintN(256))
	}
	o.Payload = payload

	optPayload := make([]byte, n*2)
	for i := range optPayload {
		optPayload[i] = byte(r.UintN(256))
	}
	o.OptPayload = &optPayload

	return o
}

// buildCatalog constructs a graph.Catalog whose two-level composition
// (Sections → Items) and Tags map scale with n. n=0 yields an empty
// Catalog with the Tail marker; larger n grows the Section count and
// the Items per Section.
func buildCatalog(seed int64, n int) graph.Catalog {
	r := newRand(seed, 0xBEEFCAFEDEAD0042)
	c := graph.Catalog{
		ID:   randomString(r, 8, 24),
		Tail: graph.EdgeMarker{Marker: true},
	}
	if n <= 0 {
		return c
	}

	sectionCount := max(n/32, 4)
	itemsPerSection := max(n/sectionCount, 1)
	c.Sections = make([]graph.Section, sectionCount)
	for s := range c.Sections {
		items := make([]graph.Item, itemsPerSection)
		for i := range items {
			sku := randomString(r, 8, 24)
			switch i % 3 {
			case 0:
				note := randomString(r, 4, 16)
				items[i] = graph.Item{SKU: sku, Note: &note}
			case 1:
				items[i] = graph.Item{SKU: sku, Note: nil}
			default:
				empty := ""
				items[i] = graph.Item{SKU: sku, Note: &empty}
			}
		}
		c.Sections[s] = graph.Section{
			Name:  randomString(r, 8, 24),
			Items: items,
		}
	}

	tagCount := n/8 + 1
	c.Tags = make(map[string]graph.Tag, tagCount)
	for i := range tagCount {
		k := fmt.Sprintf("tag-%07d-%s", i, randomString(r, 4, 12))
		switch i % 3 {
		case 0:
			w := int64(r.Uint64N(1 << 32))
			c.Tags[k] = graph.Tag{Slug: randomString(r, 4, 16), Weight: &w}
		case 1:
			c.Tags[k] = graph.Tag{Slug: randomString(r, 4, 16), Weight: nil}
		default:
			zero := int64(0)
			c.Tags[k] = graph.Tag{Slug: randomString(r, 4, 16), Weight: &zero}
		}
	}

	return c
}
