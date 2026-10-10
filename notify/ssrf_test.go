package notify

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// This file covers two related hardenings:
//   ① Delivery addresses must not use the server as a springboard into the intranet / cloud metadata (SSRF)
// ② The error message from address validation must not carry credentials contained in the address
//
// About the test environment: many test cases in this package use fake httptest receivers on 127.0.0.1, which the guard blocks by default
// ... them. So TestMain uniformly turns on AllowLocalTargetsEnv, while each SSRF case below ...
// Explicitly clear it to assert the **deny-by-default** behavior.

func TestMain(m *testing.M) {
	// Lets regular cases reach the local fake receiver; SSRF cases clear it temporarily on their own.
	_ = os.Setenv(AllowLocalTargetsEnv, "1")
	os.Exit(m.Run())
}

// TestDialGuardRejectsLoopbackByDefault is the core assertion of SSRF protection:
// Under the default configuration, delivery to a loopback address must be rejected at the **connection layer**.
func TestDialGuardRejectsLoopbackByDefault(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "") // Closing the escape hatch = default behavior
	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("Delivery to loopback addresses should be denied by default")
	}
	if hit {
		t.Fatal("Request already hit the local service -- the guard did not take effect")
	}
	// The error message must guide the user on how to allow it (a local SMTP relay is a legitimate setup).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("Rejection message should explain how to explicitly allow it: %v", err)
	}
}

// TestDialGuardAllowsLoopbackWhenOptedIn is the reverse case: after explicitly opting in it must work,
// Otherwise legitimate deployments like a local postfix / intranet relay would be broken by a blanket block.
func TestDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("Should be deliverable after explicit allowlisting: %v", err)
	}
}

func TestIsBlockedDialIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "::1",
		"169.254.169.254", // Cloud metadata endpoints -- the main reason this function exists
		"169.254.1.1", "fe80::1",
		"0.0.0.0", "::",
		"224.0.0.1", "ff02::1",
		"::ffff:127.0.0.1", // IPv4-mapped forms must be restored before checking; otherwise they are a bypass.
		"",
	}
	for _, s := range blocked {
		if !isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s should be rejected", s)
		}
	}
	// RFC1918 private networks are **deliberately allowed**: self-hosted Mattermost / SMTP relays on the intranet are common legitimate uses.
	// This assertion pins down this trade-off -- if someone casually adds private-network checks in the future, this will fail,
	// So as to force one deliberate decision (instead of silently scrapping a batch of deployments).
	allowed := []string{"10.0.0.5", "172.16.3.4", "192.168.1.10", "8.8.8.8", "2606:4700::1111"}
	for _, s := range allowed {
		if isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s should be allowed (private networks are common legitimate delivery targets)", s)
		}
	}
}

// TestValidateHTTPURLRejectsLiteralPrivateTargets covers the up-front hint at the configuration stage:
// Literal IPs should be rejected at save time instead of waiting for the first delivery to fail.
func TestValidateHTTPURLRejectsLiteralPrivateTargets(t *testing.T) {
	t.Setenv(AllowLocalTargetsEnv, "")
	for _, raw := range []string{
		"http://127.0.0.1:8080/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:8080/hook",
	} {
		if err := validateHTTPURL(raw); err == nil {
			t.Errorf("%s should be rejected at config time", raw)
		}
	}
	// Public and private addresses pass through as usual (private networks are left to the dial phase, which does not block them).
	for _, raw := range []string{"https://oapi.dingtalk.com/robot/send", "http://10.0.0.9/hook"} {
		if err := validateHTTPURL(raw); err != nil {
			t.Errorf("%s should pass validation: %v", raw, err)
		}
	}
}

// TestValidateHTTPURLErrorNeverLeaksCredentials is the branch the audit flagged as missed in the previous round.
//
// When url.Parse **fails**, it returns a *url.Error whose Error() contains the complete original address. In the previous round I only ...
// Redacted the return error of http.Client.Do but missed this spot; and the "permanent-failure path" case added back then
// (file://, gopher://, ftp://) can actually all be parsed successfully by url.Parse, taking the scheme branch,
// So all-green still cannot prove this path is safe -- it is a false assurance.
func TestValidateHTTPURLErrorNeverLeaksCredentials(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/%zz?access_token=" + leakProbeToken,         // illegal percent-encoding
		"https://a.example.com:port/x?access_token=" + leakProbeToken, // non-numeric port
		"http://[::1?access_token=" + leakProbeToken,                  // unmatched parenthesis
	}
	for _, raw := range cases {
		// First confirm that this input **really** makes url.Parse fail. Without this step, the test case might
		// Silently ending up in a different branch (that is how the previous round's false assurance came about).
		if _, err := url.Parse(raw); err == nil {
			t.Errorf("%q should have failed to parse, otherwise this case does not cover the target branch", raw)
			continue
		}
		err := validateHTTPURL(raw)
		if err == nil {
			t.Errorf("%q should fail validation", raw)
			continue
		}
		assertNoSecret(t, err.Error(), leakProbeToken)
	}
	// Confirm the channel-layer wrapper doesn't leak the address either.
	t.Setenv(AllowLocalTargetsEnv, "")
	err := (dingTalkChannel{}).Validate(map[string]any{"webhook": cases[0]})
	if err == nil {
		t.Fatal("Illegal addresses should fail validation")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestEmailDialGuardRejectsLoopbackByDefault covers the dial guard of the SMTP channel.
//
// The email channel once used a bare net.Dialer -- the only gap in the whole SSRF defense: filling host with
// 169.254.169.254 or 127.0.0.1 can be connected to directly, while a failed smtp.NewClient handshake would
// That line returned by the peer is wrapped into the error and echoed via last_error by the delivery-history API -- exactly what other channels
// ...an already-disabled semi-blind read primitive; the timing difference between "connection refused vs. timeout" can still be used to probe ports.
//
// This package's TestMain globally turns on AllowLocalTargetsEnv (many test cases use 127.0.0.1-based
// fake receiver), so this test case must clear it on its own -- otherwise it passes whether the guard is present or not,
// Which is exactly why no test ever caught the gap back then.
func TestEmailDialGuardRejectsLoopbackByDefault(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "") // Closing the escape hatch = default behavior
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Fatal("should not allow delivering mail to loopback addresses by default")
	}
	// The connection must never be established: the guard intercepts it in the Control hook, so EHLO is never sent.
	if f.sawCommand("EHLO") || f.sawCommand("HELO") {
		t.Fatal("SMTP session already established -- the guard did not take effect")
	}
	// The error message must guide users on how to allow it (a local postfix relay is a legitimate configuration).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("Rejection message should explain how to explicitly allow it: %v", err)
	}
}

// TestEmailDialGuardAllowsLoopbackWhenOptedIn is the paired reverse case: after explicitly opting in
// must still deliver normally. Self-hosted intranet SMTP / local relay is a very common deployment; the guard must not block everything indiscriminately.
func TestEmailDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("Local SMTP should be deliverable after explicit allowlisting: %v", err)
	}
	if !f.sawCommand("EHLO") {
		t.Fatal("No EHLO seen -- session was never really established")
	}
}
