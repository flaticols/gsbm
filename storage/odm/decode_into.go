package odm

// Resettable is the contract every codegenerated root type satisfies via
// its generated Reset() method. DecodeInto calls Reset on dst before the
// decoder runs so existing slice and map capacity is reused.
//
// Implementations MUST preserve underlying slice/map capacity (truncate
// slices to length 0 with `s = s[:0]`; reset maps with `clear(m)`) and
// recurse into nested structs that themselves implement Resettable.
type Resettable interface {
	Reset()
}

// ODMUnmarshaler matches the codegen's UnmarshalODM signature. It is the
// other half of the DecodeInto contract.
type ODMUnmarshaler interface {
	UnmarshalODM(r *Reader) error
}

// ODMRoot bundles the two contracts a pooled root type satisfies.
// DecodeInto takes any value that satisfies it; the codegenerated
// MarshalODM/UnmarshalODM/Reset trio implements it on the pointer
// receiver of the root struct.
type ODMRoot interface {
	Resettable
	ODMUnmarshaler
}

// DecodeInto decodes data into dst, reusing dst's pre-allocated slice and
// map storage where possible. data MUST include the 8-byte blob header;
// the header is read and validated, but its flags/schVer values are
// discarded (callers that need them should use ReadHeader directly).
//
// dst.Reset() is called first so any previously-decoded content is
// cleared without releasing the backing memory. After decode, the Reader
// is checked for a sticky error.
func DecodeInto(data []byte, dst ODMRoot) error {
	dst.Reset()
	r := NewReader(data)
	if _, _, err := r.ReadHeader(); err != nil {
		return err
	}
	if err := dst.UnmarshalODM(r); err != nil {
		return err
	}
	return r.Err()
}

// DecodeBodyInto is the headerless variant of DecodeInto: data is the
// raw root-struct body, with no 8-byte header. Tests and tools that
// produce body-only payloads (no WriteHeader) use this.
func DecodeBodyInto(data []byte, dst ODMRoot) error {
	dst.Reset()
	r := NewReader(data)
	if err := dst.UnmarshalODM(r); err != nil {
		return err
	}
	return r.Err()
}
