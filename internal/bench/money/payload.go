package money

import "math/rand/v2"

// MakeBatch returns a Batch with the issue's example shape: nLines rows,
// each row carrying three Money values (Base + one Tax + one Adjustment).
// Same seed yields the same in-memory payload across calls so the two
// sub-benchmarks see byte-identical inputs.
//
// Currencies cycle through a small list and amounts are deterministically
// generated so the resulting wire bytes are reproducible.
func MakeBatch(seed int64, nLines int) Batch {
	r := rand.New(rand.NewPCG(uint64(seed), 0xC0FFEEDECAFBAD))
	currencies := []string{"USD", "EUR", "GBP", "JPY", "CHF"}

	lines := make([]Line, nLines)
	for i := range lines {
		lines[i] = Line{
			Base: Money{
				Amount:   randomDecimal(r),
				Currency: currencies[r.IntN(len(currencies))],
			},
			Taxes: []Money{
				{
					Amount:   randomDecimal(r),
					Currency: currencies[r.IntN(len(currencies))],
				},
			},
			Adjustments: []Money{
				{
					Amount:   randomDecimal(r),
					Currency: currencies[r.IntN(len(currencies))],
				},
			},
		}
	}
	return Batch{Lines: lines}
}

// StringBatchFrom and AppendBatchFrom return two MarshalGSBM-bearing
// wrappers over the same lines. The Lines slice is shared by reference;
// the only difference between the two is which decimal codec their
// MarshalGSBM bodies invoke.
func StringBatchFrom(b Batch) StringBatch { return StringBatch{Lines: b.Lines} }
func AppendBatchFrom(b Batch) AppendBatch { return AppendBatch{Lines: b.Lines} }

func randomDecimal(r *rand.Rand) DecimalAmount {
	intDigits := 1 + r.IntN(6)
	fracDigits := r.IntN(4)
	return DecimalAmount{
		Negative: r.IntN(4) == 0,
		Integer:  randomDigits(r, intDigits),
		Fraction: randomDigits(r, fracDigits),
	}
}

func randomDigits(r *rand.Rand, n int) string {
	if n == 0 {
		return ""
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = '0' + byte(r.IntN(10))
	}
	if n > 1 && b[0] == '0' {
		b[0] = '1' + byte(r.IntN(9))
	}
	return string(b)
}
