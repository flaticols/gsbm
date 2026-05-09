package gsbmschema

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// rawPkg is a parsed-but-not-yet-typechecked input package. The `imports`
// list is the set of import paths referenced by the package's source so
// the loader can topologically order typechecking against the input set.
type rawPkg struct {
	dir     string
	path    string
	name    string
	files   []*ast.File
	imports []string
}

// LoadFromDirs typechecks the supplied directories as a connected set of
// Go packages and returns a PackageSet. It is intentionally minimalist —
// no `go list` invocation, no module graph crawling — because the schema
// input is deliberately a small, hand-curated set of directories (the
// package(s) holding `//gsbm:root` types and their direct neighbors).
//
// Imports BETWEEN supplied dirs are resolved against this loader's own
// parsed sources, not against installed export data: the dirs are
// topologically sorted by import edges and typechecked in dependency
// order through a setImporter that returns our parsed *types.Package
// before falling back to importer.Default() for stdlib / third-party
// references. This is what makes marker-driven semantics (`//gsbm:opaque`,
// `//gsbm:reserved`, `//gsbm:allow-breaking`, field `//gsbm:cycle_break_via_id`)
// on a sibling input package observable from a root that imports it: the
// pointer identity returned through cross-package field traversal matches
// the *types.Package whose AST we hold, so findStructDoc / findFieldDoc
// resolve. Without this, importer.Default() would hand back an export-data
// view of the sibling, the AST would never attach, and markers / source
// edits would silently drop — passing validation because the sibling's
// PkgPath is in the allowed input set.
func LoadFromDirs(dirs []string) (*PackageSet, error) {
	fset := token.NewFileSet()
	raws := make([]*rawPkg, 0, len(dirs))
	seenPath := make(map[string]string, len(dirs))
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", d, err)
		}
		var files []*ast.File
		var pkgName string
		importSet := map[string]struct{}{}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			// Skip generated companion files. They import storage/gsbm via
			// the module path which importer.Default cannot resolve, and
			// they carry no schema information — handwritten files are the
			// authoritative source for `//gsbm:root` and `bin:` tags.
			if strings.HasSuffix(e.Name(), "_gsbm.go") || strings.HasSuffix(e.Name(), "_gsbm_arena.go") {
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
			for _, imp := range f.Imports {
				p, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					continue
				}
				importSet[p] = struct{}{}
			}
			files = append(files, f)
		}
		if len(files) == 0 {
			return nil, fmt.Errorf("%s: no Go source files", d)
		}
		// Derive a stable import path for the package. Using the raw
		// directory path (relative or absolute) makes schemaHint depend on how
		// the caller invoked the tool: `./pkg/foo` vs `/abs/checkout/pkg/foo`
		// would produce different hashes, snapshot files, and review output
		// for the same Go package. derivePkgPath walks up to find go.mod and
		// joins the module path with the relative directory so the result is
		// the canonical Go import path. Forward slashes are forced so Linux
		// CI and macOS dev produce identical hashes. Outside any module (test
		// temp dirs) we fall back to filepath.Clean — that's stable within a
		// single test run, which is all the dedup invariant requires.
		path, err := derivePkgPath(d)
		if err != nil {
			return nil, fmt.Errorf("derive import path for %s: %w", d, err)
		}
		if dup, ok := seenPath[path]; ok {
			return nil, fmt.Errorf("duplicate input directory %s (also seen as %s)", d, dup)
		}
		seenPath[path] = d
		imports := make([]string, 0, len(importSet))
		for p := range importSet {
			imports = append(imports, p)
		}
		raws = append(raws, &rawPkg{
			dir:     d,
			path:    path,
			name:    pkgName,
			files:   files,
			imports: imports,
		})
	}

	rawByPath := make(map[string]*rawPkg, len(raws))
	for _, r := range raws {
		rawByPath[r.path] = r
	}
	order, err := topoSortRaws(raws, rawByPath)
	if err != nil {
		return nil, err
	}

	fallback := importer.Default()
	checked := make(map[string]*types.Package, len(raws))
	imp := &setImporter{packages: checked, fallback: fallback}

	pkgByPath := make(map[string]*Package, len(raws))
	for _, r := range order {
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
		checked[pkg.Path()] = pkg
		p := &Package{
			Path:  pkg.Path(),
			Name:  pkg.Name(),
			Files: r.files,
			Info:  info,
			Pkg:   pkg,
		}
		pkgByPath[p.Path] = p
	}
	// Restore caller's input order for deterministic downstream behavior.
	out := make([]*Package, 0, len(raws))
	for _, r := range raws {
		out = append(out, pkgByPath[r.path])
	}
	return &PackageSet{Fset: fset, Packages: out}, nil
}

