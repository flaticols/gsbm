package namedkey

import (
	"bytes"
	"reflect"
	"sort"
	"testing"

	"go.flaticols.dev/gsbm/storage/gsbm"
	"go.flaticols.dev/gsbm/storage/gsbmarena"
)

// TestCountsRoundTrip exercises every named-keyed map kind: named string,
// named bool, named signed int, named unsigned int. Each map is populated
// with multiple entries so the deterministic-sort path is exercised.
func TestCountsRoundTrip(t *testing.T) {
	in := Counts{
		ByCode:     map[Code]int64{"alpha": 1, "beta": 2, "gamma": 3},
		BySeverity: map[Severity]int64{-1: 10, 0: 20, 7: 30},
		ByBucket:   map[Bucket]int64{0: 100, 256: 200, 65535: 300},
		ByFlag:     map[Flag]int64{false: 1, true: 2},
	}
	w := gsbm.NewWriter(nil)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Counts
	if err := got.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, got) {
		t.Fatalf("round-trip mismatch\n want: %#v\n  got: %#v", in, got)
	}
}

// TestCountsEmptyMapsRoundTrip pins the nil-in / nil-out contract for
// every named-keyed map. The encoder does NOT elide — it writes a tag
// with a zero-length body — and the decoder skips MakeMap when n=0,
// so the decoded maps stay nil rather than becoming empty-non-nil.
func TestCountsEmptyMapsRoundTrip(t *testing.T) {
	in := Counts{}
	w := gsbm.NewWriter(nil)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Counts
	if err := got.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ByCode != nil || got.BySeverity != nil || got.ByBucket != nil || got.ByFlag != nil {
		t.Fatalf("expected nil maps after empty round-trip, got %#v", got)
	}
}

// TestNamedStringKeyMatchesBuiltinWire is the core wire-stability claim:
// `map[Code]int64` produces byte-identical output to a hand-written
// encoding of `map[string]int64` over the same key/value pairs. Without
// this guarantee, callers migrating `map[string]V → map[Code]V` would
// rewrite every on-disk blob.
func TestNamedStringKeyMatchesBuiltinWire(t *testing.T) {
	pairs := map[string]int64{"alpha": 1, "beta": 2, "gamma": 3}
	named := make(map[Code]int64, len(pairs))
	for k, v := range pairs {
		named[Code(k)] = v
	}
	in := Counts{ByCode: named}
	w := gsbm.NewWriter(nil)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Re-emit the tag-1 body by hand, using the builtin-string map-key path:
	// uvarint length, then sorted (string-key, varint-value) pairs.
	want := gsbm.NewWriter(nil)
	want.WriteTag(1, gsbm.WireLengthDelim)
	m := want.BeginLengthDelim()
	want.WriteUvarint(uint64(len(pairs)))
	keys := make([]string, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		want.WriteString(k)
		want.WriteVarint(pairs[k])
	}
	want.EndLengthDelim(m)
	// Then the empty-map tags for the remaining fields, so the prefix
	// comparison runs against a full Counts blob.
	emitEmptyMap(want, 2)
	emitEmptyMap(want, 3)
	emitEmptyMap(want, 4)
	if want.Err() != nil {
		t.Fatalf("want builder: %v", want.Err())
	}
	if !bytes.Equal(w.Bytes(), want.Bytes()) {
		t.Fatalf("named-string-keyed map wire bytes differ from builtin string-keyed baseline\n got: %x\nwant: %x", w.Bytes(), want.Bytes())
	}
}

