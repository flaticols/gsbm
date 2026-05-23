package gsbmschema

import (
	"fmt"
	"slices"
	"sort"
)

// Severity is a classifier label. Order matters: comparison operators
// rely on the numeric values (Breaking > Warning > Safe).
type Severity int

const (
	SeveritySafe Severity = iota
	SeverityWarning
	SeverityBreaking
)

func (s Severity) String() string {
	switch s {
	case SeveritySafe:
		return "safe"
	case SeverityWarning:
		return "warning"
	case SeverityBreaking:
		return "breaking"
	default:
		return fmt.Sprintf("severity(%d)", int(s))
	}
}

// Change is one row in a Diff: a single named structural delta with
// its severity and an optional acknowledged-breaking justification.
type Change struct {
	Severity     Severity
	Code         string // stable code, e.g. "field/added", "field/removed"
	Subject      string // qualified name, e.g. "pkg.Type.FieldName"
	Detail       string // human-readable description
	Acknowledged string // text from //gsbm:allow-breaking, when present
}

// Diff is the classifier output. MaxSeverity is convenient for CI gating
// (block PR iff MaxSeverity == SeverityBreaking and not Acknowledged).
type Diff struct {
	Changes     []Change
	MaxSeverity Severity
}

// Classify computes the diff between the previous (committed) snapshot
// and the proposed (current branch) snapshot. The Roots set is treated
// as additive: removing a root is breaking; adding a root is safe.
func Classify(prev, curr *Schema) Diff {
	d := Diff{}
	add := func(c Change) {
		if c.Severity > d.MaxSeverity {
			d.MaxSeverity = c.Severity
		}
		d.Changes = append(d.Changes, c)
	}

	// Roots.
	prevRoots := refKeySet(prev.Roots)
	currRoots := refKeySet(curr.Roots)
	for k := range prevRoots {
		if !currRoots[k] {
			add(Change{Severity: SeverityBreaking, Code: "root/removed", Subject: k,
				Detail: "root type removed from schema"})
		}
	}
	for k := range currRoots {
		if !prevRoots[k] {
			add(Change{Severity: SeveritySafe, Code: "root/added", Subject: k,
				Detail: "new root type"})
		}
	}

	// Structs.
	prevStructs := indexByKey(prev.Structs)
	currStructs := indexByKey(curr.Structs)
	for k, p := range prevStructs {
		c, ok := currStructs[k]
		if !ok {
			add(Change{Severity: SeverityBreaking, Code: "struct/removed", Subject: k,
				Detail: "struct removed from closure"})
			continue
		}
		add2 := func(ch Change) {
			// field/compat-write-removed and field/removed-compat-write are
			// intentionally excluded from the source-level
			// //gsbm:allow-breaking acknowledgement. Stopping the compat_write
			// dual-write is gated on calendar bake-time, which only the
			// operator can attest to via the --allow-stop-compat-write CLI
			// flag at diff time. Allowing a source annotation to satisfy it
			// — including by stacking compat_write→removed in one PR —
			// would defeat the operator-only safeguard.
			if ch.Acknowledged == "" && c.AllowBreaking != "" && ch.Severity == SeverityBreaking &&
				ch.Code != "field/compat-write-removed" &&
				ch.Code != "field/removed-compat-write" {
				ch.Acknowledged = c.AllowBreaking
			}
			add(ch)
		}
		classifyStruct(k, p, c, add2)
	}
	for k, c := range currStructs {
		if _, ok := prevStructs[k]; ok {
			continue
		}
		add(Change{Severity: SeveritySafe, Code: "struct/added", Subject: k,
			Detail: fmt.Sprintf("new struct in closure (%d fields)", len(c.Fields))})
	}
	// Stable order for downstream consumers (CI summary, PR comment).
	sort.SliceStable(d.Changes, func(i, j int) bool {
		a, b := d.Changes[i], d.Changes[j]
		if a.Severity != b.Severity {
			return a.Severity > b.Severity
		}
		if a.Subject != b.Subject {
			return a.Subject < b.Subject
		}
		return a.Code < b.Code
	})
	return d
}

