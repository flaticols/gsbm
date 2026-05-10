// Package gsbmarena is the arena-mode runtime for the gsbm tagged
// binary serializer.
//
// The wire format is identical to heap-mode (storage/gsbm); the difference
// is allocation: an Arena bump-allocates string bytes into chunked byte
// buffers and pools per-T slice allocations so a graph of N decoded
// objects costs ~O(distinct types × chunks) allocations instead of O(N).
// Arena decoders share storage/gsbm's generated UnmarshalGSBM bodies
// unchanged, routing through the SlicePoolStore + Allocator hooks the
// generated code already calls.
//
// # Lifetime contract
//
// Decoded objects are read-only by contract. Strings handed back by an
// arena decoder are unsafe.String views aliasing arena bytes, and slices
// of decoded structs are sub-slices of arena-pooled chunks. Both are
// invalidated by Arena.Release. Use-after-Release is not catchable at
// compile time — the v1 defenses are convention plus code review; static
// analysis is a follow-up tracked in the plan's Post-Completion notes.
//
// # Maps (Path A)
//
// Maps remain heap-allocated by storage/gsbm.MakeMap. Their keys/values
// may still alias arena bytes via unsafe.String, so a heap-allocated
// map's lifetime is also bounded by the Arena's. An arena-aware swiss
// table (Path B) is a follow-up optimization.
//
// # Mutation
//
// Mutating a decoded value in place is undefined behavior — the
// underlying memory is shared between callers in the worst case (a
// pooled chunk holds many decoded slices side-by-side). Code that needs
// to mutate calls Detach* (defined in companion *_gsbm_arena.go files) to
// produce a heap-allocated copy compatible with the heap-mode types.
package gsbmarena

import (
	"reflect"
	"unsafe"

	"go.flaticols.dev/gsbm/storage/gsbm"
)

// arenaChunkBytes is the starting capacity for a string-bytes chunk.
// Sized to absorb a typical offer payload's worth of decoded strings in
// a single allocation; oversized strings get their own chunk.
const arenaChunkBytes = 4 << 10 // 4 KiB

// Arena is a bump allocator for arena-mode decoding.
//
// An Arena holds three logical regions:
//
//   - Byte chunks for AcquireString, where decoded strings are copied
//     once per chunk and surfaced via unsafe.String views.
//   - Per-element-type slice pools (*gsbm.TypedPool[T]), keyed on
//     reflect.Type, populated lazily as the decoder calls
//     gsbm.MakeSlice[T].
//   - The single struct-allocation site (AllocStruct) reuses the slice
//     pool for T with n=1 so a *T view into a pooled chunk is GC-safe by
//     construction.
//
// An Arena is not safe for concurrent use. Pool one per goroutine, the
// same way storage/gsbm pools (Reader, Writer).
type Arena struct {
	bytesChunks [][]byte
	curBytes    []byte
	bytesUsed   int

	slicePools map[reflect.Type]any
}

// NewArena returns a fresh Arena. The zero value is also usable; the
// first allocation grows it on demand.
func NewArena() *Arena {
	return &Arena{}
}

// Release invalidates every reference handed out by the Arena.
//
// All decoded strings, slices, and structs become unsafe to read after
// Release returns. Backing memory is released to the GC by dropping
// internal references; subsequent Arena use re-initialises lazily.
//
// Release is idempotent.
func (a *Arena) Release() {
	a.bytesChunks = nil
	a.curBytes = nil
	a.bytesUsed = 0
	a.slicePools = nil
}

// AcquireString copies b into the arena's byte buffer and returns an
// unsafe.String view of the copied bytes. The view is valid until the
// next Release. An empty input yields the empty string with no
// allocation.
func (a *Arena) AcquireString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	dst := a.allocBytes(len(b))
	copy(dst, b)
	return unsafe.String(&dst[0], len(dst))
}

func (a *Arena) allocBytes(n int) []byte {
	if a.bytesUsed+n > len(a.curBytes) {
		c := max(arenaChunkBytes, n)
		a.curBytes = make([]byte, c)
		a.bytesChunks = append(a.bytesChunks, a.curBytes)
		a.bytesUsed = 0
	}
	out := a.curBytes[a.bytesUsed : a.bytesUsed+n : a.bytesUsed+n]
	a.bytesUsed += n
	return out
}

// SlicePools returns the per-element-type pool map. This satisfies
// gsbm.SlicePoolStore so gsbm.MakeSlice[T] can lazily look up (or create)
// a *gsbm.TypedPool[T] without any codegen change. The map is created
// lazily on first call.
func (a *Arena) SlicePools() map[reflect.Type]any {
	if a.slicePools == nil {
		a.slicePools = make(map[reflect.Type]any, 8)
	}
	return a.slicePools
}

// AllocSlice draws a slice of length n from the per-T arena pool.
// AllocSlice and gsbm.MakeSlice[T] share the same pool — the codegen
// already calls gsbm.MakeSlice, so AllocSlice is the convenience entry
// for hand-written code (e.g., the generated DecodeRoot wrappers).
func AllocSlice[T any](a *Arena, n int) []T {
	if n <= 0 {
		return nil
	}
	if a.slicePools == nil {
		a.slicePools = make(map[reflect.Type]any, 8)
	}
	rt := reflect.TypeFor[T]()
	p, ok := a.slicePools[rt]
	if !ok {
		p = &gsbm.TypedPool[T]{}
		a.slicePools[rt] = p
	}
	return p.(*gsbm.TypedPool[T]).Alloc(n)
}

// AllocStruct returns a *T pointing into the arena's per-T slice pool.
// The struct is zero-valued; code that needs different state must
// initialise the fields it cares about. The pointer is invalidated by
// Release.
func AllocStruct[T any](a *Arena) *T {
	s := AllocSlice[T](a, 1)
	return &s[0]
}
