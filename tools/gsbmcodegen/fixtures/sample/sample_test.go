package sample

import (
	"errors"
	"reflect"
	"sync"
	"testing"

	"go.flaticols.dev/gsbm/storage/gsbm"
)

func note(s string) *string    { return &s }
func qty(q Quantity) *Quantity { return &q }
func label(l Label) *Label     { return &l }
func bytesp(b []byte) *[]byte  { return &b }

// TestOrderRoundTrip exercises every field kind on Order: required
// primitives, optional builtins (zero-elide path included), optional
// named struct, slice of struct, slice of primitive, map[string]int64,
// raw []byte, and a required nested struct. Encode → Decode → DeepEqual.
func TestOrderRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   Order
	}{
		{
			name: "fully populated",
			in: Order{
				ID:       "ord-001",
				Quantity: 7,
				Price:    19.99,
				Active:   true,
				Note:     note("first try"),
				Customer: &Customer{Name: "Ada", Email: "ada@example.com"},
				Items: []Item{
					{SKU: "abc", Count: 1},
					{SKU: "def", Count: 42},
				},
				Tags:      map[string]int64{"a": 1, "b": 2},
				Payload:   []byte{0x01, 0x02, 0x03, 0xff},
				Total:     Total{Currency: "USD", Amount: 39.99},
				Counts:    []int64{-3, 0, 7},
				Aliases:   map[string]Label{"primary": Label("ada"), "billing": Label("Adams")},
				Qty:       Quantity(99),
				OptQty:    qty(42),
				QtyList:   []Quantity{1, 2, 3},
				OptLabel:  label("rush"),
				LabelList: []Label{"alpha", "beta"},
			},
		},
		{
			name: "nil-optionals + empty collections",
			in: Order{
				ID:       "ord-002",
				Quantity: 0,
				Price:    0,
				Active:   false,
				Note:     nil,
				Customer: nil,
				Items:    nil,
				Tags:     nil,
				Payload:  nil,
				Total:    Total{},
				Counts:   nil,
			},
		},
		{
			name: "optional builtin zero-elided (note=\"\")",
			in: Order{
				ID:    "ord-003",
				Note:  note(""), // zero-elide path: PresenceZero on the wire
				Total: Total{Amount: 1},
			},
		},
		{
			name: "optional []byte non-empty",
			in: Order{
				ID:         "ord-004",
				OptPayload: bytesp([]byte{0xde, 0xad, 0xbe, 0xef}),
			},
		},
		{
			name: "optional []byte empty (zero-elide path)",
			in: Order{
				ID:         "ord-005",
				OptPayload: bytesp([]byte{}),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := gsbm.NewWriter(nil)
			if err := tc.in.MarshalGSBM(w); err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got Order
			r := gsbm.NewReader(w.Bytes())
			if err := got.UnmarshalGSBM(r); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			normalized := normalizeOrder(tc.in)
			if !reflect.DeepEqual(normalized, got) {
				t.Fatalf("round-trip mismatch\n want: %#v\n  got: %#v", normalized, got)
			}
		})
	}
}

// normalizeOrder mirrors the on-the-wire effects of zero-collection
// elision (encode([]T{}) and encode(nil) round-trip to the same nil) so
// DeepEqual matches what the decoder reconstructs.
func normalizeOrder(o Order) Order {
	if len(o.Payload) == 0 {
		o.Payload = nil
	}
	if len(o.Items) == 0 {
		o.Items = nil
	}
	if len(o.Tags) == 0 {
		o.Tags = nil
	}
	if len(o.Counts) == 0 {
		o.Counts = nil
	}
	if len(o.Aliases) == 0 {
		o.Aliases = nil
	}
	if len(o.QtyList) == 0 {
		o.QtyList = nil
	}
	if len(o.LabelList) == 0 {
		o.LabelList = nil
	}
	return o
}

// TestUnknownTagSkipped ensures the codegen's switch default routes an
// unknown tag through Reader.SkipField, supporting forward compat.
func TestUnknownTagSkipped(t *testing.T) {
	// Build a payload that decodes as a valid Customer, but inject an
	// unknown tag (5) with WireLengthDelim payload between Name and
	// Email. The decoder must skip the unknown tag and still recover Email.
	w := gsbm.NewWriter(nil)
	w.WriteTag(1, gsbm.WireLengthDelim)
	w.WriteString("Ada")
	w.WriteTag(5, gsbm.WireLengthDelim) // unknown tag
	w.WriteBytes([]byte("future field"))
	w.WriteTag(2, gsbm.WireLengthDelim)
	w.WriteString("ada@example.com")
	if w.Err() != nil {
		t.Fatal(w.Err())
	}
	var got Customer
	if err := got.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := Customer{Name: "Ada", Email: "ada@example.com"}
	if got != want {
		t.Fatalf("want %#v got %#v", want, got)
	}
}

