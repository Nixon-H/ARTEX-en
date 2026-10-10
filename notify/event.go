package notify

// Snapshot is the contract of the notification_events.snapshot JSONB column. The writer is the db layer
// Vulnerability persistence transaction; readers are the server-layer delivery engine and filter matching. Defined in this package because it is
// "Notification-domain" payload: db only handles serialization and does not understand field meanings.
//
// Why store vulnerability fields redundantly instead of looking them up at render time: vulnerabilities may later be renamed, re-severitied, or re-stated,
// But the pushed content should reflect the conclusion **at the time of the incident** -- looking it up later would yield the "later changed to low"
// ... dangerously misleading. In addition, fan-out and rendering therefore need not JOIN the three findings/tasks/assets tables.
type Snapshot struct {
	// Event types: finding_created / finding_status_changed
	Kind      string  `json:"kind"`
	FindingID int64   `json:"finding_id"`
	TaskID    int64   `json:"task_id"`
	VulnClass string  `json:"vulnclass"`
	Name      string  `json:"name"`
	Severity  string  `json:"severity"`
	Summary   string  `json:"summary"`
	AssetIDs  []int64 `json:"asset_ids"`
	// Non-empty only when kind=finding_status_changed.
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status,omitempty"`
}

// Item is a pending vulnerability for channels to render.
type Item struct {
	FindingID int64
	Name      string
	VulnClass string
	Severity  string
	Summary   string
	// Assets are the parsed asset display names (e.g. domains/IPs). Filled in by the server layer --
	// This package doesn't touch the database, so names are unavailable.
	Assets []string
	// DetailURL is the vulnerability-detail backlink; empty means public_base_url is unconfigured, omitted at render time.
	DetailURL string
	// For status-change events only; renders as "pending -> fixed" when both fields are non-empty.
	FromStatus string
	ToStatus   string
}

// IsStatusChange reports whether the entry is a status-change event.
func (i Item) IsStatusChange() bool { return i.FromStatus != "" || i.ToStatus != "" }

// Title returns the entry's display title: prefers the human-given name, falls back to the vulnerability type vulnclass,
// When both are empty use a placeholder -- never emit an empty title.
func (i Item) Title() string {
	if i.Name != "" {
		return i.Name
	}
	if i.VulnClass != "" {
		return i.VulnClass
	}
	return "(unnamed vulnerability)"
}

// Message is the complete payload of one channel send.
type Message struct {
	// Length is 1 for single pushes; a whole batch for digest pushes.
	// An empty slice is illegal; callers must guarantee at least one item.
	Items []Item
	// When Batch=true, render as a digest message (different title, with time window and item count).
	Batch bool
	// WindowMinutes is the aggregation window (minutes), used only when Batch=true for the "last N minutes" copy.
	// Deliberately passed in explicitly via config instead of computing time.Since at render time: keeps rendering deterministic for testability.
	WindowMinutes int
	// HomeURL is the platform dashboard address (global public_base_url); empty means no dashboard entry.
	HomeURL string
}