func classifyStruct(key string, prev, curr *StructDecl, add func(Change)) {
	prevFields := indexFields(prev.Fields)
	currFields := indexFields(curr.Fields)
	prevByName := indexFieldsByName(prev.Fields)

	// Fields removed by tag.
	for tag, pf := range prevFields {
		cf, ok := currFields[tag]
		if !ok {
			// Removed.
			sev := SeverityBreaking
			code := "field/removed"
			detail := "field removed; the spec's append-only policy requires keeping it (use deprecated rather than delete)"
			switch {
			case pf.Deprecated && pf.CompatWrite:
				// Distinct from field/removed-deprecated because the operator-only
				// bake-time safeguard still applies: removing while in compat_write
				// stops the dual-write just as much as compat_write→deprecated does.
				code = "field/removed-compat-write"
				detail = "field removed while still in compat_write window (encoder was dual-writing); transition to plain deprecated under --allow-stop-compat-write first, then //gsbm:reserved the tag"
			case pf.Deprecated:
				code = "field/removed-deprecated"
				detail = "deprecated field removed; add the tag to //gsbm:reserved instead so it stays unavailable for future fields"
			}
			add(Change{Severity: sev, Code: code,
				Subject: fmt.Sprintf("%s.%s (tag %d)", key, pf.Name, tag),
				Detail:  detail,
			})
			continue
		}
		// Type/wire/optional checks are skipped only while the field is
		// off the wire in both snapshots — a deprecated (and not
		// compat_write) field is off the wire, so changing its declared
		// shape is harmless. Resurrecting or staying in compat_write puts
		// the field back on the wire, and any shape change at that moment
		// is breaking, not just a warning.
		shapeFrozen := pf.Deprecated && !pf.CompatWrite && cf.Deprecated && !cf.CompatWrite
		if cf.Type != pf.Type && !shapeFrozen {
			add(Change{Severity: SeverityBreaking, Code: "field/type-changed",
				Subject: fmt.Sprintf("%s.%s (tag %d)", key, pf.Name, tag),
				Detail:  fmt.Sprintf("type %s → %s", pf.Type, cf.Type),
			})
		}
		// Named slice alias transitions. The wire-bytes-relevant shape is
		// recorded in fd.Type via the underlying slice form, so a change
		// to the slice element is already covered by field/type-changed.
		// What field/type-changed CANNOT see is the alias identity: a
		// rename (`type ItemList []Item` → `type Items []Item`) keeps
		// wire bytes identical and should be safe; an alias added or
		// removed against an unchanged underlying slice (`[]Item` ↔
		// `ItemList []Item`) is likewise safe. Underlying differences
		// surface here too as breaking, but the actionable signal is
		// already field/type-changed — emit the dedicated alias change
		// only for the cases field/type-changed cannot see. Gate every
		// alias-* code on pf.Type == cf.Type: when the underlying slice
		// shape itself changed, field/type-changed has the breaking
		// signal and the alias delta would falsely claim "wire bytes
		// unchanged".
		if !shapeFrozen && pf.Type == cf.Type {
			pa, ca := pf.AliasType, cf.AliasType
			switch {
			case pa == nil && ca == nil:
				// nothing to compare
			case pa == nil && ca != nil:
				add(Change{Severity: SeveritySafe, Code: "field/alias-added",
					Subject: fmt.Sprintf("%s.%s (tag %d)", key, cf.Name, tag),
					Detail:  fmt.Sprintf("named slice alias %s wraps the existing underlying slice; wire bytes unchanged", typeRefString(*ca))})
			case pa != nil && ca == nil:
				add(Change{Severity: SeveritySafe, Code: "field/alias-removed",
					Subject: fmt.Sprintf("%s.%s (tag %d)", key, cf.Name, tag),
					Detail:  fmt.Sprintf("named slice alias %s removed; field declared with the underlying slice directly; wire bytes unchanged", typeRefString(*pa))})
			default:
				if refKey(*pa) != refKey(*ca) && underlyingEqual(pa.Underlying, ca.Underlying) {
					add(Change{Severity: SeveritySafe, Code: "field/alias-renamed",
						Subject: fmt.Sprintf("%s.%s (tag %d)", key, cf.Name, tag),
						Detail:  fmt.Sprintf("named slice alias %s → %s; underlying unchanged", typeRefString(*pa), typeRefString(*ca))})
				}
			}
		}
		// Flattened-from transitions. Moving a tag from a direct field
		// into an embedded base (or vice versa) preserves the wire bytes
		// when the type/wire shape is identical — embedding is a source-
		// level refactor that the codegen handles transparently. Gate on
		// pf.Type == cf.Type so a refactor that also changes the field's
		// shape surfaces through field/type-changed instead, where the
		// "wire bytes unchanged" detail would be a lie. The
		// FlattenedFromPointer flag controls codegen (nil-check on encode,
		// lazy allocation on decode) but does not change the per-field
		// wire shape, so swapping value-embed ↔ pointer-embed against an
		// otherwise-identical field set is still safe at the schema layer.
		if !shapeFrozen && pf.Type == cf.Type && pf.Wire == cf.Wire {
			pff, cff := pf.FlattenedFrom, cf.FlattenedFrom
			subject := fmt.Sprintf("%s.%s (tag %d)", key, cf.Name, tag)
			switch {
			case pff == "" && cff == "":
				// nothing to compare
			case pff == "" && cff != "":
				add(Change{Severity: SeveritySafe, Code: "field/flattened-from-added",
					Subject: subject,
					Detail:  fmt.Sprintf("field promoted from embedded %s; wire bytes unchanged", cff)})
			case pff != "" && cff == "":
				add(Change{Severity: SeveritySafe, Code: "field/flattened-from-removed",
					Subject: subject,
					Detail:  fmt.Sprintf("field demoted from embedded %s into a direct declaration; wire bytes unchanged", pff)})
			default:
				if pff != cff {
					add(Change{Severity: SeveritySafe, Code: "field/flattened-from-changed",
						Subject: subject,
						Detail:  fmt.Sprintf("embed chain %s → %s; wire bytes unchanged", pff, cff)})
				}
			}
		}
		// Wire-shape diff. Skip when either side carries a custom codec:
		// the codec, not the Go type, defines the wire shape, so the
		// custom-add/remove/swap event already represents the wire change.
		// Without this gate, toggling `custom=` on a previously non-custom
		// field would double-fire as `field/custom-added` + a false
		// `field/wire-changed: varint → "" ` (discover.go leaves Wire
		// empty for custom-codec fields).
		if cf.Wire != pf.Wire && !shapeFrozen && pf.Custom == "" && cf.Custom == "" {
			add(Change{Severity: SeverityBreaking, Code: "field/wire-changed",
				Subject: fmt.Sprintf("%s.%s (tag %d)", key, pf.Name, tag),
				Detail:  fmt.Sprintf("wire-type %s → %s", pf.Wire, cf.Wire),
			})
		}
		// Toggling Optional flips the encoded body shape — required uses the
		// primitive body directly, optional wraps it in a length-delim region
		// with a presence byte. Old readers and new readers cannot interop
		// across the change. fillTypeShape strips pointers before computing
		// Type/Wire, so this is the only signal that catches the toggle.
		if cf.Optional != pf.Optional && !shapeFrozen {
			add(Change{Severity: SeverityBreaking, Code: "field/optional-changed",
				Subject: fmt.Sprintf("%s.%s (tag %d)", key, pf.Name, tag),
				Detail:  fmt.Sprintf("optional %t → %t", pf.Optional, cf.Optional),
			})
		}
		// Lifecycle transitions across active → compat_write → deprecated.
		// A single transition emits one change so reviewers see one event
		// per migration step, not three independent edits.
		prevState := lifecycleState(pf)
		currState := lifecycleState(cf)
		if prevState != currState {
			subject := fmt.Sprintf("%s.%s (tag %d)", key, cf.Name, tag)
			switch {
			case prevState == fieldActive && currState == fieldCompatWrite:
				add(Change{Severity: SeveritySafe, Code: "field/compat-write-added",
					Subject: subject,
					Detail:  "field entered compat_write window: deprecated, encoder still emits during rollback bake"})
			case prevState == fieldActive && currState == fieldDeprecated:
				add(Change{Severity: SeverityWarning, Code: "field/deprecated",
					Subject: subject,
					Detail:  "field marked deprecated without a compat_write window; a rollback to old code may see this field disappear from new writes — consider landing compat_write first"})
			case prevState == fieldCompatWrite && currState == fieldDeprecated:
				add(Change{Severity: SeverityBreaking, Code: "field/compat-write-removed",
					Subject: subject,
					Detail:  "encoder is no longer dual-writing during the rollback window; pass --allow-stop-compat-write once the bake-time has elapsed"})
			case prevState == fieldCompatWrite && currState == fieldActive:
				add(Change{Severity: SeverityWarning, Code: "field/resurrected",
					Subject: subject,
					Detail:  "previously compat_write field is no longer marked deprecated"})
			case prevState == fieldDeprecated && currState == fieldActive:
				add(Change{Severity: SeverityWarning, Code: "field/resurrected",
					Subject: subject,
					Detail:  "previously deprecated field is no longer marked deprecated"})
			case prevState == fieldDeprecated && currState == fieldCompatWrite:
				add(Change{Severity: SeveritySafe, Code: "field/compat-write-added",
					Subject: subject,
					Detail:  "field re-entered compat_write window"})
			}
		}
		if cf.Name != pf.Name {
			// Rename keeps the tag → safe.
			add(Change{Severity: SeveritySafe, Code: "field/renamed",
				Subject: fmt.Sprintf("%s tag %d", key, tag),
				Detail:  fmt.Sprintf("rename %s → %s", pf.Name, cf.Name)})
		}
		// Cycle-break flag transitions. Toggling //gsbm:cycle_break_via_id
		// (or the equivalent `bin:"N,id_ref"` tag option) flips the field's
		// body shape on the wire: with the flag set, the field is encoded as
		// the referenced struct's bin:"1" ID field (a leaf scalar);
		// without it, the field is the full nested struct body. Old readers
		// and new readers cannot interop across the toggle, so both
		// directions are breaking. Skipped while the field stays deprecated
		// in both snapshots, mirroring the type/wire/optional checks above.
		if cf.CycleBreak != pf.CycleBreak && !shapeFrozen {
			switch {
			case !pf.CycleBreak && cf.CycleBreak:
				add(Change{Severity: SeverityBreaking, Code: "field/cycle-break-added",
					Subject: fmt.Sprintf("%s.%s (tag %d)", key, cf.Name, tag),
					Detail:  "id_ref added: payload changes from nested struct body to the target's bin:\"1\" ID scalar"})
			default:
				add(Change{Severity: SeverityBreaking, Code: "field/cycle-break-removed",
					Subject: fmt.Sprintf("%s.%s (tag %d)", key, cf.Name, tag),
					Detail:  "id_ref removed: payload changes from the target's bin:\"1\" ID scalar back to nested struct body"})
			}
		}
		// Map-key underlying-primitive change. For `map[Code]V` where
		// `type Code string`, swapping Code's underlying from string to
		// int64 (or any other primitive) keeps the named identifier but
		// changes the wire encoding of every key, so old blobs cannot be
		// decoded under the new schema. Skipped while the field is off
		// the wire in both snapshots, mirroring the type/wire/optional
		// checks above. The same shift also flips fd.Type via the shape
		// string (e.g. `map[Code(string)]V` → `map[Code(int64)]V`) and
		// would already be flagged as field/type-changed, but a dedicated
		// code lets reviewers see exactly what kind of change this is.
		// Gate on both sides having a named key (MapKeyUnderlying != ""):
		// a scalar↔map flip or a named↔raw key swap (e.g. `map[Code]V` ↔
		// `map[string]V`) leaves the key wire bytes unchanged when the
		// underlying primitive is identical, so it would be misleading to
		// claim "wire encoding changes" here. Those transitions are still
		// surfaced — field/type-changed already reports the schema-type
		// change.
		if pf.MapKeyUnderlying != "" && cf.MapKeyUnderlying != "" && cf.MapKeyUnderlying != pf.MapKeyUnderlying && !shapeFrozen {
			add(Change{Severity: SeverityBreaking, Code: "field/map-key-underlying-changed",
				Subject: fmt.Sprintf("%s.%s (tag %d)", key, cf.Name, tag),
				Detail:  fmt.Sprintf("map-key underlying %q → %q (key wire encoding changes; old blobs cannot be decoded)", pf.MapKeyUnderlying, cf.MapKeyUnderlying),
			})
		}
		// Width-override transitions on integer fields. The `type=W`
		// option pins or narrows the on-wire width without changing
		// fd.Type or fd.Wire, so neither field/type-changed nor
		// field/wire-changed fires — but the wire-effect contract is real
		// (docs/codecs/compatibility.md). The contract applies uniformly
		// across all integer kinds (int/uint/uintptr platform-sized and
		// the eight fixed widths int8/16/32/64, uint8/16/32/64), plus
		// named aliases of any of those. Widening (a smaller effective
		// wire width → a larger one) is safe-forward: old readers
		// gracefully reject any out-of-range value with ErrIntegerOverflow
		// rather than silently truncating. Narrowing (larger → smaller)
		// is breaking: historical blobs with values outside the new
		// width's range will reject at decode under the new schema.
		// Same-width annotation flips (e.g. un-annotated int32 ↔ `type=int32`
		// identity marker, or `int64 type=int32` ↔ a different identical-width
		// override) are byte-identical, intent-only. Skipped while the
		// field stays deprecated in both snapshots, and when either side
		// carries a custom codec (the custom-* events already represent
		// the wire change).
		if cf.WireOverride != pf.WireOverride && !shapeFrozen && pf.Custom == "" && cf.Custom == "" {
			pbits, _, pok := effectiveWireWidthFromSnapshot(pf.Type, pf.WireOverride)
			cbits, _, cok := effectiveWireWidthFromSnapshot(cf.Type, cf.WireOverride)
			if pok && cok {
				subject := fmt.Sprintf("%s.%s (tag %d)", key, cf.Name, tag)
				switch {
				case cbits > pbits:
					add(Change{Severity: SeveritySafe, Code: "field/wire-widened",
						Subject: subject,
						Detail:  fmt.Sprintf("wire width %q → %q (%d-bit → %d-bit); old readers gracefully reject out-of-range values with ErrIntegerOverflow", pf.WireOverride, cf.WireOverride, pbits, cbits)})
				case cbits < pbits:
					add(Change{Severity: SeverityBreaking, Code: "field/wire-narrowed",
						Subject: subject,
						Detail:  fmt.Sprintf("wire width %q → %q (%d-bit → %d-bit); historical blobs with values outside the new width's range will reject at decode", pf.WireOverride, cf.WireOverride, pbits, cbits)})
				default:
					add(Change{Severity: SeveritySafe, Code: "field/wire-intent-changed",
						Subject: subject,
						Detail:  fmt.Sprintf("wire width annotation %q → %q; byte-identical on the wire", pf.WireOverride, cf.WireOverride)})
				}
			}
		}
		// Custom-marshaler annotation transitions. Adding a custom codec
		// changes the field's emitted body shape, so it is a warning per the
		// spec ("add custom marshaler annotation"). Removing or swapping the
		// custom name reverts the wire shape to the default codec (or to a
		// different custom shape), which old readers cannot interop with —
		// breaking. Skipped while the field stays deprecated in both
		// snapshots, mirroring the type/wire/optional checks above.
		if cf.Custom != pf.Custom && !shapeFrozen {
			switch {
			case pf.Custom == "" && cf.Custom != "":
				add(Change{Severity: SeverityWarning, Code: "field/custom-added",
					Subject: fmt.Sprintf("%s.%s (tag %d)", key, cf.Name, tag),
					Detail:  fmt.Sprintf("custom marshaler %q", cf.Custom)})
			case pf.Custom != "" && cf.Custom == "":
				add(Change{Severity: SeverityBreaking, Code: "field/custom-removed",
					Subject: fmt.Sprintf("%s.%s (tag %d)", key, cf.Name, tag),
					Detail:  fmt.Sprintf("custom marshaler %q removed; reverts to default codec", pf.Custom)})
			default:
				add(Change{Severity: SeverityBreaking, Code: "field/custom-changed",
					Subject: fmt.Sprintf("%s.%s (tag %d)", key, cf.Name, tag),
					Detail:  fmt.Sprintf("custom marshaler %q → %q", pf.Custom, cf.Custom)})
			}
		}
	}
	// Fields added.
	for tag, cf := range currFields {
		if _, ok := prevFields[tag]; ok {
			continue
		}
		// If the same name existed under a different tag previously,
		// that's a tag-of-existing-field change (breaking).
		if pf, ok := prevByName[cf.Name]; ok {
			if pf.Tag != cf.Tag {
				add(Change{Severity: SeverityBreaking, Code: "field/tag-changed",
					Subject: fmt.Sprintf("%s.%s", key, cf.Name),
					Detail:  fmt.Sprintf("tag %d → %d", pf.Tag, cf.Tag)})
				continue
			}
		}
		add(Change{Severity: SeveritySafe, Code: "field/added",
			Subject: fmt.Sprintf("%s.%s (tag %d)", key, cf.Name, tag),
			Detail:  fmt.Sprintf("type %s, wire %s", cf.Type, cf.Wire)})
	}

	// Reserved-set changes: shrinking the reserved set is breaking
	// (someone might rely on those tags staying off-limits); growing is
	// safe.
	prevR := append([]uint32(nil), prev.Reserved...)
	currR := append([]uint32(nil), curr.Reserved...)
	slices.Sort(prevR)
	slices.Sort(currR)
	prevSet := uint32SliceToSet(prevR)
	currSet := uint32SliceToSet(currR)
	for t := range prevSet {
		if !currSet[t] {
			add(Change{Severity: SeverityBreaking, Code: "reserved/removed",
				Subject: fmt.Sprintf("%s tag %d", key, t),
				Detail:  "reserved tag is no longer reserved (a future field could collide with old data)"})
		}
	}
	for t := range currSet {
		if !prevSet[t] {
			add(Change{Severity: SeveritySafe, Code: "reserved/added",
				Subject: fmt.Sprintf("%s tag %d", key, t),
				Detail:  "reserved tag added"})
		}
	}

	// Opaque toggle.
	if prev.Opaque != curr.Opaque {
		sev := SeverityWarning
		add(Change{Severity: sev, Code: "struct/opaque-changed",
			Subject: key,
			Detail:  fmt.Sprintf("opaque %t → %t", prev.Opaque, curr.Opaque)})
	}
}

