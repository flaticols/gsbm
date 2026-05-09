package gsbm

import (
	"bytes"
	"errors"
	"math"
	"testing"
)

// TestHeaderRoundTrip exercises §2.1: magic, fmtVer, flags=0 (the only
// valid flag value for fmtVer 1), schemaHint, all little-endian. The
// expected bytes below are the wire-format invariant: the schVer→schemaHint
// rename MUST NOT change them (no fmtVer bump, no layout change).
func TestHeaderRoundTrip(t *testing.T) {
	w := NewWriter(nil)
	w.WriteHeader(0x00, 0x1234)
	if w.Err() != nil {
		t.Fatal(w.Err())
	}
	got := w.Bytes()
	want := []byte{'G', 'S', 'B', 'M', 1, 0x00, 0x34, 0x12}
	if !bytes.Equal(got, want) {
		t.Fatalf("header bytes: got %x want %x", got, want)
	}

	r := NewReader(got)
	flags, schemaHint, err := r.ReadHeader()
	if err != nil {
		t.Fatal(err)
	}
	if flags != 0x00 || schemaHint != 0x1234 {
		t.Fatalf("flags=%x schemaHint=%x", flags, schemaHint)
	}
	if r.HasMore() {
		t.Fatal("HasMore after header on 8-byte blob")
	}
}

func TestHeaderBadMagic(t *testing.T) {
	bad := []byte{'X', 'X', 'X', 'X', 1, 0, 0, 0}
	if _, _, err := NewReader(bad).ReadHeader(); !errors.Is(err, ErrBadMagic) {
		t.Fatalf("want ErrBadMagic, got %v", err)
	}
}

func TestHeaderUnsupportedVer(t *testing.T) {
	bad := []byte{'G', 'S', 'B', 'M', 9, 0, 0, 0}
	if _, _, err := NewReader(bad).ReadHeader(); !errors.Is(err, ErrUnsupportedVer) {
		t.Fatalf("want ErrUnsupportedVer, got %v", err)
	}
}

func TestHeaderTruncated(t *testing.T) {
	if _, _, err := NewReader([]byte{'G', 'S', 'B'}).ReadHeader(); !errors.Is(err, ErrTruncated) {
		t.Fatalf("want ErrTruncated, got %v", err)
	}
}

// TestHeaderReservedFlags checks that fmtVer 1 rejects any non-zero flag
// bit. fmtVer 1 defines no flag semantics; a future compression marker would
// silently corrupt decoding if old readers ignored the bit.
func TestHeaderReservedFlags(t *testing.T) {
	for _, flags := range []byte{0x01, 0x02, 0x80, 0xFF} {
		bad := []byte{'G', 'S', 'B', 'M', 1, flags, 0, 0}
		if _, _, err := NewReader(bad).ReadHeader(); !errors.Is(err, ErrReservedFlags) {
			t.Fatalf("flags=%#x: want ErrReservedFlags, got %v", flags, err)
		}
	}
}

// TestPrimitiveRoundTrip verifies every primitive encoder against the
// matching decoder for representative values.
func TestPrimitiveRoundTrip(t *testing.T) {
	w := NewWriter(nil)
	w.WriteUvarint(0)
	w.WriteUvarint(123456789)
	w.WriteVarint(-1)
	w.WriteVarint(1<<62 - 1)
	w.WriteBool(true)
	w.WriteBool(false)
	w.WriteFixed32(0xDEADBEEF)
	w.WriteFixed64(0xCAFEBABEFACEFEED)
	w.WriteFloat32(float32(math.Pi))
	w.WriteFloat64(math.E)
	w.WriteFloat32(float32(math.Inf(-1)))
	w.WriteFloat64(math.NaN())
	w.WriteString("héllo, ödm 🌍")
	w.WriteBytes([]byte{0xFF, 0x00, 0x42})
	w.WriteString("") // zero-length
	w.WriteBytes(nil) // zero-length
	if w.Err() != nil {
		t.Fatal(w.Err())
	}

	r := NewReader(w.Bytes())
	check := func(name string, ok bool, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("primitive %s read error: %v", name, err)
		}
		if !ok {
			t.Fatalf("primitive %s mismatch (pos=%d)", name, r.Pos())
		}
	}
	uv, err := r.ReadUvarint()
	check("uvar0", uv == 0, err)
	uv, err = r.ReadUvarint()
	check("uvar1", uv == 123456789, err)
	sv, err := r.ReadVarint()
	check("var-1", sv == -1, err)
	sv, err = r.ReadVarint()
	check("varBig", sv == 1<<62-1, err)
	bv, err := r.ReadBool()
	check("bool true", bv, err)
	bv, err = r.ReadBool()
	check("bool false", !bv, err)
	f32u, err := r.ReadFixed32()
	check("fix32", f32u == 0xDEADBEEF, err)
	f64u, err := r.ReadFixed64()
	check("fix64", f64u == 0xCAFEBABEFACEFEED, err)
	f32, err := r.ReadFloat32()
	check("f32 pi", f32 == float32(math.Pi), err)
	f64, err := r.ReadFloat64()
	check("f64 e", f64 == math.E, err)
	f32, err = r.ReadFloat32()
	check("f32 -inf", math.IsInf(float64(f32), -1), err)
	f64, err = r.ReadFloat64()
	check("f64 NaN bits", math.Float64bits(f64) == math.Float64bits(math.NaN()), err)
	s, err := r.ReadString()
	check("string utf8", s == "héllo, ödm 🌍", err)
	b, err := r.ReadBytes()
	check("bytes", bytes.Equal(b, []byte{0xFF, 0x00, 0x42}), err)
	s, err = r.ReadString()
	check("empty string", s == "", err)
	b, err = r.ReadBytes()
	check("empty bytes", len(b) == 0, err)
	if r.Err() != nil {
		t.Fatalf("reader sticky error after round-trip: %v", r.Err())
	}
	if r.HasMore() {
		t.Fatal("trailing bytes after primitive round-trip")
	}
}

