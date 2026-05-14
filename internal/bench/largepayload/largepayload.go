// Package largepayload is a benchmark-only fixture for the peak-heap
// streaming-vs-materializing-cached comparison (issue #30). It builds a
// batch of Attachments, each carrying a JSON-serializable payload, and
// exposes two encode roots — StreamingBatch and CachedBatch — that emit
// byte-identical wire output for the same input through different codec
// kinds:
//
//   - StreamingBatch wraps builtins.StreamJSONBytes (no callsite, no
//     scratch cache): the JSON body is materialized once per pass
//     (size + write) and discarded between passes.
//   - CachedBatch wraps a materializing-cached EmitFn (Writer scratch
//     cache keyed by callsite): the JSON body is materialized exactly
//     once per occurrence per gsbm.Marshal call and retained across the
//     size→write hand-off.
//
// The wire envelope and body bytes are identical between the two
// kinds — only the runtime retention shape differs, which is what the
// peak-heap benchmark in storage/gsbm/bench_encode_test.go measures.
package largepayload

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"

	"go.flaticols.dev/gsbm/storage/gsbm"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/codecs/builtins"
)

// LargePayload mirrors the customcodec fixture's payload type: a short
// tag plus a (potentially large) byte blob. encoding/json walks struct
// fields in declaration order, so the JSON output is deterministic and
// the streaming codec body matches byte-for-byte across the two passes.
type LargePayload struct {
	Tag  string `json:"tag"`
	Data []byte `json:"data,omitempty"`
}

// Attachment is one element of the batch — a small header field plus a
// streaming-eligible payload.
type Attachment struct {
	Name    string
	Payload LargePayload
}

// csAttachmentPayload is the materializing-cached scratch-cache callsite
// id for the Payload field. The same id keys every slice-element
// occurrence — mirroring what codegen emits for a single field across a
// slice traversal — so the scratchEntry stores occurrences in walk order
// and the write pass re-reads them through the per-lane cursor.
const csAttachmentPayload uint64 = 0x4174746163686d65 // "Attachme"

// StreamingBatch encodes its Attachments using the streaming codec for
// each Payload — body materialized per pass, no scratch cache, peak heap
// bounded by one in-flight body's worth of bytes plus the output buffer.
type StreamingBatch struct {
	Attachments []Attachment
}

// CachedBatch encodes its Attachments using a materializing-cached EmitFn
// for each Payload — body materialized once per occurrence per
// gsbm.Marshal call, retained in the Writer scratch cache for the
// duration of the encode. Peak heap is roughly two payload-volumes (the
// scratch cache plus the output buffer).
type CachedBatch struct {
	Attachments []Attachment
}

// MakeBatch builds n Attachments whose Payload.Data fields are each
// payloadDataLen bytes of deterministic pseudo-random ASCII. Same
// (seed, n, payloadDataLen) triple yields byte-identical wire output
// across calls — required so peak-heap measurements are reproducible.
//
// Panics if n or payloadDataLen is negative.
func MakeBatch(seed int64, n, payloadDataLen int) []Attachment {
	if n < 0 || payloadDataLen < 0 {
		panic(fmt.Sprintf("largepayload.MakeBatch: invalid (n=%d, payloadDataLen=%d)", n, payloadDataLen))
	}
	r := rand.New(rand.NewPCG(uint64(seed), 0xC0FFEEC0FFEE))
	atts := make([]Attachment, n)
	for i := range atts {
		data := make([]byte, payloadDataLen)
		for j := range data {
			data[j] = byte('a' + r.IntN(26))
		}
		atts[i] = Attachment{
			Name:    fmt.Sprintf("attachment-%07d", i),
			Payload: LargePayload{Tag: fmt.Sprintf("p-%07d", i), Data: data},
		}
	}
	return atts
}

// PayloadVolume returns the summed length of json.Marshal(a.Payload)
// across every attachment — the reference total used by peak-heap
// assertions to normalize against payload size. Computed once per
// benchmark setup so the assertion thresholds are tied to the actual
// JSON wire-body byte count, not the raw Data length.
func PayloadVolume(atts []Attachment) int {
	total := 0
	for i := range atts {
		b, err := json.Marshal(atts[i].Payload)
		if err != nil {
			panic(fmt.Sprintf("largepayload.PayloadVolume: json.Marshal: %v", err))
		}
		total += len(b)
	}
	return total
}

func (b *StreamingBatch) SizeGSBM() int {
	cw := gsbm.NewCountingWriter()
	_ = b.MarshalGSBM(cw)
	return cw.Size()
}

func (b *StreamingBatch) MarshalGSBM(w *gsbm.Writer) error {
	return marshalBatch(w, b.Attachments, marshalAttachmentStream)
}

func (b *CachedBatch) SizeGSBM() int {
	cw := gsbm.NewCountingWriter()
	_ = b.MarshalGSBM(cw)
	return cw.Size()
}

func (b *CachedBatch) MarshalGSBM(w *gsbm.Writer) error {
	return marshalBatch(w, b.Attachments, marshalAttachmentCached)
}

// marshalFn is the per-attachment dispatch point. The two flavors plug
// into the same outer batch frame and differ only in how Payload is
// emitted.
type marshalFn func(w *gsbm.Writer, a *Attachment) error

func marshalBatch(w *gsbm.Writer, atts []Attachment, emit marshalFn) error {
	w.WriteTag(1, gsbm.WireLengthDelim)
	m := w.BeginLengthDelim()
	w.WriteUvarint(uint64(len(atts)))
	for i := range atts {
		inner := w.BeginLengthDelim()
		if err := emit(w, &atts[i]); err != nil {
			return err
		}
		w.EndLengthDelim(inner)
	}
	w.EndLengthDelim(m)
	return w.Err()
}

func marshalAttachmentStream(w *gsbm.Writer, a *Attachment) error {
	w.WriteTag(1, gsbm.WireLengthDelim)
	w.WriteString(a.Name)
	w.WriteTag(2, gsbm.WireLengthDelim)
	if err := builtins.StreamJSONBytes(w, a.Payload); err != nil {
		return err
	}
	return w.Err()
}

func marshalAttachmentCached(w *gsbm.Writer, a *Attachment) error {
	w.WriteTag(1, gsbm.WireLengthDelim)
	w.WriteString(a.Name)
	w.WriteTag(2, gsbm.WireLengthDelim)
	if err := emitCachedJSON(w, a.Payload, csAttachmentPayload); err != nil {
		return err
	}
	return w.Err()
}

// emitCachedJSON materializes v as JSON exactly once per occurrence per
// gsbm.Marshal call and retains the bytes in the Writer's scratch cache
// across the size→write hand-off. Wire shape is the same LENGTH_DELIM
// byte body StreamJSONBytes writes, so the streaming and cached flavors
// produce byte-identical wire output for the same input.
func emitCachedJSON(w *gsbm.Writer, v LargePayload, callsite uint64) error {
	return w.WriteCachedBytes(callsite, func() []byte {
		b, err := json.Marshal(v)
		if err != nil {
			panic(fmt.Sprintf("largepayload.emitCachedJSON: json.Marshal: %v", err))
		}
		return b
	})
}
