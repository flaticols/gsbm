package gsbmschema

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"
)

// loaderMode is the package-load configuration used by both LoadFromDirs
// and LoadFromPatterns. NeedSyntax + NeedTypes + NeedTypesInfo deliver
// per-package AST (with comment groups via the ParseFile override) and
// type info; NeedDeps + NeedImports recurse the full dependency graph so
// cross-package imports — including third-party module deps — resolve
// the same way `go build` resolves them. NeedModule supplies the module
// info that supersedes the previous walk-up-to-go.mod logic.
const loaderMode = packages.NeedName |
	packages.NeedFiles |
	packages.NeedCompiledGoFiles |
	packages.NeedSyntax |
	packages.NeedTypes |
	packages.NeedTypesInfo |
	packages.NeedDeps |
	packages.NeedImports |
	packages.NeedModule

// LoadFromDirs typechecks the supplied directories as a connected set of
// Go packages and returns a PackageSet. It delegates to
// golang.org/x/tools/go/packages, which resolves transitive imports
// (stdlib, in-module, and third-party module deps) the same way the rest
// of the Go toolchain does. Each input dir must live under a `go.mod`
// ancestor; loose `.go`-file collections without a module are not
// supported, matching `go build`'s requirement.
//
// Imports BETWEEN supplied dirs that share a module are resolved once,
// so the *types.Package referenced via cross-package field traversal is
// the same pointer as the package the loader returns. This pointer
// identity is what lets //gsbm:opaque, //gsbm:reserved, //gsbm:allow-
// breaking, and field-level //gsbm:cycle_break_via_id markers on a
// sibling input package flow through to schema discovery — see
// TestLoadFromDirsCrossPackageMarkers for the regression this preserves.
//
// All input dirs must live under the same go.mod (the loader does a
// single packages.Load call against one module root). Inputs spanning
// multiple modules surface an error; that case is rare in practice and
// would require separate loads anyway.
func LoadFromDirs(dirs []string) (*PackageSet, error) {
	if len(dirs) == 0 {
		return nil, errors.New("no input directories")
	}
	abs := make([]string, 0, len(dirs))
	seen := make(map[string]string, len(dirs))
	var modRoot, modSeenIn string
	for _, d := range dirs {
		info, err := os.Stat(d)
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", d, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%s: not a directory", d)
		}
		a, err := filepath.Abs(d)
		if err != nil {
			return nil, fmt.Errorf("abs %s: %w", d, err)
		}
		a = filepath.Clean(a)
		if dup, ok := seen[a]; ok {
			return nil, fmt.Errorf("duplicate input directory %s (also seen as %s)", d, dup)
		}
		seen[a] = d
		r, err := findModuleRoot(a)
		if err != nil {
			return nil, err
		}
		if modRoot == "" {
			modRoot, modSeenIn = r, d
		} else if modRoot != r {
			return nil, fmt.Errorf("inputs span multiple Go modules: %s (from %s) and %s (from %s)",
				modRoot, modSeenIn, r, d)
		}
		abs = append(abs, a)
	}
	return loadInternal(modRoot, abs)
}

// LoadFromPatterns typechecks the supplied Go-style package patterns
// and returns a PackageSet. Patterns follow the same syntax accepted by
// `go build` / `go list`: relative wildcards (`./...`, `./pkg/...`),
// absolute import paths (`example.com/pkg/foo`), and absolute wildcards
// (`example.com/pkg/...`). Patterns are passed to
// golang.org/x/tools/go/packages, which resolves them against the
// current working directory's module — including transitive
// dependencies, internal packages, and third-party module deps the same
// way `go build` resolves them.
//
// LoadFromPatterns relies on the process's current working directory as
// the module anchor (matching `go build` ergonomics). Callers that need
// a specific module root should chdir before invoking. This is the
// counterpart to LoadFromDirs for the case the migration unblocks: a
// project can run `gsbmschema lint ./...` from its own root and have
// every transitive import resolved without enumerating dirs.
//
// All other guarantees from LoadFromDirs apply: cross-package marker
// flow, generated `_gsbm.go` / `_gsbm_arena.go` files filtered out of
// the discovery view, and consolidated diagnostics for any package-
// level errors.
func LoadFromPatterns(patterns []string) (*PackageSet, error) {
	if len(patterns) == 0 {
		return nil, errors.New("no input patterns")
	}
	if slices.Contains(patterns, "") {
		return nil, errors.New("empty pattern")
	}
	// Empty loadDir lets packages.Load use the process cwd, matching
	// the way `go list` / `go build` resolve relative patterns. The
	// CLI invokes from the user's project root, so cwd is the right
	// module anchor.
	return loadInternal("", patterns)
}

