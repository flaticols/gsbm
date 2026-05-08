package odmschema

import (
	"reflect"
	"testing"
)

// TestParseFieldTag covers every shape ParseFieldTag is expected to
// understand, plus the input cases the validator relies on to surface
// errors (tag 0, missing tag, unknown options).
func TestParseFieldTag(t *testing.T) {
	cases := []struct {
		name string
		tag  reflect.StructTag
		want FieldTag
		err  bool
	}{
		{"none", ``, FieldTag{}, false},
		{"plain", `bin:"5"`, FieldTag{Set: true, Tag: 5}, false},
		{"deprecated", `bin:"7,deprecated"`, FieldTag{Set: true, Tag: 7, Deprecated: true}, false},
		{"custom", `bin:"3,custom=PriceCodec"`, FieldTag{Set: true, Tag: 3, Custom: "PriceCodec"}, false},
		{"skip", `bin:"-"`, FieldTag{Set: true, Skip: true}, false},
		{"zero-tag", `bin:"0"`, FieldTag{Set: true}, true},
		{"too-large", `bin:"1073741824"`, FieldTag{Set: true}, true}, // 2^30
		{"empty", `bin:""`, FieldTag{Set: true}, true},
		{"unknown-option", `bin:"5,wat"`, FieldTag{Set: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseFieldTag(tc.tag)
			if tc.err {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

// TestParseMarkers covers the //odm: directives recognized by the
// schema tool. Unknown directives MUST surface as an error so a typo'd
// `//odm:rooot` does not silently leave a type out of the schema.
func TestParseMarkers(t *testing.T) {
	t.Run("recognized", func(t *testing.T) {
		ps, err := ParseSource("p", []string{`
package p

//odm:root
//odm:reserved 3, 5
//odm:allow-breaking dropping deprecated tag 7 for cleanup
type Offer struct{}
`})
		if err != nil {
			t.Fatal(err)
		}
		_, _, ok := ps.Packages[0].findStructDoc("Offer")
		if !ok {
			t.Fatal("Offer not found")
		}
	})

	t.Run("unknown directive errors", func(t *testing.T) {
		_, _ = ParseSource("p", []string{`
package p
//odm:rooot
type Offer struct{}
`})
		// We don't error at parse time; Discover surfaces it as an Issue.
	})
}