// TestTagRoundTrip exercises §3.1 / §3.2 — field key encoding and rejection
// of tag 0 and reserved wire types.
func TestTagRoundTrip(t *testing.T) {
	cases := []struct {
		tag uint32
		wt  WireType
	}{
		{1, WireVarint},
		{15, WireLengthDelim}, // single-byte key boundary
		{16, WireFixed32},     // first 2-byte key
		{2047, WireFixed64},
		{MaxTag, WireVarint},
	}
	for _, c := range cases {
		w := NewWriter(nil)
		w.WriteTag(c.tag, c.wt)
		if w.Err() != nil {
			t.Fatal(w.Err())
		}
		r := NewReader(w.Bytes())
		tag, wt, err := r.ReadTag()
		if err != nil {
			t.Fatalf("ReadTag(%d,%d): %v", c.tag, c.wt, err)
		}
		if tag != c.tag || wt != c.wt {
			t.Fatalf("got (%d,%d) want (%d,%d)", tag, wt, c.tag, c.wt)
		}
	}
}

func TestTagZeroRejected(t *testing.T) {
	w := NewWriter(nil)
	w.WriteTag(0, WireVarint)
	if !errors.Is(w.Err(), ErrZeroTag) {
		t.Fatalf("encoder: want ErrZeroTag, got %v", w.Err())
	}
	// Decoder side: hand-craft a key with tag=0 (key=0).
	r := NewReader([]byte{0x00})
	if _, _, err := r.ReadTag(); !errors.Is(err, ErrZeroTag) {
		t.Fatalf("decoder: want ErrZeroTag, got %v", err)
	}
}

func TestTagOverflow(t *testing.T) {
	w := NewWriter(nil)
	w.WriteTag(MaxTag+1, WireVarint)
	if !errors.Is(w.Err(), ErrTagOverflow) {
		t.Fatalf("want ErrTagOverflow, got %v", w.Err())
	}
}

// TestTagOverflowDecoder forges a varint key whose tag bits exceed MaxTag.
// The decoder MUST surface ErrTagOverflow rather than silently truncate
// the tag down to 29 bits.
func TestTagOverflowDecoder(t *testing.T) {
	// Hand-encode a varint key = ((MaxTag+1) << 3) | WireVarint.
	key := (uint64(MaxTag) + 1) << 3
	buf := appendUvarint(nil, key)
	r := NewReader(buf)
	if _, _, err := r.ReadTag(); !errors.Is(err, ErrTagOverflow) {
		t.Fatalf("decoder: want ErrTagOverflow, got %v", err)
	}
}

func TestReservedWireTypeRejected(t *testing.T) {
	// Encoder: refuse to write a reserved wire type.
	w := NewWriter(nil)
	w.WriteTag(1, WireType(4))
	if !errors.Is(w.Err(), ErrReservedWire) {
		t.Fatalf("encoder: want ErrReservedWire, got %v", w.Err())
	}
	// Decoder: a key with wire type 4 must be rejected as malformed.
	// key = (1<<3)|4 = 12 = 0x0C
	r := NewReader([]byte{0x0C})
	if _, _, err := r.ReadTag(); !errors.Is(err, ErrReservedWire) {
		t.Fatalf("decoder: want ErrReservedWire, got %v", err)
	}
}

