package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// This file covers the end-to-end behavior of the push feature: vulnerability persisted to DB -> event -> dispatch -> real HTTP send.
//
// One security note: these test cases **do not call the global Notifier.step()**, only for channels they created themselves
// channels call stepRealtime/stepDigest. Because step() iterates over every enabled channel in the DB ----
// on a dev DB already configured with real DingTalk/WeCom bots, a global step would push vulnerabilities generated during the test period
// actually to those groups. Calling per-channel strictly limits the blast radius to the fake receivers fabricated by the test itself.
//
// Cleanup: when a test case ends, delete the events it produced (cascading delete of deliveries) and the channels, leaving no backlog on real channels.
//
// Assertion convention: stepRealtime/stepDigest return no value and log internally, so what is asserted here is
// **observable external behavior** (what the fake receiver got, what state the delivery rows landed in), not the function's
// return value ---- this is closer to the real call path than stubbing the return value.

// notifyFixture is the shared fixture for the test cases in this file.
type notifyFixture struct {
	s       *Server
	pg      *db.DB
	request func(method, path, body string) *httptest.ResponseRecorder
	n       *Notifier
	// Self-built task/exploration: test cases record vulnerabilities here, isolated from other test cases' data.
	taskID int64
	expID  int64
	// Events generated after cleanupMark are deleted together during cleanup.
	cleanupMark int64
}

func newNotifyFixture(t *testing.T) *notifyFixture {
	t.Helper()
	// All fake receivers in this file run on 127.0.0.1, while delivery rejects loopback addresses by default
	// (to prevent SSRF from hitting same-host services and cloud metadata). The test explicitly enables this switch;
	// The guard's "deny by default" behavior is covered by the notify package's ssrf_test.go.
	t.Setenv(notify.AllowLocalTargetsEnv, "1")
	s, _, request := trafficEvidenceServer(t)
	pg := s.m.pg

	// Build a task manually: tasks created by the shared fixture trafficEvidenceServer cannot get an exploration id,
	// But recording a finding requires it.
	task, err := s.m.CreateTask("notification push test", "push behavior verification", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Exec(`DELETE FROM tasks WHERE id=$1`, taskID) })

	var mark int64
	if err := pg.QueryRow(`SELECT COALESCE(max(id),0) FROM notification_events`).Scan(&mark); err != nil {
		t.Fatal(err)
	}
	// Make the fixture self-contained: mark events existing before the fixture was built as dispatched in one go.
	//
	// Why this is necessary: FanOutPendingEvents is **global** and will ... all undispatched events in the database
	// Fan out onto all matching channels. And the shared fixture trafficEvidenceServer itself records a vulnerability evidence entry.
	// (exactly the initial finding it returned); other cases may also have leftovers. Without isolation,
	// These stray events would be dispatched onto this test case's channel, breaking assertions like "should have N deliveries"
	// ... passes and fails intermittently -- and the failure mode depends on test execution order, making it harder to track down than a straight failure.
	if _, err := pg.Exec(`UPDATE notification_events SET fanned_out = true WHERE id <= $1 AND NOT fanned_out`, mark); err != nil {
		t.Fatal(err)
	}

	f := &notifyFixture{s: s, pg: pg, request: request, n: newNotifier(s), taskID: taskID, expID: task.ExpID, cleanupMark: mark}
	t.Cleanup(func() {
		if _, err := pg.Exec(`DELETE FROM notification_events WHERE id > $1`, f.cleanupMark); err != nil {
			t.Logf("Failed to clean up notification events: %v", err)
		}
	})
	// The master switch must be on (other test cases may have turned it off).
	if err := pg.SetBool(settingNotifyEnabled, true); err != nil {
		t.Fatal(err)
	}
	return f
}

// record writes one vulnerability via the real evidence write path, returning the finding id.
// This path registers the push event in the **same transaction** -- exactly this feature's hook point.
func (f *notifyFixture) record(t *testing.T, vulnclass, severity string) int64 {
	t.Helper()
	out, err := f.s.evidenceStore().Record(context.Background(), db.RecordFindingInput{
		TaskID:        f.taskID,
		ExplorationID: f.expID,
		Worker:        "test",
		VulnClass:     vulnclass,
		Name:          vulnclass,
		Severity:      severity,
		Summary:       vulnclass + "'s summary",
		Evidence:      "poc",
	}, nil)
	if err != nil {
		t.Fatalf("Failed to record vulnerability: %v", err)
	}
	return out.FindingID
}

// channel reads back the channel config (for per-channel stepX calls).
func (f *notifyFixture) channel(t *testing.T, id int64) *db.NotificationChannel {
	t.Helper()
	ch, err := f.pg.NotificationChannelByID(context.Background(), id)
	if err != nil {
		t.Fatalf("Failed to read channel: %v", err)
	}
	return ch
}