// underlyingEqual compares two optional Underlying TypeRefs by their
// canonical refKey form. Two nil pointers are equal; nil vs non-nil are
// not. Used to gate the alias-rename safe path: name differs but
// underlying matches → safe.
func underlyingEqual(a, b *TypeRef) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return refKey(*a) == refKey(*b)
}

func refKeySet(rs []TypeRef) map[string]bool {
	m := make(map[string]bool, len(rs))
	for _, r := range rs {
		m[refKey(r)] = true
	}
	return m
}

func indexByKey(structs []*StructDecl) map[string]*StructDecl {
	m := make(map[string]*StructDecl, len(structs))
	for _, s := range structs {
		m[refKey(s.Type)] = s
	}
	return m
}

func indexFields(fs []*FieldDecl) map[uint32]*FieldDecl {
	m := make(map[uint32]*FieldDecl, len(fs))
	for _, f := range fs {
		m[f.Tag] = f
	}
	return m
}

func indexFieldsByName(fs []*FieldDecl) map[string]*FieldDecl {
	m := make(map[string]*FieldDecl, len(fs))
	for _, f := range fs {
		m[f.Name] = f
	}
	return m
}

// fieldState is the position of a field in the active → compat_write →
// deprecated lifecycle. Reserved is a struct-level concept (the field is
// removed from the closure entirely) so it does not appear here.
type fieldState int

const (
	fieldActive fieldState = iota
	fieldCompatWrite
	fieldDeprecated
)

func lifecycleState(f *FieldDecl) fieldState {
	switch {
	case f.Deprecated && f.CompatWrite:
		return fieldCompatWrite
	case f.Deprecated:
		return fieldDeprecated
	default:
		return fieldActive
	}
}

func uint32SliceToSet(s []uint32) map[uint32]bool {
	m := make(map[uint32]bool, len(s))
	for _, t := range s {
		m[t] = true
	}
	return m
}
