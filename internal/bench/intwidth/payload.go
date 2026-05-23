package intwidth

import "math/rand/v2"

// MakeRecord returns a Record populated with deterministic pseudo-random
// values inside the int32 range. Same seed yields the same in-memory
// payload across calls, and every value is small enough that both the
// bounded and unbounded encode paths run to completion — the three
// sub-benchmarks see byte-identical inputs and differ only in whether
// the bounds check is emitted.
func MakeRecord(seed int64) Record {
	r := rand.New(rand.NewPCG(uint64(seed), 0xC0FFEE48C0FFEE48))
	return Record{
		F1: nextInt32(r),
		F2: nextInt32(r),
		F3: nextInt32(r),
		F4: nextInt32(r),
		F5: nextInt32(r),
		F6: nextInt32(r),
		F7: nextInt32(r),
		F8: nextInt32(r),
	}
}

func nextInt32(r *rand.Rand) int {
	return int(int32(r.Uint32()))
}

// PlainFrom, Int32From and Int64From wrap an existing Record in the three
// MarshalGSBM-bearing flavors. The Record is copied by value so the three
// wrappers can be encoded independently without aliasing.
func PlainFrom(rec Record) Plain { return Plain{Record: rec} }
func Int32From(rec Record) Int32 { return Int32{Record: rec} }
func Int64From(rec Record) Int64 { return Int64{Record: rec} }
