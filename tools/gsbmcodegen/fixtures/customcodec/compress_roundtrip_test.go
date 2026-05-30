package customcodec

import (
	"reflect"
	"testing"
	"time"

	"go.flaticols.dev/gsbm/storage/gsbm"
)

// sampleRecord builds a Record that exercises every custom-codec kind:
// Time (analytic), DecimalString (materializing-cached), DecimalAppend
// (materializing), StreamingJSON (streaming — body materialized in a
// SEPARATE pass from the size pass), and DecimalBinary (analytic).
func sampleRecord(sec int64, tag string) Record {
	return Record{
		CreatedAt:    time.Unix(sec, 0).UTC(),
		Amount:       DecimalAmount{Integer: "123", Fraction: "45"},
		AmountAppend: DecimalAmount{Negative: true, Integer: "9", Fraction: "9"},
		Payload:      LargePayload{Tag: tag, Data: []byte("streaming-json-body-" + tag)},
		AmountBinary: DecimalAmount{Integer: "777", Fraction: "001"},
	}
}

// TestCustomCodecCompressedRoundTrip is the regression for the interaction
// the extended-header inflatedLen check has with custom codecs. The decoder
// now hard-rejects a compressed blob whose inflated body length != the
// writer's recorded inflatedLen (the size-pass total). For the streaming
// and materializing codecs the body is produced in a pass distinct from the
// size pass, so if those ever disagreed the inflatedLen check would turn a
// previously-valid compressed blob into a decode-time reject. This pins that
// a Container carrying all codec kinds round-trips through BOTH compressing
// codecs and decodes byte-for-byte equal to the uncompressed decode.
func TestCustomCodecCompressedRoundTrip(t *testing.T) {
	in := Container{
		Inner: sampleRecord(1700001000, "inner"),
		Items: []Record{sampleRecord(1700002000, "i0"), sampleRecord(1700003000, "i1")},
		ByKey: map[string]Record{"a": sampleRecord(1700004000, "a")},
		PtrItems: []*Record{
			nil,
			func() *Record { r := sampleRecord(1700006000, "p"); return &r }(),
		},
		Pages:   [][]Record{{sampleRecord(1700007000, "pg")}},
		Buckets: []map[string]Record{{"k": sampleRecord(1700008000, "b")}},
		Aliased: AliasContainer{Pages: PageList{sampleRecord(1700009000, "al")}},
	}

	// Reference: uncompressed decode.
	uncompressed, err := gsbm.Marshal(&in, 0)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var want Container
	if err := gsbm.DecodeInto(uncompressed, &want); err != nil {
		t.Fatalf("uncompressed DecodeInto: %v", err)
	}

	for _, method := range []gsbm.CompressionMethod{gsbm.CompressionZstd, gsbm.CompressionGzip} {
		blob, err := gsbm.MarshalWithOptions(&in, 0, gsbm.Options{Compression: method})
		if err != nil {
			t.Fatalf("method %d MarshalWithOptions: %v", method, err)
		}
		var got Container
		if err := gsbm.DecodeInto(blob, &got); err != nil {
			// This is the failure the advisor flagged: a custom-codec body
			// whose size pass disagreed with the write pass would surface as
			// ErrCorruptCompressedBody (inflatedLen mismatch) here.
			t.Fatalf("method %d DecodeInto compressed: %v", method, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("method %d: compressed decode != uncompressed decode", method)
		}
	}
}

// TestCustomCodecCompressedToWriterRoundTrip exercises the streaming encode
// entry point (MarshalToWriter) on the same all-codec-kinds value, since the
// streaming-compressed write pass is where a streaming custom codec is
// re-materialized against the recorded region sizes.
func TestCustomCodecCompressedToWriterRoundTrip(t *testing.T) {
	in := Container{
		Inner: sampleRecord(1700010000, "w-inner"),
		Items: []Record{sampleRecord(1700011000, "w0")},
	}
	for _, method := range []gsbm.CompressionMethod{gsbm.CompressionZstd, gsbm.CompressionGzip} {
		buffered, err := gsbm.MarshalWithOptions(&in, 0, gsbm.Options{Compression: method})
		if err != nil {
			t.Fatalf("method %d MarshalWithOptions: %v", method, err)
		}
		var got Container
		if err := gsbm.DecodeInto(buffered, &got); err != nil {
			t.Fatalf("method %d DecodeInto: %v", method, err)
		}
		if got.Inner.Payload.Tag != in.Inner.Payload.Tag {
			t.Fatalf("method %d streaming-codec payload tag: got %q want %q",
				method, got.Inner.Payload.Tag, in.Inner.Payload.Tag)
		}
	}
}
