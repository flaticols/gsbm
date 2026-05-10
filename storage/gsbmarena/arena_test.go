package gsbmarena_test

import (
	"reflect"
	"testing"
	"unsafe"

	"go.flaticols.dev/gsbm/storage/gsbm"
	"go.flaticols.dev/gsbm/storage/gsbmarena"
)

// TestAcquireStringDistinct asserts that two consecutive AcquireString
// calls land at distinct addresses (no overlap), so callers may hold
// both views simultaneously without one shadowing the other.
func TestAcquireStringDistinct(t *testing.T) {
	a := gsbmarena.NewArena()
	src := []byte("hello-arena")
	got := a.AcquireString(src)
	if got != "hello-arena" {
		t.Fatalf("AcquireString want %q got %q", "hello-arena", got)
	}
	a2 := a.AcquireString([]byte("more"))
	if unsafe.StringData(got) == unsafe.StringData(a2) {
		t.Fatal("two distinct strings share the same data pointer")
	}
}

// TestAcquireStringUsesArenaMemory asserts AcquireString returns a view
// pointing into the arena's chunk buffer, not a fresh heap allocation.
// This is the allocator hook that lets generated decoders skip per-
// string copies.
func TestAcquireStringUsesArenaMemory(t *testing.T) {
	a := gsbmarena.NewArena()
	first := a.AcquireString([]byte("aaaa"))
	second := a.AcquireString([]byte("bbbb"))
	// Adjacent acquisitions from the same chunk must land within one
	// chunk's stride of each other.
	d := uintptr(unsafe.Pointer(unsafe.StringData(second))) -
		uintptr(unsafe.Pointer(unsafe.StringData(first)))
	if d > uintptr(64<<10) {
		t.Fatalf("acquired strings not co-located in arena memory: stride=%d", d)
	}
}

// TestAcquireStringEmpty checks that empty inputs do not allocate from
// the arena and produce the canonical empty string.
func TestAcquireStringEmpty(t *testing.T) {
	a := gsbmarena.NewArena()
	if s := a.AcquireString(nil); s != "" {
		t.Fatalf("empty AcquireString got %q", s)
	}
}

// TestAcquireStringDoesNotAliasInput proves the arena copies the source
// bytes into its own buffer; the returned string survives mutation of
// the input slice.
func TestAcquireStringDoesNotAliasInput(t *testing.T) {
	a := gsbmarena.NewArena()
	src := []byte("first")
	got := a.AcquireString(src)
	src[0] = 'X'
	if got != "first" {
		t.Fatalf("string was aliased to source bytes: %q", got)
	}
}

// TestAllocSliceReusesPool decodes two slices of the same element type
// and asserts they live in the same chunk backing array — the contract
// that gives arena mode O(chunks) allocations rather than O(slices).
func TestAllocSliceReusesPool(t *testing.T) {
	a := gsbmarena.NewArena()
	s1 := gsbmarena.AllocSlice[int64](a, 10)
	s2 := gsbmarena.AllocSlice[int64](a, 10)
	if &s1[0] == &s2[0] {
		t.Fatal("two separate AllocSlice calls returned overlapping slices")
	}
	// They should be ~adjacent in memory if drawn from the same chunk.
	addr1 := uintptr(unsafe.Pointer(&s1[0]))
	addr2 := uintptr(unsafe.Pointer(&s2[0]))
	if addr1 > addr2 {
		addr1, addr2 = addr2, addr1
	}
	if addr2-addr1 > uintptr(64<<10) {
		t.Fatalf("two slices land far apart: %d bytes", addr2-addr1)
	}
}

// TestAllocSliceDistinctElementTypes confirms the per-T pool keying is
// honored — int64 and string slices must come from independent backing
// chunks so a string-pool grow doesn't corrupt int64 values.
func TestAllocSliceDistinctElementTypes(t *testing.T) {
	a := gsbmarena.NewArena()
	is := gsbmarena.AllocSlice[int64](a, 4)
	ss := gsbmarena.AllocSlice[string](a, 4)
	for i := range is {
		is[i] = int64(i)
	}
	for i := range ss {
		ss[i] = "x"
	}
	for i, v := range is {
		if v != int64(i) {
			t.Fatalf("int64 chunk corrupted: is[%d]=%d", i, v)
		}
	}
}

// TestSlicePoolStoreInterface asserts *Arena satisfies
// gsbm.SlicePoolStore. The MakeSlice routing depends on this assertion.
func TestSlicePoolStoreInterface(t *testing.T) {
	var _ gsbm.SlicePoolStore = (*gsbmarena.Arena)(nil)
	var _ gsbm.Allocator = (*gsbmarena.Arena)(nil)
}

// TestMakeSliceRoutesThroughArena drives the production code path: a
// Reader with an arena allocator hands MakeSlice through the arena's
// per-T pool. The acid test is alloc count: many MakeSlice calls
// produce far fewer allocations than make() per call would.
func TestMakeSliceRoutesThroughArena(t *testing.T) {
	a := gsbmarena.NewArena()
	r := gsbm.NewReader(nil)
	r.SetAllocator(a)
	// Drive 32 small slices of the same T into the pool.
	allocs := testingAllocs(func() {
		for range 32 {
			_ = gsbm.MakeSlice[int64](r, 4)
		}
	})
	// Expect at most a handful of underlying chunk allocations plus one
	// pool-init allocation. Order-of-magnitude assertion only.
	if allocs > 8 {
		t.Fatalf("MakeSlice did not pool: %d allocs for 32 slices", allocs)
	}
}

// TestRelease drops references to chunks. After Release the arena is
// reusable but previously-allocated values point at memory the GC can
// reclaim — caller's contract.
func TestRelease(t *testing.T) {
	a := gsbmarena.NewArena()
	_ = a.AcquireString([]byte("aaa"))
	_ = gsbmarena.AllocSlice[int64](a, 3)
	a.Release()
	// Idempotency.
	a.Release()
	// Reuse after Release.
	got := a.AcquireString([]byte("bbb"))
	if got != "bbb" {
		t.Fatalf("post-release AcquireString got %q", got)
	}
}

// TestSlicePoolsMapShared confirms the per-T pool is created exactly
// once per element type — repeated MakeSlice calls reuse the pool stored
// in the arena's SlicePools map.
func TestSlicePoolsMapShared(t *testing.T) {
	a := gsbmarena.NewArena()
	r := gsbm.NewReader(nil)
	r.SetAllocator(a)
	_ = gsbm.MakeSlice[int32](r, 1)
	_ = gsbm.MakeSlice[int32](r, 1)
	rt := reflect.TypeFor[int32]()
	pools := a.SlicePools()
	if _, ok := pools[rt]; !ok {
		t.Fatal("int32 pool not registered after MakeSlice[int32]")
	}
	if got := len(pools); got != 1 {
		t.Fatalf("want 1 pool keyed by int32, got %d entries", got)
	}
}

// testingAllocs returns the integer alloc count for one run of fn.
// "Did it pool?" checks tolerate the rounding inherent in the underlying
// AllocsPerRun call.
func testingAllocs(fn func()) int {
	return int(testing.AllocsPerRun(1, fn))
}