// TestNestedStructFraming covers §5.4: a length-prefixed body containing
// its own fields, with bound restoration on End.
func TestNestedStructFraming(t *testing.T) {
	w := NewWriter(nil)
	w.WriteTag(2, WireLengthDelim)
	m := w.BeginLengthDelim()
	w.WriteTag(1, WireVarint)
	w.WriteUvarint(42)
	w.WriteTag(3, WireLengthDelim)
	w.WriteString("nested")
	w.EndLengthDelim(m)
	if w.Err() != nil {
		t.Fatal(w.Err())
	}

	r := NewReader(w.Bytes())
	tag, wt, err := r.ReadTag()
	if err != nil || tag != 2 || wt != WireLengthDelim {
		t.Fatalf("outer tag: tag=%d wt=%d err=%v", tag, wt, err)
	}
	saved, err := r.BeginLengthDelim()
	if err != nil {
		t.Fatalf("BeginLengthDelim: %v", err)
	}
	// First inner field
	tag, wt, err = r.ReadTag()
	if err != nil || tag != 1 || wt != WireVarint {
		t.Fatalf("inner1 tag: %d %d %v", tag, wt, err)
	}
	if v, _ := r.ReadUvarint(); v != 42 {
		t.Fatal("inner1 value")
	}
	// Second inner field
	tag, wt, err = r.ReadTag()
	if err != nil || tag != 3 || wt != WireLengthDelim {
		t.Fatalf("inner2 tag: %d %d %v", tag, wt, err)
	}
	if s, _ := r.ReadString(); s != "nested" {
		t.Fatal("inner2 value")
	}
	if r.HasMore() {
		t.Fatal("HasMore inside nested after both fields consumed")
	}
	if err := r.EndLengthDelim(saved); err != nil {
		t.Fatalf("EndLengthDelim: %v", err)
	}
	if r.HasMore() {
		t.Fatal("HasMore at outer after end")
	}
}

// TestSliceFraming covers §5.2: length | count | elements.
func TestSliceFraming(t *testing.T) {
	w := NewWriter(nil)
	values := []uint64{1, 2, 3, 1 << 20}
	w.WriteTag(7, WireLengthDelim)
	m := w.BeginLengthDelim()
	w.WriteUvarint(uint64(len(values))) // count
	for _, v := range values {
		w.WriteUvarint(v) // value-only encoding per spec
	}
	w.EndLengthDelim(m)
	if w.Err() != nil {
		t.Fatal(w.Err())
	}

	r := NewReader(w.Bytes())
	tag, wt, err := r.ReadTag()
	if err != nil || tag != 7 || wt != WireLengthDelim {
		t.Fatalf("tag: %d %d %v", tag, wt, err)
	}
	saved, err := r.BeginLengthDelim()
	if err != nil {
		t.Fatal(err)
	}
	count, err := r.ReadLength()
	if err != nil {
		t.Fatal(err)
	}
	if count != len(values) {
		t.Fatalf("count=%d want %d", count, len(values))
	}
	got := make([]uint64, count)
	for i := range count {
		v, err := r.ReadUvarint()
		if err != nil {
			t.Fatalf("element %d: %v", i, err)
		}
		got[i] = v
	}
	if r.HasMore() {
		t.Fatal("trailing bytes inside slice region")
	}
	if err := r.EndLengthDelim(saved); err != nil {
		t.Fatal(err)
	}
	for i, v := range values {
		if got[i] != v {
			t.Fatalf("got[%d]=%d want %d", i, got[i], v)
		}
	}
}

// TestMapFraming exercises §5.3 over a string→int64 map shape.
func TestMapFraming(t *testing.T) {
	w := NewWriter(nil)
	w.WriteTag(11, WireLengthDelim)
	m := w.BeginLengthDelim()
	w.WriteUvarint(2) // count
	w.WriteString("alpha")
	w.WriteVarint(1)
	w.WriteString("beta")
	w.WriteVarint(-2)
	w.EndLengthDelim(m)
	if w.Err() != nil {
		t.Fatal(w.Err())
	}

	r := NewReader(w.Bytes())
	if _, _, err := r.ReadTag(); err != nil {
		t.Fatal(err)
	}
	saved, err := r.BeginLengthDelim()
	if err != nil {
		t.Fatal(err)
	}
	count, _ := r.ReadLength()
	if count != 2 {
		t.Fatalf("count=%d", count)
	}
	out := map[string]int64{}
	for range count {
		k, err := r.ReadString()
		if err != nil {
			t.Fatal(err)
		}
		v, err := r.ReadVarint()
		if err != nil {
			t.Fatal(err)
		}
		out[k] = v
	}
	if err := r.EndLengthDelim(saved); err != nil {
		t.Fatal(err)
	}
	if out["alpha"] != 1 || out["beta"] != -2 {
		t.Fatalf("map=%v", out)
	}
}

