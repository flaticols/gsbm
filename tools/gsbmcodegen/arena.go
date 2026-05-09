package odmcodegen

import (
	"bytes"
	"fmt"
	"go/format"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/flaticols/gsbm/tools/odmschema"
)

// GenerateArena emits arena-mode companion files for every //odm:root
// type in the closure. Each emitted file is named <lowercase>_odm_arena.go
// and lives next to the handwritten root declaration. The body contains:
//
//   - Decode<Root>(data []byte, a *odmarena.Arena) (*<Root>, error) — the
//     entry point arena callers use. It allocates the root from the
//     arena's per-T pool, installs the arena as the Reader's Allocator,
//     consumes the 8-byte blob header, and runs the heap-mode
//     UnmarshalODM body unchanged. Allocation routes through the
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
// (Customer, Item, Total, …) are reached by the root's UnmarshalODM,
// which the arena Reader already routes through the arena allocator;
// they need no separate Decode helper.
func GenerateArena(ps *odmschema.PackageSet, schema *odmschema.Schema) ([]GeneratedFile, error) {
	if ps == nil || schema == nil {
		return nil, fmt.Errorf("odmcodegen: nil input")
	}
	allowed := map[string]bool{}
	for _, p := range ps.Packages {
		allowed[p.Path] = true
	}
	// Mirror the generic-origin skip in Generate: heap codegen never emits
	// MarshalODM/UnmarshalODM/Reset on a generic origin type, so emitting an
	// arena Decode/Detach helper that calls those methods would not compile.
	genericRoots := map[string]bool{}
	for _, sd := range schema.Structs {
		if len(sd.Generic) > 0 {
			genericRoots[sd.Type.PkgPath+"."+sd.Type.Name] = true
		}
	}

	var files []GeneratedFile
	for _, ref := range schema.Roots {
		if !allowed[ref.PkgPath] {
			continue
		}
		if genericRoots[ref.PkgPath+"."+ref.Name] {
			fmt.Fprintf(os.Stderr, "odmcodegen arena: warning: skipping generic origin %s.%s — per-instantiation codegen not yet implemented\n", ref.PkgPath, ref.Name)
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
		fname := strings.ToLower(named.Obj().Name()) + "_odm_arena.go"
		out, err := emitArenaFile(pkg, named)
		if err != nil {
			return nil, fmt.Errorf("odmcodegen arena: %s: %w", named.Obj().Name(), err)
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

	var body bytes.Buffer
	fmt.Fprintf(&body, "// Decode%s decodes data into a *%s allocated from a. The returned\n", name, name)
	fmt.Fprintf(&body, "// value, and every string/slice it transitively references, are\n")
	fmt.Fprintf(&body, "// invalidated by a.Release. The wire format is identical to heap mode.\n")
	fmt.Fprintf(&body, "func Decode%s(data []byte, a *odmarena.Arena) (*%s, error) {\n", name, name)
	fmt.Fprintf(&body, "\tv := odmarena.AllocStruct[%s](a)\n", name)
	fmt.Fprintf(&body, "\tv.Reset()\n")
	fmt.Fprintf(&body, "\tr := odm.NewReader(data)\n")
	fmt.Fprintf(&body, "\tr.SetAllocator(a)\n")
	fmt.Fprintf(&body, "\tif _, _, err := r.ReadHeader(); err != nil { return nil, err }\n")
	fmt.Fprintf(&body, "\tif err := v.UnmarshalODM(r); err != nil { return nil, err }\n")
	fmt.Fprintf(&body, "\tif err := r.Err(); err != nil { return nil, err }\n")
	fmt.Fprintf(&body, "\treturn v, nil\n")
	fmt.Fprintf(&body, "}\n\n")

	fmt.Fprintf(&body, "// Decode%sBody is the headerless variant: data is the raw root body\n", name)
	fmt.Fprintf(&body, "// without the 8-byte blob header.\n")
	fmt.Fprintf(&body, "func Decode%sBody(data []byte, a *odmarena.Arena) (*%s, error) {\n", name, name)
	fmt.Fprintf(&body, "\tv := odmarena.AllocStruct[%s](a)\n", name)
	fmt.Fprintf(&body, "\tv.Reset()\n")
	fmt.Fprintf(&body, "\tr := odm.NewReader(data)\n")
	fmt.Fprintf(&body, "\tr.SetAllocator(a)\n")
	fmt.Fprintf(&body, "\tif err := v.UnmarshalODM(r); err != nil { return nil, err }\n")
	fmt.Fprintf(&body, "\tif err := r.Err(); err != nil { return nil, err }\n")
	fmt.Fprintf(&body, "\treturn v, nil\n")
	fmt.Fprintf(&body, "}\n\n")

	fmt.Fprintf(&body, "// Detach%s returns a heap-allocated *%s with no references into any\n", name, name)
	fmt.Fprintf(&body, "// arena. Implemented as a re-encode + heap decode round-trip so the\n")
	fmt.Fprintf(&body, "// heap-mode unmarshal body is the single source of truth.\n")
	fmt.Fprintf(&body, "func Detach%s(o *%s) (*%s, error) {\n", name, name, name)
	fmt.Fprintf(&body, "\tw := odm.NewWriter(nil)\n")
	fmt.Fprintf(&body, "\tif err := o.MarshalODM(w); err != nil { return nil, err }\n")
	fmt.Fprintf(&body, "\tif err := w.Err(); err != nil { return nil, err }\n")
	fmt.Fprintf(&body, "\tdst := &%s{}\n", name)
	fmt.Fprintf(&body, "\tif err := odm.DecodeBodyInto(w.Bytes(), dst); err != nil { return nil, err }\n")
	fmt.Fprintf(&body, "\treturn dst, nil\n")
	fmt.Fprintf(&body, "}\n")

	var out bytes.Buffer
	out.WriteString("// Code generated by odmcodegen (arena mode). DO NOT EDIT.\n\n")
	fmt.Fprintf(&out, "package %s\n\n", pkg.Name())
	out.WriteString("import (\n")
	out.WriteString("\t\"github.com/flaticols/gsbm/storage/odm\"\n")
	out.WriteString("\t\"github.com/flaticols/gsbm/storage/odmarena\"\n")
	out.WriteString(")\n\n")
	out.Write(body.Bytes())

	formatted, err := format.Source(out.Bytes())
	if err != nil {
		return out.Bytes(), fmt.Errorf("format: %w", err)
	}
	return formatted, nil
}
