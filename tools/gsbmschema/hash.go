package gsbmschema

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strings"
)

// ComputeSchemaHint derives the uint16 schema-grouping hint surfaced in
// the blob header (offset 6..7). It is computed from a canonical
// line-oriented description of the schema, which keeps it stable across
// runs and lets reviewers see exactly which inputs feed the hash.
//
// IMPORTANT: schemaHint is observability only — decoders MUST NOT
// branch on it (spec §2.1). It is a weak grouping hint, not a unique
// fingerprint and not a drift-detection mechanism: with only 16 bits it
// collides at ~256 distinct schemas (birthday bound). We deliberately
// discard most of the SHA-256 output to fit 16 bits; collisions are
// expected and harmless.
//
// The hash also explicitly mixes in FmtVer so a hypothetical future
// re-use of the same logical schema under a different fmtVer still
// produces a different schemaHint.
func ComputeSchemaHint(s *Schema) uint16 {
	canon := canonicalize(s)
	sum := sha256.Sum256([]byte(canon))
	return binary.BigEndian.Uint16(sum[:2])
}

// canonicalize renders s as a deterministic newline-delimited string
// covering every input the classifier and decoder care about. It is
// also handy as a `--debug-schema-hint` output in the CLI.
func canonicalize(s *Schema) string {
	var b strings.Builder
	_, _ = fmt.Fprintf(&b, "fmtVer=%d\n", FmtVer)
	for _, r := range s.Roots {
		_, _ = fmt.Fprintf(&b, "root=%s\n", refKey(r))
	}
	for _, sd := range s.Structs {
		_, _ = fmt.Fprintf(&b, "struct=%s opaque=%t generic=%s\n",
			refKey(sd.Type), sd.Opaque, strings.Join(sd.Generic, ","))
		for _, t := range sd.Reserved {
			_, _ = fmt.Fprintf(&b, "  reserved=%d\n", t)
		}
		for _, f := range sd.Fields {
			_, _ = fmt.Fprintf(&b,
				"  field tag=%d name=%s type=%s wire=%s optional=%t deprecated=%t compatWrite=%t cycleBreak=%t mapKey=%s mapValue=%s mapKeyUnderlying=%s elem=%s custom=%s wireOverride=%s aliasType=%s flattenedFrom=%s flattenedFromPointer=%t\n",
				f.Tag, f.Name, f.Type, f.Wire, f.Optional, f.Deprecated, f.CompatWrite, f.CycleBreak, f.MapKey, f.MapValue, f.MapKeyUnderlying, f.Elem, f.Custom, f.WireOverride, aliasTypeKey(f.AliasType), f.FlattenedFrom, f.FlattenedFromPointer)
		}
	}
	return b.String()
}

// aliasTypeKey renders a FieldDecl.AliasType for inclusion in the
// canonical hash input. nil renders as the empty string. The canonical
// field line always carries a trailing `aliasType=` segment, so adding
// this field to the schema model shifts every existing schema's hint
// by one (planned, see the plan's Post-Completion note).
func aliasTypeKey(r *TypeRef) string {
	if r == nil {
		return ""
	}
	base := refKey(*r)
	if r.Underlying == nil {
		return base
	}
	return base + "(" + refKey(*r.Underlying) + ")"
}
