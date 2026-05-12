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

// EncodeDecimalAmount is the codec EncodeFn registered for "DecimalString"
// in this fixture. It delegates to builtins.EncodeDecimalString, binding
// the generic template to DecimalAmount at registration time.
func EncodeDecimalAmount(w *gsbm.Writer, v DecimalAmount) error {
	return builtins.EncodeDecimalString(w, v)
}

// DecodeDecimalAmount is the codec DecodeFn registered for "DecimalString"
// in this fixture. Delegates to builtins.DecodeDecimalString with
// ParseDecimalAmount supplying the type-binding parse step.
func DecodeDecimalAmount(r *gsbm.Reader, v *DecimalAmount) error {
	return builtins.DecodeDecimalString(r, v, ParseDecimalAmount)
}

// SizeDecimalAmount is the codec SizeFn registered for "DecimalString" in
// this fixture. Mirrors EncodeDecimalAmount's wire form: the
// length-prefixed string body of v.String().
func SizeDecimalAmount(v DecimalAmount) int {
	return builtins.SizeDecimalString(v)
}
