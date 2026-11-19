package report

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func node(id int64, sev, name string) *db.Node {
	p, _ := json.Marshal(map[string]any{
		"vulnclass": "SQL Injection", "name": name, "severity": sev, "summary": "summary " + name,
		"evidence": map[string]any{"poc": "curl ... -> error"},
	})
	return &db.Node{ID: id, Kind: db.KindFinding, Payload: p}
}

func TestMarkdownOrdersBySeverityAndIncludesDetails(t *testing.T) {
	md := Markdown(Input{
		Title: "t", GeneratedAt: time.Unix(0, 0),
		Findings: []*db.Node{node(1, "low", "minor"), node(2, "critical", "major")},
		Status:   map[int64]string{2: db.FindingConfirmed},
		Details:  map[int64]string{2: "## Remediation\nUse parameterized queries.\n```\n# not a heading\n```"},
		Excluded: 3,
	})
	if strings.Index(md, "major") > strings.Index(md, "minor") {
		t.Fatal("critical finding should come before low")
	}
	for _, want := range []string{"(critical 1, low 1)", "Excluded after triage", ": 3", "Confirmed by reviewer", "##### Remediation", "# not a heading"} {
		if !strings.Contains(md, want) {
			t.Errorf("report missing %q:\n%s", want, md)
		}
	}
}

func TestDemoteHeadingsCapsAtSix(t *testing.T) {
	if got := demoteHeadings("#### deep"); got != "###### deep" {
		t.Fatalf("got %q", got)
	}
	if got := demoteHeadings("#hashtag"); got != "#hashtag" {
		t.Fatalf("non-heading changed: %q", got)
	}
}
