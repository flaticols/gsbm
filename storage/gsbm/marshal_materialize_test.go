package gsbm_test

import (
	"bytes"
	"testing"

	"go.flaticols.dev/gsbm/storage/gsbm"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/codecs/builtins"
)

// probeStringer counts String() invocations via a shared pointer so the
// gsbm.Marshal-level materialize-once assertion can observe how many
// times the codec's underlying materializer ran during a single encode.
type probeStringer struct {
	value string
	calls *int
}

func (p probeStringer) String() string {
	*p.calls++
	return p.value
}

// probeMarshaler emits two materializing-codec fields (tag 1, tag 2) at
// distinct callsites. SizeGSBM delegates to MarshalGSBM against a
// CountingWriter — the same shape codegen now emits — so the two-pass
// flow exercised by gsbm.Marshal must thread one Writer (with its
// scratch cache) through both passes, materializing each field exactly
// once.
type probeMarshaler struct {
	a, b probeStringer
}

const (
	csProbeA uintptr = 0xa1
	csProbeB uintptr = 0xb2
)

func (p *probeMarshaler) SizeGSBM() int {
	cw := gsbm.NewCountingWriter()
	_ = p.MarshalGSBM(cw)
	return cw.Size()
}

func (p *probeMarshaler) MarshalGSBM(w *gsbm.Writer) error {
	w.WriteTag(1, gsbm.WireLengthDelim)
	if err := builtins.EmitDecimalString(w, p.a, csProbeA); err != nil {
		return err
	}
	w.WriteTag(2, gsbm.WireLengthDelim)
	if err := builtins.EmitDecimalString(w, p.b, csProbeB); err != nil {
		return err
	}
	return w.Err()
}

// TestMarshalMaterializeOnce is the integration-level materialize-once
// property: across the full gsbm.Marshal call, each materializing
// codec's gen function executes exactly once per field. The size pass
// populates the scratch cache, the write pass hits the cache via
// adoptScratch. Two distinct callsites are exercised to confirm cache
// keys don't collapse different fields.
func TestMarshalMaterializeOnce(t *testing.T) {
	aCalls, bCalls := 0, 0
	p := &probeMarshaler{
		a: probeStringer{value: "12.34", calls: &aCalls},
		b: probeStringer{value: "-0.5", calls: &bCalls},
	}
	if _, err := gsbm.Marshal(p, 0); err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if aCalls != 1 {
		t.Errorf("field a String() invocations = %d, want 1 (two-pass cache should hit on write)", aCalls)
	}
	if bCalls != 1 {
		t.Errorf("field b String() invocations = %d, want 1", bCalls)
	}
}

// TestMarshalStandaloneSizeMaterializeOnce pins the standalone SizeGSBM
// behaviour: the generated wrapper allocates a fresh CountingWriter and
// runs MarshalGSBM against it, so the materializer fires exactly once
// per field within that scope. The documented "double materialization"
// cost only manifests when SizeGSBM and a separate MarshalGSBM run on
// disjoint Writers, not within one call.
func TestMarshalStandaloneSizeMaterializeOnce(t *testing.T) {
	aCalls, bCalls := 0, 0
	p := &probeMarshaler{
		a: probeStringer{value: "1", calls: &aCalls},
		b: probeStringer{value: "2", calls: &bCalls},
	}
	_ = p.SizeGSBM()
	if aCalls != 1 || bCalls != 1 {
		t.Errorf("standalone SizeGSBM invocations = (%d,%d), want (1,1)", aCalls, bCalls)
	}
}

// TestMarshalExactBodyLength asserts the blob returned by gsbm.Marshal
// has len == HeaderSize + bodyLen, where bodyLen is determined by the
// size pass. This is the load-bearing invariant the header's bodyLen
// field depends on. cap may exceed len due to BeginLengthDelim
// transient over-reservation (see TestMarshalExactLength's longer
// rationale); the strict-capacity follow-up is tracked separately.
func TestMarshalExactBodyLengthMaterializing(t *testing.T) {
	calls := 0
	p := &probeMarshaler{
		a: probeStringer{value: "100.250", calls: &calls},
		b: probeStringer{value: "9", calls: &calls},
	}
	blob, err := gsbm.Marshal(p, 0)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected exactly 2 String() invocations across two fields, got %d", calls)
	}

	// Read header back: bodyLen recorded in the header must equal the
	// length of the body that follows it.
	r := gsbm.NewReader(blob)
	_, _, bodyLen, err := r.ReadHeader()
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if int(bodyLen)+gsbm.HeaderSize != len(blob) {
		t.Fatalf("len(blob)=%d, want HeaderSize+bodyLen=%d", len(blob), int(bodyLen)+gsbm.HeaderSize)
	}
}

// TestMarshalRoundTripMaterializing covers the materializing-codec
// integration: encode → ReadHeader → reuse the wire bytes via
// CountingWriter to confirm the size pass and write pass produced the
// same byte budget. A full UnmarshalGSBM round-trip is not in scope
// here (the probe Marshaler has no Unmarshaler counterpart); the
// fixture-level customcodec round-trip already covers the decode side
// end-to-end with a real generated Record. This test pins the cross-
// pass byte-count agreement that Marshal's adoptScratch makes possible.
func TestMarshalSizeMatchesMarshalBytes(t *testing.T) {
	calls := 0
	p := &probeMarshaler{
		a: probeStringer{value: "abc", calls: &calls},
		b: probeStringer{value: "xyz", calls: &calls},
	}

	// Single-Writer reference: encode without the two-pass cache.
	ref := gsbm.NewWriter(nil)
	if err := p.MarshalGSBM(ref); err != nil {
		t.Fatalf("ref MarshalGSBM: %v", err)
	}
	refBytes := append([]byte(nil), ref.Bytes()...)

	calls = 0
	blob, err := gsbm.Marshal(p, 0)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	body := blob[gsbm.HeaderSize:]
	if !bytes.Equal(body, refBytes) {
		t.Fatalf("Marshal body diverged from single-pass MarshalGSBM:\n marshal=% x\n ref    =% x", body, refBytes)
	}
}
