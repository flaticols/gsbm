// Package intwidth is a benchmark-only fixture for the `bin:"N,type=…"`
// wire-width override on Go `int` fields (issue #48).
//
// Three top-level types share one in-memory shape — a record carrying
// eight ordinary `int` values — and ship three MarshalGSBM /
// UnmarshalGSBM bodies that mirror, line-for-line, what gsbmcodegen
// emits for the three flavors:
//
//   - Plain — un-annotated `int`; encode and decode bound the value to
//     the int32 range and return ErrIntegerOverflow when it escapes.
//   - Int32 — `bin:"N,type=int32"`; byte-identical wire shape and code
//     shape to Plain. Exists so the bench can confirm the no-op intent
//     marker is also a no-op at runtime.
//   - Int64 — `bin:"N,type=int64"`; skips the int32 bounds check on
//     both encode and decode. Wire shape is the same varint, but values
//     outside [MinInt32, MaxInt32] round-trip.
//
// Benchmarks exercise the encode and decode paths over a fixed payload
// whose values stay inside the int32 range, so all three flavors run to
// completion and the only measurable difference is the cost of the
// bounds check itself.
//
// The codecs are hand-written rather than codegen-driven because (a)
// gsbmcodegen's behavior is already pinned by the
// `tools/gsbmcodegen/fixtures/intwidth/` golden test, and (b) the bench
// package mirrors the layout of internal/bench/money/, which is also
// hand-written. Keep these bodies in step with codegen output: if
// codegen evolves its `types.Int` emit shape, regenerate the intwidth
// fixture in tools/gsbmcodegen/fixtures/intwidth/ and copy its
// MarshalGSBM / UnmarshalGSBM shape into the matching variant here.
package intwidth

import (
	"math"

	"go.flaticols.dev/gsbm/storage/gsbm"
)

// Record carries the int values exercised by the benchmarks. Each variant
// (Plain, Int32, Int64) wraps the same shape so the three sub-benchmarks
// operate on byte-identical inputs.
type Record struct {
	F1, F2, F3, F4, F5, F6, F7, F8 int
}

// Plain mirrors a codegen-emitted root with un-annotated `int` fields.
// Wire shape per field: varint with an inline int32 bounds check.
type Plain struct {
	Record
}

// Int32 mirrors a codegen-emitted root with `bin:"N,type=int32"` fields.
// The wire shape and code shape are byte-identical to Plain; the variant
// exists so the bench can prove that the explicit intent marker carries
// no runtime cost relative to the un-annotated default.
type Int32 struct {
	Record
}

// Int64 mirrors a codegen-emitted root with `bin:"N,type=int64"` fields.
// The int32 bounds check is omitted on both encode and decode.
type Int64 struct {
	Record
}

func (v *Plain) SizeGSBM() int {
	cw := gsbm.NewCountingWriter()
	_ = v.MarshalGSBM(cw)
	return cw.Size()
}

func (v *Int32) SizeGSBM() int {
	cw := gsbm.NewCountingWriter()
	_ = v.MarshalGSBM(cw)
	return cw.Size()
}

func (v *Int64) SizeGSBM() int {
	cw := gsbm.NewCountingWriter()
	_ = v.MarshalGSBM(cw)
	return cw.Size()
}

// MarshalGSBM bodies are intentionally straight-line per-field — the
// same shape codegen emits. A loop-over-fields helper would change the
// inlining and escape characteristics of the hot path and the bench
// would no longer measure what generated code costs.

