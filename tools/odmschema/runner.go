package odmschema

import (
	"fmt"
	"strings"
)

// AnalyzeResult bundles everything a CLI front-end (or a precommit hook,
// or a CI step) needs out of one analysis pass.
type AnalyzeResult struct {
	Schema *Schema
	Issues []Issue
}

// Analyze runs the discovery → closure → validation → fingerprint
// pipeline against ps. It returns the result even when there are
// validation issues; the caller decides whether issues are fatal.
func Analyze(ps *PackageSet) *AnalyzeResult {
	roots, dIssues := Discover(ps)
	schema, bIssues := BuildSchema(ps, roots)
	vIssues := Validate(schema, ps)
	schema.SchVer = ComputeSchVer(schema)
	all := append([]Issue{}, dIssues...)
	all = append(all, bIssues...)
	all = append(all, vIssues...)
	return &AnalyzeResult{Schema: schema, Issues: all}
}

// FormatIssues renders issues for human consumption (linter output).
func FormatIssues(issues []Issue) string {
	if len(issues) == 0 {
		return ""
	}
	var b strings.Builder
	for _, i := range issues {
		b.WriteString(i.Error())
		b.WriteByte('\n')
	}
	return b.String()
}

// CIDiffReport is what the CI step emits as its summary. Acknowledged
// breaking changes are listed but do not flip the gate.
type CIDiffReport struct {
	Diff       Diff
	GateBlocks bool
}

// CIDiff classifies prev vs curr and decides whether a CI gate should
// block. A breaking change is admissible only if every "breaking" entry
// in the diff carries an Acknowledged justification (transitively, from
// the //odm:allow-breaking directive on the affected struct).
func CIDiff(prev, curr *Schema) CIDiffReport {
	d := Classify(prev, curr)
	report := CIDiffReport{Diff: d}
	for _, c := range d.Changes {
		if c.Severity == SeverityBreaking && c.Acknowledged == "" {
			report.GateBlocks = true
			break
		}
	}
	return report
}

// FormatDiff renders d as a multi-line summary suitable for a PR
// comment or a CI step's stdout. Breaking entries are listed first.
func FormatDiff(d Diff) string {
	var b strings.Builder
	fmt.Fprintf(&b, "max severity: %s\n", d.MaxSeverity)
	for _, c := range d.Changes {
		ack := ""
		if c.Acknowledged != "" {
			ack = fmt.Sprintf("  [acknowledged: %s]", c.Acknowledged)
		}
		fmt.Fprintf(&b, "  [%s] %s — %s: %s%s\n",
			c.Severity, c.Code, c.Subject, c.Detail, ack)
	}
	return b.String()
}
