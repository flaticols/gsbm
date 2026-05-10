package gsbm

import "reflect"

// Allocator owns the strategy for materializing decoded slices, maps, and
// strings during UnmarshalGSBM. The default heap behavior is what the
// codegen would emit inline (`make`, `string([]byte)` copy); a custom
// implementation — most importantly the arena allocator in
// storage/gsbmarena — can substitute its own without any change to the
// generated code.
//
// Only AcquireString lives on the interface in v1. Slice allocation
// routes through MakeSlice (a package-level generic) which checks whether
// the installed Allocator additionally implements SlicePoolStore. Map
// allocation stays on the heap (Path A of the arena plan), so MakeMap
// always delegates to make.
type Allocator interface {
	// AcquireString returns a Go string view over b. The default heap
	// allocator returns string(b), which copies. An arena allocator
	// returns an unsafe.String pointing into the arena's byte buffer.
	AcquireString(b []byte) string
}

// SlicePoolStore is implemented by allocators (e.g. *gsbmarena.Arena) that
// pool slice allocations per element type. It is queried at MakeSlice
// call sites via type assertion; allocators that do not implement it fall
// through to make.
//
// SlicePools returns the per-element-type pool map. MakeSlice[T] reads
// the entry for reflect.TypeFor[T]() — if missing it allocates a fresh
// *TypedPool[T] and stores it. The map MUST be non-nil; the implementer
// is expected to lazy-init in SlicePools.
//
// Generic interface methods do not exist in Go — that is why the surface
// hands back the raw map and the call site does the type-keyed lookup.
// This shape avoids the per-call closure allocation a "factory func()"
// surface would otherwise force on every MakeSlice invocation.
type SlicePoolStore interface {
	SlicePools() map[reflect.Type]any
}

// TypedPool is a bump-allocated pool of slices of T. The arena keeps one
// per concrete element type encountered during decode; subsequent
// MakeSlice[T] calls draw from the same pool, amortising allocations
// across all decoded slices of T regardless of the graph shape.
//
// Capacity-preserving Reset is intentionally absent: the arena owns the
// pool's lifetime and reclaims it via Release. Reuse across decode calls
// would require chunk truncation that risks aliasing live slices handed
// out to the previous decoder.
type TypedPool[T any] struct {
	chunks   [][]T
	cur      []T
	used     int
	chunkCap int
}

// defaultChunkCap is the starting size for a TypedPool chunk. Tuned for
// the v0 BDD payload's Item-like fanout: most slices in the offer graph
// sit comfortably under 256 elements, so one chunk per element type is
// the common case. Larger requests still allocate a custom-sized chunk.
const defaultChunkCap = 256

// Alloc returns a fresh sub-slice of length n. Repeated calls bump
// through the current chunk; when a request would overflow, a new chunk
// is allocated (sized to max(chunkCap, n)) and pinned in chunks so GC
// keeps it alive while the caller holds the returned sub-slice.
func (p *TypedPool[T]) Alloc(n int) []T {
	if n <= 0 {
		return nil
	}
	if p.chunkCap == 0 {
		p.chunkCap = defaultChunkCap
	}
	if p.used+n > len(p.cur) {
		c := max(p.chunkCap, n)
		p.cur = make([]T, c)
		p.chunks = append(p.chunks, p.cur)
		p.used = 0
	}
	s := p.cur[p.used : p.used+n : p.used+n]
	p.used += n
	return s
}

// heapAllocator is the default. AcquireString uses Go's standard
// []byte→string conversion, which always copies. Codegen emits no
// special call site for the heap path — Reader.AcquireString and
// MakeSlice/MakeMap inline back to make/string conversion.
type heapAllocator struct{}

func (heapAllocator) AcquireString(b []byte) string { return string(b) }

// DefaultAllocator is the heap allocator. NewReader installs it implicitly
// (as nil) and the Reader fast-paths around the indirection in that case.
var DefaultAllocator Allocator = heapAllocator{}

// maxSliceAllocBytes caps the total memory a single MakeSlice call may
// allocate up-front. Reader.ReadLength bounds n by remaining wire bytes,
// but in-memory size scales by sizeof(T): for varint elements (1-byte
// minimum on the wire) or struct elements (1-byte length-prefix minimum),
// a malformed blob can claim n == remaining_bytes while sizeof(T) is much
// larger. The cap rejects the decode rather than letting a modest input
// force a large allocation. Mirrors the size-hint cap on MakeMap.
const maxSliceAllocBytes = 1 << 24 // 16 MiB

// MakeSlice returns a slice of length n. If the Reader's allocator
// implements SlicePoolStore, the slice is drawn from the per-T pool; the
// heap path falls through to make([]T, n). Returns nil and records
// ErrAllocTooLarge on the Reader when n*sizeof(T) exceeds the budget.
func MakeSlice[T any](r *Reader, n int) []T {
	if n == 0 {
		return nil
	}
	rt := reflect.TypeFor[T]()
	// n is bounded by remaining bytes via Reader.ReadLength, but the
	// product n*sizeof(T) can in principle wrap uint64 on a pathological
	// (huge buffer × huge element type) combination. Pre-rejecting on n
	// alone — element size is always ≥1 byte — guarantees the multiplication
	// never reaches a value that could overflow.
	if uint64(n) > maxSliceAllocBytes || uint64(n)*uint64(rt.Size()) > maxSliceAllocBytes {
		if r != nil {
			r.setErr(ErrAllocTooLarge)
		}
		return nil
	}
	if r != nil {
		if sps, ok := r.alloc.(SlicePoolStore); ok {
			pools := sps.SlicePools()
			p, ok := pools[rt]
			if !ok {
				p = &TypedPool[T]{}
				pools[rt] = p
			}
			return p.(*TypedPool[T]).Alloc(n)
		}
	}
	return make([]T, n)
}

// MakeMap returns a map sized for n entries. Path A of the arena plan
// keeps maps on the heap, so this always delegates to make — including
// from arena decoders. The Reader is accepted to keep the call shape
// uniform with MakeSlice.
//
// The size hint is capped so a malicious blob can't trick `make(map, n)`
// into eagerly preallocating O(blob_size) buckets — n still flows from
// ReadLength which is bounded by remaining bytes, not by the cost of the
// resulting map. The map will grow on demand past the cap.
func MakeMap[K comparable, V any](r *Reader, n int) map[K]V {
	_ = r
	const sizeHintCap = 1 << 10
	if n > sizeHintCap {
		n = sizeHintCap
	}
	return make(map[K]V, n)
}
