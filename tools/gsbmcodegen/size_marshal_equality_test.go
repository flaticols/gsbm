package gsbmcodegen_test

import (
	"testing"
	"time"

	"go.flaticols.dev/gsbm/storage/gsbm"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/aliasptr"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/borrowstrings"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/customcodec"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/cyclebreak"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/embed"
	addafter "go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/evolution/addfield/after"
	addbefore "go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/evolution/addfield/before"
	cwafter "go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/evolution/compatwrite/after"
	cwbefore "go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/evolution/compatwrite/before"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/graph"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/importcollision"
	icacommon "go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/importcollision/pkg/a/common"
	icbcommon "go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/importcollision/pkg/b/common"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/intwidth"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/namedkey"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/nestedcomp"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/sample"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/trackpresence"
)

// TestSizeMatchesMarshal is the load-bearing equality property test.
// For every generated type in the repo's fixture packages, we exercise
// representative values (zero, populated, nested, optional present /
// absent, slice / map empty / populated) and assert
//
//	v.SizeGSBM() == len(body bytes produced by v.MarshalGSBM())
//
// Any drift between the two field-walking templates breaks the
// gsbm.Marshal bodyLen invariant and shows up here.
func TestSizeMatchesMarshal(t *testing.T) {
	type sm interface {
		SizeGSBM() int
		MarshalGSBM(*gsbm.Writer) error
	}

	cases := []struct {
		name string
		v    sm
	}{
		// --- sample fixture ---
		{"sample/Order/zero", &sample.Order{}},
		{"sample/Order/populated", &sample.Order{
			ID:       "ord-001",
			Quantity: 7,
			Price:    19.99,
			Active:   true,
			Note:     ptr("first try"),
			Customer: &sample.Customer{Name: "Ada", Email: "ada@example.com"},
			Items: []sample.Item{
				{SKU: "abc", Count: 1},
				{SKU: "def", Count: 42},
			},
			Tags:       map[string]int64{"a": 1, "b": 2},
			Payload:    []byte{0x01, 0x02, 0x03, 0xff},
			Total:      sample.Total{Currency: "USD", Amount: 39.99},
			Counts:     []int64{-3, 0, 7},
			Aliases:    map[string]sample.Label{"primary": "ada", "billing": "Adams"},
			Qty:        99,
			OptQty:     qtyPtr(42),
			QtyList:    []sample.Quantity{1, 2, 3},
			OptLabel:   labelPtr("rush"),
			LabelList:  []sample.Label{"alpha", "beta"},
			OptPayload: bytesPtr([]byte{0xde, 0xad}),
		}},
		{"sample/Order/note-zero-elide", &sample.Order{ID: "id", Note: ptr("")}},
		{"sample/Order/empty-payload-elide", &sample.Order{OptPayload: bytesPtr(nil)}},
		{"sample/Order/empty-payload-zero", &sample.Order{OptPayload: bytesPtr([]byte{})}},
		{"sample/Customer/zero", &sample.Customer{}},
		{"sample/Customer/populated", &sample.Customer{Name: "Ada", Email: "ada@example.com"}},
		{"sample/Item/zero", &sample.Item{}},
		{"sample/Item/populated", &sample.Item{SKU: "abc", Count: 42}},
		{"sample/Total/zero", &sample.Total{}},
		{"sample/Total/populated", &sample.Total{Currency: "USD", Amount: 1.23}},
		{"sample/Renamed/zero", &sample.Renamed{}},
		{"sample/Renamed/populated", &sample.Renamed{ID: "r1", LegacyCode: "old", RetailCode: "new"}},

		// --- aliasptr fixture ---
		{"aliasptr/Batch/zero", &aliasptr.Batch{}},
		{"aliasptr/Batch/populated", &aliasptr.Batch{
			Items: []*aliasptr.Item{
				{SKU: "a", Note: ptr("note-a")},
				nil,
				{SKU: "c"},
			},
			Optional:       []*aliasptr.OptionalNote{nil, {Value: "x"}},
			Groups:         aliasptr.ItemList{{SKU: "g1"}, {SKU: "g2", Note: ptr("g2n")}},
			OptionalGroups: aliasptr.ItemPtrList{{SKU: "p1"}, nil},
		}},

		// --- cyclebreak fixture ---
		{"cyclebreak/Item/zero", &cyclebreak.Item{}},
		{"cyclebreak/Item/with-previous", &cyclebreak.Item{
			ID:       "i2",
			Label:    "second",
			Previous: &cyclebreak.Item{ID: "i1", Label: "first"},
		}},

		// --- embed fixture ---
		{"embed/Extended/zero", &embed.Extended{}},
		{"embed/Extended/populated", &embed.Extended{Base: embed.Base{Total: 99}, Reason: "ship"}},
		{"embed/Flat/zero", &embed.Flat{}},
		{"embed/Flat/populated", &embed.Flat{Total: 99, Reason: "ship"}},
		{"embed/PtrExtended/nil", &embed.PtrExtended{Reason: "no-base"}},
		{"embed/PtrExtended/with-base", &embed.PtrExtended{Base: &embed.Base{Total: 7}, Reason: "ok"}},
		// Non-nil pointer whose pointee SizeGSBM() is zero — exercises the
		// presence-byte accounting in the optional-named-struct envelope
		// distinct from both nil and populated.
		{"embed/PtrExtended/empty-base", &embed.PtrExtended{Base: &embed.Base{}, Reason: "ok"}},
		{"embed/Deep/zero", &embed.Deep{}},
		{"embed/Deep/populated", &embed.Deep{
			Mid:    embed.Mid{Base: embed.Base{Total: 1}, Note: "n"},
			Caller: "c",
		}},
		{"embed/WrappedPtr/nil", &embed.WrappedPtr{}},
		{"embed/WrappedPtr/with-base", &embed.WrappedPtr{
			PtrCarrier: embed.PtrCarrier{Base: &embed.Base{Total: 2}, Note: "n"},
			Caller:     "c",
		}},
		{"embed/DoublePtr/nil", &embed.DoublePtr{}},
		{"embed/DoublePtr/with-mid", &embed.DoublePtr{
			PtrMid: &embed.PtrMid{Base: &embed.Base{Total: 5}},
			Caller: "c",
		}},

		// --- graph fixture ---
		{"graph/Catalog/zero", &graph.Catalog{}},
		{"graph/Catalog/populated", &graph.Catalog{
			ID: "cat",
			Sections: []graph.Section{
				{Name: "s1", Items: []graph.Item{{SKU: "a"}, {SKU: "b", Note: ptr("note")}}},
			},
			Tags: map[string]graph.Tag{
				"alpha": {Slug: "a", Weight: int64Ptr(3)},
				"beta":  {Slug: "b"},
			},
			Tail: graph.EdgeMarker{Marker: true},
		}},
		{"graph/Section/zero", &graph.Section{}},
		{"graph/Item/zero", &graph.Item{}},
		{"graph/Item/with-note", &graph.Item{SKU: "x", Note: ptr("n")}},
		{"graph/Tag/zero", &graph.Tag{}},
		{"graph/Tag/with-weight", &graph.Tag{Slug: "s", Weight: int64Ptr(7)}},
		{"graph/EdgeMarker/zero", &graph.EdgeMarker{}},
		{"graph/EdgeMarker/true", &graph.EdgeMarker{Marker: true}},

		// --- namedkey fixture ---
		{"namedkey/Counts/zero", &namedkey.Counts{}},
		{"namedkey/Counts/populated", &namedkey.Counts{
			ByCode:     map[namedkey.Code]int64{"a": 1, "b": -2},
			BySeverity: map[namedkey.Severity]int64{1: 7, -1: 3},
			ByBucket:   map[namedkey.Bucket]int64{42: 99, 1: 1},
			ByFlag:     map[namedkey.Flag]int64{true: 1, false: 0},
		}},

		// --- nestedcomp fixture ---
		{"nestedcomp/Index/zero", &nestedcomp.Index{}},
		{"nestedcomp/Index/populated", &nestedcomp.Index{
			IDsByGroup: map[string][]string{
				"a": {"x", "y"},
				"b": {"z"},
			},
			LabelsByGroup: map[string]map[string]string{
				"g": {"k1": "v1", "k2": "v2"},
			},
			MetadataVariants: []map[string]string{
				{"k": "v"},
				{},
			},
			Deep: map[string][]map[string]int64{
				"g1": {{"a": 1}, {"b": 2}},
			},
		}},

		// --- evolution/addfield fixture ---
		{"evolution/addfield/before/Shipment/zero", &addbefore.Shipment{}},
		{"evolution/addfield/before/Shipment/populated", &addbefore.Shipment{
			Carrier: "ups", TrackingID: "1Z",
		}},
		{"evolution/addfield/after/Shipment/zero", &addafter.Shipment{}},
		{"evolution/addfield/after/Shipment/populated", &addafter.Shipment{
			Carrier: "ups", TrackingID: "1Z", Weight: 4321,
		}},

		// --- evolution/compatwrite fixture ---
		{"evolution/compatwrite/before/Shipment/zero", &cwbefore.Shipment{}},
		{"evolution/compatwrite/before/Shipment/populated", &cwbefore.Shipment{
			ID: "s1", Carrier: "ups",
		}},
		{"evolution/compatwrite/after/Shipment/zero", &cwafter.Shipment{}},
		{"evolution/compatwrite/after/Shipment/populated", &cwafter.Shipment{
			ID: "s1", Carrier: "ups", CarrierCode: "UPS-001",
		}},

		// --- customcodec fixture ---
		{"customcodec/Record/zero", &customcodec.Record{
			CreatedAt: time.Unix(0, 0),
			Amount:    mustDecimal(t, "0"),
		}},
		{"customcodec/Record/populated", &customcodec.Record{
			CreatedAt:  time.Unix(1700000000, 42).UTC(),
			Amount:     mustDecimal(t, "123.45"),
			OptionalAt: timePtr(time.Unix(1700000100, 0).UTC()),
		}},
		{"customcodec/Record/nil-optional", &customcodec.Record{
			CreatedAt: time.Unix(1, 0),
			Amount:    mustDecimal(t, "1"),
			// OptionalAt: nil
		}},
		// Pointer-to-zero (PresenceNonZero on the wire, codec runs on the
		// zero time). Plan task 3 calls this state out explicitly — the
		// envelope must NOT collapse to PresenceNil just because the
		// pointee is zero, so SizeGSBM and MarshalGSBM must agree on the
		// codec-payload byte count for the time.Time zero value.
		{"customcodec/Record/ptr-zero-optional", &customcodec.Record{
			CreatedAt:  time.Unix(2, 0),
			Amount:     mustDecimal(t, "2"),
			OptionalAt: timePtr(time.Time{}),
		}},
		// Pointer-to-non-zero (the third state). Distinct from
		// /populated above because that test also exercises non-zero
		// CreatedAt and a multi-digit Amount; this case isolates
		// OptionalAt presence with the other fields zeroed.
		{"customcodec/Record/ptr-nonzero-optional", &customcodec.Record{
			CreatedAt:  time.Unix(0, 0),
			Amount:     mustDecimal(t, "0"),
			OptionalAt: timePtr(time.Unix(1700000200, 0).UTC()),
		}},
		// All custom-codec field kinds populated at once: analytic
		// (DecimalString, DecimalBinary), materializing append-codec
		// (DecimalAppend), and streaming (StreamingJSON). The invariant
		// must hold across every codec category.
		{"customcodec/Record/all-codecs", &customcodec.Record{
			CreatedAt:    time.Unix(1700000300, 7).UTC(),
			Amount:       mustDecimal(t, "-1.500"),
			OptionalAt:   timePtr(time.Unix(1700000400, 0).UTC()),
			AmountAppend: mustDecimal(t, "42.000"),
			Payload:      customcodec.LargePayload{Tag: "p", Data: []byte{0xde, 0xad, 0xbe, 0xef}},
			AmountBinary: mustDecimal(t, "12.500"),
		}},
		// Container nests Record by value, by slice, by map, and by
		// pointer-slice. Each nested-child site is a WriteLength-vs-
		// BeginLengthDelim decision point on the encode side: the
		// materializing-codec fallback emits BeginLengthDelim because
		// child.SizeGSBM() runs the codec against a fresh
		// CountingWriter while child.MarshalGSBM hits the shared scratch
		// cache. The invariant is what pins the fallback's
		// byte-equivalence to the analytic path.
		{"customcodec/Container/zero", &customcodec.Container{}},
		{"customcodec/Container/value-only", &customcodec.Container{
			Inner: customcodec.Record{
				CreatedAt:    time.Unix(1700000500, 0).UTC(),
				Amount:       mustDecimal(t, "100.00"),
				AmountAppend: mustDecimal(t, "100.00"),
			},
		}},
		{"customcodec/Container/full", &customcodec.Container{
			Inner: customcodec.Record{
				CreatedAt:    time.Unix(1700000500, 0).UTC(),
				Amount:       mustDecimal(t, "1.5"),
				OptionalAt:   timePtr(time.Unix(1700000600, 0).UTC()),
				AmountAppend: mustDecimal(t, "2.25"),
				Payload:      customcodec.LargePayload{Tag: "inner", Data: []byte{0x01}},
				AmountBinary: mustDecimal(t, "3.0"),
			},
			Items: []customcodec.Record{
				{
					CreatedAt:    time.Unix(1700000700, 0).UTC(),
					Amount:       mustDecimal(t, "10"),
					AmountAppend: mustDecimal(t, "10"),
				},
				{
					CreatedAt:    time.Unix(1700000800, 0).UTC(),
					Amount:       mustDecimal(t, "-20.5"),
					AmountAppend: mustDecimal(t, "-20.5"),
				},
			},
			ByKey: map[string]customcodec.Record{
				"a": {
					CreatedAt:    time.Unix(1700000900, 0).UTC(),
					Amount:       mustDecimal(t, "0.001"),
					AmountAppend: mustDecimal(t, "0.001"),
				},
				"b": {
					CreatedAt:    time.Unix(1700001000, 0).UTC(),
					Amount:       mustDecimal(t, "999999"),
					AmountAppend: mustDecimal(t, "999999"),
				},
			},
			PtrItems: []*customcodec.Record{
				nil,
				{
					CreatedAt:    time.Unix(1700001100, 0).UTC(),
					Amount:       mustDecimal(t, "7.7"),
					AmountAppend: mustDecimal(t, "7.7"),
				},
				nil,
			},
			// Pages and Buckets exercise the transitive composite-fallback
			// path: outer slice whose element is a slice / map carrying a
			// materializing-codec field. The slice-encode composite guard
			// must route the outer envelope through BeginLengthDelim so the
			// declared length observes what the inner emit writes.
			Pages: [][]customcodec.Record{
				{
					{CreatedAt: time.Unix(1700001200, 0).UTC(), Amount: mustDecimal(t, "11"), AmountAppend: mustDecimal(t, "11")},
					{CreatedAt: time.Unix(1700001201, 0).UTC(), Amount: mustDecimal(t, "22.5"), AmountAppend: mustDecimal(t, "22.5")},
				},
				{
					{CreatedAt: time.Unix(1700001202, 0).UTC(), Amount: mustDecimal(t, "-3"), AmountAppend: mustDecimal(t, "-3")},
				},
			},
			Buckets: []map[string]customcodec.Record{
				{
					"a": {CreatedAt: time.Unix(1700001300, 0).UTC(), Amount: mustDecimal(t, "100"), AmountAppend: mustDecimal(t, "100")},
					"b": {CreatedAt: time.Unix(1700001301, 0).UTC(), Amount: mustDecimal(t, "200"), AmountAppend: mustDecimal(t, "200")},
				},
				{
					"only": {CreatedAt: time.Unix(1700001302, 0).UTC(), Amount: mustDecimal(t, "300"), AmountAppend: mustDecimal(t, "300")},
				},
			},
		}},

		// --- borrowstrings fixture ---
		// Borrow-strings is a decode-side opt-in; SizeGSBM and MarshalGSBM
		// are unchanged by the borrow flag, but pin both PlainRecord and
		// BorrowRecord so a regression in the borrow path's emit can't
		// silently change byte counts.
		{"borrowstrings/PlainRecord/zero", &borrowstrings.PlainRecord{}},
		{"borrowstrings/PlainRecord/populated", &borrowstrings.PlainRecord{
			ID:     "p-1",
			Note:   ptr("n"),
			Names:  []string{"a", "bb"},
			Labels: []borrowstrings.Label{"x", "yy"},
			Tags:   map[string]string{"k1": "v1", "k2": "v2"},
		}},
		{"borrowstrings/BorrowRecord/zero", &borrowstrings.BorrowRecord{}},
		{"borrowstrings/BorrowRecord/populated", &borrowstrings.BorrowRecord{
			ID:     "b-1",
			Note:   ptr("n"),
			Names:  []string{"a", "bb"},
			Labels: []borrowstrings.Label{"x", "yy"},
			Tags:   map[string]string{"k1": "v1", "k2": "v2"},
		}},

		// --- importcollision fixture ---
		// Cross-package element types whose Go package names collide. The
		// invariant must hold on the parent Record and on each common.Value
		// independently.
		{"importcollision/Record/zero", &importcollision.Record{}},
		{"importcollision/Record/populated", &importcollision.Record{
			A: []icacommon.Value{{Label: "a1", Count: 1}, {Label: "a2", Count: 2}},
			B: []icbcommon.Value{{Token: "t1", Score: 1.5}, {Token: "t2", Score: 2.5}},
		}},
		{"importcollision/a/Value/zero", &icacommon.Value{}},
		{"importcollision/a/Value/populated", &icacommon.Value{Label: "label", Count: 42}},
		{"importcollision/b/Value/zero", &icbcommon.Value{}},
		{"importcollision/b/Value/populated", &icbcommon.Value{Token: "tok", Score: 3.14}},

		// --- intwidth fixture ---
		// Wire-width overrides on every supported integer kind. The
		// invariant pins that the override's encoded byte count is
		// reflected in SizeGSBM identically to what MarshalGSBM emits.
		{"intwidth/Record/zero", &intwidth.Record{}},
		{"intwidth/Record/populated", &intwidth.Record{Small: 123, Large: 1 << 40}},
		{"intwidth/Record/negative", &intwidth.Record{Small: -7, Large: -(1 << 40)}},
		{"intwidth/WideRecord/zero", &intwidth.WideRecord{}},
		{"intwidth/WideRecord/populated", &intwidth.WideRecord{
			Uint:           1 << 50,
			Uintptr:        1 << 33,
			NarrowSigned:   -1234,
			NarrowUnsigned: 200,
			Identity:       77,
			NamedAlias:     intwidth.UserID(-99),
		}},

		// --- trackpresence fixture ---
		// The hidden gsbmPresent field carries no bin tag, so SizeGSBM and
		// MarshalGSBM must skip it entirely. A regression that leaked the
		// field into either template would produce a size/marshal mismatch
		// here even before FieldPresent diverges.
		{"trackpresence/Offer/zero", &trackpresence.Offer{}},
		{"trackpresence/Offer/populated", &trackpresence.Offer{
			ID:       "ofr-1",
			Quantity: 7,
			Price:    1.23,
			Active:   true,
			Note:     ptr("note"),
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.v.SizeGSBM()
			w := gsbm.NewWriter(nil)
			if err := tc.v.MarshalGSBM(w); err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := w.Err(); err != nil {
				t.Fatalf("writer err: %v", err)
			}
			got := len(w.Bytes())
			if got != want {
				t.Errorf("SizeGSBM=%d but marshal produced %d bytes", want, got)
			}

			cw := gsbm.NewCountingWriter()
			if err := tc.v.MarshalGSBM(cw); err != nil {
				t.Fatalf("marshal into CountingWriter: %v", err)
			}
			if err := cw.Err(); err != nil {
				t.Fatalf("CountingWriter err: %v", err)
			}
			if cw.Size() != want {
				t.Errorf("CountingWriter.Size=%d but SizeGSBM=%d", cw.Size(), want)
			}
		})
	}
}

