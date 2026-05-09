package odmschema

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
)

// LoadFromDirs typechecks each supplied directory as an independent Go
// package and returns a PackageSet. It is intentionally minimalist — no
// `go list` invocation, no module graph crawling — because the schema
// input is deliberately a small, hand-curated set of directories (the
// package(s) holding `//odm:root` types and their direct neighbors).
//
// External imports are resolved via the host toolchain's importer
// (importer.Default), which is sufficient for stdlib references. Cross-
// package imports BETWEEN supplied dirs are NOT resolved here: each dir
// is typechecked in isolation, with its own importer.Default(). Two
// supplied dirs that need to reference each other's types must instead
// be loaded by their installed import paths through importer.Default()
// — typically by running odmschema after `go install` or against a
// vendored module — so this loader does not need to model the module
// graph itself. Multi-dir input is still useful for surfacing roots that
// live in independent leaf packages; the schema closure that downstream
// phases walk is computed across the typechecked PackageSet.
func LoadFromDirs(dirs []string) (*PackageSet, error) {
	fset := token.NewFileSet()
	type rawPkg struct {
		dir   string
		name  string
		files []*ast.File
	}
	raws := make([]*rawPkg, 0, len(dirs))
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", d, err)
		}
		var files []*ast.File
		var pkgName string
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(d, e.Name())
			f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if err != nil {
				return nil, fmt.Errorf("parse %s: %w", path, err)
			}
			if pkgName == "" {
				pkgName = f.Name.Name
			} else if pkgName != f.Name.Name {
				return nil, fmt.Errorf("%s: mixed package names in %s (%s vs %s)",
					d, path, pkgName, f.Name.Name)
			}
			files = append(files, f)
		}
		if len(files) == 0 {
			return nil, fmt.Errorf("%s: no Go source files", d)
		}
		raws = append(raws, &rawPkg{
			dir:   d,
			name:  pkgName,
			files: files,
		})
	}

	imp := importer.Default()
	var packages []*Package
	for _, r := range raws {
		conf := &types.Config{Importer: imp}
		info := &types.Info{
			Types:      map[ast.Expr]types.TypeAndValue{},
			Defs:       map[*ast.Ident]types.Object{},
			Uses:       map[*ast.Ident]types.Object{},
			Implicits:  map[ast.Node]types.Object{},
			Selections: map[*ast.SelectorExpr]*types.Selection{},
			Scopes:     map[ast.Node]*types.Scope{},
			Instances:  map[*ast.Ident]types.Instance{},
		}
		pkg, err := conf.Check(r.name, fset, r.files, info)
		if err != nil {
			return nil, fmt.Errorf("typecheck %s: %w", r.dir, err)
		}
		packages = append(packages, &Package{
			Path:  pkg.Path(),
			Name:  pkg.Name(),
			Files: r.files,
			Info:  info,
			Pkg:   pkg,
		})
	}
	return &PackageSet{Fset: fset, Packages: packages}, nil
}
