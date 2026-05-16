// Package money is a benchmark-only fixture for measuring the
// materializing-codec hot path on a Money-shaped payload (issue #29).
//
// Two top-level types share one in-memory shape — Batch — but expose
// different MarshalGSBM bodies:
//
//   - StringBatch encodes Money.Amount via the EmitDecimalString
//     (string-returning) codec path.
//   - AppendBatch encodes Money.Amount via the EmitDecimalAppend
//     (append-style) codec path.
//   - BinaryBatch encodes Money.Amount via the EncodeDecimalBinary
//     (analytic, allocation-free) codec path.
//
// Same payload, three MarshalGSBM implementations: StringBatch and
// AppendBatch produce byte-identical wire output for any DecimalAmount
// whose String() and AppendText() outputs agree (the local DecimalAmount
// type guarantees that), so those two sub-benchmarks measure the cost of
// the materializing-codec choice alone. BinaryBatch encodes a different
// (binary varint) wire form — its sub-benchmark measures the analytic
// codec, which materializes nothing and so allocates nothing per decimal.
package money

import (
	"strconv"
	"strings"

	"go.flaticols.dev/gsbm/storage/gsbm"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/codecs/builtins"
)

// DecimalAmount mirrors the customcodec fixture's stand-in decimal type.
// Negative/Integer/Fraction is the minimum shape that exercises sign,
// trailing zeros, and the fraction separator while keeping the fixture
// dependency-free.
type DecimalAmount struct {
	Negative bool
	Integer  string
	Fraction string
}

// String returns the canonical decimal form. Same layout as AppendText
// below so the two codecs produce identical wire bytes for any value.
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

// AppendText appends the canonical decimal form to dst and returns the
// extended slice. Identical text to String for the same value.
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

// Coef returns the unsigned coefficient — the integer and fraction
// digits concatenated and read as a single integer ("12.500" → 12500).
// Together with Scale and IsNeg it satisfies builtins.BinaryDecimal, so
// DecimalAmount can be bound to the analytic binary decimal codec. The
// fixture's randomDecimal keeps coefficients well within uint64.
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

// Money is the leaf value the codec materializes.
type Money struct {
	Amount   DecimalAmount
	Currency string
}

// Line is one row in the batch. Base plus two repeated Money fields
// gives the issue example's "3 money fields per line" cardinality.
type Line struct {
	Base        Money
	Taxes       []Money
	Adjustments []Money
}

// Batch is the encode root. Lines is a repeated Line — encoded as a
// LENGTH_DELIM-wrapped count + per-element LENGTH_DELIM bodies (mirrors
// the codegen-emitted shape for `[]Struct` fields).
type Batch struct {
	Lines []Line
}

// StringBatch encodes the Batch via EmitDecimalString for Money.Amount.
type StringBatch struct {
	Lines []Line
}

// AppendBatch encodes the Batch via EmitDecimalAppend for Money.Amount.
type AppendBatch struct {
	Lines []Line
}

// BinaryBatch encodes the Batch via the analytic EncodeDecimalBinary
// codec for Money.Amount. Unlike StringBatch/AppendBatch its wire output
// is the binary `(coef, scale, sign)` varint form, not canonical text,
// so it is intentionally not byte-comparable to the two text paths.
type BinaryBatch struct {
	Lines []Line
}

// Stable callsite ids for the materializing codec scratch cache. They
// only need to be distinct across the fixture's callsites; the values
// otherwise have no meaning.
const (
	csMoneyAmount uint64 = 0x4d6f6e6579416d74 // "MoneyAmt"
)

// SizeGSBM measures the body byte length by running MarshalGSBM against
// a CountingWriter — same pattern the gsbmcodegen-emitted SizeGSBM uses.
func (b *StringBatch) SizeGSBM() int {
	cw := gsbm.NewCountingWriter()
	_ = b.MarshalGSBM(cw)
	return cw.Size()
}

// MarshalGSBM emits the batch body using the string-returning decimal
// codec for Money.Amount.
func (b *StringBatch) MarshalGSBM(w *gsbm.Writer) error {
	return marshalBatch(w, b.Lines, emitAmountString)
}

