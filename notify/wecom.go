package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// weComMarkdownLimit is the hard upper limit of the WeCom group-bot markdown content (in bytes, not characters).
// This is the tightest limit among all six channels, and the main reason TruncateBytes exists.
const weComMarkdownLimit = 4096

// weComChannel implements a WeCom group bot.
//
// Platform quirks:
//   - Authenticates solely via the key in the URL; signing is not supported -- so the webhook address itself is the entire credential.
//   - markdown content upper limit 4096 **bytes**; overlong messages are rejected whole (not truncated). Chinese text runs 3 bytes/character,
//
// Meaning the body only has room for a little over a thousand characters; the client must truncate.
// - Rate limit of 20 messages/minute, likewise absorbed by client-side rate limiting.
type weComChannel struct{}

func (weComChannel) Kind() string { return KindWeCom }

func (weComChannel) DefaultRatePerMin() int { return 20 }

// WeCom has only one credential for Webhook (the key on the URL), and it does not support signature --
// The whole address is the entire credential; no other field needs masking.
func (weComChannel) SecretKeys() []string { return []string{"webhook"} }

// WeCom has only one Webhook field, which is both destination and credential, so there is no such thing as "leftover credentials after changing the address".
func (weComChannel) DestinationKeys() []string { return []string{"webhook"} }

func (weComChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Missing Webhook URL")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Invalid Webhook URL: %w", err)
	}
	return nil
}

func (c weComChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	// A summary batch can be very long (50 entries × one line each + prefix); 4096 bytes is easily exceeded.
	// Truncate here instead of relying on platform errors: rejection means losing the whole batch, while truncation delivers at least the first several entries.
	content, kept := markdownBody(m, weComMarkdownLimit)
	payload := map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]any{"content": content},
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("Failed to parse WeCom response: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 45009 means API calls exceeded the limit -- the platform's rate-limit window rolls, so retrying after backoff is effective,
		// So it is explicitly classified as retryable. Reaching here means the client's rate_per_min is configured too aggressively,
		// Retries are only a fallback; the real fix is to lower that channel's rate-limit value.
		if res.ErrCode == 45009 {
			return 0, fmt.Errorf("WeCom rate-limited %d: %s", res.ErrCode, res.ErrMsg)
		}
		// 93000 means the webhook key is invalid -- permanent failure, retrying will not heal it.
		return 0, Permanent(fmt.Errorf("WeCom returned error %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}