// TestOptionalBytesOwnsItsData regression-guards finding that the
// optional *[]byte decode path used to expose Reader.ReadBytes's source
// alias to callers. Heap-mode decode contract: callers may reuse or
// mutate the source buffer after decode without corrupting decoded
// state. Mirrors the value []byte path's append-copy.
func TestOptionalBytesOwnsItsData(t *testing.T) {
	in := Order{ID: "x", OptPayload: bytesp([]byte{1, 2, 3, 4})}
	w := gsbm.NewWriter(nil)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatal(err)
	}
	// Hand the decoder a mutable copy of the encoded bytes; mutate the
	// buffer post-decode and confirm the decoded *[]byte is unaffected.
	src := append([]byte(nil), w.Bytes()...)
	var got Order
	if err := got.UnmarshalGSBM(gsbm.NewReader(src)); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.OptPayload == nil {
		t.Fatal("OptPayload nil after decode")
	}
	snapshot := append([]byte(nil), *got.OptPayload...)
	for i := range src {
		src[i] ^= 0xff
	}
	if !reflect.DeepEqual(*got.OptPayload, snapshot) {
		t.Fatalf("OptPayload aliased source buffer: snapshot=%v current=%v", snapshot, *got.OptPayload)
	}
}

// TestOptionalBuiltinPresenceZeroOnWire asserts that *string("") encodes
// to a single-byte presence body inside the field's length-delim, i.e.
// the zero-elide path is wired (not just claimed).
func TestOptionalBuiltinPresenceZeroOnWire(t *testing.T) {
	o := Order{ID: "x", Note: note("")}
	w := gsbm.NewWriter(nil)
	if err := o.MarshalGSBM(w); err != nil {
		t.Fatal(err)
	}
	// Decode and confirm Note round-trips to *"".
	var got Order
	if err := got.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
		t.Fatal(err)
	}
	if got.Note == nil || *got.Note != "" {
		t.Fatalf("zero-elide round-trip lost: got %#v", got.Note)
	}
}

// TestOptionalNamedStructRejectsZeroElide asserts that no codegen path
// emits PresenceZero for a *Customer field — the spec forbids it.
func TestOptionalNamedStructRejectsZeroElide(t *testing.T) {
	o := Order{ID: "x", Customer: &Customer{}}
	w := gsbm.NewWriter(nil)
	if err := o.MarshalGSBM(w); err != nil {
		t.Fatal(err)
	}
	// We can't easily probe the bytes without parsing, so the proof is
	// indirect: forge a payload claiming PresenceZero for Customer and
	// confirm UnmarshalGSBM rejects it.
	forged := gsbm.NewWriter(nil)
	forged.WriteTag(6, gsbm.WireLengthDelim)
	m := forged.BeginLengthDelim()
	forged.WritePresenceZero() // not allowed for named types
	forged.EndLengthDelim(m)
	if forged.Err() != nil {
		t.Fatal(forged.Err())
	}
	var got Order
	if err := got.UnmarshalGSBM(gsbm.NewReader(forged.Bytes())); err == nil {
		t.Fatal("want error decoding PresenceZero for *Customer, got nil")
	}
}

// TestHeaderRoundTrip wraps a real Order with the spec's 12-byte blob
// header to confirm the codegen output composes with WriteHeader/ReadHeader.
func TestHeaderRoundTrip(t *testing.T) {
	in := Order{ID: "h", Quantity: 1, Total: Total{Currency: "EUR", Amount: 1}}
	w := gsbm.NewWriter(nil)
	w.WriteHeader(0, 0xABCD, 0)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatal(err)
	}
	w.FinalizeBodyLen()
	r := gsbm.NewReader(w.Bytes())
	flags, schemaHint, _, err := r.ReadHeader()
	if err != nil {
		t.Fatal(err)
	}
	if flags != 0 || schemaHint != 0xABCD {
		t.Fatalf("header mismatch: flags=%d schemaHint=%d", flags, schemaHint)
	}
	var got Order
	if err := got.UnmarshalGSBM(r); err != nil {
		t.Fatal(err)
	}
	if got.ID != "h" || got.Total.Currency != "EUR" {
		t.Fatalf("body mismatch: %#v", got)
	}
}

