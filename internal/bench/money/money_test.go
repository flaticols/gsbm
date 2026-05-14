package money_test

import (
	"bytes"
	"testing"

	"go.flaticols.dev/gsbm/internal/bench/money"
	"go.flaticols.dev/gsbm/storage/gsbm"
)

// TestStringAppendByteEquality is the wire-drift guard for the
// money fixture: StringBatch and AppendBatch encode the same in-memory
// payload to byte-identical wire output. The two codec paths produce
// the same canonical text for any DecimalAmount, so the surrounding
// LENGTH_DELIM envelopes and varint lengths must agree byte-for-byte.
func TestStringAppendByteEquality(t *testing.T) {
	b := money.MakeBatch(0, 32)
	sb := money.StringBatchFrom(b)
	ab := money.AppendBatchFrom(b)

	bs, err := gsbm.Marshal(&sb, 1)
	if err != nil {
		t.Fatalf("Marshal StringBatch: %v", err)
	}
	ba, err := gsbm.Marshal(&ab, 1)
	if err != nil {
		t.Fatalf("Marshal AppendBatch: %v", err)
	}
	if !bytes.Equal(bs, ba) {
		t.Fatalf("wire bytes diverged between string and append paths: len(s)=%d len(a)=%d", len(bs), len(ba))
	}
}
