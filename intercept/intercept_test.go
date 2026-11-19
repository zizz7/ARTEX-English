package intercept

import "testing"

func TestToolEnabledInMCPWildcards(t *testing.T) {
	cases := []struct {
		set  map[string]bool
		name string
		want bool
	}{
		{map[string]bool{"Bash": true}, "Bash", true},
		{map[string]bool{"Bash": true}, "mcp__hexstrike__nmap_scan", false},
		{map[string]bool{MCPWildcard: true}, "mcp__hexstrike__nmap_scan", true},
		{map[string]bool{MCPWildcard: true}, "Bash", false},
		{map[string]bool{"mcp__hexstrike__*": true}, "mcp__hexstrike__execute_command", true},
		{map[string]bool{"mcp__hexstrike__*": true}, "mcp__hexbrain__find_cves", false},
		{map[string]bool{"mcp__hexstrike__nmap_scan": true}, "mcp__hexstrike__nmap_scan", true},
		{map[string]bool{"mcp__hexstrike__*": true}, "mcp__", false},
	}
	for _, c := range cases {
		if got := toolEnabledIn(c.set, c.name); got != c.want {
			t.Errorf("toolEnabledIn(%v, %q) = %v, want %v", c.set, c.name, got, c.want)
		}
	}
}
