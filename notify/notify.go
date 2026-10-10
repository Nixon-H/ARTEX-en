// Package notify implements the IM/email push channel adapter layer for vulnerability discovery.
//
// Layering: this package is a **leaf package**, depending only on the standard library. It knows nothing of databases or server. Channel configs
// Passed as map[string]any (corresponding to the notification_channels.config JSONB column),
// Content to push is passed in as Message. The benefit of splitting it this way: signature computation, UTF-8 truncation, filter matching and the like
// The genuinely error-prone spots can be unit-tested without PostgreSQL; the host only needs to orchestrate on the server side.
//
// Concurrency contract: Channel implementations must be **stateless**. The same Channel instance is shared by multiple channel configurations
// (even multiple bot instances of the same channel) reused concurrently; all credentials are always passed in via the cfg parameter,
// Must not cache things like the webhook URL in the implementation's own fields.
package notify

// Channel type identifier. Its values double as the valid set of notification_channels.kind, enforced by the server side ...
// Allowlist validation (same as findings.status: no DB CHECK, to ease adding channels later).
const (
	KindDingTalk = "dingtalk" // DingTalk custom bot
	KindFeishu   = "feishu"   // Feishu (incl. Lark) custom bot
	KindWeCom    = "wecom"    // WeCom group bot
	KindWebhook  = "webhook"  // Generic Webhook: custom method/headers/JSON template
	KindTelegram = "telegram" // Telegram Bot API
	KindEmail    = "email"    // SMTP email
)

// Event types, corresponding to notification_events.kind.
const (
	EventFindingCreated       = "finding_created"
	EventFindingStatusChanged = "finding_status_changed"
)

// InitKind is the fallback value for an empty kind in config.
const InitKind = KindDingTalk

// severityRank maps vulnerability severity to a comparable ordinal. Unknown severities return 0, so any
// min_severity setting keeps unknown severities out -- when in doubt, do not push, avoiding false-positive floods.
var severityRank = map[string]int{
	"low":      1,
	"medium":   2,
	"high":     3,
	"critical": 4,
}

// SeverityRank returns the ordinal of the severity; unknown severities return 0.
func SeverityRank(severity string) int { return severityRank[severity] }

// SeverityLabel returns the emoji-prefixed Chinese severity name, used for message titles and card colors.
// Unknown severities echo back verbatim, no invention.
func SeverityLabel(severity string) string {
	switch severity {
	case "critical":
		return "🔴 Critical"
	case "high":
		return "🟠 High"
	case "medium":
		return "🟡 Medium"
	case "low":
		return "🔵 Low"
	default:
		return severity
	}
}

// StatusLabel translates a disposition status into Chinese, for status change messages.
func StatusLabel(status string) string {
	switch status {
	case "pending":
		return "Pending"
	case "in_progress":
		return "In progress"
	case "confirmed":
		return "Confirmed"
	case "resolved":
		return "Handled"
	case "fixed":
		return "Fixed"
	case "false_positive":
		return "False positive"
	case "ignored":
		return "Ignored"
	case "duplicate":
		return "Duplicate"
	case "risk_accepted":
		return "Risk accepted"
	default:
		return status
	}
}

// AtLeast checks whether severity reaches the min threshold. An empty min means no threshold -- everything passes.
// Note that an unknown severity has ordinal 0 and will be rejected by any non-empty min (see the severityRank comment).
func AtLeast(severity, min string) bool {
	if min == "" {
		return true
	}
	return SeverityRank(severity) >= SeverityRank(min)
}
