package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// allowLocalTargets decides whether delivering messages to loopback / link-local addresses is allowed.
//
// Denied by default. These address ranges are not places where IM bots or public mail servers would appear, yet they can
// and what they can reach is sensitive: another service's admin port on the same machine, plus cloud metadata endpoints
// (169.254.169.254, which can disclose instance credentials). The delivery address is configured by the admin, but one compromised by XSS/CSRF
// A borrowed admin session, or a second person sharing the same JWT, could read back the response content by modifying the config
// -- doJSON writes the first 200 bytes of 4xx/5xx response bodies into last_error, while the delivery-history API ...
// It would echo it back -- a semi-blind read primitive.
//
// But "local SMTP relay" (postfix on 127.0.0.1:25) is a common self-hosted mail configuration,
// A blanket ban would block legitimate users. So leave an explicit escape hatch instead of hardcoding an allowlist:
// Setting ARTEX_NOTIFY_ALLOW_LOCAL=1 allows it.
//
// It is exported as AllowLocalTargetsEnv so tests can turn it on explicitly -- this package's and the server package's ...
// Test cases heavily use fake httptest receivers on 127.0.0.1; without enabling, all are blocked by the guard.
const AllowLocalTargetsEnv = "ARTEX_NOTIFY_ALLOW_LOCAL"

func allowLocalTargets() bool {
	v := strings.TrimSpace(os.Getenv(AllowLocalTargetsEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

// isBlockedDialIP reports whether the target IP falls in the "delivery disallowed by default" ranges.
//
// Only loopback, link-local (including cloud metadata 169.254.169.254), unspecified, and multicast are rejected.
// RFC1918 private networks are **not rejected**: self-hosted Mattermost / SMTP relays on the intranet are a very common legitimate use,
// Blocking those as well would make the feature unusable outright in real environments. This trade-off is deliberate --
// The guard must block truly sensitive targets without breaking normal deployments along the way.
func isBlockedDialIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// IPv4-mapped IPv6 (::ffff:127.0.0.1) must be restored to IPv4 before checking, otherwise the check is bypassed.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// blockInternalDial is the Control hook of the http.Transport dialer, invoked **when the connection is established**
// Check the destination address.
//
// Why hook the dial phase instead of only validating when the config is saved: this is where it actually takes effect.
// It also covers both ways to bypass config validation -- DNS rebinding (resolving to a public IP at validation time,
// ...resolves to the intranet at actual connection time) and redirection (although we already reject cross-host jumps, same-host jumps
// ...could still point the path elsewhere.)
func blockInternalDial(_, address string, _ syscall.RawConn) error {
	if allowLocalTargets() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("Unable to resolve target address %q", host)
	}
	if isBlockedDialIP(ip) {
		return fmt.Errorf("Refusing delivery to loopback/link-local address %s (if delivery to a local service is truly needed, set %s=1)", ip, AllowLocalTargetsEnv)
	}
	return nil
}

// notifyTransport adds only a dial guard on top of the default Transport.
// Use Clone to keep all default tuning (connection pool, HTTP/2, timeouts, proxy, etc.),
// Avoid changing other behavior just to add one check.
var notifyTransport = func() *http.Transport {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{}
	}
	clone := t.Clone()
	clone.DialContext = (&net.Dialer{Timeout: 10 * time.Second, Control: blockInternalDial}).DialContext
	return clone
}()

// httpClient is the client shared by delivery across all channels.
//
// Deliberately does **not** reuse the project's global egress proxy (GlobalProxy on the server side): that proxy serves penetration
// ...used for target traffic, often an unstable tunnel, while notification availability should not be held hostage by jitter in the target network.
// Direct connections suffice for IM push. Timeout is set to 15 seconds -- a peer slower than that is effectively already in a failed state.
//
// Cross-host redirects are rejected: this feature's delivery addresses are all of the "one fixed endpoint" form, which normally never
// ... redirects to a different host; while the credentials of these providers (DingTalk's access_token, WeCom's key, Telegram's ...
// bot token) lives **in the URL**; following a cross-host jump hands the credential to the redirect target. Same-host
// ...redirects (e.g. trailing-slash completion) are still allowed.
var httpClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: notifyTransport,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("Too many redirects")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("Deny cross-host redirect (%s -> %s)", via[0].URL.Host, req.URL.Host)
		}
		return nil
	},
}

// respBodyLimit limits how much of the response body is read. A faulty peer may spit back huge content, while we only need
// Error code plus a short error description for display in the delivery history.
const respBodyLimit = 8 << 10

