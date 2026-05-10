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

// ForgetPresence removes the sidecar entry for receiver, if any. Callers
// that allocate many ad-hoc receivers without a sync.Pool can use this to
// bound the sidecar's footprint; otherwise entries accumulate indexed by
// receiver address (each entry is ~144 bytes and is reused when the same
// address is decoded into again, so the practical impact is bounded by
// distinct addresses in flight).
func ForgetPresence(receiver any) {
	key := makeKey(receiver)
	if key.ptr == 0 {
		return
	}
	presenceStore.Delete(key)
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
