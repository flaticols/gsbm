package gsbm_test

import (
	"bytes"
	"errors"
	"runtime/debug"
	"testing"

	"go.flaticols.dev/gsbm/internal/bench"
	"go.flaticols.dev/gsbm/storage/gsbm"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/sample"
)

// documentedSentinels is the closed set of error values the gsbm runtime
// wraps around every malformed-input path. The fuzz harnesses assert any
// surfaced error chains back to one of these via errors.Is — a raw
// errors.New string deep in the call stack is a regression because
// callers cannot inspect it.
var documentedSentinels = []error{
	gsbm.ErrBadMagic,
	gsbm.ErrUnsupportedVer,
	gsbm.ErrReservedFlags,
	gsbm.ErrTruncated,
	gsbm.ErrTrailingBytes,
	gsbm.ErrVarintOverflow,
	gsbm.ErrZeroTag,
	gsbm.ErrReservedWire,
	gsbm.ErrWrongWireType,
	gsbm.ErrTagOverflow,
	gsbm.ErrInvalidMapKey,
	gsbm.ErrInvalidPresence,
	gsbm.ErrBodyTooLarge,
	gsbm.ErrIntegerOverflow,
	gsbm.ErrAllocTooLarge,
}

func isDocumentedSentinel(err error) bool {
	for _, s := range documentedSentinels {
		if errors.Is(err, s) {
			return true
		}
	}
	return false
}

// largeOrderBlob produces a stable 1-2 MiB wire blob (header + body) used
// as a fuzz seed. Failing the test if generation fails keeps seed wiring
// reproducible across machines.
func largeOrderBlob(tb testing.TB) []byte {
	tb.Helper()
	o := bench.MakeLargeOrder(0, 1<<20, 2<<20)
	w := gsbm.NewWriter(nil)
	w.WriteHeader(0, 1)
	if err := o.MarshalGSBM(w); err != nil {
		tb.Fatalf("MarshalGSBM: %v", err)
	}
	if err := w.Err(); err != nil {
		tb.Fatalf("writer err: %v", err)
	}
	return w.Bytes()
}

// FuzzReaderRobustness drives arbitrary bytes through the decoder and
// asserts: (1) no panic escapes from the Reader; (2) any returned error
// chains to a documented gsbm.Err* sentinel via errors.Is; and (3) the
// raw-Reader walk makes forward progress (every successful ReadTag +
// SkipField pair consumes at least one byte) so a malformed blob cannot
// trap the decoder in an infinite loop.
func FuzzReaderRobustness(f *testing.F) {
	f.Add(largeOrderBlob(f))
	f.Add([]byte("GSBM\x01\x00\x00\x00")) // valid header, empty body
	f.Add([]byte{})
	// Single-tag varint after a valid header (tag 1, WireVarint, value 0).
	f.Add([]byte("GSBM\x01\x00\x00\x00\x08\x00"))
	// A blob with bytes that would decode if the schema permitted them.
	f.Add([]byte("GSBM\x01\x00\x00\x00\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff"))

	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic on %x: %v\n%s", data, r, debug.Stack())
			}
		}()

		// Path 1: full DecodeInto exercise — the production hot path.
		// Drain the presence sidecar after each iteration: a successful
		// decode of a 1-2 MiB Order creates entries for the root *plus*
		// every nested *Customer / *Item / *Tag receiver (their
		// generated UnmarshalGSBM calls MarkPresent). ForgetPresence on
		// the root would leak those nested entries across millions of
		// fuzz execs and grow memory unboundedly; ResetPresenceStore
		// evicts the lot.
		var dst sample.Order
		err := gsbm.DecodeInto(data, &dst)
		gsbm.ResetPresenceStore()
		if err != nil && !isDocumentedSentinel(err) {
			t.Fatalf("DecodeInto returned non-sentinel error on %x: %v", data, err)
		}

		// Path 2: raw Reader walk — exercises the primitives directly so a
		// regression in ReadTag / SkipField surfaces independent of codegen.
		r := gsbm.NewReader(data)
		_, _, herr := r.ReadHeader()
		if herr != nil {
			if !isDocumentedSentinel(herr) {
				t.Fatalf("ReadHeader returned non-sentinel error on %x: %v", data, herr)
			}
			return
		}
		for r.HasMore() {
			pos := r.Pos()
			_, wt, terr := r.ReadTag()
			if terr != nil {
				if !isDocumentedSentinel(terr) {
					t.Fatalf("ReadTag returned non-sentinel error on %x: %v", data, terr)
				}
				return
			}
			if serr := r.SkipField(wt); serr != nil {
				if !isDocumentedSentinel(serr) {
					t.Fatalf("SkipField returned non-sentinel error on %x: %v", data, serr)
				}
				return
			}
			if r.Pos() <= pos {
				t.Fatalf("forward-progress violation on %x: pos %d -> %d", data, pos, r.Pos())
			}
		}
		if e := r.Err(); e != nil && !isDocumentedSentinel(e) {
			t.Fatalf("Reader sticky err non-sentinel on %x: %v", data, e)
		}
	})
}