// deliver dispatches the event and runs one delivery round only for the specified channel.
func (f *notifyFixture) deliver(t *testing.T, chID int64, baseURL string) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}
	f.n.stepRealtime(ctx, f.channel(t, chID), 50, baseURL)
}

// createChannel creates a channel via the HTTP API, incidentally covering the API's own validation path.
func (f *notifyFixture) createChannel(t *testing.T, payload map[string]any) int64 {
	t.Helper()
	raw, _ := json.Marshal(payload)
	r := f.request("POST", "/api/notify/channels", string(raw))
	if r.Code != 200 {
		t.Fatalf("Failed to create channel %d: %s", r.Code, r.Body)
	}
	var res struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &res); err != nil || res.ID == 0 {
		t.Fatalf("Create-channel returned an anomaly: %s (%v)", r.Body, err)
	}
	t.Cleanup(func() { f.pg.Exec(`DELETE FROM notification_channels WHERE id=$1`, res.ID) })
	return res.ID
}

// fakeWebhook is a fake receiver that records the received request bodies.
type fakeWebhook struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
}

func newFakeWebhook(t *testing.T) *fakeWebhook {
	t.Helper()
	f := &fakeWebhook{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeWebhook) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakeWebhook) body(t *testing.T, i int) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.bodies) {
		t.Fatalf("Fake receiver only got %d requests; cannot fetch request #%d", len(f.bodies), i)
	}
	return f.bodies[i]
}

func (f *fakeWebhook) last(t *testing.T) map[string]any {
	t.Helper()
	if f.count() == 0 {
		t.Fatal("Fake receiver got no requests")
	}
	return f.body(t, f.count()-1)
}

// markdownText extracts the body text from the request body, tolerating field-name differences across vendors:
// DingTalk markdown uses `text`, ActionCard uses `text`, WeCom markdown uses `content`.
func markdownText(t *testing.T, body map[string]any) string {
	t.Helper()
	for _, key := range []string{"markdown", "actionCard"} {
		section, ok := body[key].(map[string]any)
		if !ok {
			continue
		}
		for _, field := range []string{"text", "content"} {
			if s, ok := section[field].(string); ok && s != "" {
				return s
			}
		}
	}
	t.Fatalf("No recognizable body in the request: %v", body)
	return ""
}

// agePendingBatch ages the pending deliveries of a channel, used to test digest-batch expiry.
func (f *notifyFixture) agePendingBatch(t *testing.T, chID int64) {
	t.Helper()
	if _, err := f.pg.Exec(`UPDATE notification_deliveries SET created_at = now() - interval '2 hours'
WHERE channel_id=$1 AND state=$2`, chID, db.NotifyStatePending); err != nil {
		t.Fatal(err)
	}
}

