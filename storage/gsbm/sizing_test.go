package gsbm

import "testing"

func TestSizeTag(t *testing.T) {
	tests := []struct {
		tag  uint32
		wt   WireType
		want int
	}{
		{1, WireVarint, 1},
		{15, WireLengthDelim, 1},                       // (15<<3)|2 = 122, fits 1 byte
		{16, WireLengthDelim, 2},                       // (16<<3)|2 = 130, needs 2 bytes
		{2047, WireFixed64, 2},                         // (2047<<3)|1 = 16377, fits 2 bytes
		{2048, WireFixed64, 3},                         // (2048<<3)|1 = 16385, needs 3 bytes
		{MaxTag, WireLengthDelim, 5},                   // (2^29-1)<<3 | 2 fits 5 bytes
	}
	for _, tt := range tests {
		got := SizeTag(tt.tag, tt.wt)
		if got != tt.want {
			t.Errorf("SizeTag(%d, %d) = %d, want %d", tt.tag, tt.wt, got, tt.want)
		}
	}
}

func TestSizeUvarint(t *testing.T) {
	tests := []struct {
		v    uint64
		want int
	}{
		{0, 1},
		{1, 1},
		{127, 1},
		{128, 2},
		{16383, 2},
		{16384, 3},
		{1 << 21, 4},
		{1 << 28, 5},
		{1 << 35, 6},
		{1 << 42, 7},
		{1 << 49, 8},
		{1 << 56, 9},
		{1 << 63, 10},
		{^uint64(0), 10},
	}
	for _, tt := range tests {
		got := SizeUvarint(tt.v)
		if got != tt.want {
			t.Errorf("SizeUvarint(%d) = %d, want %d", tt.v, got, tt.want)
		}
	}
}

func TestSizeVarint(t *testing.T) {
	tests := []struct {
		v    int64
		want int
	}{
		{0, 1},
		{-1, 1},
		{1, 1},
		{63, 1},
		{-64, 1},
		{64, 2},
		{-65, 2},
	}
	for _, tt := range tests {
		got := SizeVarint(tt.v)
		if got != tt.want {
			t.Errorf("SizeVarint(%d) = %d, want %d", tt.v, got, tt.want)
		}
	}
}

func TestSizeBool(t *testing.T)    { if SizeBool() != 1 { t.Fatal("SizeBool != 1") } }
func TestSizeFixed32(t *testing.T) { if SizeFixed32() != 4 { t.Fatal("SizeFixed32 != 4") } }
func TestSizeFixed64(t *testing.T) { if SizeFixed64() != 8 { t.Fatal("SizeFixed64 != 8") } }

func TestSizeString(t *testing.T) {
	tests := []struct {
		s    string
		want int
	}{
		{"", 1},                 // length-prefix 0, no payload
		{"a", 2},                // length-prefix 1 + 1 byte
		{"hello", 6},            // length-prefix 1 + 5 bytes
		{makeString(127), 128},  // length-prefix 1 + 127 bytes
		{makeString(128), 130},  // length-prefix 2 + 128 bytes
		{makeString(16383), 16385},
	}
	for _, tt := range tests {
		got := SizeString(tt.s)
		if got != tt.want {
			t.Errorf("SizeString(len=%d) = %d, want %d", len(tt.s), got, tt.want)
		}
	}
}

func TestSizeBytes(t *testing.T) {
	if got := SizeBytes(nil); got != 1 {
		t.Errorf("SizeBytes(nil) = %d, want 1", got)
	}
	if got := SizeBytes([]byte{}); got != 1 {
		t.Errorf("SizeBytes([]) = %d, want 1", got)
	}
	if got := SizeBytes([]byte("abc")); got != 4 {
		t.Errorf("SizeBytes(abc) = %d, want 4", got)
	}
}

func TestSizeLengthDelim(t *testing.T) {
	tests := []struct {
		bodyLen int
		want    int
	}{
		{0, 1},
		{1, 2},
		{127, 128},
		{128, 130},
		{16383, 16385},
		{16384, 16387},
	}
	for _, tt := range tests {
		got := SizeLengthDelim(tt.bodyLen)
		if got != tt.want {
			t.Errorf("SizeLengthDelim(%d) = %d, want %d", tt.bodyLen, got, tt.want)
		}
	}
}

