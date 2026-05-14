package gsbmcodegen

import (
	"bytes"
	"fmt"
	"go/format"
	"go/types"
	"path/filepath"
	"sort"
	"strings"

	"go.flaticols.dev/gsbm/tools/gsbmschema"
)

// GenerateArena emits arena-mode companion files for every //gsbm:root
// type in the closure. Each emitted file is named <lowercase>_gsbm_arena.go
// and lives next to the handwritten root declaration. The body contains:
//
//   - Decode<Root>(data []byte, a *gsbmarena.Arena) (*<Root>, error) — the
//     entry point arena callers use. It allocates the root from the
//     arena's per-T pool, installs the arena as the Reader's Allocator,
//     consumes the 12-byte blob header, and runs the heap-mode
//     UnmarshalGSBM body unchanged. Allocation routes through the
//     SlicePoolStore + AcquireString hooks the generated code already
//     calls.
//
//   - Detach<Root>(o *<Root>) (*<Root>, error) — produces a heap-allocated
//     copy compatible with the heap-mode type. Implemented as a
//     re-encode-then-DecodeBodyInto round-trip; this is the
//     "manual generated copy code vs. reusing heap-mode unmarshal path"
//     decision flagged in the plan's Post-Completion notes — we take the
//     unmarshal-path option so there is exactly one decoder body per
//     root, not two.
//
// Generated arena files are produced only for root types. Nested structs
// (Customer, Item, Total, …) are reached by the root's UnmarshalGSBM,
// which the arena Reader already routes through the arena allocator;
// they need no separate Decode helper.
func GenerateArena(ps *gsbmschema.PackageSet, schema *gsbmschema.Schema) ([]GeneratedFile, error) {
	if ps == nil || schema == nil {
		return nil, fmt.Errorf("gsbmcodegen: nil input")
	}
	allowed := map[string]bool{}
	for _, p := range ps.Packages {
		allowed[p.Path] = true
	}
	// Mirror the heap-codegen rejection: heap codegen never emits
	// MarshalGSBM/UnmarshalGSBM/Reset on a non-opaque generic type, so
	// emitting an arena Decode/Detach helper that calls those methods
	// would not compile. Opaque generics are exempt only in the
	// direct-value-field shape (`B Box[int]`); indirect uses funnel
	// through typeExpr which strips type arguments. Generic roots,
	// opaque or not, are also rejected outright — the arena emit body
	// writes `gsbmarena.AllocStruct[Root](a)` which needs a concrete
	// type name and would render as `AllocStruct[Box]` for Box[T].
	rootKey := map[string]bool{}
	for _, ref := range schema.Roots {
		rootKey[ref.PkgPath+"."+ref.Name] = true
	}
	for _, sd := range schema.Structs {
		if len(sd.Generic) > 0 && !sd.Opaque {
			return nil, fmt.Errorf("gsbmcodegen arena: %s.%s: generic types are not supported — mark //gsbm:opaque with handwritten Marshal/Unmarshal/Reset, or replace with a non-generic type", sd.Type.PkgPath, sd.Type.Name)
		}
		if len(sd.Generic) > 0 && sd.Opaque && rootKey[sd.Type.PkgPath+"."+sd.Type.Name] {
			return nil, fmt.Errorf("gsbmcodegen arena: %s.%s: generic root types are not supported even when //gsbm:opaque — arena Decode/Detach helpers need a concrete type name", sd.Type.PkgPath, sd.Type.Name)
		}
	}
	for _, sd := range schema.Structs {
		if sd.Opaque {
			continue
		}
		if !allowed[sd.Type.PkgPath] {
			continue
		}
		named, _ := lookupNamed(ps, sd.Type)
		if named == nil {
			continue
		}
		str, _ := named.Underlying().(*types.Struct)
		if str == nil {
			continue
		}
		if err := rejectIndirectGenerics(sd.Type.Name, str); err != nil {
			return nil, fmt.Errorf("gsbmcodegen arena: %w", err)
		}
	}

	var files []GeneratedFile
	for _, ref := range schema.Roots {
		if !allowed[ref.PkgPath] {
			continue
		}
		named, pkg := lookupNamed(ps, ref)
		if named == nil {
			continue
		}
		if _, ok := named.Underlying().(*types.Struct); !ok {
			continue
		}
		path := ps.Fset.Position(named.Obj().Pos()).Filename
		if path == "" {
			continue
		}
		dir := filepath.Dir(path)
		fname := strings.ToLower(named.Obj().Name()) + "_gsbm_arena.go"
		out, err := emitArenaFile(pkg, named)
		if err != nil {
			return nil, fmt.Errorf("gsbmcodegen arena: %s: %w", named.Obj().Name(), err)
		}
		files = append(files, GeneratedFile{
			Path:     filepath.Join(dir, fname),
			Package:  pkg.Name(),
			TypeName: named.Obj().Name(),
			Contents: out,
		})
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func emitArenaFile(pkg *types.Package, named *types.Named) ([]byte, error) {
	name := named.Obj().Name()

	// Route runtime imports through addImport so a user package that
	// declares a top-level `gsbm` or `gsbmarena` identifier (or otherwise
	// shadows those names) gets disambiguated aliases. Without this, the
	// emitted file references `gsbm.NewReader` / `gsbmarena.Arena` while
	// the same identifiers are already bound in package scope, and Go
	// fails to compile the generated file. Mirrors the heap-mode runtime-
	// alias threading in emitFile.
	e := &emitter{pkg: pkg, imports: map[string]string{}, nameToPath: map[string]string{}}
	gsbmAlias := e.addImport("go.flaticols.dev/gsbm/storage/gsbm", "gsbm")
	arenaAlias := e.addImport("go.flaticols.dev/gsbm/storage/gsbmarena", "gsbmarena")

	var body bytes.Buffer
	fmt.Fprintf(&body, "// Decode%s decodes data into a *%s allocated from a. The returned\n", name, name)
	fmt.Fprintf(&body, "// value and every string/slice acquired via the Reader's allocator\n")
	fmt.Fprintf(&body, "// (Acquire*/SetAllocator path) are invalidated by a.Release: the\n")
	fmt.Fprintf(&body, "// returned *%s itself, plus any string/slice header it contains,\n", name)
	fmt.Fprintf(&body, "// become unsafe to read after Release.\n")
	fmt.Fprintf(&body, "//\n")
	fmt.Fprintf(&body, "// Sub-trees produced by codecs that bypass the allocator have\n")
	fmt.Fprintf(&body, "// heap-backed referents whose own memory outlives Release, but their\n")
	fmt.Fprintf(&body, "// headers still live inside the arena-decoded struct and must be\n")
	fmt.Fprintf(&body, "// copied out before Release to be read afterwards. encoding/json\n")
	fmt.Fprintf(&body, "// output (e.g. via builtins.DecodeJSONBytes) is heap-owned in its\n")
	fmt.Fprintf(&body, "// referents; bytes read via r.ReadBytes alias the source buffer\n")
	fmt.Fprintf(&body, "// passed to NewReader, so their referent's lifetime is bounded by\n")
	fmt.Fprintf(&body, "// the source buffer (which the caller owns), not by the arena. In\n")
	fmt.Fprintf(&body, "// both cases, accessing the field through the arena-decoded value\n")
	fmt.Fprintf(&body, "// after a.Release is use-after-release; detach (copy out) before\n")
	fmt.Fprintf(&body, "// Release to outlive it.\n")
	fmt.Fprintf(&body, "//\n")
	fmt.Fprintf(&body, "// The wire format is identical to heap mode.\n")
	fmt.Fprintf(&body, "func Decode%s(data []byte, a *%s.Arena) (*%s, error) {\n", name, arenaAlias, name)
	fmt.Fprintf(&body, "\tv := %s.AllocStruct[%s](a)\n", arenaAlias, name)
	fmt.Fprintf(&body, "\tv.Reset()\n")
	fmt.Fprintf(&body, "\tr := %s.NewReader(data)\n", gsbmAlias)
	fmt.Fprintf(&body, "\tr.SetAllocator(a)\n")
	fmt.Fprintf(&body, "\tif _, _, _, err := r.ReadHeader(); err != nil { return nil, err }\n")
	fmt.Fprintf(&body, "\tif err := v.UnmarshalGSBM(r); err != nil { return nil, err }\n")
	fmt.Fprintf(&body, "\tif err := r.Err(); err != nil { return nil, err }\n")
	fmt.Fprintf(&body, "\treturn v, nil\n")
	fmt.Fprintf(&body, "}\n\n")

	fmt.Fprintf(&body, "// Decode%sBody is the headerless variant: data is the raw root body\n", name)
	fmt.Fprintf(&body, "// without the 12-byte blob header.\n")
	fmt.Fprintf(&body, "func Decode%sBody(data []byte, a *%s.Arena) (*%s, error) {\n", name, arenaAlias, name)
	fmt.Fprintf(&body, "\tv := %s.AllocStruct[%s](a)\n", arenaAlias, name)
	fmt.Fprintf(&body, "\tv.Reset()\n")
	fmt.Fprintf(&body, "\tr := %s.NewReader(data)\n", gsbmAlias)
	fmt.Fprintf(&body, "\tr.SetAllocator(a)\n")
	fmt.Fprintf(&body, "\tif err := v.UnmarshalGSBM(r); err != nil { return nil, err }\n")
	fmt.Fprintf(&body, "\tif err := r.Err(); err != nil { return nil, err }\n")
	fmt.Fprintf(&body, "\treturn v, nil\n")
	fmt.Fprintf(&body, "}\n\n")

	fmt.Fprintf(&body, "// Detach%s returns a heap-allocated *%s with no references into any\n", name, name)
	fmt.Fprintf(&body, "// arena. Implemented as a re-encode + heap decode round-trip so the\n")
	fmt.Fprintf(&body, "// heap-mode unmarshal body is the single source of truth.\n")
	fmt.Fprintf(&body, "func Detach%s(o *%s) (*%s, error) {\n", name, name, name)
	fmt.Fprintf(&body, "\tw := %s.NewWriter(nil)\n", gsbmAlias)
	fmt.Fprintf(&body, "\tif err := o.MarshalGSBM(w); err != nil { return nil, err }\n")
	fmt.Fprintf(&body, "\tif err := w.Err(); err != nil { return nil, err }\n")
	fmt.Fprintf(&body, "\tdst := &%s{}\n", name)
	fmt.Fprintf(&body, "\tif err := %s.DecodeBodyInto(w.Bytes(), dst); err != nil { return nil, err }\n", gsbmAlias)
	fmt.Fprintf(&body, "\treturn dst, nil\n")
	fmt.Fprintf(&body, "}\n")

	var out bytes.Buffer
	out.WriteString("// Code generated by gsbmcodegen (arena mode). DO NOT EDIT.\n\n")
	fmt.Fprintf(&out, "package %s\n\n", pkg.Name())
	out.WriteString("import (\n")
	paths := make([]string, 0, len(e.imports))
	for p := range e.imports {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		alias := e.imports[p]
		if alias == "" || alias == pathToIdent(lastPathSegment(p)) {
			fmt.Fprintf(&out, "\t%q\n", p)
		} else {
			fmt.Fprintf(&out, "\t%s %q\n", alias, p)
		}
	}
	out.WriteString(")\n\n")
	out.Write(body.Bytes())

	formatted, err := format.Source(out.Bytes())
	if err != nil {
		return out.Bytes(), fmt.Errorf("format: %w", err)
	}
	return formatted, nil
}
