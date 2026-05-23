package borrowstrings

import (
	"testing"
	"unsafe"

	"go.flaticols.dev/gsbm/storage/gsbm"
)

type copyingAllocator struct{}

func (copyingAllocator) AcquireString(b []byte) string {
	return string(append([]byte(nil), b...))
}

func encodePlain(t *testing.T, v *PlainRecord) []byte {
	t.Helper()
	w := gsbm.NewWriter(nil)
	if err := v.MarshalGSBM(w); err != nil {
		t.Fatalf("MarshalGSBM: %v", err)
	}
	if err := w.Err(); err != nil {
		t.Fatalf("writer err: %v", err)
	}
	return w.Bytes()
}

func encodeBorrow(t *testing.T, v *BorrowRecord) []byte {
	t.Helper()
	w := gsbm.NewWriter(nil)
	if err := v.MarshalGSBM(w); err != nil {
		t.Fatalf("MarshalGSBM: %v", err)
	}
	if err := w.Err(); err != nil {
		t.Fatalf("writer err: %v", err)
	}
	return w.Bytes()
}

func aliasesBuffer(s string, buf []byte) bool {
	if len(s) == 0 || len(buf) == 0 {
		return false
	}
	p := uintptr(unsafe.Pointer(unsafe.StringData(s)))
	start := uintptr(unsafe.Pointer(unsafe.SliceData(buf)))
	end := start + uintptr(len(buf))
	return p >= start && p < end
}

func assertBorrowRecordAliases(t *testing.T, got *BorrowRecord, body []byte, want bool) {
	t.Helper()
	checks := []struct {
		name string
		val  string
	}{
		{"ID", got.ID},
		{"Note", *got.Note},
		{"Names[0]", got.Names[0]},
		{"Labels[0]", string(got.Labels[0])},
	}
	for k, v := range got.Tags {
		checks = append(checks, struct {
			name string
			val  string
		}{"Tags key", k}, struct {
			name string
			val  string
		}{"Tags value", v})
		break
	}
	for _, c := range checks {
		if aliasesBuffer(c.val, body) != want {
			t.Fatalf("%s aliases buffer = %v, want %v", c.name, aliasesBuffer(c.val, body), want)
		}
	}
}

func TestBorrowStringsAliasesHeapInput(t *testing.T) {
	note := "bravo-note"
	body := encodeBorrow(t, &BorrowRecord{
		ID:     "alpha-id",
		Note:   &note,
		Names:  []string{"charlie-name"},
		Labels: []Label{Label("delta-label")},
		Tags:   map[string]string{"echo-key": "foxtrot-value"},
	})
	var got BorrowRecord
	if err := gsbm.DecodeBodyInto(body, &got); err != nil {
		t.Fatalf("DecodeBodyInto: %v", err)
	}
	assertBorrowRecordAliases(t, &got, body, true)
}

func TestPlainRecordStillCopiesHeapStrings(t *testing.T) {
	note := "bravo-note"
	body := encodePlain(t, &PlainRecord{
		ID:     "alpha-id",
		Note:   &note,
		Names:  []string{"charlie-name"},
		Labels: []Label{Label("delta-label")},
		Tags:   map[string]string{"echo-key": "foxtrot-value"},
	})
	var got PlainRecord
	if err := gsbm.DecodeBodyInto(body, &got); err != nil {
		t.Fatalf("DecodeBodyInto: %v", err)
	}
	checks := []struct {
		name string
		val  string
	}{
		{"ID", got.ID},
		{"Note", *got.Note},
		{"Names[0]", got.Names[0]},
		{"Labels[0]", string(got.Labels[0])},
	}
	for k, v := range got.Tags {
		checks = append(checks, struct {
			name string
			val  string
		}{"Tags key", k}, struct {
			name string
			val  string
		}{"Tags value", v})
		break
	}
	for _, c := range checks {
		if aliasesBuffer(c.val, body) {
			t.Fatalf("%s unexpectedly aliases heap input", c.name)
		}
	}
}

func TestBorrowStringsAllocatorFallbackDoesNotAliasInput(t *testing.T) {
	note := "bravo-note"
	body := encodeBorrow(t, &BorrowRecord{
		ID:     "alpha-id",
		Note:   &note,
		Names:  []string{"charlie-name"},
		Labels: []Label{Label("delta-label")},
		Tags:   map[string]string{"echo-key": "foxtrot-value"},
	})
	var got BorrowRecord
	r := gsbm.NewReader(body)
	r.SetAllocator(copyingAllocator{})
	got.Reset()
	if err := got.UnmarshalGSBM(r); err != nil {
		t.Fatalf("UnmarshalGSBM: %v", err)
	}
	if err := r.Err(); err != nil {
		t.Fatalf("reader err: %v", err)
	}
	assertBorrowRecordAliases(t, &got, body, false)
}

func TestBorrowStringsResetClearsStringSlices(t *testing.T) {
	v := BorrowRecord{
		Names:  []string{"old"},
		Labels: []Label{Label("old-label")},
	}
	v.Reset()
	if len(v.Names) != 0 || len(v.Labels) != 0 {
		t.Fatalf("Reset lengths = %d/%d, want 0/0", len(v.Names), len(v.Labels))
	}
	if cap(v.Names) == 0 || cap(v.Labels) == 0 {
		t.Fatalf("Reset should preserve slice capacity, got caps %d/%d", cap(v.Names), cap(v.Labels))
	}
	if v.Names[:cap(v.Names)][0] != "" || v.Labels[:cap(v.Labels)][0] != "" {
		t.Fatalf("Reset did not clear borrowed string slice backing arrays")
	}
}