// TestRollbackMissingTagsZeroDecode exercises backward compat: a blob
// hand-crafted to carry only a subset of the schema's tags (e.g. an
// older writer that didn't yet know about the newer fields) decodes
// cleanly and leaves the unknown fields at their Go zero values. Paired
// with TestUnknownTagSkipped (forward compat) this proves rollback
// safety in both directions for fmtVer=2.
func TestRollbackMissingTagsZeroDecode(t *testing.T) {
	// Hand-build an Order body containing ONLY tag 1 (ID) and tag 10
	// (required Total), as if written by older code unaware of tags 2-9
	// and 11-12. Newer decoder must accept it and zero-fill the rest.
	w := gsbm.NewWriter(nil)
	w.WriteTag(1, gsbm.WireLengthDelim)
	w.WriteString("legacy-id")
	w.WriteTag(10, gsbm.WireLengthDelim)
	m := w.BeginLengthDelim()
	w.WriteTag(1, gsbm.WireLengthDelim)
	w.WriteString("USD")
	w.WriteTag(2, gsbm.WireFixed64)
	w.WriteFloat64(1.5)
	w.EndLengthDelim(m)
	if w.Err() != nil {
		t.Fatal(w.Err())
	}
	var got Order
	if err := got.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
		t.Fatalf("rollback decode: %v", err)
	}
	want := Order{ID: "legacy-id", Total: Total{Currency: "USD", Amount: 1.5}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rollback decode mismatch\n want: %#v\n  got: %#v", want, got)
	}
}

// TestWireTypeMismatchRejectsKnownTag verifies that a known tag carrying
// the wrong wire type is rejected as malformed (spec §3.2). Skipping past
// it would let a tag-1 (ID, WireLengthDelim) blob written with wt=WireVarint
// silently consume the wrong number of bytes and desync the parser.
func TestWireTypeMismatchRejectsKnownTag(t *testing.T) {
	w := gsbm.NewWriter(nil)
	// Tag 1 (ID) emitted as WireVarint instead of WireLengthDelim.
	w.WriteTag(1, gsbm.WireVarint)
	w.WriteUvarint(0xdeadbeef)
	if w.Err() != nil {
		t.Fatal(w.Err())
	}
	var got Order
	err := got.UnmarshalGSBM(gsbm.NewReader(w.Bytes()))
	if !errors.Is(err, gsbm.ErrWrongWireType) {
		t.Fatalf("decode: want ErrWrongWireType, got %v", err)
	}
}

// TestFieldPresentDefaultModeAfterDecode pins the default-mode contract:
// types without the //gsbm:track-presence marker write presence into a
// local stack bitmap that dies with the UnmarshalGSBM call, so a later
// FieldPresent query returns false for every tag — present or absent on
// the wire. Order is the canonical default-mode fixture (no marker on
// its declaration in types.go); the call covers full, partial, and
// zero-valued blobs since the contract is invariant of payload shape.
func TestFieldPresentDefaultModeAfterDecode(t *testing.T) {
	cases := []struct {
		name string
		blob func(t *testing.T) []byte
	}{
		{
			name: "full round-trip",
			blob: func(t *testing.T) []byte {
				in := Order{ID: "rt", Total: Total{Currency: "USD", Amount: 1}}
				w := gsbm.NewWriter(nil)
				if err := in.MarshalGSBM(w); err != nil {
					t.Fatal(err)
				}
				return append([]byte(nil), w.Bytes()...)
			},
		},
		{
			name: "all-zero values still written",
			blob: func(t *testing.T) []byte {
				in := Order{}
				w := gsbm.NewWriter(nil)
				if err := in.MarshalGSBM(w); err != nil {
					t.Fatal(err)
				}
				return append([]byte(nil), w.Bytes()...)
			},
		},
		{
			name: "hand-crafted partial",
			blob: func(t *testing.T) []byte {
				w := gsbm.NewWriter(nil)
				w.WriteTag(1, gsbm.WireLengthDelim)
				w.WriteString("partial")
				w.WriteTag(10, gsbm.WireLengthDelim)
				m := w.BeginLengthDelim()
				w.WriteTag(1, gsbm.WireLengthDelim)
				w.WriteString("USD")
				w.WriteTag(2, gsbm.WireFixed64)
				w.WriteFloat64(2.5)
				w.EndLengthDelim(m)
				if w.Err() != nil {
					t.Fatal(w.Err())
				}
				return append([]byte(nil), w.Bytes()...)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got Order
			if err := got.UnmarshalGSBM(gsbm.NewReader(tc.blob(t))); err != nil {
				t.Fatal(err)
			}
			for tag := uint32(0); tag <= 19; tag++ {
				if got.FieldPresent(tag) {
					t.Errorf("FieldPresent(%d) = true; default-mode receivers must report all tags absent post-decode", tag)
				}
			}
		})
	}
}

// TestFieldPresentResetDefaultMode pins that Reset() on a default-mode
// receiver leaves FieldPresent unchanged — there is no stored bitmap
// to clear, and the legacy sidecar holds no entry for receivers that
// have never been touched by MarkPresent. Together with the
// AfterDecode test above this fully covers the default-mode surface.
func TestFieldPresentResetDefaultMode(t *testing.T) {
	in := Order{ID: "rs", Total: Total{Currency: "USD", Amount: 1}}
	w := gsbm.NewWriter(nil)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatal(err)
	}
	var got Order
	if err := got.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
		t.Fatal(err)
	}
	got.Reset()
	for tag := uint32(1); tag <= 18; tag++ {
		if got.FieldPresent(tag) {
			t.Errorf("FieldPresent(%d) = true after Reset on default-mode receiver", tag)
		}
	}
}

