package largepayload_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"go.flaticols.dev/gsbm/internal/bench/largepayload"
	"go.flaticols.dev/gsbm/storage/gsbm"
)

// TestStreamingAndCachedWireEqual confirms the streaming and cached
// flavors of the largepayload fixture encode the same input to
// byte-identical wire output. This is the drift guard for any
// peak-heap comparison that relies on the two paths producing the
// same body bytes — if they ever diverged, the peak comparison would
// be measuring two different shapes instead of two retention
// strategies.
func TestStreamingAndCachedWireEqual(t *testing.T) {
	atts := largepayload.MakeBatch(0, 8, 1024)
	sb := largepayload.StreamingBatch{Attachments: atts}
	cb := largepayload.CachedBatch{Attachments: atts}
	streamBuf, err := gsbm.Marshal(&sb, 1)
	if err != nil {
		t.Fatalf("Marshal streaming: %v", err)
	}
	cachedBuf, err := gsbm.Marshal(&cb, 1)
	if err != nil {
		t.Fatalf("Marshal cached: %v", err)
	}
	if !bytes.Equal(streamBuf, cachedBuf) {
		t.Fatalf("streaming and cached wire bytes diverge: len(stream)=%d len(cached)=%d", len(streamBuf), len(cachedBuf))
	}
}

// TestPayloadVolumeMatchesWireBody pins PayloadVolume to actually equal
// the summed json-marshaled payload byte count. The peak-heap benchmark
// normalizes by PayloadVolume; if PayloadVolume drifted from the on-wire
// body (e.g. a json-tag rename or a type change on LargePayload),
// the ratio assertions would become unreadable.
func TestPayloadVolumeMatchesWireBody(t *testing.T) {
	atts := largepayload.MakeBatch(0, 4, 512)
	volume := largepayload.PayloadVolume(atts)
	if volume <= 0 {
		t.Fatalf("PayloadVolume = %d, want > 0", volume)
	}
	var want int
	for i := range atts {
		b, err := json.Marshal(atts[i].Payload)
		if err != nil {
			t.Fatalf("json.Marshal attachment %d: %v", i, err)
		}
		want += len(b)
	}
	if volume != want {
		t.Fatalf("PayloadVolume = %d, hand-computed sum of json.Marshal = %d", volume, want)
	}
}