// loadInternal runs packages.Load with loaderMode and converts the
// result into the *PackageSet shape the rest of the schema pipeline
// consumes. It is shared by LoadFromDirs and LoadFromPatterns.
//
// loadDir is the working directory passed to packages.Load — typically
// the go.mod root for the inputs. It anchors module resolution so
// third-party dep paths and intra-module imports both resolve cleanly.
func loadInternal(loadDir string, patterns []string) (*PackageSet, error) {
	cfg := &packages.Config{
		Mode: loaderMode,
		Dir:  loadDir,
		ParseFile: func(fset *token.FileSet, name string, src []byte) (*ast.File, error) {
			return parser.ParseFile(fset, name, src, parser.ParseComments)
		},
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, fmt.Errorf("packages.Load: %w", err)
	}
	if len(pkgs) == 0 {
		return nil, errors.New("no packages matched the given inputs")
	}

	// Surface package-level errors (parse, typecheck, import-resolution)
	// up front. Positions are FileSet-relative file:line:col strings, so
	// CLI reporters can route them to the same diagnostic channel they
	// already use for schema validation issues.
	var loadErrs []string
	for _, pkg := range pkgs {
		for _, e := range pkg.Errors {
			loadErrs = append(loadErrs, e.Error())
		}
	}
	if len(loadErrs) > 0 {
		return nil, fmt.Errorf("loader errors:\n  %s", strings.Join(loadErrs, "\n  "))
	}

	// Defensive dedup on PkgPath: if go/packages somehow returns the
	// same package twice (e.g., a pattern overlap), keep the first
	// occurrence so the dedup-key invariant downstream is preserved.
	seenPath := make(map[string]bool, len(pkgs))
	var fset *token.FileSet
	out := make([]*Package, 0, len(pkgs))
	for _, pkg := range pkgs {
		if seenPath[pkg.PkgPath] {
			continue
		}
		seenPath[pkg.PkgPath] = true
		if fset == nil {
			fset = pkg.Fset
		}
		// Skip generated _gsbm.go / _gsbm_arena.go siblings from the
		// Files slice that schema discovery walks. They carry no
		// //gsbm: markers; the hand-written sources are authoritative.
		// They remain visible to the typechecker (so any methods they
		// declare on user types still resolve) but are invisible to
		// Discover / BuildSchema / classifier.
		files := make([]*ast.File, 0, len(pkg.Syntax))
		for _, f := range pkg.Syntax {
			name := filepath.Base(pkg.Fset.Position(f.Pos()).Filename)
			if strings.HasSuffix(name, "_gsbm.go") || strings.HasSuffix(name, "_gsbm_arena.go") {
				continue
			}
			files = append(files, f)
		}
		out = append(out, &Package{
			Path:  pkg.PkgPath,
			Name:  pkg.Name,
			Files: files,
			Info:  pkg.TypesInfo,
			Pkg:   pkg.Types,
		})
	}
	if fset == nil {
		return nil, errors.New("loader produced no usable package data")
	}
	return &PackageSet{Fset: fset, Packages: out}, nil
}

// findModuleRoot walks up from dir until it finds a directory
// containing a go.mod and returns that directory. Returns an error
// when no ancestor has a go.mod — that case is reported up rather
// than silently falling back to GOPATH-style resolution.
func findModuleRoot(dir string) (string, error) {
	cur := dir
	for {
		fi, err := os.Stat(filepath.Join(cur, "go.mod"))
		if err == nil && !fi.IsDir() {
			return cur, nil
		}
		if err != nil && !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fmt.Errorf("no go.mod found in %s or any parent", dir)
		}
		cur = parent
	}
}
