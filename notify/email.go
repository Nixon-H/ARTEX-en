package notify

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// emailDialTimeout / emailSessionTimeout constrain connection setup and the whole SMTP session respectively.
// net/smtp itself has no timeout mechanism; without these two guards, a stuck peer will
// The delivery goroutine would hang there forever -- while the dispatcher processes serially on a single goroutine, ...
// Equivalent to a full notification-system outage.
const (
	emailDialTimeout    = 10 * time.Second
	emailSessionTimeout = 45 * time.Second
)

// emailChannel implements SMTP mail delivery.
type emailChannel struct{}

func (emailChannel) Kind() string { return KindEmail }

// Email has no platform rate limit, but should not be used for spamming; give it a lenient default.
func (emailChannel) DefaultRatePerMin() int { return 60 }

// Only the password is redacted. SMTP host, account, and recipients are not secrets; redacting them only makes editing painful.
func (emailChannel) SecretKeys() []string { return []string{"password"} }

// host/port decides which server gets the password; tls decides whether transport is encrypted. A change to any of the three
// Require restating the password -- this incidentally forces the "turn off TLS" step to explicitly carry credentials instead of being changed casually.
func (emailChannel) DestinationKeys() []string { return []string{"host", "port", "tls"} }

func (emailChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "host") == "" {
		return errors.New("Missing SMTP server address")
	}
	port := cfgInt(cfg, "port")
	if port <= 0 || port > 65535 {
		return errors.New("Invalid SMTP port (must be 1-65535)")
	}
	if cfgString(cfg, "from") == "" {
		return errors.New("Missing sender address")
	}
	if len(cfgStrings(cfg, "to")) == 0 {
		return errors.New("At least one recipient address is required")
	}
	return nil
}

func (c emailChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	host := cfgString(cfg, "host")
	port := cfgInt(cfg, "port")
	from := cfgString(cfg, "from")
	to := cfgStrings(cfg, "to")
	username := cfgString(cfg, "username")
	password := cfgString(cfg, "password")
	implicitTLS := cfgBool(cfg, "tls")

	msg, err := buildEmailMessage(from, to, m)
	if err != nil {
		return 0, Permanent(err)
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	client, err := emailDial(ctx, addr, host, implicitTLS)
	if err != nil {
		return 0, err
	}
	defer client.Close()

	// STARTTLS: upgrade when the peer supports it. No credentials on plaintext sessions (see the auth note below).
	if !implicitTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
				return 0, fmt.Errorf("STARTTLS failed: %w", err)
			}
		}
	}
	if username != "" {
		if err := client.Auth(smtp.PlainAuth("", username, password, host)); err != nil {
			// smtp.PlainAuth refuses to send credentials over unencrypted connections (unless the target is localhost).
			// This is the **correct** security behavior and cannot be bypassed, but the reason must be spelled out clearly --
			// Otherwise users only see "unencrypted connection" without knowing what to do.
			if strings.Contains(err.Error(), "unencrypted connection") {
				return 0, Permanent(fmt.Errorf("Refusing to send credentials: connection is unencrypted. Please enable TLS, or switch to port 465 (implicit TLS), or check \"Enable TLS\" (%w)", err))
			}
			return 0, Permanent(fmt.Errorf("SMTP authentication failed: %w", err))
		}
	}
	if err := client.Mail(from); err != nil {
		return 0, smtpStageError(fmt.Sprintf("Sender %s rejected", from), err)
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return 0, smtpStageError(fmt.Sprintf("Recipient %s rejected", rcpt), err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return 0, fmt.Errorf("SMTP DATA failed: %w", err)
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return 0, fmt.Errorf("Failed to write email body: %w", err)
	}
	if err := w.Close(); err != nil {
		return 0, fmt.Errorf("Failed to submit email: %w", err)
	}
	// A Quit failure does not change the fact that "the mail has been received by the server", so its error is ignored.
	_ = client.Quit()
	// Email has no length truncation (the full HTML body is sent); the whole batch counts as delivered.
	return len(m.Items), nil
}