func TestNotifyEndToEndRealtimeDelivery(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "real-time push",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	f.record(t, "SQL injection", "high")
	f.deliver(t, chID, "")

	if hook.count() != 1 {
		t.Fatalf("expected 1 message, got %d", hook.count())
	}
	text := markdownText(t, hook.last(t))
	for _, want := range []string{"SQL injection", "high-risk", "summary"} {
		if !strings.Contains(text, want) {
			t.Fatalf("message body missing %q:\n%s", want, text)
		}
	}
	// Delivery should transition to sent.
	var pending int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1 AND state <> $2`,
		chID, db.NotifyStateSent).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("%d deliveries still not marked sent after dispatch", pending)
	}
}

func TestNotifyChannelAPIMasksSecretsAndPreservesOnUpdate(t *testing.T) {
	f := newNotifyFixture(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "Masking test case",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "https://oapi.dingtalk.com/robot/send?access_token=abc123456", "secret": "SECabcdef123456"},
	})

	r := f.request("GET", "/api/notify/channels", "")
	if r.Code != 200 {
		t.Fatalf("List-channels failed %d: %s", r.Code, r.Body)
	}
	if strings.Contains(r.Body.String(), "abc123456") || strings.Contains(r.Body.String(), "SECabcdef123456") {
		t.Fatalf("Interface echo leaked a credential: %s", r.Body)
	}
	var listed struct {
		Channels []struct {
			ID         int64          `json:"id"`
			Config     map[string]any `json:"config"`
			SecretKeys []string       `json:"secret_keys"`
		} `json:"channels"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	var mine *struct {
		ID         int64          `json:"id"`
		Config     map[string]any `json:"config"`
		SecretKeys []string       `json:"secret_keys"`
	}
	for i := range listed.Channels {
		if listed.Channels[i].ID == chID {
			mine = &listed.Channels[i]
		}
	}
	if mine == nil {
		t.Fatal("Newly created channel missing from the list")
	}
	if !notify.IsMasked(fmt.Sprint(mine.Config["webhook"])) || !notify.IsMasked(fmt.Sprint(mine.Config["secret"])) {
		t.Fatalf("Credential fields should hold masked values: %v", mine.Config)
	}
	if len(mine.SecretKeys) == 0 {
		t.Fatal("API should tell the frontend which fields are credentials")
	}

	// PATCH renames only + echoes back masked credentials: real credentials must be preserved as-is.
	body, _ := json.Marshal(map[string]any{
		"name":   "After rename",
		"config": map[string]any{"webhook": fmt.Sprint(mine.Config["webhook"]), "secret": fmt.Sprint(mine.Config["secret"])},
	})
	if r := f.request("PATCH", fmt.Sprintf("/api/notify/channels/%d", chID), string(body)); r.Code != 200 {
		t.Fatalf("Update failed %d: %s", r.Code, r.Body)
	}
	cfg := f.channelConfig(t, chID)
	if cfg["webhook"] != "https://oapi.dingtalk.com/robot/send?access_token=abc123456" {
		t.Fatalf("Masked echo overwrote the real credential: %v", cfg["webhook"])
	}
	if cfg["secret"] != "SECabcdef123456" {
		t.Fatalf("Masked echo-back overwrote secret: %v", cfg["secret"])
	}
	if f.channel(t, chID).Name != "After rename" {
		t.Fatal("Name not updated")
	}

	// Explicitly clearing secret should take effect (as opposed to 'returning the mask = keep unchanged').
	body, _ = json.Marshal(map[string]any{"config": map[string]any{"secret": ""}})
	if r := f.request("PATCH", fmt.Sprintf("/api/notify/channels/%d", chID), string(body)); r.Code != 200 {
		t.Fatalf("Failed to clear secret %d: %s", r.Code, r.Body)
	}
	if _, still := f.channelConfig(t, chID)["secret"]; still {
		t.Fatal("Empty string should clear the secret")
	}
}

func (f *notifyFixture) channelConfig(t *testing.T, id int64) map[string]any {
	t.Helper()
	var cfg map[string]any
	if err := json.Unmarshal(f.channel(t, id).Config, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestNotifyChannelAPICreateValidation(t *testing.T) {
	f := newNotifyFixture(t)
	cases := []struct {
		name    string
		payload map[string]any
		wantSub string
	}{
		{"Invalid type", map[string]any{"name": "x", "kind": "nope", "config": map[string]any{}}, "Channel type invalid"},
		{"Missing name", map[string]any{"kind": notify.KindDingTalk, "config": map[string]any{"webhook": "https://e.com/h"}}, "Missing channel name"},
		{"Missing webhook", map[string]any{"name": "x", "kind": notify.KindDingTalk, "config": map[string]any{}}, "Webhook"},
		{"webhook protocol illegal", map[string]any{"name": "x", "kind": notify.KindDingTalk, "config": map[string]any{"webhook": "file:///etc/passwd"}}, "Webhook address invalid"},
		{"Invalid mode", map[string]any{"name": "x", "kind": notify.KindDingTalk, "mode": "sometimes", "config": map[string]any{"webhook": "https://e.com/h"}}, "Invalid push mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.payload)
			r := f.request("POST", "/api/notify/channels", string(raw))
			if r.Code != 400 {
				t.Fatalf("Expected 400, got %d: %s", r.Code, r.Body)
			}
			if !strings.Contains(r.Body.String(), tc.wantSub) {
				t.Fatalf("Error message should mention %q, got %s", tc.wantSub, r.Body)
			}
		})
	}
	if r := f.request("DELETE", "/api/notify/channels/99999999", ""); r.Code != 404 {
		t.Fatalf("Deleting a nonexistent channel should return 404, got %d", r.Code)
	}
}

func TestNotifyFilterBlocksBelowThreshold(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "Critical-only",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
		"filter": map[string]any{"min_severity": "critical"},
	})
	f.record(t, "Low-severity issue", "low")
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("Vulnerabilities below the threshold should not produce deliveries, got %d", n)
	}
	f.n.stepRealtime(context.Background(), f.channel(t, chID), 50, "")
	if hook.count() != 0 {
		t.Fatal("Filtered-out findings should produce no messages")
	}
}

