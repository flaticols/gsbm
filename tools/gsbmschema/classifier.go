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
			if ch.Acknowledged == "" && c.AllowBreaking != "" && ch.Severity == SeverityBreaking {
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
			if pf.Deprecated {
				code = "field/removed-deprecated"
			}
			add(Change{Severity: sev, Code: code,
				Subject: fmt.Sprintf("%s.%s (tag %d)", key, pf.Name, tag),
				Detail:  "field removed; the spec's append-only policy requires keeping it (use deprecated rather than delete)",
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
		if cf.Wire != pf.Wire && !shapeFrozen {
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
