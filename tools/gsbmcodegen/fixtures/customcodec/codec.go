package customcodec

import (
	"fmt"
	"strconv"
	"strings"

	"go.flaticols.dev/gsbm/storage/gsbm"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/codecs/builtins"
)

// DecimalAmount is a stand-in for a third-party decimal type. The String /
// ParseDecimalAmount pair gives the DecimalString codec a stable canonical
// form (sign + integer + optional fractional separator) without pulling in
// big.Float or shopspring/decimal as a fixture dependency.
type DecimalAmount struct {
	// Negative carries the sign separately so a zero amount is unambiguous
	// regardless of source: parsing "-0.0" yields {Negative:true,...} only
	// if the user wrote it that way; ParseDecimalAmount preserves the
	// input form on string round-trip.
	Negative bool
	// Integer is the digits to the left of the decimal point.
	Integer string
	// Fraction is the digits to the right of the decimal point (without
	// the dot). Empty means an integer-only value like "42".
	Fraction string
}

// String returns the canonical decimal form, e.g. "-12.500" / "0" / "0.0".
// Trailing zeros are preserved — DecimalString explicitly does not
// normalize so round-trip via the wire is byte-stable.
func (d DecimalAmount) String() string {
	var b strings.Builder
	if d.Negative {
		b.WriteByte('-')
	}
	if d.Integer == "" {
		b.WriteByte('0')
	} else {
		b.WriteString(d.Integer)
	}
	if d.Fraction != "" {
		b.WriteByte('.')
		b.WriteString(d.Fraction)
	}
	return b.String()
}

// AppendText writes the canonical decimal form into dst and returns the
// extended slice. Mirrors String exactly — same sign / integer / optional
// fractional separator layout, same trailing-zero preservation — so the
// DecimalString and DecimalAppend codecs produce byte-identical wire
// output for any DecimalAmount. The error return is always nil today; it
// is part of the encoding.TextAppender contract that EmitDecimalAppend's
// generic constraint relies on.
func (d DecimalAmount) AppendText(dst []byte) ([]byte, error) {
	if d.Negative {
		dst = append(dst, '-')
	}
	if d.Integer == "" {
		dst = append(dst, '0')
	} else {
		dst = append(dst, d.Integer...)
	}
	if d.Fraction != "" {
		dst = append(dst, '.')
		dst = append(dst, d.Fraction...)
	}
	return dst, nil
}

// Coef returns the unsigned coefficient — the integer and fraction digits
// concatenated and read as a single integer ("12.500" → 12500). Together
// with Scale and IsNeg it satisfies builtins.BinaryDecimal, so DecimalAmount
// can be bound to the analytic binary decimal codec. The fixture keeps
// coefficients well within uint64; a value that overflowed would truncate,
// which is acceptable for a test fixture but is why production bindings
// validate their own range before encoding.
func (d DecimalAmount) Coef() uint64 {
	digits := d.Integer + d.Fraction
	if digits == "" {
		return 0
	}
	n, _ := strconv.ParseUint(digits, 10, 64)
	return n
}

// Scale returns the number of fractional digits — len(d.Fraction).
func (d DecimalAmount) Scale() int { return len(d.Fraction) }

// IsNeg reports whether the amount is negative.
func (d DecimalAmount) IsNeg() bool { return d.Negative }

// ReconstructDecimalAmount rebuilds a DecimalAmount from the (coef, scale,
// neg) triple DecodeDecimalBinary unpacks from the wire. It is the inverse
// of the Coef/Scale/IsNeg accessors: coef is rendered as a decimal string
// and its last `scale` digits become the fraction (zero-padded when coef
// has fewer digits than scale). Unlike the text codec the binary form does
// not preserve leading zeros in the integer part — it carries a numeric
// coefficient, not the original text — so "007.5" decodes back as "7.5".
func ReconstructDecimalAmount(coef uint64, scale int, neg bool) (DecimalAmount, error) {
	if scale < 0 {
		return DecimalAmount{}, fmt.Errorf("customcodec: decimal: negative scale %d", scale)
	}
	digits := strconv.FormatUint(coef, 10)
	for len(digits) < scale {
		digits = "0" + digits
	}
	split := len(digits) - scale
	return DecimalAmount{Negative: neg, Integer: digits[:split], Fraction: digits[split:]}, nil
}

