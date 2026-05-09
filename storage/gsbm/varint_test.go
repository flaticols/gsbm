package odm

import (
	"errors"
	"testing"
)

func TestVarintRoundTrip(t *testing.T) {
	cases := []uint64{
		0, 1, 127, 128, 129, 16383, 16384,
		1<<28 - 1, 1 << 28,
		1<<35 - 1, 1 << 35,
		1<<42 - 1, 1 << 42,
		1<<49 - 1, 1 << 49,
		1<<56 - 1, 1 << 56,
		1<<63 - 1, 1 << 63, ^uint64(0),
	}
	for _, v := range cases {
		buf := appendUvarint(nil, v)
		if got := varintLen(v); got != len(buf) {
			t.Fatalf("varintLen(%d)=%d, encoded=%d", v, got, len(buf))
		}
		decoded, n, err := readUvarint(buf, 0)
		if err != nil {
			t.Fatalf("readUvarint(%d): %v", v, err)
		}
		if n != len(buf) {
			t.Fatalf("readUvarint(%d) consumed %d, want %d", v, n, len(buf))
		}
		if decoded != v {
			t.Fatalf("varint round-trip: in=%d out=%d", v, decoded)
		}
	}
}

func TestVarintTruncated(t *testing.T) {
	_, _, err := readUvarint([]byte{0x80}, 0)
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("want ErrTruncated, got %v", err)
	}
	_, _, err = readUvarint(nil, 0)
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("empty: want ErrTruncated, got %v", err)
	}
}

func TestVarintOverflow(t *testing.T) {
	// 11 continuation bytes: overflow.
	bad := []byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x01}
	if _, _, err := readUvarint(bad, 0); !errors.Is(err, ErrVarintOverflow) {
		t.Fatalf("want ErrVarintOverflow, got %v", err)
	}
	// 10th byte > 1 also overflows uint64.
	bad2 := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x02}
	if _, _, err := readUvarint(bad2, 0); !errors.Is(err, ErrVarintOverflow) {
		t.Fatalf("want ErrVarintOverflow on 10th byte, got %v", err)
	}
}

func TestZigzag(t *testing.T) {
	cases := []int64{0, -1, 1, -2, 2, -64, 63, 1 << 30, -(1 << 30), 1<<63 - 1, -1 << 63}
	for _, v := range cases {
		got := zigzagDecode64(zigzagEncode64(v))
		if got != v {
			t.Fatalf("zigzag round-trip: in=%d out=%d", v, got)
		}
	}
	// Spec sanity: zigzag(0)=0, zigzag(-1)=1, zigzag(1)=2.
	if zigzagEncode64(0) != 0 || zigzagEncode64(-1) != 1 || zigzagEncode64(1) != 2 {
		t.Fatal("zigzag spec mapping wrong")
	}
}