// TestSkipUnknownField covers §3.2 / §7.1: a decoder that does not know a
// tag must skip its value using the wire-type rules and continue.
func TestSkipUnknownField(t *testing.T) {
	w := NewWriter(nil)
	// Known tag 1, varint
	w.WriteTag(1, WireVarint)
	w.WriteUvarint(7)
	// Unknown tag 5, length-delim payload
	w.WriteTag(5, WireLengthDelim)
	w.WriteString("opaque future field")
	// Unknown tag 6, fixed64
	w.WriteTag(6, WireFixed64)
	w.WriteFixed64(0xAABBCCDDEEFF0011)
	// Unknown tag 7, fixed32
	w.WriteTag(7, WireFixed32)
	w.WriteFixed32(0x12345678)
	// Unknown tag 8, varint
	w.WriteTag(8, WireVarint)
	w.WriteUvarint(1<<35 + 9)
	// Known tag 9, string
	w.WriteTag(9, WireLengthDelim)
	w.WriteString("after the unknowns")
	if w.Err() != nil {
		t.Fatal(w.Err())
	}

	r := NewReader(w.Bytes())
	var (
		gotInt uint64
		gotStr string
	)
	for r.HasMore() {
		tag, wt, err := r.ReadTag()
		if err != nil {
			t.Fatalf("ReadTag: %v", err)
		}
		switch tag {
		case 1:
			gotInt, err = r.ReadUvarint()
		case 9:
			gotStr, err = r.ReadString()
		default:
			err = r.SkipField(wt)
		}
		if err != nil {
			t.Fatalf("tag %d: %v", tag, err)
		}
	}
	if gotInt != 7 || gotStr != "after the unknowns" {
		t.Fatalf("unknown-skip: int=%d str=%q", gotInt, gotStr)
	}
}

// TestSkipReservedWireType ensures SkipField rejects reserved wire types
// rather than guessing how to advance.
func TestSkipReservedWireType(t *testing.T) {
	r := NewReader(nil)
	if err := r.SkipField(WireType(4)); !errors.Is(err, ErrReservedWire) {
		t.Fatalf("want ErrReservedWire, got %v", err)
	}
}

// TestNestedLengthShortening confirms that EndLengthDelim shifts the body
// left when the actual length encodes in fewer bytes than the reserved
// slot, producing a tightly packed blob.
func TestNestedLengthShortening(t *testing.T) {
	w := NewWriter(nil)
	m := w.BeginLengthDelim()
	w.WriteUvarint(7) // 1 body byte
	w.EndLengthDelim(m)
	got := w.Bytes()
	// Expect: length=1 (one byte), then body byte 0x07
	if len(got) != 2 || got[0] != 0x01 || got[1] != 0x07 {
		t.Fatalf("packed nested: % x", got)
	}
}

// TestNestedTrailingBytesRejected ensures EndLengthDelim refuses to leave
// part of a bounded region unread (decoder stays honest about its bounds).
func TestNestedTrailingBytesRejected(t *testing.T) {
	w := NewWriter(nil)
	m := w.BeginLengthDelim()
	w.WriteUvarint(1)
	w.WriteUvarint(2)
	w.EndLengthDelim(m)
	r := NewReader(w.Bytes())
	saved, err := r.BeginLengthDelim()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadUvarint(); err != nil {
		t.Fatal(err)
	}
	// One uvarint left unread inside the region.
	if err := r.EndLengthDelim(saved); !errors.Is(err, ErrTrailingBytes) {
		t.Fatalf("want ErrTrailingBytes, got %v", err)
	}
}

// TestNestedTruncatedLength: a length prefix that exceeds available bytes
// must be rejected immediately, not on the eventual short read.
func TestNestedTruncatedLength(t *testing.T) {
	// length = 100, but only 1 body byte follows.
	r := NewReader([]byte{100, 0x07})
	if _, err := r.BeginLengthDelim(); !errors.Is(err, ErrTruncated) {
		t.Fatalf("want ErrTruncated, got %v", err)
	}
}

