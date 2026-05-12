package gsbmcodegen_test

import (
	"testing"
	"time"

	"go.flaticols.dev/gsbm/storage/gsbm"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/aliasptr"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/customcodec"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/cyclebreak"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/embed"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/graph"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/namedkey"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/nestedcomp"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/sample"
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
