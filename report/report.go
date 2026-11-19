// Package report renders a deliverable Markdown report from the exploration
// graph: confirmed findings (with severity/evidence) (docs §15 P5).
package report

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
)

// Input bundles what the report needs.
type Input struct {
	Title       string
	Goal        string
	GeneratedAt time.Time
	AssetCounts map[string]int
	Findings    []*db.Node
	Status      map[int64]string // finding node id -> triage status (optional)
	Details     map[int64]string // finding node id -> reporter write-up in Markdown (optional)
	Excluded    int              // findings left out by triage (false positive / duplicate / ignored)
}

type findingView struct {
	VulnClass string
	Name      string
	Severity  string
	Summary   string
	PoC       string
	Status    string
	Detail    string
}

func parseFinding(n *db.Node) findingView {
	var p struct {
		VulnClass string `json:"vulnclass"`
		Name      string `json:"name"`
		Severity  string `json:"severity"`
		Summary   string `json:"summary"`
		Evidence  struct {
			PoC string `json:"poc"`
		} `json:"evidence"`
	}
	_ = json.Unmarshal(n.Payload, &p)
	return findingView{VulnClass: p.VulnClass, Name: p.Name, Severity: strings.ToLower(p.Severity), Summary: p.Summary, PoC: p.Evidence.PoC}
}

var sevRank = map[string]int{"critical": 0, "high": 1, "medium": 2, "low": 3, "": 4}

var statusLabel = map[string]string{
	db.FindingInProgress:   "In progress",
	db.FindingConfirmed:    "Confirmed by reviewer",
	db.FindingResolved:     "Resolved",
	db.FindingRiskAccepted: "Risk accepted",
}

// Markdown renders the report.
func Markdown(in Input) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Penetration Test Report - %s\n\n", nz(in.Title, "Untitled task"))
	fmt.Fprintf(&b, "- **Task goal**: %s\n", nz(in.Goal, "(not specified)"))
	fmt.Fprintf(&b, "- **Generated at**: %s\n\n", in.GeneratedAt.Format("2006-01-02 15:04:05"))

	fs := make([]findingView, 0, len(in.Findings))
	for _, n := range in.Findings {
		f := parseFinding(n)
		f.Status = in.Status[n.ID]
		f.Detail = in.Details[n.ID]
		fs = append(fs, f)
	}
	sort.SliceStable(fs, func(i, j int) bool { return sevRank[fs[i].Severity] < sevRank[fs[j].Severity] })

	// summary
	fmt.Fprintf(&b, "## Summary\n\n")
	fmt.Fprintf(&b, "- Reported findings: **%d**", len(fs))
	bySev := map[string]int{}
	for _, f := range fs {
		bySev[f.Severity]++
	}
	var parts []string
	for _, s := range []string{"critical", "high", "medium", "low"} {
		if bySev[s] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", s, bySev[s]))
		}
	}
	if len(parts) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(parts, ", "))
	}
	b.WriteString("\n")
	if in.Excluded > 0 {
		fmt.Fprintf(&b, "- Excluded after triage (false positive / duplicate / ignored): %d\n", in.Excluded)
	}
	fmt.Fprintf(&b, "- Assets: ")
	var types []string
	for t := range in.AssetCounts {
		types = append(types, t)
	}
	sort.Strings(types)
	for i, t := range types {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s %d", t, in.AssetCounts[t])
	}
	b.WriteString("\n\n")

	// findings
	fmt.Fprintf(&b, "## Findings\n\n")
	if len(fs) == 0 {
		b.WriteString("_No vulnerabilities confirmed in this run._\n\n")
		return b.String()
	}
	for i, f := range fs {
		fmt.Fprintf(&b, "### %d. [%s] %s\n\n", i+1, strings.ToUpper(nz(f.Severity, "info")), nz(f.Name, nz(f.VulnClass, "Uncategorized")))
		if f.VulnClass != "" {
			fmt.Fprintf(&b, "- **Class**: %s\n", f.VulnClass)
		}
		if lbl := statusLabel[f.Status]; lbl != "" {
			fmt.Fprintf(&b, "- **Status**: %s\n", lbl)
		}
		b.WriteString("\n")
		fmt.Fprintf(&b, "%s\n\n", nz(f.Summary, ""))
		if f.PoC != "" {
			fmt.Fprintf(&b, "**PoC / Evidence:**\n\n```\n%s\n```\n\n", f.PoC)
		}
		if strings.TrimSpace(f.Detail) != "" {
			fmt.Fprintf(&b, "**Details:**\n\n%s\n\n", demoteHeadings(strings.TrimSpace(f.Detail)))
		}
	}
	return b.String()
}

// demoteHeadings pushes Markdown headings in an embedded write-up below the
// report's own "###" finding headings so the document outline stays intact.
func demoteHeadings(md string) string {
	lines := strings.Split(md, "\n")
	inFence := false
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			inFence = !inFence
			continue
		}
		text := strings.TrimLeft(l, "#")
		if inFence || len(text) == len(l) || !strings.HasPrefix(text, " ") {
			continue
		}
		level := len(l) - len(text) + 3
		if level > 6 {
			level = 6
		}
		lines[i] = strings.Repeat("#", level) + text
	}
	return strings.Join(lines, "\n")
}

func nz(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}