// FuzzWriterReaderRoundTripCanonical validates the canonical-form
// invariant: re-encoding a decoded value yields the same bytes as the
// canonical wire form. We assert this idempotently — given input D, we
// compute B1 = encode(decode(D)); then B2 = encode(decode(B1)); and
// require B1 == B2. The idempotent form is what the spec guarantees:
// the original D may be non-canonical (unsorted map keys per spec §5.3,
// non-minimum varints per spec §4.1, last-wins duplicates per spec §3.3,
// or unknown tags forward-skipped via SkipField), so a strict D == B1
// check would flag known design choices rather than real bugs. The
// idempotent check still catches an encoder that drifts away from
// canonical form on every pass.
func FuzzWriterReaderRoundTripCanonical(f *testing.F) {
	f.Add(largeOrderBlob(f))
	f.Add([]byte("GSBM\x01\x00\x00\x00"))
	f.Add([]byte("GSBM\x01\x00\x00\x00\x08\x00"))

	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic on %x: %v\n%s", data, r, debug.Stack())
			}
		}()

		// Drain the presence sidecar at the end so nested receivers
		// (Customer, Item[i], etc.) don't accumulate across long fuzz
		// runs — see FuzzReaderRobustness for the rationale.
		defer gsbm.ResetPresenceStore()

		var first sample.Order
		r := gsbm.NewReader(data)
		flags, schemaHint, err := r.ReadHeader()
		if err != nil {
			return
		}
		if err := first.UnmarshalGSBM(r); err != nil {
			return
		}
		if err := r.Err(); err != nil {
			return
		}

		w1 := gsbm.NewWriter(nil)
		w1.WriteHeader(flags, schemaHint)
		if err := first.MarshalGSBM(w1); err != nil {
			t.Fatalf("first re-encode failed on %x: %v", data, err)
		}
		if err := w1.Err(); err != nil {
			t.Fatalf("first writer err on %x: %v", data, err)
		}
		canonical := append([]byte(nil), w1.Bytes()...)

		// Decode the canonical form and re-encode; the second pass must
		// reproduce `canonical` byte-for-byte. If it does not, the encoder
		// is failing to converge on a single canonical representation.
		var second sample.Order
		r2 := gsbm.NewReader(canonical)
		flags2, schemaHint2, err := r2.ReadHeader()
		if err != nil {
			t.Fatalf("ReadHeader on canonical re-encode failed: %v", err)
		}
		if flags2 != flags || schemaHint2 != schemaHint {
			t.Fatalf("header drift: flags %#x->%#x schemaHint %#x->%#x",
				flags, flags2, schemaHint, schemaHint2)
		}
		if err := second.UnmarshalGSBM(r2); err != nil {
			t.Fatalf("UnmarshalGSBM on canonical re-encode failed: %v", err)
		}
		if err := r2.Err(); err != nil {
			t.Fatalf("Reader err on canonical re-encode: %v", err)
		}

		w2 := gsbm.NewWriter(nil)
		w2.WriteHeader(flags2, schemaHint2)
		if err := second.MarshalGSBM(w2); err != nil {
			t.Fatalf("second re-encode failed: %v", err)
		}
		if err := w2.Err(); err != nil {
			t.Fatalf("second writer err: %v", err)
		}
		if !bytes.Equal(w2.Bytes(), canonical) {
			t.Fatalf("non-canonical re-encode (encoder did not converge):\n  pass1: %x\n  pass2: %x", canonical, w2.Bytes())
		}
	})
}

// FuzzHeaderCorruption seeds with a valid 1-2 MiB blob and lets the
// fuzzer replace the 8-byte header. The body is left untouched so the
// failure mode is isolated to header validation: any deviation from the
// magic / fmtVer / flags rules must surface the matching sentinel, never
// a panic, and never decode successfully.
func FuzzHeaderCorruption(f *testing.F) {
	valid := largeOrderBlob(f)

	f.Add([]byte("GSBM\x01\x00\x00\x00"))     // canonical header
	f.Add([]byte("XXXX\x01\x00\x00\x00"))     // bad magic
	f.Add([]byte("GSBM\x09\x00\x00\x00"))     // bad fmtVer
	f.Add([]byte("GSBM\x01\x01\x00\x00"))     // reserved flag bit set
	f.Add([]byte("GSBM\x01\x00\x34\x12"))     // schemaHint variant (still valid)
	f.Add([]byte("GSBM\x01\x80\xff\xff"))     // both flags+schemaHint mutated
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0})     // all-zero header

	f.Fuzz(func(t *testing.T, header []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic on header %x: %v\n%s", header, r, debug.Stack())
			}
		}()
		if len(header) < 8 {
			return
		}
		header = header[:8]

		blob := make([]byte, len(valid))
		copy(blob, valid)
		copy(blob[:8], header)

		var dst sample.Order
		err := gsbm.DecodeInto(blob, &dst)
		// Drain nested presence entries too; see FuzzReaderRobustness.
		gsbm.ResetPresenceStore()

		magicOK := bytes.Equal(header[:4], []byte(gsbm.Magic))
		verOK := header[4] == gsbm.FmtVer1
		flagsOK := header[5] == 0

		switch {
		case !magicOK:
			if !errors.Is(err, gsbm.ErrBadMagic) {
				t.Fatalf("header %x: want ErrBadMagic, got %v", header, err)
			}
		case !verOK:
			if !errors.Is(err, gsbm.ErrUnsupportedVer) {
				t.Fatalf("header %x: want ErrUnsupportedVer, got %v", header, err)
			}
		case !flagsOK:
			if !errors.Is(err, gsbm.ErrReservedFlags) {
				t.Fatalf("header %x: want ErrReservedFlags, got %v", header, err)
			}
		default:
			// Magic + fmtVer + flags all valid; header[6:8] is schemaHint
			// and is informational. Body is unchanged from the seed, so
			// decode must succeed.
			if err != nil {
				t.Fatalf("valid header %x with intact body decoded with err %v", header, err)
			}
		}
	})
}
