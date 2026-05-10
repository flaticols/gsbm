package bench

import (
	"bytes"
	"strings"
	"testing"

	"go.flaticols.dev/gsbm/storage/gsbm"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/graph"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/sample"
)

const (
	oneMiB = 1 << 20
	twoMiB = 2 << 20
)

func encodedBytes(t *testing.T, m Marshaler) []byte {
	t.Helper()
	w := gsbm.NewWriter(nil)
	if err := m.MarshalGSBM(w); err != nil {
		t.Fatalf("MarshalGSBM: %v", err)
	}
	if err := w.Err(); err != nil {
		t.Fatalf("writer err: %v", err)
	}
	out := w.Bytes()
	cp := make([]byte, len(out))
	copy(cp, out)
	return cp
}

func TestMakeLargeOrderInRange(t *testing.T) {
	o := MakeLargeOrder(0, oneMiB, twoMiB)
	blob := encodedBytes(t, &o)
	if len(blob) < oneMiB || len(blob) > twoMiB {
		t.Fatalf("encoded size %d outside [%d, %d]", len(blob), oneMiB, twoMiB)
	}
	size, err := EncodedSize(&o)
	if err != nil {
		t.Fatalf("EncodedSize: %v", err)
	}
	if size != len(blob) {
		t.Fatalf("EncodedSize mismatch: %d vs len(blob)=%d", size, len(blob))
	}
}

func TestMakeLargeOrderDeterministic(t *testing.T) {
	a := MakeLargeOrder(42, oneMiB, twoMiB)
	b := MakeLargeOrder(42, oneMiB, twoMiB)
	ab := encodedBytes(t, &a)
	bb := encodedBytes(t, &b)
	if !bytes.Equal(ab, bb) {
		t.Fatalf("same seed produced different encoded bytes: len(a)=%d len(b)=%d", len(ab), len(bb))
	}
}

func TestMakeLargeOrderDifferentSeeds(t *testing.T) {
	a := MakeLargeOrder(1, oneMiB, twoMiB)
	b := MakeLargeOrder(2, oneMiB, twoMiB)
	if bytes.Equal(encodedBytes(t, &a), encodedBytes(t, &b)) {
		t.Fatal("different seeds produced identical bytes; PRNG salt is not connecting to seed")
	}
}

func TestMakeLargeCatalogInRange(t *testing.T) {
	c := MakeLargeCatalog(0, oneMiB, twoMiB)
	blob := encodedBytes(t, &c)
	if len(blob) < oneMiB || len(blob) > twoMiB {
		t.Fatalf("encoded size %d outside [%d, %d]", len(blob), oneMiB, twoMiB)
	}
	size, err := EncodedSize(&c)
	if err != nil {
		t.Fatalf("EncodedSize: %v", err)
	}
	if size != len(blob) {
		t.Fatalf("EncodedSize mismatch: %d vs len(blob)=%d", size, len(blob))
	}
}

func TestMakeLargeCatalogDeterministic(t *testing.T) {
	a := MakeLargeCatalog(7, oneMiB, twoMiB)
	b := MakeLargeCatalog(7, oneMiB, twoMiB)
	if !bytes.Equal(encodedBytes(t, &a), encodedBytes(t, &b)) {
		t.Fatal("same seed produced different Catalog bytes")
	}
}

// TestMakeLargeOrderRoundTrips decodes the generator output and re-encodes
// it. The result must be byte-identical, which both sanity-checks the
// generator's wire shape and proves the deterministic on-wire ordering
// (sorted map keys per spec §5.3) survives a full round-trip.
func TestMakeLargeOrderRoundTrips(t *testing.T) {
	o := MakeLargeOrder(0, oneMiB, twoMiB)
	blob := encodedBytes(t, &o)
	var got sample.Order
	if err := gsbm.DecodeBodyInto(blob, &got); err != nil {
		t.Fatalf("DecodeBodyInto: %v", err)
	}
	reEnc := encodedBytes(t, &got)
	if !bytes.Equal(blob, reEnc) {
		t.Fatalf("re-encoded bytes diverge: len(orig)=%d len(re)=%d", len(blob), len(reEnc))
	}
}

func TestMakeLargeCatalogRoundTrips(t *testing.T) {
	c := MakeLargeCatalog(0, oneMiB, twoMiB)
	blob := encodedBytes(t, &c)
	var got graph.Catalog
	if err := gsbm.DecodeBodyInto(blob, &got); err != nil {
		t.Fatalf("DecodeBodyInto: %v", err)
	}
	reEnc := encodedBytes(t, &got)
	if !bytes.Equal(blob, reEnc) {
		t.Fatalf("re-encoded Catalog bytes diverge: len(orig)=%d len(re)=%d", len(blob), len(reEnc))
	}
}

func TestMakeLargeOrderRejectsInvalidRange(t *testing.T) {
	cases := []struct {
		name      string
		minS, maxS int
		wantSubstr string
	}{
		{"min greater than max", twoMiB, oneMiB, "invalid byte-size range"},
		{"negative min", -1, oneMiB, "invalid byte-size range"},
		{"too narrow", oneMiB, oneMiB + 16, "too narrow"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				p := recover()
				if p == nil {
					t.Fatalf("expected panic, got none")
				}
				msg, ok := p.(string)
				if !ok {
					t.Fatalf("panic value not string: %T %v", p, p)
				}
				if !strings.Contains(msg, tc.wantSubstr) {
					t.Fatalf("panic %q missing substring %q", msg, tc.wantSubstr)
				}
			}()
			_ = MakeLargeOrder(0, tc.minS, tc.maxS)
		})
	}
}

func TestMakeLargeCatalogRejectsInvalidRange(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for min > max")
		}
	}()
	_ = MakeLargeCatalog(0, twoMiB, oneMiB)
}
