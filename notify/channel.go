package notify

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// Channel is an adapter for a notification channel. Implementations must be **stateless**: the same instance is shared by multiple channels
// Configs are shared concurrently; credentials always come from the cfg parameter.
type Channel interface {
	// Kind returns the channel type identifier, which must match the registry key.
	Kind() string
	// Validate is called when saving config, verifying required fields and formats. Returned errors are shown directly to
	// the configurator, so the copy must say "which field is missing" instead of a generic "invalid configuration".
	Validate(cfg map[string]any) error
	// Send delivers one message, returning the **number of entries actually delivered** and an error.
	//
	// Why return the count: every platform has a message-length limit; a summary message that cannot fit the whole batch gets truncated.
	// If the caller unconditionally marks the whole batch as delivered, the truncated entries vanish -- invisible in the message,
	// ...the delivery history also shows success, with nowhere to discover the vulnerability was never sent. After returning kept,
	// The caller only marks the first kept items, leaving the rest for the next batch.
	//
	// A returned error means delivery failed, where *PermanentError means it should not be retried.
	// kept is meaningless on failure; callers should ignore it.
	Send(ctx context.Context, cfg map[string]any, m Message) (int, error)
	// DefaultRatePerMin returns the channel's officially recommended per-minute delivery cap, used as the new-channel instance
	// Default rate-limit value for this case. Returning 0 means no known limit.
	DefaultRatePerMin() int
	// SecretKeys returns the credential key names in this channel's config. Their values are masked when the API echoes them back,
	// On update, a received masked value keeps the stored original. Only the implementation itself knows which fields count as credentials
	// (WeCom's entire Webhook address is the credential, while DingTalk's is only the secret within),
	// So this knowledge must come from the channel itself, not be guessed by upper layers.
	SecretKeys() []string
	// DestinationKeys returns the key names in this channel's config that determine "where the message is sent".
	//
	// Like SecretKeys, a security-related matter: destination addresses and credentials are two independent sets of fields,
	// If "change only the address, keep the credentials as-is" were allowed, anyone able to modify the channel config could get at the real credentials stored in the store
	// When sending to a server under one's own control, the mask in the channel configuration becomes completely meaningless.
	// See PrepareConfigUpdate for details.
	DestinationKeys() []string
}

// registry is the channel registry. Deliberately uses explicit literals instead of init() self-registration: so "which channels exist"
// ...visible in one place, and adding a new channel exposes omissions at compile time rather than via runtime side effects.
var registry = map[string]Channel{
	KindDingTalk: dingTalkChannel{},
	KindFeishu:   feishuChannel{},
	KindWeCom:    weComChannel{},
	KindWebhook:  webhookChannel{},
	KindTelegram: telegramChannel{},
	KindEmail:    emailChannel{},
}

// Get fetches the channel implementation by type.
func Get(kind string) (Channel, bool) {
	c, ok := registry[kind]
	return c, ok
}

// ValidKind reports whether kind is a supported channel type.
func ValidKind(kind string) bool {
	_, ok := registry[kind]
	return ok
}

// Kinds returns all supported channel types, sorted lexicographically (for stable UI dropdown display).
func Kinds() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PermanentError marks a delivery failure that must not be retried: credential errors, destination refusal, illegal request body, etc.
// Retries only make sense for transient failures (network jitter, rate limiting, peer 5xx); repeatedly backing off and retrying permanent failures
// It will never succeed, and will wash the real error away in the retry logs.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent marks err as a permanent failure. Returns nil when err is nil,
// Convenient for writing `return Permanent(someCheck())`.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent reports whether the err chain carries the permanent-failure marker.
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

// ---- config-reading helpers ----
//
// Channel configuration comes from a JSONB column in the database; after encoding/json deserialization it is a map[string]any,
// Numbers are uniformly float64, arrays []any. The helpers below unify this conversion layer and tolerate user
// Type skew from leaving fields blank in the UI (e.g. filling a port as a string).

// cfgString reads a string config item, trimming all surrounding whitespace -- copy-paste from web forms easily picks some up.
func cfgString(cfg map[string]any, key string) string {
	v, ok := cfg[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// cfgInt reads an integer config item, accepting both float64 (JSON default) and string sources.
func cfgInt(cfg map[string]any, key string) int {
	switch v := cfg[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// cfgBool reads a boolean config entry, compatible with the strings "true"/"1".
func cfgBool(cfg map[string]any, key string) bool {
	switch v := cfg[key].(type) {
	case bool:
		return v
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		return s == "true" || s == "1" || s == "yes"
	default:
		return false
	}
}

// cfgStrings reads a string-slice config entry, auto-trimming whitespace and dropping empty strings.
func cfgStrings(cfg map[string]any, key string) []string {
	raw, ok := cfg[key].([]any)
	if !ok {
		// A single string is also accepted, to simplify form submission when there is only one value.
		if s := cfgString(cfg, key); s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// cfgMap reads a string-map config item (e.g. custom HTTP headers), trimming whitespace on keys and values and dropping empty keys.
func cfgMap(cfg map[string]any, key string) map[string]string {
	raw, ok := cfg[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		out[k] = s
	}
	return out
}