// emailDial establishes the SMTP connection.
//
// implicitTLS=true uses the "TLS-on-connect" mode like port 465; false establishes a plaintext connection on 25/587 first and then
// STARTTLS. The two must not be mixed: sending a plaintext greeting to port 465 gets disconnected immediately.
//
// The session deadline is set **at connection setup** (not retrofitted later), because net/smtp's Client takes the underlying
// The connection hides in an unexported field, unreachable from outside; once handed out it can only rely on the preset deadline
// Fallback. This also covers blocking during the handshake phase.
// Control hooks blockInternalDial, sharing the same guard with the HTTP-family channels. Without it, SMTP ...
// ... is precisely the gap in the whole SSRF protection: filling host with 169.254.169.254 or 127.0.0.1 connects directly,
// while smtp.NewClient wraps the peer-returned line into the error on handshake failure, via last_error
// ...echoed back via the delivery-history API, forming a semi-blind read primitive; the timing difference between "connection refused vs. timeout" can still
// Used to probe ports. The dial phase is the final enforcement point, and it also covers DNS rebinding.
func emailDial(ctx context.Context, addr, host string, implicitTLS bool) (*smtp.Client, error) {
	d := &net.Dialer{Timeout: emailDialTimeout, Control: blockInternalDial}
	var conn net.Conn
	var err error
	if implicitTLS {
		conn, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("Failed to connect to SMTP server: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(emailSessionTimeout))
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SMTP handshake failed: %w", err)
	}
	return client, nil
}

// smtpStageError splits a stage's failure by SMTP reply code into "retryable" vs "permanent failure".
//
// Why the distinction is required: SMTP 4xx and 5xx have completely different semantics --
//   - 4xx (450 greylisting, 451 local error, 452 insufficient storage) is a **temporary** rejection,
//     The proper practice is to retry later; greylisting in particular is met on almost every first delivery attempt.
//   - 5xx (550 user unknown, 553 address illegal) is a permanent rejection; retrying is pointless.
//
// If everything were judged a permanent failure, a greylisting-enabled mail server would make **every** delivery fail on its first
// ...falls into failed after retrying -- yet such failures are exactly where automatic retry should matter most.
// Take the reply code as the first three digits of the error text; when no code can be taken, treat as retryable (better one extra attempt,
// Nor should an unparseable response condemn a possibly transient failure as permanent.)
func smtpStageError(what string, err error) error {
	code := smtpReplyCode(err.Error())
	if code >= 500 && code < 600 {
		return Permanent(fmt.Errorf("%s: %w", what, err))
	}
	return fmt.Errorf("%s: %w", what, err)
}

// smtpReplyCode takes the leading three-digit reply code from SMTP error text, returning 0 when unavailable.
// net/smtp doesn't export an error-code field, so it can only be taken from the text; format is "450 4.7.1 ...".
func smtpReplyCode(text string) int {
	if len(text) < 3 {
		return 0
	}
	n, err := strconv.Atoi(text[:3])
	if err != nil {
		return 0
	}
	return n
}

// buildEmailMessage assembles the complete RFC 5322 email.
//
// There are two reasons the body is base64-encoded: first, SMTP mandates lines no longer than 1000 bytes, while HTML
// The body (especially digest emails) easily grows overlong lines; second, base64 never produces lines starting with "."
// ...lines, sparing us SMTP dot-escaping hassles.
func buildEmailMessage(from string, to []string, m Message) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	// Chinese subjects must use RFC 2047 encoding, otherwise clients will render them as mojibake.
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", htmlTitle(m)))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	// Email has no hard length cap, so the body is never truncated.
	b.WriteString("\r\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(htmlBody(m, 0)))
	// base64 wraps every 76 characters, per RFC 2045.
	for len(encoded) > 76 {
		b.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded + "\r\n")
	return b.String(), nil
}
