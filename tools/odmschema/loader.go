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
	"sort"
	"strings"
)

// LoadFromDirs typechecks one or more local directories as separate Go
// packages and returns a PackageSet. It is intentionally minimalist —
// no `go list` invocation, no module graph crawling — because the
// schema input is deliberately a small, hand-curated set of directories
// (the package(s) holding `//odm:root` types and their direct neighbors).
//
// External imports (anything outside dirs) are resolved via the host
// toolchain's importer (importer.Default), which is sufficient for
// stdlib references. Cross-package imports between supplied dirs are
// resolved manually by toposorted typechecking.
func LoadFromDirs(dirs []string) (*PackageSet, error) {
	fset := token.NewFileSet()
	type rawPkg struct {
		dir   string
		name  string
		path  string
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
			path:  pkgName, // sufficient identity for this build-time tool
			files: files,
		})
	}

	// Topo-sort the dirs by their declared imports so a package can use
	// types from another package in dirs. This is the simplest possible
	// resolver — for the small input size we expect, the n^2 pass is
	// fine.
	sort.SliceStable(raws, func(i, j int) bool { return raws[i].path < raws[j].path })

	// Build a name → typechecked package map and an importer that
	// returns those before falling back to importer.Default().
	tcPackages := map[string]*types.Package{}
	imp := composedImporter{tcPackages: tcPackages, fallback: importer.Default()}

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
		pkg, err := conf.Check(r.path, fset, r.files, info)
		if err != nil {
			return nil, fmt.Errorf("typecheck %s: %w", r.dir, err)
		}
		tcPackages[r.path] = pkg
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

type composedImporter struct {
	tcPackages map[string]*types.Package
	fallback   types.Importer
}

func (c composedImporter) Import(path string) (*types.Package, error) {
	if p, ok := c.tcPackages[path]; ok {
		return p, nil
	}
	return c.fallback.Import(path)
}
