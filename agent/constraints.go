package agent

import (
	"strings"

	"github.com/Autumn-27/artex/db"
)

// constraintBlock renders this task's operation constraints (task_constraints) as a
// high-priority block appended to the planner/worker system prompt. allow/deny are
// grouped; empty string when there are no constraints (or ts is nil). The framing
// deliberately puts these ABOVE the exploration/expansion heuristics so a declared
// boundary wins the tug-of-war against "chase another entry surface".
func constraintBlock(ts *db.ExplorationStore) string {
	if ts == nil {
		return ""
	}
	rows, err := ts.ListConstraints()
	if err != nil || len(rows) == 0 {
		return ""
	}
	var allow, deny []string
	for _, c := range rows {
		text := strings.TrimSpace(c.Text)
		if text == "" {
			continue
		}
		if c.Kind == "allow" {
			allow = append(allow, "- "+text)
		} else {
			deny = append(deny, "- "+text)
		}
	}
	if len(allow) == 0 && len(deny) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n[OPERATION CONSTRAINTS (highest priority — overrides ALL exploration/expansion heuristics below; before generating any intent or executing any action, you MUST first self-check for violations; if violated, the action MUST NOT proceed)]:")
	if len(allow) > 0 {
		b.WriteString("\nAllowed operations:\n")
		b.WriteString(strings.Join(allow, "\n"))
	}
	if len(deny) > 0 {
		b.WriteString("\nProhibited operations:\n")
		b.WriteString(strings.Join(deny, "\n"))
	}
	b.WriteString("\n(Discovering new targets/ports/hosts outside these constraints does NOT grant authorization: unless they fall within the allowed scope above, record them as out-of-scope facts and skip them — do not derive intents or execute actions for them.)")
	return b.String()
}
