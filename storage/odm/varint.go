package odm

// varintLen returns the number of bytes needed to varint-encode v.
func varintLen(v uint64) int {
	switch {
	case v < 1<<7:
		return 1
	case v < 1<<14:
		return 2
	case v < 1<<21:
		return 3
	case v < 1<<28:
		return 4
	case v < 1<<35:
		return 5
	case v < 1<<42:
		return 6
	case v < 1<<49:
		return 7
	case v < 1<<56:
		return 8
	case v < 1<<63:
		return 9
	default:
		return 10
	}
}

// appendUvarint appends the LEB128-style varint encoding of v to b.
func appendUvarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

// putUvarint writes v into b starting at index 0 and returns the number
// of bytes written. b must be large enough — this is used to patch a
// previously reserved slot.
func putUvarint(b []byte, v uint64) int {
	i := 0
	for v >= 0x80 {
		b[i] = byte(v) | 0x80
		v >>= 7
		i++
	}
	b[i] = byte(v)
	return i + 1
}

// readUvarint decodes a varint from b at offset pos. On success it returns
// the value and the number of bytes consumed. On error consumed is 0.
func readUvarint(b []byte, pos int) (v uint64, n int, err error) {
	var shift uint
	for i := range 10 {
		if pos+i >= len(b) {
			return 0, 0, ErrTruncated
		}
		c := b[pos+i]
		if i == 9 && c > 1 {
			return 0, 0, ErrVarintOverflow
		}
		v |= uint64(c&0x7f) << shift
		if c < 0x80 {
			return v, i + 1, nil
		}
		shift += 7
	}
	return 0, 0, ErrVarintOverflow
}
