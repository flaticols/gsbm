package gsbm_test

import (
	"reflect"
	"runtime/debug"
	"testing"

	"go.flaticols.dev/gsbm/storage/gsbm"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/sample"
)

// FuzzInternedDecodeMatchesHeap pins the interning invariant on arbitrary
// inputs: installing an Interner allocator changes only how decoded string
// bytes are stored, never the decoded values or the accept/reject
// decision. For any input, heap DecodeInto and DecodeInterned MUST agree on
// success-vs-error and, on success, produce deep-equal graphs. A
// divergence would mean interning corrupted a value or altered control
// flow — the core risk of routing every string through AcquireString.
func FuzzInternedDecodeMatchesHeap(f *testing.F) {
	f.Add(largeOrderBlob(f))
	f.Add([]byte("GSBM\x02\x00\x00\x00\x00\x00\x00\x00"))
	f.Add([]byte("GSBM\x02\x00\x00\x00\x02\x00\x00\x00\x08\x00"))
	{
		var zero sample.Order
		if gz, err := gsbm.MarshalWithOptions(&zero, 1, gsbm.Options{Compression: gsbm.CompressionGzip}); err == nil {
			f.Add(gz)
		}
		if zs, err := gsbm.MarshalWithOptions(&zero, 1, gsbm.Options{Compression: gsbm.CompressionZstd}); err == nil {
			f.Add(zs)
		}
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic on %x: %v\n%s", data, r, debug.Stack())
			}
		}()

		var heapDst sample.Order
		heapErr := gsbm.DecodeInto(data, &heapDst)

		var internDst sample.Order
		internErr := gsbm.DecodeInterned(data, &internDst)

		// Same accept/reject decision. (Errors need only agree on
		// presence — interning never changes WHY a blob is malformed.)
		if (heapErr == nil) != (internErr == nil) {
			t.Fatalf("decode outcome diverged on %x: heap=%v interned=%v", data, heapErr, internErr)
		}
		if heapErr != nil {
			return
		}
		if !reflect.DeepEqual(heapDst, internDst) {
			t.Fatalf("interned decode not deep-equal to heap decode on %x", data)
		}
	})
}
