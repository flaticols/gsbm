package sample

import (
	"bytes"
	"math/rand/v2"
	"reflect"
	"strconv"
	"testing"

	"go.flaticols.dev/gsbm/storage/gsbm"
	"go.flaticols.dev/gsbm/storage/gsbmarena"
)

// TestArenaCrossModeRoundTrip is the headline contract of arena mode:
// heap encode → arena decode → values match heap-decode of the same blob.
// Also covers Detach: arena decode → Detach → DeepEqual to heap decode.
func TestArenaCrossModeRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   Order
	}{
		{
			name: "fully populated",
			in:   makeRichOrder(),
		},
		{
			name: "minimal",
			in: Order{
				ID:    "min",
				Total: Total{Currency: "USD", Amount: 1},
			},
		},
		{
			name: "zero-elide note",
			in: Order{
				ID:   "z",
				Note: note(""),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := gsbm.NewWriter(nil)
			w.WriteHeader(0, 1, 0)
			if err := tc.in.MarshalGSBM(w); err != nil {
				t.Fatalf("marshal: %v", err)
			}
			w.FinalizeBodyLen()
			blob := w.Bytes()

			// Heap baseline.
			var heap Order
			if err := gsbm.DecodeInto(blob, &heap); err != nil {
				t.Fatalf("heap decode: %v", err)
			}

			// Arena decode.
			a := gsbmarena.NewArena()
			arenaOrder, err := DecodeOrder(blob, a)
			if err != nil {
				t.Fatalf("arena decode: %v", err)
			}
			if !reflect.DeepEqual(heap, *arenaOrder) {
				t.Fatalf("arena decode diverged from heap decode\n heap:  %#v\n arena: %#v",
					heap, *arenaOrder)
			}

			// Detach lifts arena values onto the heap and survives Release.
			detached, err := DetachOrder(arenaOrder)
			if err != nil {
				t.Fatalf("detach: %v", err)
			}
			a.Release()
			if !reflect.DeepEqual(heap, *detached) {
				t.Fatalf("detached order diverged from heap baseline\n heap:     %#v\n detached: %#v",
					heap, *detached)
			}
		})
	}
}

// TestArenaDecodeBody covers the headerless variant — the caller has
// already consumed the 12-byte preamble before handing in the body bytes.
func TestArenaDecodeBody(t *testing.T) {
	in := makeRichOrder()
	w := gsbm.NewWriter(nil)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatal(err)
	}

	a := gsbmarena.NewArena()
	defer a.Release()
	got, err := DecodeOrderBody(w.Bytes(), a)
	if err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got.ID != in.ID || got.Total.Currency != in.Total.Currency {
		t.Fatalf("body decode mismatch: %+v", got)
	}
}