func TestNotifyDigestBatchesMultipleFindingsIntoOneMessage(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "Digest push",
		"kind":   notify.KindDingTalk,
		"mode":   db.NotifyModeDigest,
		"config": map[string]any{"webhook": hook.URL},
	})
	for i := 0; i < 3; i++ {
		f.record(t, fmt.Sprintf("Digest finding %d", i+1), "high")
	}
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	ch := f.channel(t, chID)

	// Not yet due: don't send.
	f.n.stepDigest(ctx, ch, 50, "")
	if hook.count() != 0 {
		t.Fatal("Digest batch sent before due")
	}

	// After flushing stale batches: three items merged into one message.
	f.agePendingBatch(t, chID)
	f.n.stepDigest(ctx, ch, 50, "")
	if got := hook.count(); got != 1 {
		t.Fatalf("Three items should be aggregated into one message, actually sent %d", got)
	}
	text := markdownText(t, hook.last(t))
	if !strings.Contains(text, "Near") || !strings.Contains(text, "3 findings") {
		t.Fatalf("Aggregated message missing count/time-window copy:\n%s", text)
	}
	for i := 1; i <= 3; i++ {
		if !strings.Contains(text, fmt.Sprintf("Digest finding %d", i)) {
			t.Fatalf("Digest message is missing item %d:\n%s", i, text)
		}
	}
	// Items in the same batch should share a batch_id.
	var distinct, total int
	if err := f.pg.QueryRow(`SELECT count(DISTINCT batch_id), count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&distinct, &total); err != nil {
		t.Fatal(err)
	}
	if total != 3 || distinct != 1 {
		t.Fatalf("Three deliveries should share one batch_id, got distinct=%d total=%d", distinct, total)
	}
}

func TestNotifyDisabledChannelDoesNotSend(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":    "Disabled channel",
		"kind":    notify.KindDingTalk,
		"enabled": false,
		"config":  map[string]any{"webhook": hook.URL},
	})
	f.record(t, "Findings during the disabled period", "critical")
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("Disabled channels should produce no deliveries, got %d", n)
	}
}

func TestNotifyStatusChangeDelivery(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "Status-change subscription",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
		"filter": map[string]any{"on_status_change": true},
	})
	finding := f.record(t, "Status change test case", "high")
	r := f.request("PATCH", fmt.Sprintf("/api/exploration/findings/%d", finding), `{"status":"fixed"}`)
	if r.Code != 200 {
		t.Fatalf("failed to change status %d: %s", r.Code, r.Body)
	}
	f.deliver(t, chID, "")

	// There should be two: the fixed one is a status change; the finding_created one may also be emitted in the same round.
	// The status-change one was actually created later, but ordering is not relied upon \u2014 search the full set.
	found := false
	for i := 0; i < hook.count(); i++ {
		text := markdownText(t, hook.body(t, i))
		if strings.Contains(text, "Status change") && strings.Contains(text, "Fixed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("Did not receive any message containing \"status change \u2192 fixed\" (%d messages in total)", hook.count())
	}
}

func TestNotifyStatusChangeSuppressedByDefault(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "Not subscribed to status changes",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	finding := f.record(t, "Not subscribed to changes", "high")
	if r := f.request("PATCH", fmt.Sprintf("/api/exploration/findings/%d", finding), `{"status":"false_positive"}`); r.Code != 200 {
		t.Fatalf("failed to change status %d: %s", r.Code, r.Body)
	}
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id
WHERE d.channel_id=$1 AND e.kind=$2`, chID, notify.EventFindingStatusChanged).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("Channels not subscribed to status changes should not receive status-change deliveries; got %d", n)
	}
}

func TestNotifyTestMessageEndpoint(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "Test send",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	if r := f.request("POST", fmt.Sprintf("/api/notify/channels/%d/test", chID), ""); r.Code != 200 {
		t.Fatalf("Test send failed %d: %s", r.Code, r.Body)
	}
	if hook.count() != 1 {
		t.Fatalf("Fake receiver should have received 1 test message; got %d", hook.count())
	}
	// Test messages must be recognizable as tests at a glance, and must not be mistaken for real vulnerabilities.
	if text := markdownText(t, hook.last(t)); !strings.Contains(text, "test") {
		t.Fatalf("Test message should be marked as a test: %s", text)
	}
	// When the config is broken, the channel's original error should be returned to the user as-is.
	badID := f.createChannel(t, map[string]any{
		"name":   "Bad address",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "http://127.0.0.1:1/hook"},
	})
	if r := f.request("POST", fmt.Sprintf("/api/notify/channels/%d/test", badID), ""); r.Code != 502 {
		t.Fatalf("Failed delivery should return 502; got %d: %s", r.Code, r.Body)
	}
}