// TestSizeGSBMAllocsZero pins the Task 4 promise that analytic SizeGSBM
// is allocation-free on generated types. Materializing- and streaming-
// codec fields are documented exemptions because the size-side fallback
// runs the codec into a per-call CountingWriter; the customcodec fixture
// carries both shapes and is exempted explicitly below.
func TestSizeGSBMAllocsZero(t *testing.T) {
	type sizer interface{ SizeGSBM() int }
	cases := []struct {
		name string
		v    sizer
	}{
		{"sample/Order/populated", &sample.Order{
			ID:       "ord-001",
			Quantity: 7,
			Price:    19.99,
			Active:   true,
			Note:     ptr("note"),
			Customer: &sample.Customer{Name: "Ada", Email: "ada@example.com"},
			Items:    []sample.Item{{SKU: "a", Count: 1}, {SKU: "b", Count: 2}},
			Tags:     map[string]int64{"k": 1},
			Payload:  []byte{0x01, 0x02},
			Total:    sample.Total{Currency: "USD", Amount: 1.23},
			Counts:   []int64{1, 2, 3},
			QtyList:  []sample.Quantity{1, 2},
		}},
		{"graph/Catalog/populated", &graph.Catalog{
			Sections: []graph.Section{
				{Name: "s1", Items: []graph.Item{{SKU: "i1"}}},
			},
		}},
		{"embed/Extended/populated", &embed.Extended{Base: embed.Base{Total: 1}, Reason: "ok"}},
		{"trackpresence/Offer/populated", &trackpresence.Offer{ID: "x", Note: ptr("n")}},
		// nestedcomp.Index exercises the deepest map-of-slice-of-map shape;
		// populate every field so the zero-alloc claim covers the analytic
		// body-sum loops, not just empty branches.
		{"nestedcomp/Index/populated", &nestedcomp.Index{
			IDsByGroup:       map[string][]string{"a": {"x", "y"}, "b": {"z"}},
			LabelsByGroup:    map[string]map[string]string{"g": {"k1": "v1", "k2": "v2"}},
			MetadataVariants: []map[string]string{{"k": "v"}, {}},
			Deep:             map[string][]map[string]int64{"g1": {{"a": 1}, {"b": 2}}},
		}},
		// Bool-keyed map plus other named-key shapes — exercises the
		// emitMapSize `_` underscore branch and the named-key cast paths.
		{"namedkey/Counts/populated", &namedkey.Counts{
			ByCode:     map[namedkey.Code]int64{"a": 1, "b": -2},
			BySeverity: map[namedkey.Severity]int64{1: 7, -1: 3},
			ByBucket:   map[namedkey.Bucket]int64{42: 99, 1: 1},
			ByFlag:     map[namedkey.Flag]int64{true: 1, false: 0},
		}},
		// Slice-of-pointer-to-named-struct — exercises the per-element
		// presence-byte envelope (1 + SizeGSBM()) on the size side.
		{"aliasptr/Batch/populated", &aliasptr.Batch{
			Items:          []*aliasptr.Item{{SKU: "a", Note: ptr("n")}, nil, {SKU: "c"}},
			Optional:       []*aliasptr.OptionalNote{nil, {Value: "x"}},
			Groups:         aliasptr.ItemList{{SKU: "g1"}, {SKU: "g2", Note: ptr("g2n")}},
			OptionalGroups: aliasptr.ItemPtrList{{SKU: "p1"}, nil},
		}},
		// Wire-width-override integer kinds — exercises emitIntSize's
		// WireOverrideCompat dispatch on signed and unsigned widths.
		{"intwidth/WideRecord/populated", &intwidth.WideRecord{
			Uint:           1 << 50,
			Uintptr:        1 << 33,
			NarrowSigned:   -1234,
			NarrowUnsigned: 200,
			Identity:       77,
			NamedAlias:     intwidth.UserID(-99),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := testing.AllocsPerRun(100, func() {
				_ = tc.v.SizeGSBM()
			})
			if got != 0 {
				t.Errorf("SizeGSBM allocs/op = %v, want 0", got)
			}
		})
	}
}

func ptr[T any](v T) *T                         { return &v }
func qtyPtr(q sample.Quantity) *sample.Quantity { return &q }
func labelPtr(l sample.Label) *sample.Label     { return &l }
func bytesPtr(b []byte) *[]byte                 { return &b }
func int64Ptr(v int64) *int64                   { return &v }
func timePtr(t time.Time) *time.Time            { return &t }

func mustDecimal(t *testing.T, s string) customcodec.DecimalAmount {
	t.Helper()
	d, err := customcodec.ParseDecimalAmount(s)
	if err != nil {
		t.Fatalf("ParseDecimalAmount(%q): %v", s, err)
	}
	return d
}