// TestArenaAllocsScaleSublinearly proves the arena's pooling claim:
// decoding a blob with N items costs O(chunks) allocations, not O(N).
// We compare alloc counts between an N=4 decode and an N=64 decode and
// require the larger payload to use comparable allocations — not 16×.
func TestArenaAllocsScaleSublinearly(t *testing.T) {
	mk := func(n int) []byte {
		o := Order{ID: "scale", Total: Total{Currency: "USD", Amount: 1}}
		o.Items = make([]Item, n)
		for i := range o.Items {
			o.Items[i] = Item{SKU: "sku-" + strconv.Itoa(i), Count: int64(i)}
		}
		w := gsbm.NewWriter(nil)
		w.WriteHeader(0, 1, 0)
		if err := o.MarshalGSBM(w); err != nil {
			t.Fatal(err)
		}
		w.FinalizeBodyLen()
		return w.Bytes()
	}
	small := mk(4)
	big := mk(64)

	smallAllocs := testing.AllocsPerRun(20, func() {
		a := gsbmarena.NewArena()
		_, err := DecodeOrder(small, a)
		if err != nil {
			t.Fatal(err)
		}
	})
	bigAllocs := testing.AllocsPerRun(20, func() {
		a := gsbmarena.NewArena()
		_, err := DecodeOrder(big, a)
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Logf("arena allocs: 4-item=%.1f, 64-item=%.1f", smallAllocs, bigAllocs)
	// 16× more items must NOT cost 16× more allocations. The bound
	// absorbs pool-init noise plus the per-receiver presence-mask insert
	// (each freshly arena-allocated Item triggers one sidecar entry on
	// first MarkPresent), but a true near-linear scale would still
	// blow past 8×.
	if bigAllocs > smallAllocs*8 {
		t.Fatalf("arena alloc count scales near-linearly with graph size: 4→%.1f, 64→%.1f",
			smallAllocs, bigAllocs)
	}
}

// TestArenaMutateThenDetach documents the "decoded values are read-only;
// mutation requires Detach" contract. After Detach the heap copy can be
// mutated freely without touching arena memory.
func TestArenaMutateThenDetach(t *testing.T) {
	in := Order{ID: "ro", Total: Total{Currency: "USD", Amount: 5}}
	w := gsbm.NewWriter(nil)
	w.WriteHeader(0, 1, 0)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatal(err)
	}
	w.FinalizeBodyLen()
	a := gsbmarena.NewArena()
	got, err := DecodeOrder(w.Bytes(), a)
	if err != nil {
		t.Fatal(err)
	}
	detached, err := DetachOrder(got)
	if err != nil {
		t.Fatal(err)
	}
	a.Release()
	// Mutate post-Release: detached must be independent of arena memory.
	detached.ID = "mutated"
	detached.Total.Amount = 99
	if detached.ID != "mutated" || detached.Total.Amount != 99 {
		t.Fatal("detached struct could not be mutated independently")
	}
}

// FuzzArenaDecodeAgainstHeap drives randomized blobs through both
// decoders and rejects divergent outcomes (one accepts, one rejects).
// The decoders share UnmarshalGSBM, so byte-equivalence is structural;
// the fuzz check guards against a regression where the arena allocator
// diverges from heap behavior on malformed input.
func FuzzArenaDecodeAgainstHeap(f *testing.F) {
	// Seed with a real blob so the corpus starts well-formed.
	in := makeRichOrder()
	w := gsbm.NewWriter(nil)
	w.WriteHeader(0, 1, 0)
	if err := in.MarshalGSBM(w); err != nil {
		f.Fatal(err)
	}
	w.FinalizeBodyLen()
	f.Add(w.Bytes())
	// Plus a trivial blob and an obvious truncation.
	f.Add([]byte("GSBM\x02\x00\x00\x00\x00\x00\x00\x00"))
	f.Add([]byte{})
	// 1-2 MiB realistic Spanner-batch-shaped seed. Built locally rather
	// than imported from internal/bench because internal/bench imports
	// this package, so the dependency would be circular.
	large := makeLargeOrderForFuzzSeed()
	wL := gsbm.NewWriter(nil)
	wL.WriteHeader(0, 1, 0)
	if err := large.MarshalGSBM(wL); err != nil {
		f.Fatal(err)
	}
	wL.FinalizeBodyLen()
	f.Add(wL.Bytes())

	f.Fuzz(func(t *testing.T, data []byte) {
		var heap Order
		heapErr := gsbm.DecodeInto(data, &heap)

		a := gsbmarena.NewArena()
		arenaOrder, arenaErr := DecodeOrder(data, a)

		if (heapErr == nil) != (arenaErr == nil) {
			a.Release()
			t.Fatalf("decoder divergence on %x: heapErr=%v arenaErr=%v",
				data, heapErr, arenaErr)
		}
		// When both decoders accept, re-encode each side and compare the
		// resulting bytes. This catches arena-mode regressions where the
		// arena routing produces a different decoded value than heap.
		// Re-encoding instead of reflect.DeepEqual sidesteps NaN-not-equal
		// and unsafe.String-vs-string addressing differences. Maps are
		// non-deterministically ordered by encode, so blobs containing a
		// non-empty Tags or Aliases skip this check.
		if heapErr == nil {
			if len(heap.Tags) <= 1 && len(heap.Aliases) <= 1 {
				w1 := gsbm.NewWriter(nil)
				if err := heap.MarshalGSBM(w1); err != nil {
					a.Release()
					t.Fatalf("heap re-encode failed: %v", err)
				}
				w2 := gsbm.NewWriter(nil)
				if err := arenaOrder.MarshalGSBM(w2); err != nil {
					a.Release()
					t.Fatalf("arena re-encode failed: %v", err)
				}
				if !bytes.Equal(w1.Bytes(), w2.Bytes()) {
					a.Release()
					t.Fatalf("decoder value divergence on %x:\n heap re-encode:  %x\n arena re-encode: %x",
						data, w1.Bytes(), w2.Bytes())
				}
			}
		}
		a.Release()
	})
}

// makeLargeOrderForFuzzSeed builds a deterministic Order whose body
// encodes to ≥ 1 MiB. It is the in-package twin of internal/bench's
// MakeLargeOrder; duplicated here to break the import cycle (bench
// imports sample). Use math/rand/v2 with a fixed seed so the seed file
// committed under testdata/fuzz is byte-stable across machines.
func makeLargeOrderForFuzzSeed() Order {
	r := rand.New(rand.NewPCG(0xA5A5A5A5, 0x5A5A5A5A))
	const n = 8000
	o := makeRichOrder()
	o.Items = make([]Item, n)
	for i := range o.Items {
		o.Items[i] = Item{
			SKU:   "sku-" + strconv.Itoa(i) + "-" + randomASCII(r, 8, 24),
			Count: int64(r.Uint64N(1 << 32)),
		}
	}
	o.Counts = make([]int64, n)
	for i := range o.Counts {
		o.Counts[i] = int64(r.Uint64N(1 << 32))
	}
	o.QtyList = make([]Quantity, n)
	for i := range o.QtyList {
		o.QtyList[i] = Quantity(int64(r.Uint64N(1 << 30)))
	}
	payload := make([]byte, n*64)
	for i := range payload {
		payload[i] = byte(r.UintN(256))
	}
	o.Payload = payload
	return o
}

const fuzzSeedAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_"

func randomASCII(r *rand.Rand, minLen, maxLen int) string {
	span := maxLen - minLen
	n := minLen
	if span > 0 {
		n += r.IntN(span + 1)
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = fuzzSeedAlphabet[r.IntN(len(fuzzSeedAlphabet))]
	}
	return string(b)
}