func TestNotifyDeliveriesHistoryAndRetry(t *testing.T) {
	f := newNotifyFixture(t)
	// Point at an address guaranteed to fail, producing a failed delivery.
	chID := f.createChannel(t, map[string]any{
		"name":   "Failure retry",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "http://127.0.0.1:1/hook"},
	})
	f.record(t, "Push that will fail", "high")
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	ch := f.channel(t, chID)
	// Keep delivering until the retry budget is exhausted.
	for i := 0; i < db.MaxNotifyAttempts; i++ {
		f.n.stepRealtime(ctx, ch, 50, "")
		if _, err := f.pg.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE channel_id=$1`, chID); err != nil {
			t.Fatal(err)
		}
	}
	var state string
	if err := f.pg.QueryRow(`SELECT state FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != db.NotifyStateFailed {
		t.Fatalf("Should be failed after retries are exhausted, got %s", state)
	}

	r := f.request("GET", fmt.Sprintf("/api/notify/deliveries?channel_id=%d&state=failed", chID), "")
	if r.Code != 200 {
		t.Fatalf("History lookup failed %d: %s", r.Code, r.Body)
	}
	var hist struct {
		Deliveries []struct {
			ID        int64  `json:"id"`
			State     string `json:"state"`
			LastError string `json:"last_error"`
			Attempts  int    `json:"attempts"`
			Title     string `json:"title"`
		} `json:"deliveries"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &hist); err != nil {
		t.Fatal(err)
	}
	if hist.Total != 1 || len(hist.Deliveries) != 1 {
		t.Fatalf("Should find 1 failed delivery, got total=%d len=%d", hist.Total, len(hist.Deliveries))
	}
	if hist.Deliveries[0].LastError == "" {
		t.Fatal("History should include the failure reason, otherwise users cannot troubleshoot")
	}
	if hist.Deliveries[0].Attempts < db.MaxNotifyAttempts {
		t.Fatalf("Attempt count should be recorded, got %d", hist.Deliveries[0].Attempts)
	}
	if hist.Deliveries[0].Title != "Push that will fail" {
		t.Fatalf("History should include the finding title, got %q", hist.Deliveries[0].Title)
	}

	// Manual resend: should return to pending and reset the counter.
	if r := f.request("POST", fmt.Sprintf("/api/notify/deliveries/%d/retry", hist.Deliveries[0].ID), ""); r.Code != 200 {
		t.Fatalf("Resend failed %d: %s", r.Code, r.Body)
	}
	var attempts int
	if err := f.pg.QueryRow(`SELECT state, attempts FROM notification_deliveries WHERE id=$1`, hist.Deliveries[0].ID).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != db.NotifyStatePending || attempts != 0 {
		t.Fatalf("Should be pending with attempts=0 after resend, got %s/%d", state, attempts)
	}
}

func TestNotifyMetaAndSettingsRoundTrip(t *testing.T) {
	f := newNotifyFixture(t)
	r := f.request("GET", "/api/notify/meta", "")
	if r.Code != 200 {
		t.Fatalf("meta failed: %s", r.Body)
	}
	var meta struct {
		Kinds []struct {
			Kind       string   `json:"kind"`
			SecretKeys []string `json:"secret_keys"`
		} `json:"kinds"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if len(meta.Kinds) != len(notify.Kinds()) {
		t.Fatalf("meta should list all %d channels, got %d", len(notify.Kinds()), len(meta.Kinds))
	}
	for _, k := range meta.Kinds {
		if len(k.SecretKeys) == 0 {
			t.Errorf("Channel %s did not report credential fields", k.Kind)
		}
	}

	// Round-trip of three global settings. Trailing slashes should be normalized away, otherwise back-links would come out as "//function/...".
	if r := f.request("PUT", "/api/settings", `{"notify_public_base_url":"https://artex.example.com/","notify_digest_interval_min":15,"notify_enabled":true}`); r.Code != 200 {
		t.Fatalf("Settings write failed %d: %s", r.Code, r.Body)
	}
	t.Cleanup(func() {
		f.pg.Exec(`DELETE FROM settings WHERE key IN ($1,$2)`, settingNotifyPublicBaseURL, settingNotifyDigestMinutes)
	})
	payload := f.s.settingsPayload()
	if payload["notify_public_base_url"] != "https://artex.example.com" {
		t.Fatalf("Callback address not normalized: %v", payload["notify_public_base_url"])
	}
	if payload["notify_digest_interval_min"] != 15 {
		t.Fatalf("Digest cycle not in effect: %v", payload["notify_digest_interval_min"])
	}

	// Illegal values should be rejected.
	for _, body := range []string{
		`{"notify_public_base_url":"ftp://x"}`,
		`{"notify_digest_interval_min":0}`,
		`{"notify_digest_interval_min":99999}`,
	} {
		if r := f.request("PUT", "/api/settings", body); r.Code != 400 {
			t.Errorf("%s should return 400, got %d", body, r.Code)
		}
	}
}

// TestNotifyDeepLinkUsesPublicBaseURL covers deep-link assembly: when public_base_url is configured
// A single message must use a buttoned ActionCard with the link pointing to the vulnerability detail page.
func TestNotifyDeepLinkUsesPublicBaseURL(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "Callback link",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	finding := f.record(t, "Finding with a callback link", "high")
	f.deliver(t, chID, "https://artex.example.com")

	body := hook.last(t)
	card, _ := body["actionCard"].(map[string]any)
	if card == nil {
		t.Fatalf("Apply ActionCard when a callback link is present, got msgtype=%v", body["msgtype"])
	}
	want := fmt.Sprintf("https://artex.example.com/function/findings/detail?id=%d", finding)
	if card["singleURL"] != want {
		t.Fatalf("Callback link mismatch\nExpected %s\nGot %v", want, card["singleURL"])
	}
}

// TestNotifyNoDeepLinkWithoutBaseURL reverse coverage: when no external base URL is configured, no broken link should be produced.
// (e.g. pointing to localhost or a relative path), should fall back to pure markdown.
func TestNotifyNoDeepLinkWithoutBaseURL(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "No callback link",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	f.record(t, "Finding without a callback link", "high")
	f.deliver(t, chID, "")

	body := hook.last(t)
	if body["msgtype"] != "markdown" {
		t.Fatalf("Should send markdown when no external address is configured, got %v", body["msgtype"])
	}
	if text := markdownText(t, body); strings.Contains(text, "View details") {
		t.Fatalf("Detail link should not appear when no external address is configured:\n%s", text)
	}
}

// TestNotifyDigestSegmentsAndDefersRemainder is the end-to-end evidence for the "silent-loss" fix.
//
// Digest messages are bound by the channel length cap (WeCom: 4096 bytes); when a batch does not fit, it must be split **by whole message**:
// Those packed into this message are marked delivered; the rest return to the queue for the next message. The old implementation marked the whole batch
// Success -- the truncated items appear neither in the message nor in the failure list, while the delivery history still shows success,
// And just like that the finding was gone.
//
// Assert four things: ① only the actually fitted count was marked ② the rest remain pending ③ the deferred items [...]
// **No retry consumed** ④ Running one more round sends the rest (no deadlock).
func TestNotifyDigestSegmentsAndDefersRemainder(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	// Using WeCom: the markdown cap is 4096 bytes, the tightest of the six channels.
	chID := f.createChannel(t, map[string]any{
		"name":   "Segmented digest",
		"kind":   notify.KindWeCom,
		"mode":   db.NotifyModeDigest,
		"config": map[string]any{"webhook": hook.URL},
	})
	const total = 60
	// Use a longer title so that 60 entries far exceed 4096 bytes, forcing segmentation.
	longName := strings.Repeat("overlong vulnerability name", 6)
	for i := 0; i < total; i++ {
		f.record(t, longName+strconv.Itoa(i+1), "high")
	}
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	f.agePendingBatch(t, chID)
	ch := f.channel(t, chID)

	f.n.stepDigest(ctx, ch, 50, "")
	if hook.count() != 1 {
		t.Fatalf("Expected exactly one message, got %d", hook.count())
	}

	var sent, pending int
	if err := f.pg.QueryRow(`SELECT
    count(*) FILTER (WHERE state=$2),
    count(*) FILTER (WHERE state=$3)
  FROM notification_deliveries WHERE channel_id=$1`, chID, db.NotifyStateSent, db.NotifyStatePending).
		Scan(&sent, &pending); err != nil {
		t.Fatal(err)
	}
	if sent == 0 {
		t.Fatal("Some entries should be marked delivered")
	}
	if pending == 0 {
		t.Fatalf("A batch of %d items cannot possibly all fit in 4096 bytes; some should remain pending; sent=%d", total, sent)
	}
	if sent+pending != total {
		t.Fatalf("Item count mismatch: sent=%d pending=%d total=%d (neither delivered nor pending = lost)", sent, pending, total)
	}
	// The message body must truthfully state how many more entries are not included in this one.
	if text := markdownText(t, hook.last(t)); !strings.Contains(text, "Remaining") {
		t.Fatalf("Message should state that some entries are not included in this one:\n%.400s", text)
	}

	// Deferred items must not consume retry budget: attempts was optimistically +1 at claim time, and must be decremented back on deferral.
	var maxAttempts int
	if err := f.pg.QueryRow(`SELECT COALESCE(max(attempts),0) FROM notification_deliveries
WHERE channel_id=$1 AND state=$2`, chID, db.NotifyStatePending).Scan(&maxAttempts); err != nil {
		t.Fatal(err)
	}
	if maxAttempts > 0 {
		t.Fatalf("Deferred entries should not consume retry attempts (otherwise they would be judged as failed after a few entries), got attempts=%d", maxAttempts)
	}

	// Run repeatedly until convergence. What is asserted is **final full delivery** with multiple rounds in the middle--
	// This is stronger than 'second round done': it proves segmentation neither deadlocks nor drops the remaining entries.
	rounds := 0
	for {
		var undelivered int
		if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries
WHERE channel_id=$1 AND state <> $2 AND state <> $3`, chID, db.NotifyStateSent, db.NotifyStateFailed).
			Scan(&undelivered); err != nil {
			t.Fatal(err)
		}
		if undelivered == 0 {
			break
		}
		rounds++
		if rounds > total+5 {
			t.Fatalf("Segmented delivery did not converge: %d items still pending after %d rounds", rounds, undelivered)
		}
		before := hook.count()
		f.n.stepDigest(ctx, ch, 50, "")
		if hook.count() == before {
			t.Fatalf("Round %d made no progress; the remaining %d items will be stuck forever", rounds, undelivered)
		}
	}
	if rounds < 2 {
		t.Fatalf("A 4096-byte message cannot hold %d long-title vulnerabilities and should be sent over multiple ticks, but only %d ticks were actually used", total, rounds)
	}
	// Every round after the first should be **pure continuation**; there should be no entries rejected by the channel.
	var failed int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1 AND state=$2`,
		chID, db.NotifyStateFailed).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if failed != 0 {
		t.Fatalf("Fake receiver always returns success; there should be no failed entries, got %d", failed)
	}
}

// TestNotifyBackoffTableMatchesAttemptBudget is an anti-drift assertion.
//
// Retry budget (db.MaxNotifyAttempts) and backoff sequence table (notifyBackoff) live in two separate packages:
// The former is the state machine's policy, the latter the engine's execution cadence. Changing only one of them -- e.g. raising the budget to 5
// attempts but forgetting to add backoff tiers -- the code will not error; it will only make the 4th and 5th retries reuse the last tier's interval,
// Manifests as "retries slowing down for no apparent reason", which is hard to trace back here during troubleshooting.
// Assert both lengths match so such drift surfaces in CI.
func TestNotifyBackoffTableMatchesAttemptBudget(t *testing.T) {
	if len(notifyBackoff) != db.MaxNotifyAttempts {
		t.Fatalf("Backoff tier count (%d) is inconsistent with max attempts (%d) -- changing one must change the other too",
			len(notifyBackoff), db.MaxNotifyAttempts)
	}
	// The backoff interval must be monotonically non-decreasing; otherwise retries get more aggressive and worsen rate limiting.
	for i := 1; i < len(notifyBackoff); i++ {
		if notifyBackoff[i] < notifyBackoff[i-1] {
			t.Fatalf("Backoff intervals must be non-decreasing: level %d %v < level %d %v",
				i, notifyBackoff[i], i-1, notifyBackoff[i-1])
		}
	}
}

// TestNotifyRateLimitDoesNotConsumeRetryBudget locks in the "acquire token before claiming" order.
// If reversed (claim first, drop later), deliveries blocked by rate limiting would already have consumed one attempts count,
// The budget would be drained by pure waiting and end up in failed.
func TestNotifyRateLimitDoesNotConsumeRetryBudget(t *testing.T) {
	// Tests only the token bucket itself; no Server needed (nor should one be created for it).
	n := &Notifier{buckets: map[int64]*notifyBucket{}}
	now := time.Now()
	// 1 per minute: at most 1 when the bucket is full.
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now); got != 1 {
		t.Fatalf("With a full bucket, 1 message per minute should take 1 token, got %d", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Millisecond)); got != 0 {
		t.Fatalf("Should return 0 immediately after tokens are exhausted, got %d", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(30*time.Second)); got != 0 {
		t.Fatalf("Should not refill a full token halfway, got %d", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Minute)); got != 1 {
		t.Fatalf("One token should be refilled after a full period, got %d", got)
	}
	// Unthrottled channels use a finite cap to avoid a single round being stalled by unbounded backlog.
	if got := n.takeTokens(2, 0, notifyUnlimitedBurstPerTick+10, now); got != notifyUnlimitedBurstPerTick {
		t.Fatalf("Unlimited rate should return per-round cap %d, got %d", notifyUnlimitedBurstPerTick, got)
	}
	// Token buckets are independent across channels.
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Millisecond)); got != 0 {
		t.Fatalf("Channel 1's bucket should still be empty, got %d", got)
	}
}

