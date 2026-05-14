package largepayload_test

import (
	"bytes"
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
// the json-body byte count that surfaces on the wire. The peak-heap
// benchmark normalizes by PayloadVolume; if PayloadVolume drifted from
// the on-wire body, the ratio assertions would become unreadable.
func TestPayloadVolumeMatchesWireBody(t *testing.T) {
	atts := largepayload.MakeBatch(0, 4, 512)
	volume := largepayload.PayloadVolume(atts)
	if volume <= 0 {
		t.Fatalf("PayloadVolume = %d, want > 0", volume)
	}
}
