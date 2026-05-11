// Package codecs holds the codegen-time registry of named custom codecs.
//
// A custom codec lets a schema field opt out of the standard type-traversal
// emit path: instead of recursing into the field's Go type, codegen emits a
// direct call to a user-supplied pair of free functions registered against a
// name. The schema records the codec name only (see FieldDecl.Custom); this
// package resolves that name to a CodecDecl at codegen time and the emitter
// renders the call site.
//
// Registration is a codegen-time concern, not a runtime one — there is no
// global registry, no init-time side effects, no vtable. A typical caller
// constructs a Registry once per codegen invocation (e.g. via
// NewBuiltinRegistry), adds project-specific codecs, and hands it to the
// emitter.
package codecs

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Wire-type labels used in CodecDecl.WireType. These match the
// gsbmschema.Wire* constants byte-for-byte so a codec's declared wire type
// can flow straight into FieldDecl.Wire / snapshot without translation.
// Use WireIdent to recover the corresponding storage/gsbm Go identifier
// (WireVarint, WireLengthDelim, …) for emit-time rendering.
const (
	WireVarint      = "varint"
	WireFixed64     = "fixed64"
	WireFixed32     = "fixed32"
	WireLengthDelim = "length-delim"
)

// CodecDecl is the codegen-time descriptor of a named custom codec. It
// carries the wire-type the codec advertises and the fully-qualified names
// of the encode/decode functions; the emitter renders calls of the form
// `<pkg>.<EncodeFn>(w, v.Field)` and `<pkg>.<DecodeFn>(r, &v.Field)`.
//
// Field semantics:
//   - Name is the identifier used in `bin:"N,custom=Name"` tags. Must match
//     `[A-Za-z_][A-Za-z0-9_]*` (a Go identifier); the registry does not
//     enforce that today — invalid names surface at codegen time when the
//     emitter tries to render an unparsable tag.
//   - GoType is the fully-qualified type the codec handles, e.g.
//     `time.Time` or `myapp/v1.Decimal`. Surfaced in diagnostics so a
//     mistyped `custom=` points the user at the wrong type cleanly.
//   - WireType is one of WireVarint/WireFixed64/WireFixed32/WireLengthDelim.
//   - EncodeFn / DecodeFn are unqualified function identifiers inside
//     PkgImport (e.g. "EncodeTimeUnixNano"). Codegen prepends the package
//     alias when emitting calls.
//   - PkgImport is the Go import path that defines the encode/decode
//     functions; codegen adds this to the generated file's imports.
type CodecDecl struct {
	Name      string
	GoType    string
	WireType  string
	EncodeFn  string
	DecodeFn  string
	PkgImport string
}

// Registry is the codegen-time map of codec names to CodecDecls. Two
// distinct registrations against the same name are an error — codecs are
// load-bearing for wire shape and a silent overwrite would mean two
// different builds of the same schema emit different bytes.
type Registry struct {
	mu sync.RWMutex
	m  map[string]CodecDecl
}

// NewRegistry returns an empty Registry. Use NewBuiltinRegistry for the
// common case of starting with the ship-default codecs already present.
func NewRegistry() *Registry {
	return &Registry{m: map[string]CodecDecl{}}
}

// ErrDuplicateCodec is returned by Register when the same codec name is
// registered twice with non-identical CodecDecls. Re-registering an
// identical CodecDecl is a no-op.
var ErrDuplicateCodec = errors.New("codecs: duplicate registration")

// Register adds c to r. Returns ErrDuplicateCodec wrapped with the name
// when a different CodecDecl is already registered under c.Name; an
// idempotent re-registration (same CodecDecl value) is allowed.
func (r *Registry) Register(c CodecDecl) error {
	if c.Name == "" {
		return errors.New("codecs: empty codec name")
	}
	if c.EncodeFn == "" || c.DecodeFn == "" {
		return fmt.Errorf("codecs: %s: EncodeFn and DecodeFn must be set", c.Name)
	}
	if !isKnownWireType(c.WireType) {
		return fmt.Errorf("codecs: %s: unknown WireType %q (want %q, %q, %q, or %q)",
			c.Name, c.WireType, WireVarint, WireFixed64, WireFixed32, WireLengthDelim)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.m[c.Name]; ok {
		if existing == c {
			return nil
		}
		return fmt.Errorf("%w: %s already registered as %+v", ErrDuplicateCodec, c.Name, existing)
	}
	r.m[c.Name] = c
	return nil
}

// Lookup returns the CodecDecl registered under name and a found flag.
func (r *Registry) Lookup(name string) (CodecDecl, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.m[name]
	return c, ok
}

// Names returns the sorted list of registered codec names. Stable order
// matters for the unregistered diagnostic — users see the same suggestion
// list across runs, which makes typos easier to spot in CI logs.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.m))
	for n := range r.m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// UnregisteredError builds the diagnostic surfaced when a schema field
// references an unknown codec name. The error string carries the code
// `codec/unregistered` so callers (the codegen lint pipeline) can detect
// and classify it, plus the sorted list of registered names so a user
// staring at "did I typo it?" sees the closest hit immediately.
//
// Codegen calls this; the registry itself does not — Lookup returns
// (CodecDecl{}, false) and the caller chooses whether the absence is an
// error in its context.
func UnregisteredError(name string, registered []string) error {
	return fmt.Errorf("codec/unregistered: codec %q is not registered (registered: %v)", name, registered)
}

// WireIdent maps a CodecDecl.WireType label to the corresponding
// storage/gsbm Go identifier (e.g. "varint" → "WireVarint"). Returns ""
// for unknown labels — callers should validate via isKnownWireType first.
func WireIdent(wt string) string {
	switch wt {
	case WireVarint:
		return "WireVarint"
	case WireFixed64:
		return "WireFixed64"
	case WireFixed32:
		return "WireFixed32"
	case WireLengthDelim:
		return "WireLengthDelim"
	}
	return ""
}

func isKnownWireType(wt string) bool {
	switch wt {
	case WireVarint, WireFixed64, WireFixed32, WireLengthDelim:
		return true
	}
	return false
}
