package gsbm

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

// GSBMUnmarshaler matches the codegen's UnmarshalGSBM signature. It is the
// other half of the DecodeInto contract.
type GSBMUnmarshaler interface {
	UnmarshalGSBM(r *Reader) error
}

// GSBMRoot bundles the two contracts a pooled root type satisfies.
// DecodeInto takes any value that satisfies it; the codegenerated
// MarshalGSBM/UnmarshalGSBM/Reset trio implements it on the pointer
// receiver of the root struct.
type GSBMRoot interface {
	Resettable
	GSBMUnmarshaler
}

// DecodeInto decodes data into dst, reusing dst's pre-allocated slice and
// map storage where possible. data MUST include the 12-byte blob header;
// the header is read and validated, but its flags/schemaHint/bodyLen
// values are discarded (callers that need them should use ReadHeader
// directly).
//
// dst.Reset() is called first so any previously-decoded content is
// cleared without releasing the backing memory. After decode, the Reader
// is checked for a sticky error.
func DecodeInto(data []byte, dst GSBMRoot) error {
	dst.Reset()
	r := NewReader(data)
	if _, _, _, err := r.ReadHeader(); err != nil {
		return err
	}
	if err := dst.UnmarshalGSBM(r); err != nil {
		return err
	}
	return r.Err()
}

// DecodeBodyInto is the headerless variant of DecodeInto: data is the
// raw root-struct body, with no blob header. Tests and tools that
// produce body-only payloads (no WriteHeader) use this.
func DecodeBodyInto(data []byte, dst GSBMRoot) error {
	dst.Reset()
	r := NewReader(data)
	if err := dst.UnmarshalGSBM(r); err != nil {
		return err
	}
	return r.Err()
}