// TestNamedSignedIntKeyMatchesBuiltinWire mirrors the named-string case
// for `type Severity int8`: bytes must match a builtin map[int8]int64.
func TestNamedSignedIntKeyMatchesBuiltinWire(t *testing.T) {
	pairs := map[int8]int64{-1: 10, 0: 20, 7: 30}
	named := make(map[Severity]int64, len(pairs))
	for k, v := range pairs {
		named[Severity(k)] = v
	}
	in := Counts{BySeverity: named}
	w := gsbm.NewWriter(nil)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := gsbm.NewWriter(nil)
	emitEmptyMap(want, 1)
	want.WriteTag(2, gsbm.WireLengthDelim)
	m := want.BeginLengthDelim()
	want.WriteUvarint(uint64(len(pairs)))
	keys := make([]int8, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, k := range keys {
		want.WriteVarint(int64(k))
		want.WriteVarint(pairs[k])
	}
	want.EndLengthDelim(m)
	emitEmptyMap(want, 3)
	emitEmptyMap(want, 4)
	if want.Err() != nil {
		t.Fatalf("want builder: %v", want.Err())
	}
	if !bytes.Equal(w.Bytes(), want.Bytes()) {
		t.Fatalf("named-int8-keyed map wire bytes differ from builtin int8-keyed baseline\n got: %x\nwant: %x", w.Bytes(), want.Bytes())
	}
}

// TestNamedUnsignedIntKeyMatchesBuiltinWire pins the named-uint16 path
// (`type Bucket uint16`) against a builtin uint16-keyed baseline.
func TestNamedUnsignedIntKeyMatchesBuiltinWire(t *testing.T) {
	pairs := map[uint16]int64{0: 100, 256: 200, 65535: 300}
	named := make(map[Bucket]int64, len(pairs))
	for k, v := range pairs {
		named[Bucket(k)] = v
	}
	in := Counts{ByBucket: named}
	w := gsbm.NewWriter(nil)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := gsbm.NewWriter(nil)
	emitEmptyMap(want, 1)
	emitEmptyMap(want, 2)
	want.WriteTag(3, gsbm.WireLengthDelim)
	m := want.BeginLengthDelim()
	want.WriteUvarint(uint64(len(pairs)))
	keys := make([]uint16, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, k := range keys {
		want.WriteUvarint(uint64(k))
		want.WriteVarint(pairs[k])
	}
	want.EndLengthDelim(m)
	emitEmptyMap(want, 4)
	if want.Err() != nil {
		t.Fatalf("want builder: %v", want.Err())
	}
	if !bytes.Equal(w.Bytes(), want.Bytes()) {
		t.Fatalf("named-uint16-keyed map wire bytes differ from builtin uint16-keyed baseline\n got: %x\nwant: %x", w.Bytes(), want.Bytes())
	}
}

// TestNamedBoolKeyMatchesBuiltinWire pins the named-bool path
// (`type Flag bool`) against a builtin bool-keyed baseline. The sort
// order is false→true; both keys are written so a single-entry blob
// would not exercise the comparator.
func TestNamedBoolKeyMatchesBuiltinWire(t *testing.T) {
	pairs := map[bool]int64{true: 2, false: 1}
	named := make(map[Flag]int64, len(pairs))
	for k, v := range pairs {
		named[Flag(k)] = v
	}
	in := Counts{ByFlag: named}
	w := gsbm.NewWriter(nil)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := gsbm.NewWriter(nil)
	emitEmptyMap(want, 1)
	emitEmptyMap(want, 2)
	emitEmptyMap(want, 3)
	want.WriteTag(4, gsbm.WireLengthDelim)
	m := want.BeginLengthDelim()
	want.WriteUvarint(uint64(len(pairs)))
	// false sorts before true.
	want.WriteBool(false)
	want.WriteVarint(pairs[false])
	want.WriteBool(true)
	want.WriteVarint(pairs[true])
	want.EndLengthDelim(m)
	if want.Err() != nil {
		t.Fatalf("want builder: %v", want.Err())
	}
	if !bytes.Equal(w.Bytes(), want.Bytes()) {
		t.Fatalf("named-bool-keyed map wire bytes differ from builtin bool-keyed baseline\n got: %x\nwant: %x", w.Bytes(), want.Bytes())
	}
}

// TestArenaRoundTrip — the arena entry points in counts_gsbm_arena.go are
// generated code; the golden test only pins their text. This exercises
// them at runtime so a regression in named-map-key arena routing (e.g.,
// the named-key cast going via a non-arena temp on decode) surfaces here.
// Detach is included so the "lift to heap" path is also covered.
func TestArenaRoundTrip(t *testing.T) {
	in := Counts{
		ByCode:     map[Code]int64{"alpha": 1, "beta": 2, "gamma": 3},
		BySeverity: map[Severity]int64{-1: 10, 0: 20, 7: 30},
		ByBucket:   map[Bucket]int64{0: 100, 256: 200, 65535: 300},
		ByFlag:     map[Flag]int64{false: 1, true: 2},
	}
	w := gsbm.NewWriter(nil)
	w.WriteHeader(0, 1)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	blob := w.Bytes()

	a := gsbmarena.NewArena()
	got, err := DecodeCounts(blob, a)
	if err != nil {
		t.Fatalf("arena decode: %v", err)
	}
	if !reflect.DeepEqual(in, *got) {
		t.Fatalf("arena decode mismatch\n want: %#v\n  got: %#v", in, *got)
	}

	detached, err := DetachCounts(got)
	if err != nil {
		t.Fatalf("detach: %v", err)
	}
	a.Release()
	if !reflect.DeepEqual(in, *detached) {
		t.Fatalf("detached counts mismatch\n want: %#v\n  got: %#v", in, *detached)
	}
}

// emitEmptyMap mirrors the codegen's encode-of-nil-map output: tag, an
// outer length-delim wrapper, and the zero-count uvarint inside it. Used
// by the wire-equality tests to construct full Counts payloads where
// only one field is populated.
func emitEmptyMap(w *gsbm.Writer, tag uint32) {
	w.WriteTag(tag, gsbm.WireLengthDelim)
	m := w.BeginLengthDelim()
	w.WriteUvarint(0)
	w.EndLengthDelim(m)
}