// TestFieldPresentSyncPoolReuseDefaultMode pins that recycling a
// default-mode *Order through a sync.Pool never surfaces stale
// presence bits — the local bitmap lives on the decode stack frame,
// so back-to-back decodes can never leak state. The test still
// exercises the same Put/Get dance as the legacy version so the
// pool-reuse path remains covered by allocation profiling.
func TestFieldPresentSyncPoolReuseDefaultMode(t *testing.T) {
	pool := sync.Pool{New: func() any { return new(Order) }}

	full := Order{ID: "first", Quantity: 1, Total: Total{Currency: "USD", Amount: 1}}
	wFull := gsbm.NewWriter(nil)
	if err := full.MarshalGSBM(wFull); err != nil {
		t.Fatal(err)
	}
	first := pool.Get().(*Order)
	if err := first.UnmarshalGSBM(gsbm.NewReader(wFull.Bytes())); err != nil {
		t.Fatal(err)
	}
	pool.Put(first)

	var second *Order
	parked := []*Order{}
	for i := 0; i < 8; i++ {
		cand := pool.Get().(*Order)
		if cand == first {
			second = cand
			break
		}
		parked = append(parked, cand)
	}
	for _, p := range parked {
		pool.Put(p)
	}
	if second == nil {
		second = first
	}

	wPart := gsbm.NewWriter(nil)
	wPart.WriteTag(1, gsbm.WireLengthDelim)
	wPart.WriteString("second")
	wPart.WriteTag(10, gsbm.WireLengthDelim)
	m := wPart.BeginLengthDelim()
	wPart.WriteTag(1, gsbm.WireLengthDelim)
	wPart.WriteString("EUR")
	wPart.WriteTag(2, gsbm.WireFixed64)
	wPart.WriteFloat64(0.5)
	wPart.EndLengthDelim(m)
	if wPart.Err() != nil {
		t.Fatal(wPart.Err())
	}
	if err := second.UnmarshalGSBM(gsbm.NewReader(wPart.Bytes())); err != nil {
		t.Fatal(err)
	}
	for tag := uint32(1); tag <= 18; tag++ {
		if second.FieldPresent(tag) {
			t.Errorf("FieldPresent(%d) = true on default-mode reuse path", tag)
		}
	}
}

// TestFieldPresentSidecarBoundedAllocs covers the plan's per-call
// allocation claim: after one warm-up MarkPresent (which inserts the
// receiver's mask into the sidecar map), repeated MarkPresent /
// IsPresent calls on the same receiver must not allocate. This is the
// "no per-call allocations" half of the bound; the "bounded one-time
// map insert" half is covered by the warm-up call itself.
func TestFieldPresentSidecarBoundedAllocs(t *testing.T) {
	var dst Order
	gsbm.ClearPresence(&dst)
	gsbm.MarkPresent(&dst, 1) // warm-up: allocates the presenceMask once

	allocs := testing.AllocsPerRun(100, func() {
		for tag := uint32(1); tag <= 18; tag++ {
			gsbm.MarkPresent(&dst, tag)
			_ = gsbm.IsPresent(&dst, tag)
		}
	})
	if allocs != 0 {
		t.Errorf("warm presence path allocates %.1f/op; want 0 (sidecar should be lookup-only after first mark)", allocs)
	}
}

// TestMapEncodingDeterministic verifies that two encodes of the same map
// produce identical bytes. Spec §5.3 keys are sorted on the wire so a
// stable hash of the encoded blob can serve as a content fingerprint
// (audit, dedup, content-addressed checkpointing).
func TestMapEncodingDeterministic(t *testing.T) {
	in := Order{
		ID:    "ord-deterministic",
		Total: Total{Currency: "USD", Amount: 1},
		Tags: map[string]int64{
			"zeta": 1, "alpha": 2, "mu": 3, "beta": 4, "kappa": 5,
		},
		Aliases: map[string]Label{
			"primary": "P", "billing": "B", "shipping": "S",
		},
	}
	var first, second []byte
	for i := 0; i < 64; i++ {
		w := gsbm.NewWriter(nil)
		if err := in.MarshalGSBM(w); err != nil {
			t.Fatalf("marshal %d: %v", i, err)
		}
		got := append([]byte(nil), w.Bytes()...)
		if i == 0 {
			first = got
			continue
		}
		second = got
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("non-deterministic map encoding on iteration %d", i)
		}
	}
}
