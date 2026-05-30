package gsbm

import (
	"strconv"
	"testing"
	"unsafe"
)

// sameBacking reports whether two strings share the same backing array —
// the property that proves interning returned the cached copy rather than
// a fresh allocation.
func sameBacking(a, b string) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	return unsafe.StringData(a) == unsafe.StringData(b)
}

func TestInternerDedupsRepeatedValues(t *testing.T) {
	in := NewInterner()
	// Distinct []byte sources with identical contents must collapse to one
	// backing array, and the returned values must compare equal.
	first := in.AcquireString([]byte("USD"))
	again := in.AcquireString([]byte("USD"))
	if first != again {
		t.Fatalf("values differ: %q vs %q", first, again)
	}
	if !sameBacking(first, again) {
		t.Fatal("repeated AcquireString returned distinct backing arrays — not interned")
	}
	if in.Len() != 1 {
		t.Fatalf("Len = %d, want 1 distinct string", in.Len())
	}
	// A different value gets its own entry.
	other := in.AcquireString([]byte("EUR"))
	if sameBacking(first, other) {
		t.Fatal("distinct values share backing — wrong")
	}
	if in.Len() != 2 {
		t.Fatalf("Len = %d, want 2", in.Len())
	}
}

func TestInternerEmptyString(t *testing.T) {
	in := NewInterner()
	got := in.AcquireString(nil)
	if got != "" {
		t.Fatalf("nil input = %q, want empty", got)
	}
	got = in.AcquireString([]byte{})
	if got != "" {
		t.Fatalf("empty input = %q, want empty", got)
	}
	if in.Len() != 0 {
		t.Fatalf("empty input retained an entry: Len = %d", in.Len())
	}
}

func TestInternerDoesNotAliasSource(t *testing.T) {
	in := NewInterner()
	src := []byte("mutable")
	s := in.AcquireString(src)
	src[0] = 'X' // mutate the source after interning
	if s != "mutable" {
		t.Fatalf("interned string aliased its source: %q", s)
	}
}

func TestInternerCapFallsBackToCopy(t *testing.T) {
	// Multi-byte values: Go's runtime returns a shared static backing for
	// single-byte string([]byte) conversions, which would mask the
	// not-retained property the cap is supposed to give.
	in := NewInternerN(2)
	a := in.AcquireString([]byte("alpha"))
	b := in.AcquireString([]byte("bravo"))
	// Table is full (2 distinct). A new distinct value must still decode
	// correctly, just without retention.
	c := in.AcquireString([]byte("charlie"))
	if c != "charlie" {
		t.Fatalf("capped interner returned %q, want \"charlie\"", c)
	}
	if in.Len() != 2 {
		t.Fatalf("Len = %d, want 2 (cap)", in.Len())
	}
	// Already-cached values still dedup after the cap is hit.
	a2 := in.AcquireString([]byte("alpha"))
	if !sameBacking(a, a2) {
		t.Fatal("cached value lost its backing after cap reached")
	}
	// A repeat of the over-cap value is a fresh copy each time (not retained).
	c2 := in.AcquireString([]byte("charlie"))
	if c2 != "charlie" {
		t.Fatalf("over-cap repeat = %q", c2)
	}
	if sameBacking(c, c2) {
		t.Fatal("over-cap value was retained despite the cap")
	}
	_ = b
}

func TestInternerResetReleasesTable(t *testing.T) {
	in := NewInterner()
	for i := range 100 {
		in.AcquireString([]byte(strconv.Itoa(i)))
	}
	if in.Len() != 100 {
		t.Fatalf("Len = %d, want 100", in.Len())
	}
	in.Reset()
	if in.Len() != 0 {
		t.Fatalf("after Reset Len = %d, want 0", in.Len())
	}
	// Reusable after Reset.
	s := in.AcquireString([]byte("x"))
	if s != "x" || in.Len() != 1 {
		t.Fatalf("post-Reset acquire: %q Len=%d", s, in.Len())
	}
}

func TestInternerZeroValueUsable(t *testing.T) {
	var in Interner // zero value: nil table
	s := in.AcquireString([]byte("z"))
	if s != "z" {
		t.Fatalf("zero-value interner returned %q", s)
	}
	if in.Len() != 1 {
		t.Fatalf("Len = %d, want 1", in.Len())
	}
}

// TestInternerSatisfiesAllocator pins that *Interner is a drop-in
// Allocator — the whole point of the zero-codegen-change design.
func TestInternerSatisfiesAllocator(t *testing.T) {
	var _ Allocator = NewInterner()
	var _ Allocator = (*Interner)(nil)
}
