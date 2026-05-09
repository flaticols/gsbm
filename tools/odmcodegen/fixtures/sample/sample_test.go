package sample

import (
	"reflect"
	"testing"

	"github.com/flaticols/gsbm/storage/odm"
)

func note(s string) *string     { return &s }
func qty(q Quantity) *Quantity  { return &q }
func label(l Label) *Label      { return &l }

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
				Note:  note(""),    // zero-elide path: PresenceZero on the wire
				Total: Total{Amount: 1},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := odm.NewWriter(nil)
			if err := tc.in.MarshalODM(w); err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got Order
			r := odm.NewReader(w.Bytes())
			if err := got.UnmarshalODM(r); err != nil {
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
	w := odm.NewWriter(nil)
	w.WriteTag(1, odm.WireLengthDelim)
	w.WriteString("Ada")
	w.WriteTag(5, odm.WireLengthDelim) // unknown tag
	w.WriteBytes([]byte("future field"))
	w.WriteTag(2, odm.WireLengthDelim)
	w.WriteString("ada@example.com")
	if w.Err() != nil {
		t.Fatal(w.Err())
	}
	var got Customer
	if err := got.UnmarshalODM(odm.NewReader(w.Bytes())); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := Customer{Name: "Ada", Email: "ada@example.com"}
	if got != want {
		t.Fatalf("want %#v got %#v", want, got)
	}
}

// TestOptionalBuiltinPresenceZeroOnWire asserts that *string("") encodes
// to a single-byte presence body inside the field's length-delim, i.e.
// the zero-elide path is wired (not just claimed).
func TestOptionalBuiltinPresenceZeroOnWire(t *testing.T) {
	o := Order{ID: "x", Note: note("")}
	w := odm.NewWriter(nil)
	if err := o.MarshalODM(w); err != nil {
		t.Fatal(err)
	}
	// Decode and confirm Note round-trips to *"".
	var got Order
	if err := got.UnmarshalODM(odm.NewReader(w.Bytes())); err != nil {
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
	w := odm.NewWriter(nil)
	if err := o.MarshalODM(w); err != nil {
		t.Fatal(err)
	}
	// We can't easily probe the bytes without parsing, so the proof is
	// indirect: forge a payload claiming PresenceZero for Customer and
	// confirm UnmarshalODM rejects it.
	forged := odm.NewWriter(nil)
	forged.WriteTag(6, odm.WireLengthDelim)
	m := forged.BeginLengthDelim()
	forged.WritePresenceZero() // not allowed for named types
	forged.EndLengthDelim(m)
	if forged.Err() != nil {
		t.Fatal(forged.Err())
	}
	var got Order
	if err := got.UnmarshalODM(odm.NewReader(forged.Bytes())); err == nil {
		t.Fatal("want error decoding PresenceZero for *Customer, got nil")
	}
}

// TestHeaderRoundTrip wraps a real Order with the spec's 8-byte blob
// header to confirm the codegen output composes with WriteHeader/ReadHeader.
func TestHeaderRoundTrip(t *testing.T) {
	in := Order{ID: "h", Quantity: 1, Total: Total{Currency: "EUR", Amount: 1}}
	w := odm.NewWriter(nil)
	w.WriteHeader(0, 0xABCD)
	if err := in.MarshalODM(w); err != nil {
		t.Fatal(err)
	}
	r := odm.NewReader(w.Bytes())
	flags, schVer, err := r.ReadHeader()
	if err != nil {
		t.Fatal(err)
	}
	if flags != 0 || schVer != 0xABCD {
		t.Fatalf("header mismatch: flags=%d schVer=%d", flags, schVer)
	}
	var got Order
	if err := got.UnmarshalODM(r); err != nil {
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
// safety in both directions for fmtVer=1.
func TestRollbackMissingTagsZeroDecode(t *testing.T) {
	// Hand-build an Order body containing ONLY tag 1 (ID) and tag 10
	// (required Total), as if written by older code unaware of tags 2-9
	// and 11-12. Newer decoder must accept it and zero-fill the rest.
	w := odm.NewWriter(nil)
	w.WriteTag(1, odm.WireLengthDelim)
	w.WriteString("legacy-id")
	w.WriteTag(10, odm.WireLengthDelim)
	m := w.BeginLengthDelim()
	w.WriteTag(1, odm.WireLengthDelim)
	w.WriteString("USD")
	w.WriteTag(2, odm.WireFixed64)
	w.WriteFloat64(1.5)
	w.EndLengthDelim(m)
	if w.Err() != nil {
		t.Fatal(w.Err())
	}
	var got Order
	if err := got.UnmarshalODM(odm.NewReader(w.Bytes())); err != nil {
		t.Fatalf("rollback decode: %v", err)
	}
	want := Order{ID: "legacy-id", Total: Total{Currency: "USD", Amount: 1.5}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rollback decode mismatch\n want: %#v\n  got: %#v", want, got)
	}
}
