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

// CodecKind classifies a CodecDecl as analytic (size derivable from v
// alone), materializing-cached (size depends on producing the body; result
// retained for the write pass), or streaming (size depends on producing
// the body; result discarded between passes, materialized twice).
// See CodecDecl.Kind for derivation rules.
type CodecKind int

const (
	// CodecKindAnalytic is a codec whose body size is a pure function of
	// v: declared via the `(SizeFn, EncodeFn)` pair. The size pass calls
	// SizeFn(v) without producing the body bytes; the write pass calls
	// EncodeFn(w, v).
	CodecKindAnalytic CodecKind = iota + 1
	// CodecKindMaterializing is a codec whose body size depends on
	// materializing the body (e.g. DecimalString, JSON, compression):
	// declared via EmitFn alone. The same EmitFn(w, v) runs in both
	// passes; the Writer is mode-aware (size vs write) and the
	// per-call scratch cache makes materialization happen exactly once.
	CodecKindMaterializing
	// CodecKindStreaming is a codec whose body size depends on
	// materializing the body, but where the materialized bytes are too
	// large to keep alongside the output buffer (e.g. ~100 MiB JSON,
	// compression of large blobs). Declared via StreamFn alone. The
	// same StreamFn(w, v) runs in both passes against a mode-aware
	// Writer; nothing is cached between passes, so the body is
	// materialized twice (2× CPU) but never retained alongside the
	// output (1× peak heap).
	CodecKindStreaming
)

// CodecDecl is the codegen-time descriptor of a named custom codec. It
// carries the wire-type the codec advertises and the fully-qualified names
// of the encode/decode functions; the emitter renders calls of the form
// `<pkg>.<EncodeFn>(w, v.Field)` and `<pkg>.<DecodeFn>(r, &v.Field)`.
//
// A CodecDecl declares its body emission shape in exactly one of three ways:
//
//   - Analytic (`SizeFn` AND `EncodeFn` set, `EmitFn` and `StreamFn` empty):
//     the codec can compute its body size from v alone. Codegen emits
//     `SizeFn(v)` in the size pass and `EncodeFn(w, v)` in the write pass —
//     the hot path stays branch-free.
//   - Materializing-cached (`EmitFn` set, all of `SizeFn`/`EncodeFn`/
//     `StreamFn` empty): the codec's body size depends on producing the
//     body. Codegen emits a single `EmitFn(w, v, callsite)` call in
//     MarshalGSBM; the Writer is mode-aware (size or write) so the same
//     function works in both passes. The Writer's scratch cache makes the
//     materialization run exactly once per gsbm.Marshal call — at the
//     cost of retaining the materialized bytes alongside the output.
//   - Streaming (`StreamFn` set, all of `SizeFn`/`EncodeFn`/`EmitFn`
//     empty): the codec's body size depends on producing the body, but
//     the materialized bytes are too large to retain alongside the
//     output (e.g. ~100 MiB JSON). Codegen emits `StreamFn(w, v)` with
//     no callsite argument; the same function runs in both passes
//     against a mode-aware Writer, and the result is discarded between
//     passes. The body is therefore materialized twice (2× CPU) but
//     never retained (1× peak heap).
//
// DecodeFn is required for all kinds.
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
//   - EncodeFn / SizeFn are unqualified function identifiers inside
//     PkgImport for the analytic shape (e.g. "EncodeTime", "SizeTime").
//     SizeFn must return `int` and have the same
//     value-parameter shape as EncodeFn so the emitter can swap
//     `EncodeFn(w, v)` for `SizeFn(v)` at the call site. They must be
//     set together or both empty.
//   - EmitFn is the unqualified materializing-codec function identifier;
//     it has signature `func(w *gsbm.Writer, v T, callsite uint64) error`
//     and is called in both size and write passes against a mode-aware
//     Writer. Mutually exclusive with SizeFn/EncodeFn and StreamFn.
//   - StreamFn is the unqualified streaming-codec function identifier;
//     it has signature `func(w *gsbm.Writer, v T) error` — no callsite,
//     no caching. Called in both passes against a mode-aware Writer;
//     the body is materialized once per pass. Mutually exclusive with
//     SizeFn/EncodeFn and EmitFn.
//   - DecodeFn is required for every codec.
//   - PkgImport is the Go import path that defines the encode/decode/
//     size/emit/stream functions; codegen adds this to the generated
//     file's imports.
type CodecDecl struct {
	Name      string
	GoType    string
	WireType  string
	EncodeFn  string
	DecodeFn  string
	SizeFn    string
	EmitFn    string
	StreamFn  string
	PkgImport string
}

// Kind returns the codec's emission shape derived from which fields are
// set. CodecKindAnalytic when (SizeFn, EncodeFn) is set; CodecKindMaterializing
// when EmitFn alone is set; CodecKindStreaming when StreamFn alone is set;
// the zero value (no recognized combination, or multiple) when the decl is
// invalid — Register rejects such decls before they reach lookup, so callers
// traversing a built Registry can treat the zero value as unreachable.
func (c CodecDecl) Kind() CodecKind {
	switch {
	case c.EmitFn != "" && c.SizeFn == "" && c.EncodeFn == "" && c.StreamFn == "":
		return CodecKindMaterializing
	case c.StreamFn != "" && c.SizeFn == "" && c.EncodeFn == "" && c.EmitFn == "":
		return CodecKindStreaming
	case c.EmitFn == "" && c.StreamFn == "" && c.SizeFn != "" && c.EncodeFn != "":
		return CodecKindAnalytic
	}
	return 0
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
	if c.DecodeFn == "" {
		return fmt.Errorf("codecs: %s: DecodeFn must be set", c.Name)
	}
	hasAnalytic := c.SizeFn != "" || c.EncodeFn != ""
	hasEmit := c.EmitFn != ""
	hasStream := c.StreamFn != ""
	switch {
	case hasStream && (hasEmit || hasAnalytic):
		return fmt.Errorf("codec/conflicting-kinds: codec %q: StreamFn is mutually exclusive with EmitFn and SizeFn/EncodeFn (analytic shape uses (SizeFn, EncodeFn); materializing-cached shape uses EmitFn alone; streaming shape uses StreamFn alone)", c.Name)
	case hasEmit && hasAnalytic:
		return fmt.Errorf("codec/conflicting-kinds: codec %q: EmitFn is mutually exclusive with SizeFn/EncodeFn (analytic shape uses (SizeFn, EncodeFn); materializing-cached shape uses EmitFn alone; streaming shape uses StreamFn alone)", c.Name)
	case hasEmit, hasStream:
		// materializing-cached or streaming: a single non-empty function
		// identifier — already validated above as non-conflicting.
	case c.EncodeFn == "":
		return fmt.Errorf("codec/missing-size-fn: codec %q: none of the three codec shapes were declared — set both SizeFn and EncodeFn for an analytic codec, EmitFn for a materializing-cached codec, or StreamFn for a streaming codec", c.Name)
	case c.SizeFn == "":
		return fmt.Errorf("codec/missing-size-fn: codec %q: SizeFn must be set (a `func(v T) int` matching EncodeFn's value shape) — or migrate to EmitFn for materializing-cached codecs / StreamFn for streaming codecs", c.Name)
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
