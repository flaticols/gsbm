package odm_test

import (
	"testing"
	"unsafe"

	"github.com/flaticols/gsbm/storage/odm"
)

// TestHeapAcquireStringCopies asserts the default (heap) allocator path
// hands back a string whose data pointer differs from the source slice's.
// This is the contract that lets a Reader's source buffer be reused after
// decode.
func TestHeapAcquireStringCopies(t *testing.T) {
	src := []byte("hello")
	r := odm.NewReader(nil)
	got := r.AcquireString(src)
	if got != "hello" {
		t.Fatalf("AcquireString want %q got %q", "hello", got)
	}
	srcPtr := unsafe.Pointer(unsafe.SliceData(src))
	gotPtr := unsafe.StringData(got)
	if srcPtr == unsafe.Pointer(gotPtr) {
		t.Fatal("heap AcquireString returned an aliasing string; want a copy")
	}
}

// fakeArena is a stand-in for the future arena allocator. It returns
// strings via unsafe.String, aliasing the source bytes — proving the
// Allocator hook is honored end-to-end (Reader.AcquireString ⇒ Allocator).
type fakeArena struct{}

func (fakeArena) AcquireString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(&b[0], len(b))
}

func TestCustomAllocatorRouted(t *testing.T) {
	r := odm.NewReader(nil)
	r.SetAllocator(fakeArena{})
	src := []byte("aliased")
	got := r.AcquireString(src)
	if got != "aliased" {
		t.Fatalf("got %q", got)
	}
	if unsafe.Pointer(unsafe.StringData(got)) != unsafe.Pointer(unsafe.SliceData(src)) {
		t.Fatal("custom allocator was not used; string was copied")
	}
}

// TestMakeSliceMakeMap exercises the generic helpers in heap mode. The
// Reader is unused but accepted so an arena variant can shadow these
// calls with a same-shape function.
func TestMakeSliceMakeMap(t *testing.T) {
	r := odm.NewReader(nil)
	s := odm.MakeSlice[int64](r, 3)
	if len(s) != 3 || cap(s) < 3 {
		t.Fatalf("MakeSlice: len/cap = %d/%d", len(s), cap(s))
	}
	m := odm.MakeMap[string, int64](r, 4)
	if m == nil {
		t.Fatal("MakeMap returned nil")
	}
	m["k"] = 1
	if m["k"] != 1 {
		t.Fatal("MakeMap returned non-functional map")
	}
}

// TestAllocatorPreservedAcrossReset confirms that Reader.Reset(src) keeps
// the installed allocator — the pooled (Reader, Allocator) pair stays
// intact across decode calls.
func TestAllocatorPreservedAcrossReset(t *testing.T) {
	r := odm.NewReader(nil)
	r.SetAllocator(fakeArena{})
	r.Reset([]byte("y"))
	if r.Allocator() == nil {
		t.Fatal("allocator was cleared by Reset")
	}
}
