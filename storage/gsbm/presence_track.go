package gsbm

import (
	"sync"
	"sync/atomic"
	"unsafe"
)

// MaxTrackedTag is the largest tag whose presence is tracked by the sidecar
// bitmap. Tags above this cap silently no-op on MarkPresent and return false
// from IsPresent. The cap is the bit-width of presenceMask.
const MaxTrackedTag uint32 = 1024

// presenceMask is a fixed-width bit set covering tags 1..MaxTrackedTag.
// Bit (tag-1) is set when the corresponding tag was decoded into the
// receiver. Tag 0 is reserved by the wire format and is never tracked.
// Words are atomic so concurrent MarkPresent / IsPresent / ClearPresence
// on the same receiver — though atypical in single-goroutine decode — is
// race-free.
type presenceMask [16]atomic.Uint64

func (m *presenceMask) set(tag uint32) {
	idx := (tag - 1) >> 6
	bit := (tag - 1) & 63
	m[idx].Or(1 << bit)
}

func (m *presenceMask) get(tag uint32) bool {
	idx := (tag - 1) >> 6
	bit := (tag - 1) & 63
	return m[idx].Load()&(1<<bit) != 0
}

func (m *presenceMask) clear() {
	for i := range m {
		m[i].Store(0)
	}
}

// receiverKey identifies a sidecar entry by (concrete type identity, data
// address). Both halves are uintptr rather than unsafe.Pointer so the entry
// does not pin the receiver via the Go GC: an unsafe.Pointer stored in
// sync.Map's interface value would be traced and keep the pointee alive,
// turning the sidecar into an unbounded memory leak for any program that
// allocates ad-hoc receivers and drops them. Including the type identity
// disambiguates a parent struct from a generated nested struct that lives
// at offset 0 (where &parent == &parent.Field as raw pointers); without it
// the nested decoder's ClearPresence would wipe the parent's bits.
type receiverKey struct {
	typ uintptr
	ptr uintptr
}

// presenceStore is the package-level sidecar mapping receiver identities to
// their presence masks.
var presenceStore sync.Map // map[receiverKey]*presenceMask

// makeKey extracts the type and data words from receiver's interface header.
// The caller is expected to pass a non-nil pointer; passing a nil interface
// or a nil typed pointer yields the zero key, which the helpers treat as a
// no-op. Passing a non-pointer value type silently misbehaves (each call
// boxes a fresh copy at a new address); generated code always passes *T.
func makeKey(receiver any) receiverKey {
	if receiver == nil {
		return receiverKey{}
	}
	type iface struct {
		typ unsafe.Pointer
		ptr unsafe.Pointer
	}
	h := (*iface)(unsafe.Pointer(&receiver))
	return receiverKey{uintptr(h.typ), uintptr(h.ptr)}
}

// MarkPresent records that tag was decoded into receiver. Tags greater than
// MaxTrackedTag and tag == 0 are silently ignored. A fresh mask is allocated
// the first time a given receiver is marked.
func MarkPresent(receiver any, tag uint32) {
	if tag == 0 || tag > MaxTrackedTag {
		return
	}
	key := makeKey(receiver)
	if key.ptr == 0 {
		return
	}
	if v, ok := presenceStore.Load(key); ok {
		v.(*presenceMask).set(tag)
		return
	}
	mask := &presenceMask{}
	mask.set(tag)
	actual, loaded := presenceStore.LoadOrStore(key, mask)
	if loaded {
		actual.(*presenceMask).set(tag)
	}
}

// ClearPresence drops every bit on the mask associated with receiver,
// keeping the mask itself in the sidecar so a later MarkPresent on the
// same receiver does not allocate a fresh mask. Called by generated
// UnmarshalGSBM at the start of a decode and by Reset(). It is safe to
// call on a receiver that has never been marked.
//
// The sidecar entry is keyed by (type, address) as bare uintptrs, so it
// does not pin the receiver: when the receiver is GC'd, the entry persists
// but its key becomes stale. A future allocation that lands at the same
// address (with the same concrete type) reuses the stale entry — which is
// safe because UnmarshalGSBM begins by calling ClearPresence again. Use
// ForgetPresence to evict the entry explicitly when discarding an ad-hoc
// receiver early.
func ClearPresence(receiver any) {
	key := makeKey(receiver)
	if key.ptr == 0 {
		return
	}
	v, ok := presenceStore.Load(key)
	if !ok {
		return
	}
	v.(*presenceMask).clear()
}

// ForgetPresence removes the single sidecar entry keyed on receiver, if
// any. It is a low-level primitive: a struct that owns nested-struct
// fields, optional struct pointees, or slice-of-struct elements has a
// sidecar entry per descendant, and this function does not walk the
// closure. The generated Reset() method is the higher-level cleanup
// path: it zeroes the receiver's mask in place, evicts subtrees whose
// addresses become unreachable (optional-pointer pointees before
// nilling, map-of-struct value descendants before clearing), and
// zeroes-in-place the masks of value-struct sub-fields and slice-of-
// struct elements (whose addresses share the live parent). Reset is
// capacity-preserving so it is cheap to call on a pooled receiver
// between decode cycles.
func ForgetPresence(receiver any) {
	key := makeKey(receiver)
	if key.ptr == 0 {
		return
	}
	presenceStore.Delete(key)
}

// PresenceStoreLen returns the current number of entries in the package
// sidecar. Intended for tests and runtime monitoring (the post-completion
// note in the design plan calls this out as the way to spot a sidecar
// leak); production code should not depend on the exact value, which can
// shift as receivers are GC'd between Range passes.
func PresenceStoreLen() int {
	n := 0
	presenceStore.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// IsPresent reports whether tag was recorded as decoded into receiver. It
// returns false for an unknown receiver, a missing mask, tag == 0, or any
// tag greater than MaxTrackedTag.
func IsPresent(receiver any, tag uint32) bool {
	if tag == 0 || tag > MaxTrackedTag {
		return false
	}
	key := makeKey(receiver)
	if key.ptr == 0 {
		return false
	}
	v, ok := presenceStore.Load(key)
	if !ok {
		return false
	}
	return v.(*presenceMask).get(tag)
}
