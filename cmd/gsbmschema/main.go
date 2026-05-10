// gsbmschema is the build-time tool for the gsbm tagged binary
// serializer. It discovers `//gsbm:root` types in one or more Go
// packages, computes the transitive struct closure, validates it
// against the append-only schema policy, and emits the schema.yaml /
// schema_snapshot.json artifacts. It also classifies a proposed
// snapshot against the committed snapshot for CI gating.
//
// Subcommands:
//
//	lint    <input>...                — run discovery+validation; non-zero on issues
//	snapshot <input>... -o <out-dir>  — write schema.yaml + schema_snapshot.json
//	diff    --prev <file> --curr <file> — classify two snapshots
//	hash    <input>...                — print the schemaHint for the inputs
//	gen     <input>...                — generate heap-mode codecs
//	gen-arena <input>...              — generate arena-mode codecs
//
// Each <input> is one of:
//
//   - a directory on disk (today's behavior, e.g. `./pkg/model` or an
//     absolute path),
//   - a Go-style package pattern accepted by `go build` / `go list`:
//     `./...`, `./pkg/...`, `example.com/pkg/foo`, `example.com/pkg/...`.
//
// Classification is per-argument: an input that exists as a directory
// on disk is loaded as a directory; otherwise it is treated as a
// package pattern. Mixed lists are accepted and resolved against the
// process's current working directory's module — match `go build`
// ergonomics. When all inputs are directories the loader anchors on
// the directories' shared go.mod root, which lets the tool be invoked
// from outside the target module.
//
// All subcommands use the same Analyze() pipeline; only the post-
// processing differs. Exit codes are stable for hook authors:
//
//	0 — success / no relevant changes
//	1 — internal error
//	2 — validation issues (lint) or breaking changes without override (diff)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"go.flaticols.dev/gsbm/tools/gsbmcodegen"
	"go.flaticols.dev/gsbm/tools/gsbmschema"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "lint":
		os.Exit(cmdLint(os.Args[2:]))
	case "snapshot":
		os.Exit(cmdSnapshot(os.Args[2:]))
	case "diff":
		os.Exit(cmdDiff(os.Args[2:]))
	case "hash":
		os.Exit(cmdHash(os.Args[2:]))
	case "gen":
		os.Exit(cmdGen(os.Args[2:]))
	case "gen-arena":
		os.Exit(cmdGenArena(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `gsbmschema - schema discovery, validation, and diff classifier for gsbm.

usage:
  gsbmschema lint <input>...
  gsbmschema snapshot <input>... -o <out-dir>
  gsbmschema diff --prev <file> --curr <file> [--allow-stop-compat-write]
  gsbmschema hash <input>...
  gsbmschema gen <input>...
  gsbmschema gen-arena <input>...

each <input> is either a directory on disk or a Go package pattern
(./..., ./pkg/..., example.com/pkg/foo, example.com/pkg/...).
patterns resolve against the current working directory's module.
`)
}

// argKind classifies a positional argument as either a filesystem
// directory or a Go package pattern. Classification is purely based on
// what the argument names: a path that exists on disk as a directory is
// dir; everything else is pattern. Patterns are not validated up front
// — go/packages produces a precise diagnostic when a pattern names no
// package.
type argKind int

const (
	argDir argKind = iota
	argPattern
)

// classifyArg decides whether arg names a directory on disk or a Go
// package pattern. Existing on-disk dirs always win — that preserves
// the historical behavior where `gsbmschema lint ./pkg` always meant
// "load the ./pkg directory" even from within a project where ./pkg
// could also be parsed as a relative pattern.
//
// A non-existent argument is treated as a pattern. This is intentional:
// patterns like `example.com/pkg/foo` or `./missing/...` should reach
// go/packages so the tool's diagnostic surface (file:line, "no
// packages found") matches `go build`'s. classifyArg deliberately does
// not reject "looks unparseable" — the loader is the source of truth.
func classifyArg(arg string) argKind {
	if info, err := os.Stat(arg); err == nil && info.IsDir() {
		return argDir
	}
	return argPattern
}

// classifyArgs splits the input set into directory args and pattern
// args. The order within each slice is preserved; cross-list ordering
// is not. Used by every subcommand to pick a loader entry point.
func classifyArgs(args []string) (dirs, patterns []string) {
	for _, a := range args {
		switch classifyArg(a) {
		case argDir:
			dirs = append(dirs, a)
		default:
			patterns = append(patterns, a)
		}
	}
	return dirs, patterns
}