func (v *Plain) MarshalGSBM(w *gsbm.Writer) error {
	w.WriteTag(1, gsbm.WireVarint)
	if int64(v.F1) < math.MinInt32 || int64(v.F1) > math.MaxInt32 {
		return gsbm.ErrIntegerOverflow
	}
	w.WriteVarint(int64(v.F1))
	w.WriteTag(2, gsbm.WireVarint)
	if int64(v.F2) < math.MinInt32 || int64(v.F2) > math.MaxInt32 {
		return gsbm.ErrIntegerOverflow
	}
	w.WriteVarint(int64(v.F2))
	w.WriteTag(3, gsbm.WireVarint)
	if int64(v.F3) < math.MinInt32 || int64(v.F3) > math.MaxInt32 {
		return gsbm.ErrIntegerOverflow
	}
	w.WriteVarint(int64(v.F3))
	w.WriteTag(4, gsbm.WireVarint)
	if int64(v.F4) < math.MinInt32 || int64(v.F4) > math.MaxInt32 {
		return gsbm.ErrIntegerOverflow
	}
	w.WriteVarint(int64(v.F4))
	w.WriteTag(5, gsbm.WireVarint)
	if int64(v.F5) < math.MinInt32 || int64(v.F5) > math.MaxInt32 {
		return gsbm.ErrIntegerOverflow
	}
	w.WriteVarint(int64(v.F5))
	w.WriteTag(6, gsbm.WireVarint)
	if int64(v.F6) < math.MinInt32 || int64(v.F6) > math.MaxInt32 {
		return gsbm.ErrIntegerOverflow
	}
	w.WriteVarint(int64(v.F6))
	w.WriteTag(7, gsbm.WireVarint)
	if int64(v.F7) < math.MinInt32 || int64(v.F7) > math.MaxInt32 {
		return gsbm.ErrIntegerOverflow
	}
	w.WriteVarint(int64(v.F7))
	w.WriteTag(8, gsbm.WireVarint)
	if int64(v.F8) < math.MinInt32 || int64(v.F8) > math.MaxInt32 {
		return gsbm.ErrIntegerOverflow
	}
	w.WriteVarint(int64(v.F8))
	return w.Err()
}

func (v *Int32) MarshalGSBM(w *gsbm.Writer) error {
	w.WriteTag(1, gsbm.WireVarint)
	if int64(v.F1) < math.MinInt32 || int64(v.F1) > math.MaxInt32 {
		return gsbm.ErrIntegerOverflow
	}
	w.WriteVarint(int64(v.F1))
	w.WriteTag(2, gsbm.WireVarint)
	if int64(v.F2) < math.MinInt32 || int64(v.F2) > math.MaxInt32 {
		return gsbm.ErrIntegerOverflow
	}
	w.WriteVarint(int64(v.F2))
	w.WriteTag(3, gsbm.WireVarint)
	if int64(v.F3) < math.MinInt32 || int64(v.F3) > math.MaxInt32 {
		return gsbm.ErrIntegerOverflow
	}
	w.WriteVarint(int64(v.F3))
	w.WriteTag(4, gsbm.WireVarint)
	if int64(v.F4) < math.MinInt32 || int64(v.F4) > math.MaxInt32 {
		return gsbm.ErrIntegerOverflow
	}
	w.WriteVarint(int64(v.F4))
	w.WriteTag(5, gsbm.WireVarint)
	if int64(v.F5) < math.MinInt32 || int64(v.F5) > math.MaxInt32 {
		return gsbm.ErrIntegerOverflow
	}
	w.WriteVarint(int64(v.F5))
	w.WriteTag(6, gsbm.WireVarint)
	if int64(v.F6) < math.MinInt32 || int64(v.F6) > math.MaxInt32 {
		return gsbm.ErrIntegerOverflow
	}
	w.WriteVarint(int64(v.F6))
	w.WriteTag(7, gsbm.WireVarint)
	if int64(v.F7) < math.MinInt32 || int64(v.F7) > math.MaxInt32 {
		return gsbm.ErrIntegerOverflow
	}
	w.WriteVarint(int64(v.F7))
	w.WriteTag(8, gsbm.WireVarint)
	if int64(v.F8) < math.MinInt32 || int64(v.F8) > math.MaxInt32 {
		return gsbm.ErrIntegerOverflow
	}
	w.WriteVarint(int64(v.F8))
	return w.Err()
}

func (v *Int64) MarshalGSBM(w *gsbm.Writer) error {
	w.WriteTag(1, gsbm.WireVarint)
	w.WriteVarint(int64(v.F1))
	w.WriteTag(2, gsbm.WireVarint)
	w.WriteVarint(int64(v.F2))
	w.WriteTag(3, gsbm.WireVarint)
	w.WriteVarint(int64(v.F3))
	w.WriteTag(4, gsbm.WireVarint)
	w.WriteVarint(int64(v.F4))
	w.WriteTag(5, gsbm.WireVarint)
	w.WriteVarint(int64(v.F5))
	w.WriteTag(6, gsbm.WireVarint)
	w.WriteVarint(int64(v.F6))
	w.WriteTag(7, gsbm.WireVarint)
	w.WriteVarint(int64(v.F7))
	w.WriteTag(8, gsbm.WireVarint)
	w.WriteVarint(int64(v.F8))
	return w.Err()
}