// TestNotifyTakeTokensKeepsUnusedTokens locks in the "take only want items" semantics.
//
// An older implementation drained the whole bucket before the caller truncated it, so a rate=100/min channel filled its bucket and
// Only 5 are used per round; the remaining 95 tokens are discarded directly; the same deduction applies when the channel has no pending deliveries this round.
// The result is that the comment's claim of "bursting rate_per_min items at once when backlogged" is unachievable in any case.
func TestNotifyTakeTokensKeepsUnusedTokens(t *testing.T) {
	n := &Notifier{buckets: map[int64]*notifyBucket{}}
	now := time.Now()
	// Bucket starts full (100); this round only needs 5.
	if got := n.takeTokens(1, 100, 5, now); got != 5 {
		t.Fatalf("With want=5, exactly 5 tokens should be taken, got %d", got)
	}
	// Key assertion: the remaining 95 must still be in the bucket, not drained and discarded.
	// Do not advance time, ensuring tokens taken can only come from existing stock, not refills.
	if got := n.takeTokens(1, 100, 95, now); got != 95 {
		t.Fatalf("Remaining tokens should still be retrievable (expected 95), got %d -- the bucket was drained in a whole round", got)
	}
	if got := n.takeTokens(1, 100, 1, now); got != 0 {
		t.Fatalf("Bucket fully drained; should return 0, got %d", got)
	}
	// want<=0 should deduct no tokens (empty rounds are free).
	n2 := &Notifier{buckets: map[int64]*notifyBucket{}}
	if got := n2.takeTokens(1, 20, 0, now); got != 0 {
		t.Fatalf("want=0 should return 0, got %d", got)
	}
	if got := n2.takeTokens(1, 20, 20, now); got != 20 {
		t.Fatalf("The call with want=0 should not consume tokens; a full 20 should still be retrievable, got %d", got)
	}
}

