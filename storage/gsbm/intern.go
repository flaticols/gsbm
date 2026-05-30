package gsbm

// Interner is an Allocator that deduplicates decoded strings: the first
// time a distinct byte sequence is decoded it is copied once into a shared
// table, and every later occurrence of the same bytes returns that one Go
// string instead of a fresh copy. For payloads where the same short
// strings recur across many records — currency codes, airport codes,
// status enums, feature flags — this collapses the duplicate string
// backing arrays in the decoded graph down to one per distinct value.
//
// It implements [Allocator] via [Interner.AcquireString], so it installs
// through [Reader.SetAllocator] with no change to generated code, no wire
// format change, and no effect on decoded values: the resulting graph is
// equal (by ==) to a heap decode, only its string storage is shared. Every
// generated string call site — plain fields, slice elements, map keys,
// map values, and //gsbm:borrow-strings structs (which route through
// AcquireString whenever an allocator is installed) — funnels through
// AcquireString, so interning reaches all of them.
//
// Interned strings own their bytes and are immutable, so unlike
// //gsbm:borrow-strings views they are stable: safe to use as map keys and
// safe after the source blob is reused or freed. The decoded graph holds
// no pointer into the reader's buffer, so the blob need not be pinned.
//
// # Lifetime and growth
//
// An Interner retains every distinct string it has seen until [Interner.Reset]
// (or until the Interner itself is collected). Two usage modes follow:
//
//   - Per-decode (the safe default): one Interner per decode, discarded
//     when the call returns. Memory is bounded by the decoded graph; there
//     is no cross-call retention. [DecodeInterned] is the convenience entry
//     point for this mode.
//   - Shared across decodes: reuse one Interner over many decodes to
//     deduplicate across blobs too. This retains the union of all distinct
//     strings, so it MUST only be used with a bounded value universe (a
//     fixed set of codes/enums) or with an explicit [Interner.Reset] cadence;
//     an unbounded stream of distinct strings would grow without limit.
//     [NewInternerN] caps the table as a safety valve — past the cap,
//     AcquireString stops retaining and falls back to a plain copy.
//
// An Interner is not safe for concurrent use; pool one per goroutine the
// same way [Reader] and [Writer] are pooled.
type Interner struct {
	tab map[string]string
	// max caps the number of distinct strings retained. Zero means
	// unbounded. Once len(tab) reaches max, AcquireString returns a plain
	// copy without inserting, so a shared Interner over a hostile or
	// unbounded value universe cannot grow without limit.
	max int
}

// NewInterner returns an empty unbounded Interner. Suitable as a per-decode
// interner (see [DecodeInterned]) or as a caller-managed shared interner
// over a bounded value universe.
func NewInterner() *Interner {
	return &Interner{tab: make(map[string]string)}
}

// NewInternerN returns an Interner that retains at most maxDistinct
// distinct strings. Once the table is full, further distinct values are
// returned as plain copies (still correct, just not deduplicated) so a
// shared interner over an unbounded value universe stays memory-bounded. A
// maxDistinct <= 0 is treated as unbounded, identical to [NewInterner].
func NewInternerN(maxDistinct int) *Interner {
	if maxDistinct < 0 {
		maxDistinct = 0
	}
	hint := maxDistinct
	if hint <= 0 || hint > 1024 {
		hint = 1024
	}
	return &Interner{tab: make(map[string]string, hint), max: maxDistinct}
}

// AcquireString returns a shared, deduplicated Go string for b. The first
// occurrence of a distinct byte sequence is copied once and retained; later
// occurrences return that same string with no further allocation. The empty
// input yields the empty string with no allocation or table entry.
//
// AcquireString satisfies [Allocator].
func (i *Interner) AcquireString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if i.tab == nil {
		i.tab = make(map[string]string)
	}
	// m[string(b)] on a []byte is special-cased by the compiler to look up
	// without allocating a string, so a hit costs no allocation at all.
	if s, ok := i.tab[string(b)]; ok {
		return s
	}
	// Table full (capped interner): stay correct, just stop retaining.
	if i.max > 0 && len(i.tab) >= i.max {
		return string(b)
	}
	s := string(b)
	i.tab[s] = s
	return s
}

// Len reports the number of distinct strings currently retained. Exposed
// for observability and tests (e.g. asserting the dedup ratio on a known
// fixture).
func (i *Interner) Len() int { return len(i.tab) }

// Reset drops every retained string, releasing the table's memory for GC
// while preserving the Interner for reuse. Any strings already handed out
// remain valid — they own their bytes — so Reset is safe to call between
// decodes whose results have been consumed or copied.
func (i *Interner) Reset() {
	clear(i.tab)
}

// DecodeInterned decodes data into dst using a fresh per-call [Interner],
// deduplicating repeated strings within the blob so each distinct value
// costs one backing allocation instead of one per occurrence. data MUST
// include the 12-byte blob header.
//
// It is the interning counterpart to [DecodeInto]: dst.Reset() is called
// first, the header is read and validated (compressed bodies are inflated
// transparently), and the Reader's sticky error is returned. The interner
// is discarded when the call returns, so memory is bounded by the decoded
// graph — no string is retained across calls. Callers that want
// cross-blob deduplication construct one [Interner], install it via
// [Reader.SetAllocator] across decodes, and manage its [Interner.Reset]
// cadence themselves.
func DecodeInterned(data []byte, dst GSBMRoot) error {
	dst.Reset()
	r := NewReader(data)
	r.SetAllocator(NewInterner())
	if _, _, _, err := r.ReadHeader(); err != nil {
		return err
	}
	if err := dst.UnmarshalGSBM(r); err != nil {
		return err
	}
	return r.Err()
}
