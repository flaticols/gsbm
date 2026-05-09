// gsbmschema is the build-time tool for the gsbm tagged binary
// serializer. It discovers `//gsbm:root` types in one or more Go
// packages, computes the transitive struct closure, validates it
// against the append-only schema policy, and emits the schema.yaml /
// schema_snapshot.json artifacts. It also classifies a proposed
// snapshot against the committed snapshot for CI gating.
//
// Subcommands:
//
//	lint    <dir>...                 — run discovery+validation; non-zero on issues
//	snapshot <dir>... -o <out-dir>   — write schema.yaml + schema_snapshot.json
//	diff    --prev <file> --curr <file> — classify two snapshots
//	hash    <dir>...                 — print the schemaHint for the inputs
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
  gsbmschema lint <dir>...
  gsbmschema snapshot <dir>... -o <out-dir>
  gsbmschema diff --prev <file> --curr <file> [--allow-stop-compat-write]
  gsbmschema hash <dir>...
  gsbmschema gen <dir>...
  gsbmschema gen-arena <dir>...
`)
}

func cmdLint(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "lint: at least one package directory required")
		return 1
	}
	ps, err := gsbmschema.LoadFromDirs(args)
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
	dirs := fs.Args()
	if len(dirs) == 0 {
		fmt.Fprintln(os.Stderr, "snapshot: at least one package directory required")
		return 1
	}
	ps, err := gsbmschema.LoadFromDirs(dirs)
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
		fmt.Fprintln(os.Stderr, "hash: at least one package directory required")
		return 1
	}
	ps, err := gsbmschema.LoadFromDirs(args)
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

// cmdGen runs the heap-mode codegen against one or more input dirs and
// writes the emitted *_gsbm.go files next to their handwritten siblings.
// Failures from validation issues block code emission — generated code is
// only as trustworthy as the schema that fed it.
func cmdGen(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "gen: at least one package directory required")
		return 1
	}
	ps, err := gsbmschema.LoadFromDirs(args)
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
		fmt.Fprintln(os.Stderr, "gen-arena: at least one package directory required")
		return 1
	}
	ps, err := gsbmschema.LoadFromDirs(args)
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