// TestDigestTickPlanDecouplesBatchSizeFromSendBudget locks in the two dimensions of digest mode.
//
// Once the digest batch size is tied to the per-tick request budget, a channel with rate_per_min=20 can only, in each
// 3-second tick, refill 1 token, so each digest message holds only 1 vulnerability -- functionally equivalent to having no
// digest, while the message header still says "1 new vulnerability in the last 30 minutes". This degradation raises no error,
// The existing end-to-end cases cannot catch this either (they manually pass stepDigest a large enough limit,
// (Bypasses the quota calculation in step), so assert the decision itself directly here.
func TestDigestTickPlanDecouplesBatchSizeFromSendBudget(t *testing.T) {
	tokens, claimLimit := digestTickPlan()
	// One batch = one message = one request = one token. The token's unit is messages, not vulnerabilities.
	if tokens != 1 {
		t.Fatalf("A summarized batch sends only one message and should consume exactly 1 token; got %d", tokens)
	}
	if claimLimit != db.MaxDigestBatchSize {
		t.Fatalf("The digest batch size should be the in-memory upper bound db.MaxDigestBatchSize=%d, got %d",
			db.MaxDigestBatchSize, claimLimit)
	}
	// Key relationship: batch size must be far larger than the per-round request budget. Once the two are on the same order of magnitude,
	// Means "how many messages to send" and "how many vulnerabilities per batch" were conflated into a single number again.
	if claimLimit <= notifyMaxSendsPerChannelPerTick {
		t.Fatalf("Summary batch size %d should not be constrained by the per-round request budget %d\u2014\u2014"+
			"Request budget is the 'number of requests' derived back from the lease, a different dimension from 'how many vulnerabilities fit in one batch'",
			claimLimit, notifyMaxSendsPerChannelPerTick)
	}
}

// TestNotifyTickBudgetFitsWithinLease is another anti-drift assertion.
//
// The per-round delivery cap of a single channel (notifyMaxSendsPerChannelPerTick) is derived backwards from the lease duration:
// The worst-case serial delivery time in one round must be < the lease; otherwise the lease expires before the last few items finish,
// In multi-instance deployments, the peer will reclaim and resend them. These three constants live in different places, and changing any one of them could break the relationship without any error--hence it is pinned down here.
// Changing any one of them may break the relationship without any error -- so pin it down here.
func TestNotifyTickBudgetFitsWithinLease(t *testing.T) {
	worst := time.Duration(notifyMaxSendsPerChannelPerTick) * notifySendTimeout
	if worst >= notifyLease {
		t.Fatalf("Worst-case single-channel single-round latency %v should not reach or exceed the lease %v"+
			" (notifyMaxSendsPerChannelPerTick=%d × notifySendTimeout=%v)----"+
			"Changing any of these three constants requires checking the other two in sync",
			worst, notifyLease, notifyMaxSendsPerChannelPerTick, notifySendTimeout)
	}
}
