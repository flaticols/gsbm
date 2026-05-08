package odmschema

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strings"
)

// ComputeSchVer derives the uint16 fingerprint surfaced in the blob
// header (offset 6..7). It is computed from a canonical line-oriented
// description of the schema, which keeps it stable across runs and lets
// reviewers see exactly which inputs feed the hash.
//
// IMPORTANT: schVer is observability only — decoders MUST NOT branch on
// it (spec §2.1). We deliberately discard most of the SHA-256 output to
// fit 16 bits; collisions are expected and harmless.
//
// The hash also explicitly mixes in FmtVer so a hypothetical future
// re-use of the same logical schema under a different fmtVer still
// produces a different schVer.
func ComputeSchVer(s *Schema) uint16 {
	canon := canonicalize(s)
	sum := sha256.Sum256([]byte(canon))
	return binary.BigEndian.Uint16(sum[:2])
}

// canonicalize renders s as a deterministic newline-delimited string
// covering every input the classifier and decoder care about. It is
// also handy as a `--debug-schver` output in the CLI.
func canonicalize(s *Schema) string {
	var b strings.Builder
	fmt.Fprintf(&b, "fmtVer=%d\n", FmtVer)
	for _, r := range s.Roots {
		fmt.Fprintf(&b, "root=%s\n", refKey(r))
	}
	for _, sd := range s.Structs {
		fmt.Fprintf(&b, "struct=%s opaque=%t generic=%s\n",
			refKey(sd.Type), sd.Opaque, strings.Join(sd.Generic, ","))
		for _, t := range sd.Reserved {
			fmt.Fprintf(&b, "  reserved=%d\n", t)
		}
		for _, f := range sd.Fields {
			fmt.Fprintf(&b,
				"  field tag=%d name=%s type=%s wire=%s optional=%t deprecated=%t cycleBreak=%t mapKey=%s mapValue=%s elem=%s\n",
				f.Tag, f.Name, f.Type, f.Wire, f.Optional, f.Deprecated, f.CycleBreak, f.MapKey, f.MapValue, f.Elem)
		}
	}
	return b.String()
}
