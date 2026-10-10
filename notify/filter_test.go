package notify

import "testing"

func TestParseFilterMalformedFallsBackToMatchAll(t *testing.T) {
	// Malformed JSON, empty input, wrongly-typed fields -- all must degrade to the zero-value Filter,
	// i.e. "no filtering". This invariant is where "rather over-push than miss a push" lands:
	// Once this becomes an error or a partial parse, a single mistyped character from the user would silently drop all critical notifications.
	cases := []struct {
		name string
		raw  string
	}{
		{"empty input", ""},
		{"Illegal JSON", `{not json`},
		{"Truncated JSON", `{"min_severity":`},
		{"Type mismatch", `{"min_severity": 123, "task_ids": "abc"}`},
		{"Top level is an array", `[1,2,3]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := ParseFilter([]byte(tc.raw))
			if f.MinSeverity != "" || len(f.TaskIDs) != 0 || len(f.AssetIDs) != 0 {
				t.Fatalf("a malformed configuration should degrade to a zero-value Filter, got %+v", f)
			}
			// A zero-value Filter must match any event.
			ev := Snapshot{Kind: EventFindingCreated, Severity: "low", VulnClass: "XSS"}
			if !Match(f, ev) {
				t.Fatal("Zero-value Filter should match all events")
			}
		})
	}
}

func TestMatchSeverityThreshold(t *testing.T) {
	ev := func(sev string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: sev}
	}
	cases := []struct {
		min    string
		sev    string
		expect bool
	}{
		{"", "low", true},
		{"", "critical", true},
		{"high", "critical", true},
		{"high", "high", true},
		{"high", "medium", false},
		{"high", "low", false},
		{"critical", "high", false},
		{"critical", "critical", true},
		// Unknown severities have ordinal 0 and should be blocked by any non-empty threshold (when in doubt, do not push).
		{"low", "", false},
		{"low", "unknown", false},
		{"", "", true},
	}
	for _, tc := range cases {
		got := Match(Filter{MinSeverity: tc.min}, ev(tc.sev))
		if got != tc.expect {
			t.Errorf("min=%q sev=%q: expected %v got %v", tc.min, tc.sev, tc.expect, got)
		}
	}
}

func TestMatchScopeRestrictions(t *testing.T) {
	ev := Snapshot{
		Kind:      EventFindingCreated,
		Severity:  "high",
		TaskID:    7,
		AssetIDs:  []int64{10, 20},
		VulnClass: "SQL injection",
	}
	cases := []struct {
		name   string
		filter Filter
		expect bool
	}{
		{"Empty scope = unlimited", Filter{}, true},
		{"Task matched", Filter{TaskIDs: []int64{7}}, true},
		{"Task missed", Filter{TaskIDs: []int64{8}}, false},
		{"Multi-select tasks include a match", Filter{TaskIDs: []int64{8, 7}}, true},
		{"Assets overlap", Filter{AssetIDs: []int64{20, 99}}, true},
		{"Assets don't overlap", Filter{AssetIDs: []int64{99}}, false},
		{"Task and asset both matched", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{10}}, true},
		{"Task matched but asset missed", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{99}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev); got != tc.expect {
				t.Errorf("Expected %v, got %v", tc.expect, got)
			}
		})
	}
}

func TestMatchVulnClassKeywords(t *testing.T) {
	ev := func(class string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: "high", VulnClass: class}
	}
	cases := []struct {
		name   string
		filter Filter
		class  string
		expect bool
	}{
		{"include empty = accept all", Filter{}, "Any type", true},
		{"include matched", Filter{VulnClassInclude: []string{"SQL"}}, "SQL injection", true},
		{"include missed", Filter{VulnClassInclude: []string{"Command execution"}}, "SQL injection", false},
		{"include: any of several words matching", Filter{VulnClassInclude: []string{"Command execution", "SQL"}}, "SQL injection", true},
		{"Case-insensitive", Filter{VulnClassInclude: []string{"sql"}}, "SQL injection", true},
		{"exclude: a match means exclusion", Filter{VulnClassExclude: []string{"Information disclosure"}}, "Information disclosure", false},
		{"exclude: pass when nothing matches", Filter{VulnClassExclude: []string{"Information disclosure"}}, "SQL injection", true},
		// Exclusion beats inclusion: matching both means exclusion.
		{"Exclusion beats inclusion", Filter{
			VulnClassInclude: []string{"SQL"},
			VulnClassExclude: []string{"Injection"},
		}, "SQL injection", false},
		// All-whitespace keywords should be ignored, otherwise they degenerate into "matching every string containing a space".
		{"Blank keywords are ignored", Filter{VulnClassInclude: []string{"", "  "}}, "SQL injection", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev(tc.class)); got != tc.expect {
				t.Errorf("Expected %v, got %v", tc.expect, got)
			}
		})
	}
}

func TestMatchStatusChangeRequiresOptIn(t *testing.T) {
	ev := Snapshot{Kind: EventFindingStatusChanged, Severity: "critical", FromStatus: "pending", ToStatus: "fixed"}
	// Off by default: what most people mean by "push vulnerabilities" is newly found vulnerabilities, not a status ledger.
	if Match(Filter{MinSeverity: "low"}, ev) {
		t.Fatal("Status-change events should be skipped when not enabled")
	}
	if !Match(Filter{OnStatusChange: true}, ev) {
		t.Fatal("Status-change events should match once on_status_change is enabled")
	}
	// Creation events are unaffected by on_status_change.
	created := Snapshot{Kind: EventFindingCreated, Severity: "critical"}
	if !Match(Filter{MinSeverity: "low"}, created) {
		t.Fatal("creation events should not depend on on_status_change")
	}
}
