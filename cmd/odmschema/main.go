// odmschema is the build-time tool for the odm-bin tagged binary
// serializer. It discovers `//odm:root` types in one or more Go
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
//	hash    <dir>...                 — print the schVer for the inputs
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

	"github.com/flaticols/gsbm/tools/odmschema"
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
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `odmschema - schema discovery, validation, and diff classifier for odm-bin.

usage:
  odmschema lint <dir>...
  odmschema snapshot <dir>... -o <out-dir>
  odmschema diff --prev <file> --curr <file>
  odmschema hash <dir>...
`)
}

func cmdLint(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "lint: at least one package directory required")
		return 1
	}
	ps, err := odmschema.LoadFromDirs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lint: %v\n", err)
		return 1
	}
	res := odmschema.Analyze(ps)
	if len(res.Issues) > 0 {
		fmt.Fprint(os.Stderr, odmschema.FormatIssues(res.Issues))
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
	ps, err := odmschema.LoadFromDirs(dirs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "snapshot: %v\n", err)
		return 1
	}
	res := odmschema.Analyze(ps)
	for _, i := range res.Issues {
		fmt.Fprintln(os.Stderr, i.Error())
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "snapshot: %v\n", err)
		return 1
	}
	jsonBytes, err := odmschema.MarshalJSON(res.Schema)
	if err != nil {
		fmt.Fprintf(os.Stderr, "snapshot: marshal json: %v\n", err)
		return 1
	}
	if err := os.WriteFile(filepath.Join(*out, "schema_snapshot.json"), jsonBytes, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "snapshot: write json: %v\n", err)
		return 1
	}
	yamlBytes := odmschema.MarshalYAML(res.Schema)
	if err := os.WriteFile(filepath.Join(*out, "schema.yaml"), yamlBytes, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "snapshot: write yaml: %v\n", err)
		return 1
	}
	fmt.Printf("schVer=%d structs=%d roots=%d\n", res.Schema.SchVer, len(res.Schema.Structs), len(res.Schema.Roots))
	return 0
}

func cmdDiff(args []string) int {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	prevPath := fs.String("prev", "", "previous snapshot json")
	currPath := fs.String("curr", "", "current snapshot json")
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
	report := odmschema.CIDiff(prev, curr)
	fmt.Print(odmschema.FormatDiff(report.Diff))
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
	ps, err := odmschema.LoadFromDirs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hash: %v\n", err)
		return 1
	}
	res := odmschema.Analyze(ps)
	fmt.Println(res.Schema.SchVer)
	return 0
}

func readSnapshot(path string) (*odmschema.Schema, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s odmschema.Schema
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return &s, nil
}
