package odmschema

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
)

// PackageSet is the typechecked input that schema discovery operates on.
// In production this is built from a `go list`-style package graph; in
// tests, ParseSource builds one from in-memory source strings.
type PackageSet struct {
	Fset     *token.FileSet
	Packages []*Package
}

// Package is one Go package in the input set, with the AST file list and
// the type-check info the discovery walker needs.
type Package struct {
	Path  string
	Name  string
	Files []*ast.File
	Info  *types.Info
	Pkg   *types.Package
}

// ParseSource is a hermetic helper used by tests to typecheck a single
// package given as a slice of source files. The package import path is
// "test/<name>" by default; callers wanting a specific path should pass
// it via the optional importPath argument.
//
// It uses the host toolchain's importer for stdlib references, which is
// sufficient for tests that don't reach into third-party modules.
func ParseSource(pkgName string, sources []string, importPath ...string) (*PackageSet, error) {
	fset := token.NewFileSet()
	files := make([]*ast.File, 0, len(sources))
	for i, src := range sources {
		f, err := parser.ParseFile(fset, fmt.Sprintf("%s_%d.go", pkgName, i), src, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parse %s_%d.go: %w", pkgName, i, err)
		}
		files = append(files, f)
	}
	conf := &types.Config{Importer: importer.Default()}
	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Implicits:  map[ast.Node]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
		Scopes:     map[ast.Node]*types.Scope{},
		Instances:  map[*ast.Ident]types.Instance{},
	}
	path := "test/" + pkgName
	if len(importPath) > 0 && importPath[0] != "" {
		path = importPath[0]
	}
	pkg, err := conf.Check(path, fset, files, info)
	if err != nil {
		return nil, fmt.Errorf("typecheck: %w", err)
	}
	return &PackageSet{
		Fset: fset,
		Packages: []*Package{{
			Path:  pkg.Path(),
			Name:  pkg.Name(),
			Files: files,
			Info:  info,
			Pkg:   pkg,
		}},
	}, nil
}

// findStructDoc returns the doc CommentGroup attached to a struct named
// name in any file of the package. Both `type T struct { ... }` and a
// grouped `type ( T struct{ ... } )` form are handled — the doc lives on
// either the GenDecl or the TypeSpec.
func (p *Package) findStructDoc(name string) (*ast.CommentGroup, *ast.StructType, bool) {
	for _, f := range p.Files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.Name.Name != name {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					return nil, nil, false
				}
				doc := ts.Doc
				if doc == nil && len(gd.Specs) == 1 {
					doc = gd.Doc
				}
				return doc, st, true
			}
		}
	}
	return nil, nil, false
}

// findFieldDoc returns the doc CommentGroup attached to a named field in
// a struct's AST node, if present. (Anonymous embedded fields are not
// addressable by name and return false.)
func findFieldDoc(st *ast.StructType, name string) (*ast.CommentGroup, bool) {
	if st == nil {
		return nil, false
	}
	for _, f := range st.Fields.List {
		for _, ident := range f.Names {
			if ident.Name == name {
				return f.Doc, true
			}
		}
	}
	return nil, false
}