func (v *Plain) UnmarshalGSBM(r *gsbm.Reader) error {
	for r.HasMore() {
		tag, wt, err := r.ReadTag()
		if err != nil {
			return err
		}
		switch tag {
		case 1:
			if wt != gsbm.WireVarint {
				return gsbm.ErrWrongWireType
			}
			x, err := r.ReadVarint()
			if err != nil {
				return err
			}
			if x < math.MinInt32 || x > math.MaxInt32 {
				return gsbm.ErrIntegerOverflow
			}
			v.F1 = int(x)
		case 2:
			if wt != gsbm.WireVarint {
				return gsbm.ErrWrongWireType
			}
			x, err := r.ReadVarint()
			if err != nil {
				return err
			}
			if x < math.MinInt32 || x > math.MaxInt32 {
				return gsbm.ErrIntegerOverflow
			}
			v.F2 = int(x)
		case 3:
			if wt != gsbm.WireVarint {
				return gsbm.ErrWrongWireType
			}
			x, err := r.ReadVarint()
			if err != nil {
				return err
			}
			if x < math.MinInt32 || x > math.MaxInt32 {
				return gsbm.ErrIntegerOverflow
			}
			v.F3 = int(x)
		case 4:
			if wt != gsbm.WireVarint {
				return gsbm.ErrWrongWireType
			}
			x, err := r.ReadVarint()
			if err != nil {
				return err
			}
			if x < math.MinInt32 || x > math.MaxInt32 {
				return gsbm.ErrIntegerOverflow
			}
			v.F4 = int(x)
		case 5:
			if wt != gsbm.WireVarint {
				return gsbm.ErrWrongWireType
			}
			x, err := r.ReadVarint()
			if err != nil {
				return err
			}
			if x < math.MinInt32 || x > math.MaxInt32 {
				return gsbm.ErrIntegerOverflow
			}
			v.F5 = int(x)
		case 6:
			if wt != gsbm.WireVarint {
				return gsbm.ErrWrongWireType
			}
			x, err := r.ReadVarint()
			if err != nil {
				return err
			}
			if x < math.MinInt32 || x > math.MaxInt32 {
				return gsbm.ErrIntegerOverflow
			}
			v.F6 = int(x)
		case 7:
			if wt != gsbm.WireVarint {
				return gsbm.ErrWrongWireType
			}
			x, err := r.ReadVarint()
			if err != nil {
				return err
			}
			if x < math.MinInt32 || x > math.MaxInt32 {
				return gsbm.ErrIntegerOverflow
			}
			v.F7 = int(x)
		case 8:
			if wt != gsbm.WireVarint {
				return gsbm.ErrWrongWireType
			}
			x, err := r.ReadVarint()
			if err != nil {
				return err
			}
			if x < math.MinInt32 || x > math.MaxInt32 {
				return gsbm.ErrIntegerOverflow
			}
			v.F8 = int(x)
		default:
			if err := r.SkipField(wt); err != nil {
				return err
			}
		}
	}
	return r.Err()
}