// TestWriterReset exercises pool reuse semantics: after Reset the writer's
// state and sticky error are cleared.
func TestWriterReset(t *testing.T) {
	w := NewWriter(nil)
	w.WriteTag(0, WireVarint) // sets sticky error
	if w.Err() == nil {
		t.Fatal("expected sticky error")
	}
	buf := make([]byte, 0, 64)
	w.Reset(buf)
	if w.Err() != nil {
		t.Fatal("Reset must clear err")
	}
	w.WriteUvarint(1)
	if w.Err() != nil {
		t.Fatal(w.Err())
	}
	if len(w.Bytes()) != 1 || w.Bytes()[0] != 1 {
		t.Fatalf("post-reset bytes: % x", w.Bytes())
	}
}

// TestRoundTripBlobWithHeader is an end-to-end exercise: a header plus a
// root struct with a primitive, a nested struct (with one primitive of its
// own), a slice of strings, and a map[string]int64. Verifies the
// HasMore-driven decode loop terminates exactly at the end of input.
func TestRoundTripBlobWithHeader(t *testing.T) {
	w := NewWriter(nil)
	w.WriteHeader(0, 0xFEED)

	// tag 1: uint64 = 100
	w.WriteTag(1, WireVarint)
	w.WriteUvarint(100)

	// tag 2: nested struct with tag 1: bool true
	w.WriteTag(2, WireLengthDelim)
	m := w.BeginLengthDelim()
	w.WriteTag(1, WireVarint)
	w.WriteBool(true)
	w.EndLengthDelim(m)

	// tag 3: []string{"a", "bb"}
	w.WriteTag(3, WireLengthDelim)
	m = w.BeginLengthDelim()
	w.WriteUvarint(2)
	w.WriteString("a")
	w.WriteString("bb")
	w.EndLengthDelim(m)

	// tag 4: map[string]int64{"x": -10}
	w.WriteTag(4, WireLengthDelim)
	m = w.BeginLengthDelim()
	w.WriteUvarint(1)
	w.WriteString("x")
	w.WriteVarint(-10)
	w.EndLengthDelim(m)

	if w.Err() != nil {
		t.Fatal(w.Err())
	}

	r := NewReader(w.Bytes())
	if _, schemaHint, err := r.ReadHeader(); err != nil || schemaHint != 0xFEED {
		t.Fatalf("header: schemaHint=%x err=%v", schemaHint, err)
	}

	var (
		topU64    uint64
		nestedB   bool
		strSlice  []string
		strIntMap = map[string]int64{}
	)
	for r.HasMore() {
		tag, wt, err := r.ReadTag()
		if err != nil {
			t.Fatal(err)
		}
		switch tag {
		case 1:
			topU64, err = r.ReadUvarint()
		case 2:
			saved, e := r.BeginLengthDelim()
			if e != nil {
				err = e
				break
			}
			for r.HasMore() {
				_, _, e := r.ReadTag()
				if e != nil {
					err = e
					break
				}
				nestedB, err = r.ReadBool()
				if err != nil {
					break
				}
			}
			if err == nil {
				err = r.EndLengthDelim(saved)
			}
		case 3:
			saved, e := r.BeginLengthDelim()
			if e != nil {
				err = e
				break
			}
			n, _ := r.ReadLength()
			for range n {
				s, e := r.ReadString()
				if e != nil {
					err = e
					break
				}
				strSlice = append(strSlice, s)
			}
			if err == nil {
				err = r.EndLengthDelim(saved)
			}
		case 4:
			saved, e := r.BeginLengthDelim()
			if e != nil {
				err = e
				break
			}
			n, _ := r.ReadLength()
			for range n {
				k, e := r.ReadString()
				if e != nil {
					err = e
					break
				}
				v, e := r.ReadVarint()
				if e != nil {
					err = e
					break
				}
				strIntMap[k] = v
			}
			if err == nil {
				err = r.EndLengthDelim(saved)
			}
		default:
			err = r.SkipField(wt)
		}
		if err != nil {
			t.Fatalf("tag %d: %v", tag, err)
		}
	}
	if topU64 != 100 || !nestedB ||
		len(strSlice) != 2 || strSlice[0] != "a" || strSlice[1] != "bb" ||
		strIntMap["x"] != -10 {
		t.Fatalf("decoded: topU64=%d nestedB=%v slice=%v map=%v",
			topU64, nestedB, strSlice, strIntMap)
	}
}