// setImporter resolves imports against a map of already-typechecked
// packages first, falling back to a host importer for everything else.
// The map is mutated by LoadFromDirs as input packages are typechecked
// in topological order.
type setImporter struct {
	packages map[string]*types.Package
	fallback types.Importer
}

func (s *setImporter) Import(path string) (*types.Package, error) {
	if p, ok := s.packages[path]; ok {
		return p, nil
	}
	return s.fallback.Import(path)
}

// topoSortRaws orders input packages so each package is typechecked
// after every other input it imports. Edges that point outside the input
// set are ignored — they're resolved by the fallback importer. Cycles
// among inputs are reported as an error (Go forbids them anyway, but a
// clear message beats a deep typecheck failure).
func topoSortRaws(raws []*rawPkg, byPath map[string]*rawPkg) ([]*rawPkg, error) {
	const (
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(raws))
	order := make([]*rawPkg, 0, len(raws))
	var visit func(r *rawPkg, stack []string) error
	visit = func(r *rawPkg, stack []string) error {
		switch color[r.path] {
		case gray:
			cycle := append([]string(nil), stack...)
			cycle = append(cycle, r.path)
			return fmt.Errorf("import cycle among input packages: %s", strings.Join(cycle, " -> "))
		case black:
			return nil
		}
		color[r.path] = gray
		stack = append(stack, r.path)
		for _, imp := range r.imports {
			dep, ok := byPath[imp]
			if !ok {
				continue
			}
			if err := visit(dep, stack); err != nil {
				return err
			}
		}
		color[r.path] = black
		order = append(order, r)
		return nil
	}
	for _, r := range raws {
		if err := visit(r, nil); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// derivePkgPath returns a stable Go import path for dir. It walks up
// from dir looking for a go.mod, parses the module path, and joins it
// with the relative directory under the module root. The result is
// independent of whether the caller passed a relative or absolute path
// and uses forward slashes so the value is portable across platforms.
//
// When no go.mod is found (test temp dirs, scratch directories) the
// cleaned absolute path is returned. That value is stable within a
// single process run, which is enough for the loader's dedup check; the
// canonical-path stability requirement only applies to packages that
// live inside a module.
func derivePkgPath(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	cur := abs
	for {
		modPath, ok, err := readModulePath(filepath.Join(cur, "go.mod"))
		if err != nil {
			return "", err
		}
		if ok {
			rel, err := filepath.Rel(cur, abs)
			if err != nil {
				return "", err
			}
			rel = filepath.ToSlash(rel)
			if rel == "." {
				return modPath, nil
			}
			return modPath + "/" + rel, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return filepath.ToSlash(abs), nil
		}
		cur = parent
	}
}

// readModulePath reads `module <path>` from a go.mod file. Returns
// (path, true, nil) on success, ("", false, nil) when the file does not
// exist, and an error for any other failure (unreadable, malformed).
func readModulePath(path string) (string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "module") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "module"))
		if rest == "" {
			continue
		}
		if i := strings.IndexAny(rest, " \t"); i >= 0 {
			rest = strings.TrimSpace(rest[:i])
		}
		rest = strings.Trim(rest, "\"`")
		if rest != "" {
			return rest, true, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", false, err
	}
	return "", false, fmt.Errorf("%s: no module directive", path)
}