// Int32.UnmarshalGSBM is byte-identical to Plain.UnmarshalGSBM by
// design — the override is a no-op intent marker on the decode side too.
func (v *Int32) UnmarshalGSBM(r *gsbm.Reader) error {
	for r.HasMore() {
		tag, wt, err := r.ReadTag()
		if err != nil {
			return err
		}
		switch tag {
		case 1:
			if wt != gsbm.WireVarint {
				return gsbm.ErrWrongWireType
			}
			x, err := r.ReadVarint()
			if err != nil {
				return err
			}
			if x < math.MinInt32 || x > math.MaxInt32 {
				return gsbm.ErrIntegerOverflow
			}
			v.F1 = int(x)
		case 2:
			if wt != gsbm.WireVarint {
				return gsbm.ErrWrongWireType
			}
			x, err := r.ReadVarint()
			if err != nil {
				return err
			}
			if x < math.MinInt32 || x > math.MaxInt32 {
				return gsbm.ErrIntegerOverflow
			}
			v.F2 = int(x)
		case 3:
			if wt != gsbm.WireVarint {
				return gsbm.ErrWrongWireType
			}
			x, err := r.ReadVarint()
			if err != nil {
				return err
			}
			if x < math.MinInt32 || x > math.MaxInt32 {
				return gsbm.ErrIntegerOverflow
			}
			v.F3 = int(x)
		case 4:
			if wt != gsbm.WireVarint {
				return gsbm.ErrWrongWireType
			}
			x, err := r.ReadVarint()
			if err != nil {
				return err
			}
			if x < math.MinInt32 || x > math.MaxInt32 {
				return gsbm.ErrIntegerOverflow
			}
			v.F4 = int(x)
		case 5:
			if wt != gsbm.WireVarint {
				return gsbm.ErrWrongWireType
			}
			x, err := r.ReadVarint()
			if err != nil {
				return err
			}
			if x < math.MinInt32 || x > math.MaxInt32 {
				return gsbm.ErrIntegerOverflow
			}
			v.F5 = int(x)
		case 6:
			if wt != gsbm.WireVarint {
				return gsbm.ErrWrongWireType
			}
			x, err := r.ReadVarint()
			if err != nil {
				return err
			}
			if x < math.MinInt32 || x > math.MaxInt32 {
				return gsbm.ErrIntegerOverflow
			}
			v.F6 = int(x)
		case 7:
			if wt != gsbm.WireVarint {
				return gsbm.ErrWrongWireType
			}
			x, err := r.ReadVarint()
			if err != nil {
				return err
			}
			if x < math.MinInt32 || x > math.MaxInt32 {
				return gsbm.ErrIntegerOverflow
			}
			v.F7 = int(x)
		case 8:
			if wt != gsbm.WireVarint {
				return gsbm.ErrWrongWireType
			}
			x, err := r.ReadVarint()
			if err != nil {
				return err
			}
			if x < math.MinInt32 || x > math.MaxInt32 {
				return gsbm.ErrIntegerOverflow
			}
			v.F8 = int(x)
		default:
			if err := r.SkipField(wt); err != nil {
				return err
			}
		}
	}
	return r.Err()
}

func (v *Int64) UnmarshalGSBM(r *gsbm.Reader) error {
	for r.HasMore() {
		tag, wt, err := r.ReadTag()
		if err != nil {
			return err
		}
		switch tag {
		case 1:
			if wt != gsbm.WireVarint {
				return gsbm.ErrWrongWireType
			}
			x, err := r.ReadVarint()
			if err != nil {
				return err
			}
			v.F1 = int(x)
		case 2:
			if wt != gsbm.WireVarint {
				return gsbm.ErrWrongWireType
			}
			x, err := r.ReadVarint()
			if err != nil {
				return err
			}
			v.F2 = int(x)
		case 3:
			if wt != gsbm.WireVarint {
				return gsbm.ErrWrongWireType
			}
			x, err := r.ReadVarint()
			if err != nil {
				return err
			}
			v.F3 = int(x)
		case 4:
			if wt != gsbm.WireVarint {
				return gsbm.ErrWrongWireType
			}
			x, err := r.ReadVarint()
			if err != nil {
				return err
			}
			v.F4 = int(x)
		case 5:
			if wt != gsbm.WireVarint {
				return gsbm.ErrWrongWireType
			}
			x, err := r.ReadVarint()
			if err != nil {
				return err
			}
			v.F5 = int(x)
		case 6:
			if wt != gsbm.WireVarint {
				return gsbm.ErrWrongWireType
			}
			x, err := r.ReadVarint()
			if err != nil {
				return err
			}
			v.F6 = int(x)
		case 7:
			if wt != gsbm.WireVarint {
				return gsbm.ErrWrongWireType
			}
			x, err := r.ReadVarint()
			if err != nil {
				return err
			}
			v.F7 = int(x)
		case 8:
			if wt != gsbm.WireVarint {
				return gsbm.ErrWrongWireType
			}
			x, err := r.ReadVarint()
			if err != nil {
				return err
			}
			v.F8 = int(x)
		default:
			if err := r.SkipField(wt); err != nil {
				return err
			}
		}
	}
	return r.Err()
}

func (v *Plain) Reset() { v.Record = Record{} }
func (v *Int32) Reset() { v.Record = Record{} }
func (v *Int64) Reset() { v.Record = Record{} }