// ParseDecimalAmount is the inverse of String. It rejects empty input and
// non-digit content so a corrupted decode surfaces as an error rather than
// silently producing junk fields.
func ParseDecimalAmount(s string) (DecimalAmount, error) {
	if s == "" {
		return DecimalAmount{}, fmt.Errorf("customcodec: decimal: empty input")
	}
	d := DecimalAmount{}
	rest := s
	if strings.HasPrefix(rest, "-") {
		d.Negative = true
		rest = rest[1:]
	}
	if rest == "" {
		return DecimalAmount{}, fmt.Errorf("customcodec: decimal: missing digits after sign")
	}
	if intPart, fracPart, hasDot := strings.Cut(rest, "."); hasDot {
		d.Integer = intPart
		d.Fraction = fracPart
	} else {
		d.Integer = rest
	}
	for _, part := range []string{d.Integer, d.Fraction} {
		if part == "" {
			continue
		}
		if _, err := strconv.ParseUint(part, 10, 64); err != nil {
			return DecimalAmount{}, fmt.Errorf("customcodec: decimal: %w", err)
		}
	}
	return d, nil
}

// EmitDecimalAmount is the codec EmitFn registered for "DecimalString" in
// this fixture. It delegates to builtins.EmitDecimalString, binding the
// generic template to DecimalAmount at registration time. The Writer is
// mode-aware (size or write); the callsite id keys the Writer's scratch
// cache so v.String() runs exactly once per gsbm.Marshal call.
func EmitDecimalAmount(w *gsbm.Writer, v DecimalAmount, callsite uint64) error {
	return builtins.EmitDecimalString(w, v, callsite)
}

// DecodeDecimalAmount is the codec DecodeFn registered for "DecimalString"
// in this fixture. Delegates to builtins.DecodeDecimalString with
// ParseDecimalAmount supplying the type-binding parse step.
func DecodeDecimalAmount(r *gsbm.Reader, v *DecimalAmount) error {
	return builtins.DecodeDecimalString(r, v, ParseDecimalAmount)
}

// EmitDecimalAmountAppend is the codec EmitFn registered for
// "DecimalAppend" in this fixture. It delegates to
// builtins.EmitDecimalAppend, binding the generic AppendText constraint
// to DecimalAmount at registration time. Wire output is byte-identical
// to EmitDecimalAmount for the same value because DecimalAmount.String
// and DecimalAmount.AppendText produce the same text.
func EmitDecimalAmountAppend(w *gsbm.Writer, v DecimalAmount, callsite uint64) error {
	return builtins.EmitDecimalAppend(w, v, callsite)
}

// DecodeDecimalAmountAppend reads a LENGTH_DELIM string and parses it
// back into *v. The append codec writes a length-prefixed text body, so
// the decode side is the same DecodeDecimalString template as the
// string-form codec.
func DecodeDecimalAmountAppend(r *gsbm.Reader, v *DecimalAmount) error {
	return builtins.DecodeDecimalString(r, v, ParseDecimalAmount)
}

// EncodeDecimalAmountBinary is the codec EncodeFn registered for
// "DecimalBinary" in this fixture. It delegates to
// builtins.EncodeDecimalBinary, binding the generic BinaryDecimal
// constraint to DecimalAmount at registration time. This is an analytic
// codec — no callsite is threaded; the body is `uvarint(coef) ++
// uvarint(scale<<1 | signbit)` and is allocation-free.
func EncodeDecimalAmountBinary(w *gsbm.Writer, v DecimalAmount) error {
	return builtins.EncodeDecimalBinary(w, v)
}

// SizeDecimalAmountBinary is the codec SizeFn registered for
// "DecimalBinary" in this fixture. It delegates to
// builtins.SizeDecimalBinary; codegen calls it in the size pass to compute
// the LENGTH_DELIM length prefix without materializing the body.
func SizeDecimalAmountBinary(v DecimalAmount) int {
	return builtins.SizeDecimalBinary(v)
}

// DecodeDecimalAmountBinary is the codec DecodeFn registered for
// "DecimalBinary" in this fixture. Delegates to
// builtins.DecodeDecimalBinary with ReconstructDecimalAmount supplying the
// type-binding rebuild step from the decoded (coef, scale, neg) triple.
func DecodeDecimalAmountBinary(r *gsbm.Reader, v *DecimalAmount) error {
	return builtins.DecodeDecimalBinary(r, v, ReconstructDecimalAmount)
}

// StreamLargePayload is the codec StreamFn registered for "StreamingJSON"
// in this fixture. It delegates to builtins.StreamJSONBytes, binding the
// generic template to LargePayload at registration time. No callsite is
// threaded — the streaming kind materializes the body per pass and never
// retains it across the size→write hand-off.
func StreamLargePayload(w *gsbm.Writer, v LargePayload) error {
	return builtins.StreamJSONBytes(w, v)
}

// DecodeLargePayload is the codec DecodeFn registered for "StreamingJSON"
// in this fixture. Delegates to builtins.DecodeJSONBytes, which reads the
// LENGTH_DELIM byte payload and runs json.Unmarshal into *v.
func DecodeLargePayload(r *gsbm.Reader, v *LargePayload) error {
	return builtins.DecodeJSONBytes(r, v)
}