// Cross-check helpers against the actual Writer: writing the value should
// produce exactly Size* bytes.
func TestSizeMatchesWriter(t *testing.T) {
	check := func(name string, want int, do func(*Writer)) {
		w := NewWriter(nil)
		do(w)
		if w.Err() != nil {
			t.Fatalf("%s: writer err %v", name, w.Err())
		}
		if got := len(w.Bytes()); got != want {
			t.Errorf("%s: writer produced %d bytes, Size* said %d", name, got, want)
		}
	}
	check("tag", SizeTag(15, WireLengthDelim), func(w *Writer) { w.WriteTag(15, WireLengthDelim) })
	check("tag-2byte", SizeTag(16, WireLengthDelim), func(w *Writer) { w.WriteTag(16, WireLengthDelim) })
	check("uvarint-0", SizeUvarint(0), func(w *Writer) { w.WriteUvarint(0) })
	check("uvarint-127", SizeUvarint(127), func(w *Writer) { w.WriteUvarint(127) })
	check("uvarint-128", SizeUvarint(128), func(w *Writer) { w.WriteUvarint(128) })
	check("varint-0", SizeVarint(0), func(w *Writer) { w.WriteVarint(0) })
	check("varint-neg", SizeVarint(-100), func(w *Writer) { w.WriteVarint(-100) })
	check("bool", SizeBool(), func(w *Writer) { w.WriteBool(true) })
	check("fixed32", SizeFixed32(), func(w *Writer) { w.WriteFixed32(42) })
	check("fixed64", SizeFixed64(), func(w *Writer) { w.WriteFixed64(42) })
	check("string", SizeString("hello"), func(w *Writer) { w.WriteString("hello") })
	check("bytes", SizeBytes([]byte("xyz")), func(w *Writer) { w.WriteBytes([]byte("xyz")) })
}

func TestSizeNullable(t *testing.T) {
	// SizeNullable* helpers must agree with SizeLengthDelim(1) for the
	// elided cases and with SizeLengthDelim(1 + SizeValue) otherwise.
	t.Run("string", func(t *testing.T) {
		if got := SizeNullableString(nil); got != SizeLengthDelim(1) {
			t.Errorf("nil: got %d", got)
		}
		empty := ""
		if got := SizeNullableString(&empty); got != SizeLengthDelim(1) {
			t.Errorf("empty: got %d", got)
		}
		s := "hello"
		if got, want := SizeNullableString(&s), SizeLengthDelim(1+SizeString(s)); got != want {
			t.Errorf("hello: got %d want %d", got, want)
		}
	})
	t.Run("bytes", func(t *testing.T) {
		if got := SizeNullableBytes(nil); got != SizeLengthDelim(1) {
			t.Errorf("nil: got %d", got)
		}
		empty := []byte{}
		if got := SizeNullableBytes(&empty); got != SizeLengthDelim(1) {
			t.Errorf("empty: got %d", got)
		}
		b := []byte("xyz")
		if got, want := SizeNullableBytes(&b), SizeLengthDelim(1+SizeBytes(b)); got != want {
			t.Errorf("xyz: got %d want %d", got, want)
		}
	})
	t.Run("bool", func(t *testing.T) {
		if got := SizeNullableBool(nil); got != SizeLengthDelim(1) {
			t.Errorf("nil: got %d", got)
		}
		f := false
		if got := SizeNullableBool(&f); got != SizeLengthDelim(1) {
			t.Errorf("false: got %d", got)
		}
		tr := true
		if got, want := SizeNullableBool(&tr), SizeLengthDelim(1+SizeBool()); got != want {
			t.Errorf("true: got %d want %d", got, want)
		}
	})
	t.Run("int64", func(t *testing.T) {
		var z int64
		if got := SizeNullableInt64(nil); got != SizeLengthDelim(1) {
			t.Errorf("nil: got %d", got)
		}
		if got := SizeNullableInt64(&z); got != SizeLengthDelim(1) {
			t.Errorf("zero: got %d", got)
		}
		v := int64(-42)
		if got, want := SizeNullableInt64(&v), SizeLengthDelim(1+SizeVarint(v)); got != want {
			t.Errorf("-42: got %d want %d", got, want)
		}
	})
	t.Run("uint64", func(t *testing.T) {
		var z uint64
		if got := SizeNullableUint64(nil); got != SizeLengthDelim(1) {
			t.Errorf("nil: got %d", got)
		}
		if got := SizeNullableUint64(&z); got != SizeLengthDelim(1) {
			t.Errorf("zero: got %d", got)
		}
		v := uint64(1 << 20)
		if got, want := SizeNullableUint64(&v), SizeLengthDelim(1+SizeUvarint(v)); got != want {
			t.Errorf("big: got %d want %d", got, want)
		}
	})
	t.Run("float32", func(t *testing.T) {
		if got := SizeNullableFloat32(nil); got != SizeLengthDelim(1) {
			t.Errorf("nil: got %d", got)
		}
		var z float32
		if got := SizeNullableFloat32(&z); got != SizeLengthDelim(1) {
			t.Errorf("zero: got %d", got)
		}
		v := float32(1.5)
		if got, want := SizeNullableFloat32(&v), SizeLengthDelim(1+SizeFixed32()); got != want {
			t.Errorf("1.5: got %d want %d", got, want)
		}
	})
	t.Run("float64", func(t *testing.T) {
		if got := SizeNullableFloat64(nil); got != SizeLengthDelim(1) {
			t.Errorf("nil: got %d", got)
		}
		var z float64
		if got := SizeNullableFloat64(&z); got != SizeLengthDelim(1) {
			t.Errorf("zero: got %d", got)
		}
		v := 3.14
		if got, want := SizeNullableFloat64(&v), SizeLengthDelim(1+SizeFixed64()); got != want {
			t.Errorf("pi: got %d want %d", got, want)
		}
	})
}

func makeString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}
