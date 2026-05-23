package repeatednested

import (
	"fmt"
	"math/rand/v2"
)

// Airport-code, currency-code and tax-code pools are intentionally small.
// Repetition across the batch is the load-bearing property of this
// fixture — without it, a general compressor has nothing to exploit and
// the compression benchmark would not measure what it sets out to.
var (
	airportPool = []string{
		"AKL", "AMS", "ANC", "ATH", "ATL", "AUH", "BCN", "BKK", "BLR", "BOG",
		"BOM", "BOS", "BRU", "CAI", "CDG", "CGK", "CMN", "CPH", "CPT", "DEL",
		"DEN", "DFW", "DOH", "DUB", "DXB", "EWR", "EZE", "FCO", "FRA", "GIG",
		"GRU", "HEL", "HKG", "HND", "IAD", "IAH", "ICN", "IST", "JFK", "JNB",
		"KEF", "KIX", "KUL", "LAX", "LGA", "LGW", "LHR", "LIM", "LIS", "MAD",
	}
	currencyPool = []string{
		"USD", "EUR", "GBP", "JPY", "CHF", "CAD", "AUD", "NZD", "SGD", "HKD",
	}
	taxCodePool = []string{
		"VAT", "GST", "PST", "HST", "CIT", "MWS", "BTW", "IVA", "TVA", "MWT",
		"DUT", "EXC", "SUR", "FED", "STA", "LOC", "AIR", "FUE", "CIV", "ECO",
	}
)

// MakeBatch returns a Batch shaped for compression benchmarks: nItems
// items, each carrying nLinesPerItem lines, each line carrying
// nTaxesPerLine taxes. Short string fields (Origin, Dest, Currency, Line
// Code, Tax Code) are drawn from the small fixed pools above so the
// resulting wire blob has heavy short-string repetition — exactly the
// shape the ticket targets.
//
// Same (seed, nItems, nLinesPerItem, nTaxesPerLine) tuple yields a
// byte-identical Batch across calls so encode/decode benchmarks see
// reproducible inputs and recorded bytes/op numbers are stable.
//
// Panics if any cardinality argument is negative.
func MakeBatch(seed int64, nItems, nLinesPerItem, nTaxesPerLine int) Batch {
	if nItems < 0 || nLinesPerItem < 0 || nTaxesPerLine < 0 {
		panic(fmt.Sprintf("repeatednested.MakeBatch: invalid (nItems=%d, nLinesPerItem=%d, nTaxesPerLine=%d)",
			nItems, nLinesPerItem, nTaxesPerLine))
	}
	r := rand.New(rand.NewPCG(uint64(seed), 0xC0FFEE53C0DE0053))
	items := make([]Item, nItems)
	for i := range items {
		lines := make([]Line, nLinesPerItem)
		for j := range lines {
			taxes := make([]Tax, nTaxesPerLine)
			for k := range taxes {
				taxes[k] = Tax{
					Code: taxCodePool[r.IntN(len(taxCodePool))],
					Amount: Money{
						Units:    int64(r.IntN(1_000_000)),
						Scale:    2,
						Currency: currencyPool[r.IntN(len(currencyPool))],
					},
				}
			}
			lines[j] = Line{
				Code: airportPool[r.IntN(len(airportPool))],
				Amount: Money{
					Units:    int64(r.IntN(10_000_000)),
					Scale:    2,
					Currency: currencyPool[r.IntN(len(currencyPool))],
				},
				Taxes: taxes,
			}
		}
		items[i] = Item{
			ID:       fmt.Sprintf("ITEM-%08d", i),
			Origin:   airportPool[r.IntN(len(airportPool))],
			Dest:     airportPool[r.IntN(len(airportPool))],
			Currency: currencyPool[r.IntN(len(currencyPool))],
			Lines:    lines,
		}
	}
	return Batch{Items: items}
}

// priceLabelPool is a small fixed pool of short labels for [Price.Label]
// so the repetition signal across the NestedBatch wire stays heavy.
var priceLabelPool = []string{
	"BASE", "FUEL", "TAX", "SVC", "INS", "FEE", "DSC", "BAG", "SEA", "MEA",
}

// MakeNestedBatch returns a NestedBatch shaped for the "many parallel
// nested length-delimited regions per item" workload of issue #58.
// Each NestedItem carries three parallel nested slices (Legs, Prices,
// Tags), so the total number of inner length-delim regions per Batch is
// nItems * (nLegsPerItem + nPricesPerItem + nTagsPerItem) — the
// Cartesian product where (*Writer).BeginLengthDelim's recordedRegions
// append shows up as the top allocator on the streaming-compressed
// encode path before this PR.
//
// Same (seed, nItems, nLegsPerItem, nPricesPerItem, nTagsPerItem) tuple
// yields a byte-identical NestedBatch across calls so encode/decode
// benchmarks see reproducible inputs.
//
// Panics if any cardinality argument is negative.
func MakeNestedBatch(seed int64, nItems, nLegsPerItem, nPricesPerItem, nTagsPerItem int) NestedBatch {
	if nItems < 0 || nLegsPerItem < 0 || nPricesPerItem < 0 || nTagsPerItem < 0 {
		panic(fmt.Sprintf("repeatednested.MakeNestedBatch: invalid (nItems=%d, nLegsPerItem=%d, nPricesPerItem=%d, nTagsPerItem=%d)",
			nItems, nLegsPerItem, nPricesPerItem, nTagsPerItem))
	}
	r := rand.New(rand.NewPCG(uint64(seed), 0xD0DE53C0FFEE5800))
	items := make([]NestedItem, nItems)
	for i := range items {
		legs := make([]Leg, nLegsPerItem)
		for j := range legs {
			legs[j] = Leg{
				Origin: airportPool[r.IntN(len(airportPool))],
				Dest:   airportPool[r.IntN(len(airportPool))],
			}
		}
		prices := make([]Price, nPricesPerItem)
		for j := range prices {
			prices[j] = Price{
				Amount: Money{
					Units:    int64(r.IntN(1_000_000)),
					Scale:    2,
					Currency: currencyPool[r.IntN(len(currencyPool))],
				},
				Label: priceLabelPool[r.IntN(len(priceLabelPool))],
			}
		}
		tags := make([]Tag, nTagsPerItem)
		for j := range tags {
			tags[j] = Tag{
				Code: taxCodePool[r.IntN(len(taxCodePool))],
				Rate: int32(r.IntN(2500)),
			}
		}
		items[i] = NestedItem{
			ID:     fmt.Sprintf("NITEM-%08d", i),
			Legs:   legs,
			Prices: prices,
			Tags:   tags,
		}
	}
	return NestedBatch{Items: items}
}