// loadInputs dispatches a positional argument set to the right loader.
//
//   - All-dirs → LoadFromDirs (anchors on the dirs' shared module root,
//     so the tool works from outside the target module).
//   - Otherwise → LoadFromPatterns (anchors on the process cwd; matches
//     `go build` / `go list` ergonomics). Directory args in a mixed
//     list are converted to absolute paths, which packages.Load
//     accepts as patterns provided each dir lives in the cwd module —
//     `go list /abs/path` only works for packages of the module that
//     anchors the load. Mixing a dir from another module surfaces a
//     clean upfront error instead of go/packages's opaque
//     "no Go files" diagnostic.
//
// The same-module precheck is skipped when no single-module anchor
// exists for cwd: workspace mode (a go.work in cwd or any ancestor) and
// "cwd is outside any module" both admit dirs that wouldn't satisfy the
// precheck but that go/packages can resolve. In those cases we delegate
// to packages.Load and surface its diagnostic.
func loadInputs(args []string) (*gsbmschema.PackageSet, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("at least one input required")
	}
	dirs, patterns := classifyArgs(args)
	if len(patterns) == 0 {
		return gsbmschema.LoadFromDirs(dirs)
	}
	absDirs := make([]string, 0, len(dirs))
	for _, d := range dirs {
		abs, err := filepath.Abs(d)
		if err != nil {
			return nil, fmt.Errorf("abs %s: %w", d, err)
		}
		absDirs = append(absDirs, abs)
	}
	if len(absDirs) > 0 {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("getwd: %w", err)
		}
		if cwdMod, ok := moduleAnchor(cwd); ok {
			for i, abs := range absDirs {
				dMod, err := moduleRoot(abs)
				if err != nil {
					return nil, fmt.Errorf("input dir %s: %w", dirs[i], err)
				}
				if dMod != cwdMod {
					return nil, fmt.Errorf(
						"input dir %s is in module rooted at %s, but cwd %s is in module rooted at %s — mixed dir+pattern invocations require every dir to live in the cwd module (run the tool from the target module, or pass dirs only and let LoadFromDirs anchor on their shared go.mod)",
						dirs[i], dMod, cwd, cwdMod)
				}
			}
		}
	}
	out := make([]string, 0, len(args))
	out = append(out, absDirs...)
	out = append(out, patterns...)
	return gsbmschema.LoadFromPatterns(out)
}

// moduleAnchor returns the module root that pins cwd for the mixed-input
// precheck, plus a flag indicating whether the precheck applies. The
// flag is false when cwd is in workspace mode (go.work present in cwd
// or any ancestor, unless GOWORK=off) or when cwd is outside any
// module — both cases admit dirs from peer modules and cannot be
// enforced by a single-module-equality check.
func moduleAnchor(cwd string) (string, bool) {
	if inWorkspace(cwd) {
		return "", false
	}
	root, err := moduleRoot(cwd)
	if err != nil {
		return "", false
	}
	return root, true
}

// inWorkspace reports whether dir is under an active Go workspace.
// Matches `go build`'s GOWORK resolution: "off" disables workspace mode;
// "auto" or unset walks up from dir looking for a go.work file; any
// other value is an explicit workspace-file path. For the explicit
// case we report true unconditionally — whether the named file
// exists, is absolute, or has the right extension is Go's contract to
// enforce, and packages.Load surfaces the authoritative diagnostic.
// Returning false on a malformed explicit path would route the
// mixed-input invocation into the single-module precheck, which would
// emit a misleading module-mismatch error instead of the real GOWORK
// problem.
func inWorkspace(dir string) bool {
	switch os.Getenv("GOWORK") {
	case "off":
		return false
	case "", "auto":
		// fall through to ancestor walk
	default:
		return true
	}
	cur := dir
	for {
		fi, err := os.Stat(filepath.Join(cur, "go.work"))
		if err == nil && !fi.IsDir() {
			return true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return false
		}
		cur = parent
	}
}

// moduleRoot walks up from dir until it finds a directory containing a
// go.mod and returns that directory. Returns an error when no ancestor
// has a go.mod. Duplicates the loader-package helper so the CLI can
// validate mixed-input preconditions without taking a dep on an
// internal symbol.
func moduleRoot(dir string) (string, error) {
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

func cmdLint(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "lint: at least one package input required")
		return 1
	}
	ps, err := loadInputs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lint: %v\n", err)
		return 1
	}
	res := gsbmschema.Analyze(ps)
	if len(res.Issues) > 0 {
		fmt.Fprint(os.Stderr, gsbmschema.FormatIssues(res.Issues))
		return 2
	}
	return 0
}

