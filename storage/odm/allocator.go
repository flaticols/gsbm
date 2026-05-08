package odm

// Allocator owns the strategy for materializing decoded slices, maps, and
// strings during UnmarshalODM. The default heap behavior is what the
// codegen would emit inline (`make`, `string([]byte)` copy); a custom
// implementation — most importantly the planned arena allocator — can
// substitute its own without any change to the generated code.
//
// Only AcquireString varies in v1. Slice/map allocation goes through the
// package-level generic helpers MakeSlice and MakeMap so an arena can
// later supply specialized variants of those calls without forcing the
// Allocator type itself to grow methods Go cannot express on an interface
// (Go has no generic interface methods).
type Allocator interface {
	// AcquireString returns a Go string view over b. The default heap
	// allocator returns string(b), which copies. An arena allocator can
	// return an unsafe.String pointing into the arena buffer; see Task 9.
	AcquireString(b []byte) string
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

// MakeSlice returns a slice of length n. The Reader is accepted so an
// arena variant in storage/odmarena can shadow this call site with a
// signature-compatible helper that allocates from arena memory. In heap
// mode this compiles down to make([]T, n).
func MakeSlice[T any](r *Reader, n int) []T {
	_ = r
	return make([]T, n)
}

// MakeMap returns a map sized for n entries. Heap mode delegates to make.
// Path A of the arena plan keeps maps on the heap, so even arena decoders
// continue to call this.
func MakeMap[K comparable, V any](r *Reader, n int) map[K]V {
	_ = r
	return make(map[K]V, n)
}
