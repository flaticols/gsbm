package gsbm

// PresenceState is the meaningful state encoded in the low two bits of a
// presence byte (spec §5.1). Bit 0 is "present", bit 1 is "zero-elided".
// Bits 2..7 are reserved in fmtVer 2 and MUST be zero on the wire.
type PresenceState uint8

const (
	// PresenceNil — bits 0b00. The field is absent. No payload follows.
	PresenceNil PresenceState = 0x00
	// PresenceNonZero — bits 0b01. The field is present and non-zero. The
	// caller MUST follow the presence byte with the value payload.
	PresenceNonZero PresenceState = 0x01
	// PresenceZero — bits 0b11. The field is present and zero-elided; no
	// payload follows. The decoder restores the type's zero value.
	//
	// Per spec §5.1, encoders MUST NOT emit PresenceZero for fields whose
	// schema type is not a Go builtin primitive (int*/uint*/float*/bool/
	// string/[]byte). Decoders for ineligible types MUST reject this state
	// as malformed — see Reader.ReadPresenceByte's allowZeroElide flag.
	PresenceZero PresenceState = 0x03

	// presenceReservedMask covers bits 2..7. In fmtVer 2 these MUST be zero.
	presenceReservedMask uint8 = 0xFC
)

// WritePresenceNil writes a presence byte with state nil (0b00). No value
// payload follows.
func (w *Writer) WritePresenceNil() {
	if w.err != nil {
		return
	}
	if w.sizeOnly {
		w.sizeAcc++
		return
	}
	w.buf = append(w.buf, byte(PresenceNil))
}

// WritePresenceNonZero writes a presence byte indicating a present, non-zero
// value (0b01). The caller MUST follow with the field's value payload.
func (w *Writer) WritePresenceNonZero() {
	if w.err != nil {
		return
	}
	if w.sizeOnly {
		w.sizeAcc++
		return
	}
	w.buf = append(w.buf, byte(PresenceNonZero))
}

// WritePresenceZero writes a presence byte indicating a present, zero-elided
// value (0b11). No payload follows. Use only for fields whose schema type is
// a Go builtin primitive; for named or user-defined types the writer must
// emit PresenceNonZero followed by the full payload instead.
func (w *Writer) WritePresenceZero() {
	if w.err != nil {
		return
	}
	if w.sizeOnly {
		w.sizeAcc++
		return
	}
	w.buf = append(w.buf, byte(PresenceZero))
}

// ReadPresenceByte consumes a presence byte and returns its state. It rejects:
//   - inputs with any of bits 2..7 set (reserved in fmtVer 2),
//   - the reserved 0b10 state, and
//   - the present-and-zero state when allowZeroElide is false.
//
// Codegen passes allowZeroElide=true only for fields whose schema type is a
// Go builtin (int*/uint*/float*/bool/string/[]byte) and false for named or
// user-defined types, enforcing the eligibility rule from spec §5.1.
func (r *Reader) ReadPresenceByte(allowZeroElide bool) (PresenceState, error) {
	if r.err != nil {
		return 0, r.err
	}
	if r.end-r.pos < 1 {
		r.setErr(ErrTruncated)
		return 0, r.err
	}
	b := r.buf[r.pos]
	r.pos++
	if b&presenceReservedMask != 0 {
		r.setErr(ErrInvalidPresence)
		return 0, r.err
	}
	switch PresenceState(b) {
	case PresenceNil, PresenceNonZero:
		return PresenceState(b), nil
	case PresenceZero:
		if !allowZeroElide {
			r.setErr(ErrInvalidPresence)
			return 0, r.err
		}
		return PresenceZero, nil
	default:
		// 0b10 — reserved, malformed.
		r.setErr(ErrInvalidPresence)
		return 0, r.err
	}
}
