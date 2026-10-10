package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"
)

// dingTalkChannel implements a DingTalk custom bot.
//
// Platform quirks (driving the implementation trade-offs here):
//   - Single-bot rate limit of 20 messages/min; over-sent messages are silently dropped (HTTP may still be 200),
//     So rate limiting must be done client-side, see DefaultRatePerMin.
//   - Security settings, pick one of three: signature / custom keyword / IP allowlist. Signature is the only one that does not depend on
//
// ... message-content scheme, so only signature is supported (a bare webhook with none of the three enabled is also supported).
//   - Both success and failure return HTTP 200, distinguished by the errcode in the body -- not checking errcode ...
//     ...would record a failed delivery as a success.
type dingTalkChannel struct{}

func (dingTalkChannel) Kind() string { return KindDingTalk }

func (dingTalkChannel) DefaultRatePerMin() int { return 20 }

// DingTalk's Webhook address carries access_token, making it a credential itself, so it is masked as a whole.
func (dingTalkChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// The target is the DingTalk Webhook address itself; changing the address must restate the signature secret for the new address at the same time.
func (dingTalkChannel) DestinationKeys() []string { return []string{"webhook"} }

func (dingTalkChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Missing Webhook URL")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Invalid Webhook URL: %w", err)
	}
	return nil
}

// Send delivers one message. Uses ActionCard (with buttons) when there is a backlink and it is a single item, otherwise markdown.
func (c dingTalkChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	hook := cfgString(cfg, "webhook")
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := dingTalkSignedURL(hook, cfgString(cfg, "secret"), time.Now())
	if err != nil {
		return 0, Permanent(err)
	}

	title := markdownTitle(m)
	// DingTalk markdown body has no explicit byte cap, but a cap guard is still applied to prevent evidence fields from bloating abnormally.
	text, kept := markdownBody(m, 20000)

	var payload any
	if !m.Batch && len(m.Items) == 1 && m.Items[0].DetailURL != "" {
		payload = map[string]any{
			"msgtype": "actionCard",
			"actionCard": map[string]any{
				"title":          title,
				"text":           text,
				"btnOrientation": "0",
				"singleTitle":    "View details",
				"singleURL":      m.Items[0].DetailURL,
			},
		}
	} else {
		payload = map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]any{"title": title, "text": text},
		}
	}

	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	// DingTalk hides business errors inside 200 responses.
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("Failed to parse DingTalk response: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 301000 means signature verification failed, 310000 means keyword mismatch -- both are configuration errors,
		// Retries won't self-heal.
		return 0, Permanent(fmt.Errorf("DingTalk returned error %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}

// dingTalkSignedURL appends the timestamp and sign parameters to the webhook following the official signing rules.
//
// Rule: string-to-sign = timestamp + "\n" + secret, with HMAC-SHA256 **keyed by secret too**,
// The result is base64-encoded then URL-encoded. timestamp is in milliseconds. When secret is empty, return as-is,
// To support bots without signing enabled.
func dingTalkSignedURL(hook, secret string, now time.Time) (string, error) {
	if secret == "" {
		return hook, nil
	}
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	u, err := url.Parse(hook)
	if err != nil {
		// Does not pass through err: the url.Parse error text carries the full address (including access_token).
		return "", fmt.Errorf("Failed to parse Webhook URL: %s", redactRequestTarget(hook))
	}
	q := u.Query()
	q.Set("timestamp", ts)
	q.Set("sign", sign)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// validateHTTPURL checks the address is usable and the protocol supported, and judges literal-IP targets for intranet.
//
// Two subtleties:
//
//  1. **Error messages must be redacted**. What url.Parse itself returns is a *url.Error, whose Error() carries
//
// **complete original address**, while these vendors' addresses embed credentials (DingTalk access_token,
//
//	   WeCom key, Telegram bot token, Feishu hook id). This used to be a bare `return err` here,
//	   Thus the "invalid address format" error carried the credentials out with it, flowing into the test endpoint's 400 response,
//	   ...last_error persisted on every delivery, server-side logs, and the delivery-history API.
//
//	2. **Literal IPs are checked for internal addresses directly**, leaving domain names to the dial stage (blockInternalDial is the final ...)
//	   enforcement point, also covering DNS rebinding). Doing it here surfaces the hint as early as saving the config,
//	   ...rather than waiting for the first delivery to fail.
//
// Restricting protocols is defensive: file:///gopher:// and the like would cause http.Client to produce unexpected ...
// behavior (already blocked by the scheme check, but there is no reason to open up this front).
func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("Address unresolvable (%s)", redactRequestTarget(raw))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("Only http/https supported, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("Missing hostname")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && isBlockedDialIP(ip) && !allowLocalTargets() {
		return fmt.Errorf("Refusing delivery to loopback/link-local address %s (if delivery to a local service is truly needed, set %s=1)", ip, AllowLocalTargetsEnv)
	}
	return nil
}