func (b *AppendBatch) SizeGSBM() int {
	cw := gsbm.NewCountingWriter()
	_ = b.MarshalGSBM(cw)
	return cw.Size()
}

func (b *AppendBatch) MarshalGSBM(w *gsbm.Writer) error {
	return marshalBatch(w, b.Lines, emitAmountAppend)
}

func (b *BinaryBatch) SizeGSBM() int {
	cw := gsbm.NewCountingWriter()
	_ = b.MarshalGSBM(cw)
	return cw.Size()
}

// MarshalGSBM emits the batch body using the analytic binary decimal
// codec for Money.Amount.
func (b *BinaryBatch) MarshalGSBM(w *gsbm.Writer) error {
	return marshalBatch(w, b.Lines, emitAmountBinary)
}

// emitFn is the materializing-codec dispatch point. Same signature as
// the codegen-emitted EmitFn so the two flavors plug in directly.
type emitFn func(w *gsbm.Writer, v DecimalAmount, callsite uint64) error

func emitAmountString(w *gsbm.Writer, v DecimalAmount, callsite uint64) error {
	return builtins.EmitDecimalString(w, v, callsite)
}

func emitAmountAppend(w *gsbm.Writer, v DecimalAmount, callsite uint64) error {
	return builtins.EmitDecimalAppend(w, v, callsite)
}

// emitAmountBinary writes the analytic binary decimal codec field. Unlike
// the materializing emitters it threads no callsite (the analytic codec
// has no scratch cache); it writes the LENGTH_DELIM length prefix from
// SizeDecimalBinary and then the body — the exact shape codegen emits for
// an analytic codec. Nothing is materialized, so nothing allocates.
func emitAmountBinary(w *gsbm.Writer, v DecimalAmount, _ uint64) error {
	w.WriteUvarint(uint64(builtins.SizeDecimalBinary(v)))
	return builtins.EncodeDecimalBinary(w, v)
}

func marshalBatch(w *gsbm.Writer, lines []Line, emit emitFn) error {
	// tag 1 Lines (repeated)
	w.WriteTag(1, gsbm.WireLengthDelim)
	m := w.BeginLengthDelim()
	w.WriteUvarint(uint64(len(lines)))
	for i := range lines {
		inner := w.BeginLengthDelim()
		if err := marshalLine(w, &lines[i], emit); err != nil {
			return err
		}
		w.EndLengthDelim(inner)
	}
	w.EndLengthDelim(m)
	return w.Err()
}

func marshalLine(w *gsbm.Writer, l *Line, emit emitFn) error {
	// tag 1 Base
	w.WriteTag(1, gsbm.WireLengthDelim)
	{
		m := w.BeginLengthDelim()
		if err := marshalMoney(w, &l.Base, emit); err != nil {
			return err
		}
		w.EndLengthDelim(m)
	}
	// tag 2 Taxes (repeated)
	w.WriteTag(2, gsbm.WireLengthDelim)
	{
		m := w.BeginLengthDelim()
		w.WriteUvarint(uint64(len(l.Taxes)))
		for i := range l.Taxes {
			inner := w.BeginLengthDelim()
			if err := marshalMoney(w, &l.Taxes[i], emit); err != nil {
				return err
			}
			w.EndLengthDelim(inner)
		}
		w.EndLengthDelim(m)
	}
	// tag 3 Adjustments (repeated)
	w.WriteTag(3, gsbm.WireLengthDelim)
	{
		m := w.BeginLengthDelim()
		w.WriteUvarint(uint64(len(l.Adjustments)))
		for i := range l.Adjustments {
			inner := w.BeginLengthDelim()
			if err := marshalMoney(w, &l.Adjustments[i], emit); err != nil {
				return err
			}
			w.EndLengthDelim(inner)
		}
		w.EndLengthDelim(m)
	}
	return w.Err()
}

func marshalMoney(w *gsbm.Writer, m *Money, emit emitFn) error {
	// tag 1 Amount — decimal codec (materializing or analytic)
	w.WriteTag(1, gsbm.WireLengthDelim)
	if err := emit(w, m.Amount, csMoneyAmount); err != nil {
		return err
	}
	// tag 2 Currency
	w.WriteTag(2, gsbm.WireLengthDelim)
	w.WriteString(m.Currency)
	return w.Err()
}
