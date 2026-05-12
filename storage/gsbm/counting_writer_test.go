package gsbm

import (
	"math"
	"testing"
)

// Each test compares a size-mode Writer against an equivalent real-mode
// Writer running the same sequence of Write* calls. The size-mode
// accumulator MUST always equal len(real.Bytes()) once both have
// processed identical operations — this is the property that lets
// hand-written MarshalGSBM callers compute exact size via
// NewCountingWriter without diverging from the real encode path.

func TestCountingWriterZeroValue(t *testing.T) {
	w := NewCountingWriter()
	if w.Size() != 0 {
		t.Fatalf("fresh CountingWriter Size=%d, want 0", w.Size())
	}
	if w.Bytes() != nil {
		t.Fatalf("fresh CountingWriter Bytes()=%v, want nil", w.Bytes())
	}
	if w.Err() != nil {
		t.Fatalf("fresh CountingWriter Err=%v, want nil", w.Err())
	}
}

func TestCountingWriterWriteHeader(t *testing.T) {
	w := NewCountingWriter()
	w.WriteHeader(0, 0x1234, 0)
	if w.Size() != HeaderSize {
		t.Fatalf("Size after WriteHeader = %d, want %d", w.Size(), HeaderSize)
	}
	if w.Bytes() != nil {
		t.Fatalf("size-mode Bytes() should remain nil, got %d bytes", len(w.Bytes()))
	}
}

// TestCountingWriterMatchesRealMode is the per-primitive size equivalence
// table. It plays back each scenario into both a real Writer and a
// size-mode Writer, then asserts they report the same byte count.
func TestCountingWriterMatchesRealMode(t *testing.T) {
	cases := []struct {
		name string
		play func(w *Writer)
	}{
		{"tag/small", func(w *Writer) { w.WriteTag(1, WireVarint) }},
		{"tag/2-byte", func(w *Writer) { w.WriteTag(16, WireLengthDelim) }},
		{"tag/max", func(w *Writer) { w.WriteTag(MaxTag, WireLengthDelim) }},
		{"uvarint/0", func(w *Writer) { w.WriteUvarint(0) }},
		{"uvarint/127", func(w *Writer) { w.WriteUvarint(127) }},
		{"uvarint/128", func(w *Writer) { w.WriteUvarint(128) }},
		{"uvarint/16383", func(w *Writer) { w.WriteUvarint(16383) }},
		{"uvarint/16384", func(w *Writer) { w.WriteUvarint(16384) }},
		{"uvarint/max", func(w *Writer) { w.WriteUvarint(math.MaxUint64) }},
		{"varint/neg", func(w *Writer) { w.WriteVarint(-1) }},
		{"varint/pos", func(w *Writer) { w.WriteVarint(63) }},
		{"varint/boundary", func(w *Writer) { w.WriteVarint(64) }},
		{"bool/true", func(w *Writer) { w.WriteBool(true) }},
		{"bool/false", func(w *Writer) { w.WriteBool(false) }},
		{"fixed32", func(w *Writer) { w.WriteFixed32(0xdeadbeef) }},
		{"fixed64", func(w *Writer) { w.WriteFixed64(0xdeadbeefcafebabe) }},
		{"float32", func(w *Writer) { w.WriteFloat32(3.14) }},
		{"float64", func(w *Writer) { w.WriteFloat64(2.718281828) }},
		{"string/empty", func(w *Writer) { w.WriteString("") }},
		{"string/short", func(w *Writer) { w.WriteString("hello") }},
		{"string/127", func(w *Writer) { w.WriteString(strRepeat('x', 127)) }},
		{"string/128", func(w *Writer) { w.WriteString(strRepeat('x', 128)) }},
		{"bytes/empty", func(w *Writer) { w.WriteBytes(nil) }},
		{"bytes/some", func(w *Writer) { w.WriteBytes([]byte{1, 2, 3, 4}) }},
		{"length-delim/empty", func(w *Writer) {
			m := w.BeginLengthDelim()
			w.EndLengthDelim(m)
		}},
		{"length-delim/short-body", func(w *Writer) {
			m := w.BeginLengthDelim()
			w.WriteString("hi")
			w.EndLengthDelim(m)
		}},
		{"length-delim/127-body", func(w *Writer) {
			m := w.BeginLengthDelim()
			w.WriteBytes(make([]byte, 125)) // 1-byte length prefix + 125 = 126; total body = 127
			w.EndLengthDelim(m)
		}},
		{"length-delim/128-body", func(w *Writer) {
			m := w.BeginLengthDelim()
			w.WriteBytes(make([]byte, 126)) // 1-byte length prefix + 126 = 127; total body grows length prefix
			w.EndLengthDelim(m)
		}},
		{"length-delim/nested", func(w *Writer) {
			outer := w.BeginLengthDelim()
			w.WriteTag(1, WireLengthDelim)
			inner := w.BeginLengthDelim()
			w.WriteString("inner")
			w.EndLengthDelim(inner)
			w.WriteTag(2, WireVarint)
			w.WriteVarint(42)
			w.EndLengthDelim(outer)
		}},
		{"mixed-sequence", func(w *Writer) {
			w.WriteHeader(0, 0xabcd, 0)
			w.WriteTag(1, WireVarint)
			w.WriteVarint(7)
			w.WriteTag(2, WireLengthDelim)
			w.WriteString("hello world")
			w.WriteTag(3, WireFixed64)
			w.WriteFloat64(1.5)
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			real := NewWriter(nil)
			tc.play(real)
			if real.Err() != nil {
				t.Fatalf("real-mode err: %v", real.Err())
			}

			size := NewCountingWriter()
			tc.play(size)
			if size.Err() != nil {
				t.Fatalf("size-mode err: %v", size.Err())
			}

			if got, want := size.Size(), len(real.Bytes()); got != want {
				t.Errorf("size-mode Size=%d, real-mode len(Bytes)=%d", got, want)
			}
			if size.Bytes() != nil {
				t.Errorf("size-mode Bytes() leaked a non-nil slice")
			}
		})
	}
}

func TestCountingWriterResetClearsAcc(t *testing.T) {
	w := NewCountingWriter()
	w.WriteVarint(123)
	if w.Size() == 0 {
		t.Fatal("expected nonzero size before reset")
	}
	w.Reset(nil)
	if w.Size() != 0 {
		t.Fatalf("Size after Reset = %d, want 0", w.Size())
	}
	if !w.sizeOnly {
		t.Fatal("Reset must not clear size-mode flag")
	}
}

func TestCountingWriterFinalizeBodyLenNoOp(t *testing.T) {
	w := NewCountingWriter()
	w.WriteHeader(0, 0, 0)
	w.WriteVarint(1)
	before := w.Size()
	w.FinalizeBodyLen() // should be a no-op in size-mode
	if w.Size() != before {
		t.Fatalf("FinalizeBodyLen changed Size: %d → %d", before, w.Size())
	}
}

func TestCountingWriterStickyError(t *testing.T) {
	w := NewCountingWriter()
	w.WriteTag(0, WireVarint) // tag 0 is reserved → sets error
	if w.Err() == nil {
		t.Fatal("expected error after tag=0")
	}
	before := w.Size()
	w.WriteVarint(42)
	if w.Size() != before {
		t.Fatalf("Size advanced after sticky error: %d → %d", before, w.Size())
	}
}

func strRepeat(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