func cmdSnapshot(args []string) int {
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	out := fs.String("o", "", "output directory for schema.yaml + schema_snapshot.json")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *out == "" {
		fmt.Fprintln(os.Stderr, "snapshot: -o is required")
		return 1
	}
	inputs := fs.Args()
	if len(inputs) == 0 {
		fmt.Fprintln(os.Stderr, "snapshot: at least one package input required")
		return 1
	}
	ps, err := loadInputs(inputs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "snapshot: %v\n", err)
		return 1
	}
	res := gsbmschema.Analyze(ps)
	for _, i := range res.Issues {
		fmt.Fprintln(os.Stderr, i.Error())
	}
	// Snapshot is still written even when the schema has issues — the
	// artifact is useful for triage. Exit code 2 signals to CI that the
	// gate did not pass, matching the lint subcommand's behavior.
	hadIssues := len(res.Issues) > 0
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "snapshot: %v\n", err)
		return 1
	}
	jsonBytes, err := gsbmschema.MarshalJSON(res.Schema)
	if err != nil {
		fmt.Fprintf(os.Stderr, "snapshot: marshal json: %v\n", err)
		return 1
	}
	if err := os.WriteFile(filepath.Join(*out, "schema_snapshot.json"), jsonBytes, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "snapshot: write json: %v\n", err)
		return 1
	}
	yamlBytes := gsbmschema.MarshalYAML(res.Schema)
	if err := os.WriteFile(filepath.Join(*out, "schema.yaml"), yamlBytes, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "snapshot: write yaml: %v\n", err)
		return 1
	}
	fmt.Printf("schemaHint=%d structs=%d roots=%d\n", res.Schema.SchemaHint, len(res.Schema.Structs), len(res.Schema.Roots))
	if hadIssues {
		return 2
	}
	return 0
}

func cmdDiff(args []string) int {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	prevPath := fs.String("prev", "", "previous snapshot json")
	currPath := fs.String("curr", "", "current snapshot json")
	allowStopCompatWrite := fs.Bool("allow-stop-compat-write", false,
		"acknowledge that the rollback bake window has elapsed; admits compat_write → deprecated transitions that would otherwise gate the CI check")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *prevPath == "" || *currPath == "" {
		fmt.Fprintln(os.Stderr, "diff: --prev and --curr are required")
		return 1
	}
	prev, err := readSnapshot(*prevPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "diff: prev: %v\n", err)
		return 1
	}
	curr, err := readSnapshot(*currPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "diff: curr: %v\n", err)
		return 1
	}
	report := gsbmschema.CIDiffWithOptions(prev, curr, gsbmschema.DiffOptions{
		AllowStopCompatWrite: *allowStopCompatWrite,
	})
	fmt.Print(gsbmschema.FormatDiff(report.Diff))
	if report.GateBlocks {
		return 2
	}
	return 0
}

func cmdHash(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "hash: at least one package input required")
		return 1
	}
	ps, err := loadInputs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hash: %v\n", err)
		return 1
	}
	res := gsbmschema.Analyze(ps)
	// The schemaHint is well-defined for the parsed schema even when
	// validation flags issues, so emit it on stdout for tooling that wants
	// the value. But the exit code must still signal failure, matching
	// lint/gen — otherwise a CI check that only watches the exit code would
	// treat a schema with `field/custom-not-supported` (or any other rule
	// violation) as green.
	fmt.Println(res.Schema.SchemaHint)
	if len(res.Issues) > 0 {
		fmt.Fprint(os.Stderr, gsbmschema.FormatIssues(res.Issues))
		return 2
	}
	return 0
}

// cmdGen runs the heap-mode codegen against one or more inputs and
// writes the emitted *_gsbm.go files next to their handwritten siblings.
// Failures from validation issues block code emission — generated code is
// only as trustworthy as the schema that fed it.
func cmdGen(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "gen: at least one package input required")
		return 1
	}
	ps, err := loadInputs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gen: %v\n", err)
		return 1
	}
	res := gsbmschema.Analyze(ps)
	if len(res.Issues) > 0 {
		fmt.Fprint(os.Stderr, gsbmschema.FormatIssues(res.Issues))
		return 2
	}
	files, err := gsbmcodegen.Generate(ps, res.Schema)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gen: %v\n", err)
		return 1
	}
	for _, gf := range files {
		if err := os.WriteFile(gf.Path, gf.Contents, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "gen: write %s: %v\n", gf.Path, err)
			return 1
		}
		fmt.Println("wrote", gf.Path)
	}
	return 0
}

// cmdGenArena runs the arena-mode codegen and writes <root>_gsbm_arena.go
// next to the handwritten root files. Heap-mode generation must already
// be in place — the arena helpers reference the heap-mode UnmarshalGSBM
// method on the same type.
func cmdGenArena(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "gen-arena: at least one package input required")
		return 1
	}
	ps, err := loadInputs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gen-arena: %v\n", err)
		return 1
	}
	res := gsbmschema.Analyze(ps)
	if len(res.Issues) > 0 {
		fmt.Fprint(os.Stderr, gsbmschema.FormatIssues(res.Issues))
		return 2
	}
	files, err := gsbmcodegen.GenerateArena(ps, res.Schema)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gen-arena: %v\n", err)
		return 1
	}
	for _, gf := range files {
		if err := os.WriteFile(gf.Path, gf.Contents, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "gen-arena: write %s: %v\n", gf.Path, err)
			return 1
		}
		fmt.Println("wrote", gf.Path)
	}
	return 0
}

func readSnapshot(path string) (*gsbmschema.Schema, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s gsbmschema.Schema
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return &s, nil
}
