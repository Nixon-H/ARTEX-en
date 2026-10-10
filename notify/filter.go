package notify

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Filter is the contract of the notification_channels.filter JSONB column: the filter conditions of a channel instance.
// All fields are optional; absent means "no filtering" -- exactly the fallback semantics for malformed configs, see ParseFilter.
type Filter struct {
	// MinSeverity is the minimum severity threshold (low/medium/high/critical); empty = no threshold.
	MinSeverity string `json:"min_severity"`
	// Empty TaskIDs / AssetIDs arrays mean unlimited; non-empty requires the event to intersect with them.
	TaskIDs  []int64 `json:"task_ids"`
	AssetIDs []int64 `json:"asset_ids"`
	// An empty VulnClassInclude means accept all; non-empty requires vulnclass to hit any of the keywords.
	// VulnClassExclude excludes on any keyword hit (exclusion takes precedence over inclusion).
	// Matching is case-insensitive substring -- safer than regex: a user-misconfigured regex will not silently disable the channel.
	VulnClassInclude []string `json:"vulnclass_include"`
	VulnClassExclude []string `json:"vulnclass_exclude"`
	// OnStatusChange decides whether this channel receives vulnerability status-change events (only meaningful in realtime mode).
	OnStatusChange bool `json:"on_status_change"`
}

// ParseFilter parses channel filter configs.
//
// **Never returns error.** This is a deliberate design choice: when filter criteria are malformed, always degrade to the zero value
// Filter (= no filtering = everything hits), because for a vulnerability notification system, **one extra push is far better than
// Silently missing one critical **. Turning parse failure into "don't push" gives the user a seemingly-configured,
// A channel that actually pushes nothing -- the worst failure mode.
func ParseFilter(raw []byte) Filter {
	var f Filter
	if len(raw) == 0 {
		return f
	}
	// On parse failure f stays zero-valued, i.e. no filtering.
	_ = json.Unmarshal(raw, &f)
	return f
}

// ValidMinSeverity reports whether s is a valid severity threshold (empty string means no threshold).
func ValidMinSeverity(s string) bool {
	if s == "" {
		return true
	}
	_, ok := severityRank[s]
	return ok
}

// Validate checks the **value-restricted** fields in the filter config, called when saving a channel.
//
// Why it must be blocked at write time: Match judges unknown thresholds with `rank >= 0`, always true --
// That is, a single typo in min_severity ("hgih") makes the filter **silently fail** and become
// "push-all". This matches this package's "rather over-push than miss" trade-off direction (nothing missed),
// But the consequence is that users think they are doing tiered pushes while actually flooding the group with every vulnerability,
// and with no sign whatsoever telling him he misconfigured it. This kind of "silent downgrade" is exactly what should be blocked at the entry point.
//
// Note Validate is only for the **write** path. The read path still uses ParseFilter's lenient semantics,
// This way bad values already present in historical data do not make the whole channel unreadable.
func (f Filter) Validate() error {
	if !ValidMinSeverity(f.MinSeverity) {
		return fmt.Errorf("Minimum severity %q invalid, options: low / medium / high / critical, or leave empty for no limit", f.MinSeverity)
	}
	return nil
}

// Match decides whether an event should be delivered to a channel bearing this filter condition.
//
// **Never returns error**, same rationale as ParseFilter: any internal exception is treated as a "hit".
// Evaluation order: event type -> severity threshold -> task/asset scope -> vulnerability-type keywords.
func Match(f Filter, s Snapshot) bool {
	// Status-change events are received only by explicitly opted-in channels. Off by default, because most users
	// expecting "push" to mean "a new vulnerability was found", not a running-ledger follow-up of every status transition.
	if s.Kind == EventFindingStatusChanged && !f.OnStatusChange {
		return false
	}
	if !AtLeast(s.Severity, f.MinSeverity) {
		return false
	}
	if len(f.TaskIDs) > 0 && !slices.Contains(f.TaskIDs, s.TaskID) {
		return false
	}
	if len(f.AssetIDs) > 0 && !intersectsInt(f.AssetIDs, s.AssetIDs) {
		return false
	}
	// Exclusion wins: hitting any exclusion keyword disqualifies the event, even if it also hits the inclusion list.
	if len(f.VulnClassExclude) > 0 && containsAnyFold(s.VulnClass, f.VulnClassExclude) {
		return false
	}
	if len(f.VulnClassInclude) > 0 && !containsAnyFold(s.VulnClass, f.VulnClassInclude) {
		return false
	}
	return true
}

func intersectsInt(a, b []int64) bool {
	// A linear scan over the small set suffices; both sides are on the order of "a few dozen hand-picked items",
	// Building a map costs more than it gains.
	for _, v := range b {
		if slices.Contains(a, v) {
			return true
		}
	}
	return false
}

// containsAnyFold reports whether s contains any keyword in keywords (case-insensitive).
func containsAnyFold(s string, keywords []string) bool {
	lower := strings.ToLower(s)
	for _, kw := range keywords {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if kw != "" && strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}
