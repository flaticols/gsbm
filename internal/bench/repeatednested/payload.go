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
