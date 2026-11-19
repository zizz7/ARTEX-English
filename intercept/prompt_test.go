package intercept

import "testing"

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		wantAction string
		wantReason string
	}{
		{"bare allow", "ALLOW", "allow", ""},
		{"bare deny", "DENY:deleted production files (D4)", "deny", "deleted production files (D4)"},
		{"bare ask", "ASK:ownership unclear", "ask", "ownership unclear"},
		{"lowercase", "deny: rm system files", "deny", "rm system files"},
		{"fullwidth colon", "DENY：hits D4", "deny", "hits D4"},
		// The model echoing the prompt's label must NOT fall through to
		// unparseable (which would fail-open on a real DENY).
		{"echoed label deny", "Block:DENY:hits D5", "deny", "hits D5"},
		{"echoed label allow", "Pass:ALLOW", "allow", ""},
		{"echoed label ask", "Escalate：ASK:cannot determine blast radius", "ask", "cannot determine blast radius"},
		{"leading whitespace/blank line", "\n  ALLOW\n", "allow", ""},
		{"extra trailing text on later line", "DENY:wipe database (D4)\nother explanation", "deny", "wipe database (D4)"},
		{"unparseable", "I think this command is fine", "", ""},
		{"empty", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := ParseVerdict(c.in)
			if v.Action != c.wantAction {
				t.Errorf("action = %q, want %q (in=%q)", v.Action, c.wantAction, c.in)
			}
			if v.Reason != c.wantReason {
				t.Errorf("reason = %q, want %q (in=%q)", v.Reason, c.wantReason, c.in)
			}
		})
	}
}

func TestParseVerdictReasonCapped(t *testing.T) {
	long := "DENY:" + string(make([]byte, 500))
	if got := ParseVerdict(long); len(got.Reason) > 200 {
		t.Errorf("reason length = %d, want <= 200", len(got.Reason))
	}
}
