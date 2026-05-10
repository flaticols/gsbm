package gsbm

import (
	"sync"
	"unsafe"
)

// MaxTrackedTag is the largest tag whose presence is tracked by the sidecar
// bitmap. Tags above this cap silently no-op on MarkPresent and return false
// from IsPresent. The cap is the bit-width of presenceMask.
const MaxTrackedTag uint32 = 1024

// presenceMask is a fixed-width bit set covering tags 1..MaxTrackedTag.
// Bit (tag-1) is set when the corresponding tag was decoded into the
// receiver. Tag 0 is reserved by the wire format and is never tracked.
type presenceMask [16]uint64

func (m *presenceMask) set(tag uint32) {
	idx := (tag - 1) >> 6
	bit := (tag - 1) & 63
	m[idx] |= 1 << bit
}

func (m *presenceMask) get(tag uint32) bool {
	idx := (tag - 1) >> 6
	bit := (tag - 1) & 63
	return m[idx]&(1<<bit) != 0
}

// presenceStore is the package-level sidecar mapping receiver pointers to
// their presence masks. The key is an unsafe.Pointer rather than the typed
// any value so MarkPresent / IsPresent do not box the receiver into a fresh
// interface header on every call.
var presenceStore sync.Map // map[unsafe.Pointer]*presenceMask

// receiverKey extracts the underlying data pointer from a non-nil pointer
// receiver. The caller is expected to pass *T; passing a non-pointer or a
// nil pointer yields the zero key, which the helpers treat as a no-op.
func receiverKey(receiver any) unsafe.Pointer {
	if receiver == nil {
		return nil
	}
	type iface struct {
		_   unsafe.Pointer
		ptr unsafe.Pointer
	}
	return (*iface)(unsafe.Pointer(&receiver)).ptr
}

// MarkPresent records that tag was decoded into receiver. Tags greater than
// MaxTrackedTag and tag == 0 are silently ignored. A fresh mask is allocated
// the first time a given receiver is marked.
func MarkPresent(receiver any, tag uint32) {
	if tag == 0 || tag > MaxTrackedTag {
		return
	}
	key := receiverKey(receiver)
	if key == nil {
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
// call on a receiver that has never been marked. The sidecar entry is
// retained until the receiver pointer is no longer used; presence_track
// does not free entries on its own (see plan §Post-Completion).
func ClearPresence(receiver any) {
	key := receiverKey(receiver)
	if key == nil {
		return
	}
	v, ok := presenceStore.Load(key)
	if !ok {
		return
	}
	m := v.(*presenceMask)
	for i := range m {
		m[i] = 0
	}
}

// IsPresent reports whether tag was recorded as decoded into receiver. It
// returns false for an unknown receiver, a missing mask, tag == 0, or any
// tag greater than MaxTrackedTag.
func IsPresent(receiver any, tag uint32) bool {
	if tag == 0 || tag > MaxTrackedTag {
		return false
	}
	key := receiverKey(receiver)
	if key == nil {
		return false
	}
	v, ok := presenceStore.Load(key)
	if !ok {
		return false
	}
	return v.(*presenceMask).get(tag)
}