// doJSON sends one request and returns the response body (already length-limited).
//
// When payload is nil, an empty body is sent (for GET or scenarios where the platform requires no body).
// Key-values in headers are attached verbatim, for custom headers of generic webhooks.
//
// Error classification is this function's core duty: network-layer failures and 5xx/408/429 go to "retryable",
// Other 4xx go to "permanent failure" -- retrying a 403 just logs the same error 3 times.
func doJSON(ctx context.Context, method, url string, headers map[string]string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			// A serialization failure is a local bug (wrong config field types); retrying will not make it better.
			return nil, Permanent(fmt.Errorf("Failed to build request body: %w", err))
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		// Illegal URL -- usually the user mistyped the address; counts as a permanent failure.
		// err must not be passed through here either: the error text from url.Parse contains the full address.
		return nil, Permanent(fmt.Errorf("Illegal request URL: %s", redactRequestTarget(url)))
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// Connection refused, DNS failure, timeout -- mostly transient failures; hand them to backoff retries.
		//
		// Error text must be redacted before being passed outward. Reason: http.Client.Do returns *url.Error,
		// Its Error() is `Op "complete URL": underlying error`, and the credentials of these providers in this feature are **right in the URL** ...
		// (DingTalk access_token, WeCom key, Feishu hook id, Telegram /bot<token>/).
		// Without redaction, credentials flow along this error string to four places: notification_deliveries
		// 's last_error (stored in plaintext), the delivery-history API response (**bypassing channel-config masking**),
		// Server-side logs, and the 502 text the test-send API returns to the frontend.
		return nil, fmt.Errorf("request failed: %s", redactTransportError(err))
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, respBodyLimit))
	if readErr != nil {
		return nil, fmt.Errorf("Failed to read response: %w", readErr)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}
	// 429 (rate limiting) and 408 (timeout) are worth retrying; other 4xx are config or permission issues where retrying is pointless.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
		return nil, fmt.Errorf("Remote rate-limited or timed out (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("Remote service error (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	return nil, Permanent(fmt.Errorf("Remote rejected the request (HTTP %d): %s", resp.StatusCode, snippet(raw)))
}

// snippet compresses the response body into a short single-line text for error messages. Responses may contain newlines and lots of whitespace,
// Stuffing it directly into last_error would break the layout of the delivery-history page.
func snippet(raw []byte) string {
	return OneLine(string(raw), 200)
}

// redactRequestTarget compresses the delivery address into "scheme://host/..." for use in error messages.
//
// This is this package's only address-redaction rule, deliberately made **crude enough**: besides scheme and host,
// Everything else is discarded. The reason: there is no "generic and safe" way to tell which segment of a URL is a credential:
//
// DingTalk credentials in query      /robot/send?access_token=xxx
// WeCom credentials in query      /cgi-bin/webhook/send?key=xxx
// Feishu credentials in the **trailing path segment** /open-apis/bot/v2/hook/<hook_id>
//
//	Telegram credentials sit **mid-path** /bot<token>/sendMessage
//
// Trying to "keep only the useful part" would require per-channel patches, and missing any single one means a credential leak.
// Keeping the host is enough for troubleshooting (DNS unresolvable, unreachable, certificate mismatch can all be located),
// Which bot it is exactly is identified via the masked-tail-number hint in the channel configuration.
//
// On parse failure return a fixed placeholder -- never echo the original string back out.
func redactRequestTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(address unparseable)"
	}
	return u.Scheme + "://" + u.Host + "/..."
}

// redactTransportError strips the address out of transport-layer errors, keeping only the underlying cause.
//
// The structure of *url.Error is {Op, URL, Err}; Error() prints the URL along with it.
// Here the Err field is read explicitly, bypassing its Error() -- more reliable than doing string replacement afterwards,
// Because replacement must correctly handle all URL-encoded/escaped variants, which are easy to miss.
func redactTransportError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		host := ""
		if u, parseErr := url.Parse(uerr.URL); parseErr == nil {
			host = u.Host
		}
		if uerr.Err != nil {
			return fmt.Sprintf("%s %s: %s", uerr.Op, host, uerr.Err)
		}
		return fmt.Sprintf("%s %s: unknown error", uerr.Op, host)
	}
	// Non-*url.Error errors (e.g. errors returned by the redirect policy) may also carry addresses; route them all through redaction.
	return redactURLsInText(err.Error())
}

// redactURLsInText replaces http(s) addresses appearing in a piece of text with their redacted form.
//
// Fallback for errors with no structured fields available (redirect-policy errors, custom errors from third-party libraries).
// Only http/https prefixes are recognized; splitting is done on whitespace and quotes -- addresses never contain either character class.
func redactURLsInText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		rest := s[i:]
		if strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
			end := len(rest)
			if j := strings.IndexAny(rest, " \t\n\"'"); j >= 0 {
				end = j
			}
			b.WriteString(redactRequestTarget(rest[:end]))
			i += end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
