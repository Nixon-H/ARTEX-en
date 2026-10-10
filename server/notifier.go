package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// Global setting keys (live in the settings key-value table, no new table needed).
const (
	// settingNotifyEnabled is the master push switch. On by default: it serves as a one-click kill switch during maintenance,
	// Rather than the feature's enablement condition -- the real enablement condition is "whether any channel is configured".
	settingNotifyEnabled = "notify_enabled"
	// settingNotifyPublicBaseURL is the external access address used to generate vulnerability-detail back-links
	// (e.g. https://artex.example.com). When left empty, messages carry no back-link button.
	// There is no reusable external-address configuration in the project, so a new one is added here.
	settingNotifyPublicBaseURL = "notify_public_base_url"
	// settingNotifyDigestMinutes is the period (in minutes) of digest mode.
	settingNotifyDigestMinutes = "notify_digest_interval_min"
)

const (
	// notifyTick is the polling interval of the delivery engine. 3 seconds is the upper bound on this engine's real-time responsiveness,
	// and the main source of latency between "vulnerability stored in DB" and "message reaching IM".
	notifyTick = 3 * time.Second
	// notifyLease is the lease duration when claiming a delivery. It must be significantly longer than the worst-case time of a single delivery
	// (the notify package's HTTP client times out at 15 seconds), otherwise the same row may be delivered by two
	// dispatchers at the same time.
	notifyLease = 3 * time.Minute
	// notifyFanOutPerTick limits the number of events dispatched per round, to avoid -- when a channel is first enabled --
	// expanding the entire historical backlog into delivery tasks all at once.
	notifyFanOutPerTick = 200
	// notifyDefaultDigestMinutes is the default value of the digest period.
	notifyDefaultDigestMinutes = 30
	// notifyUnlimitedBurstPerTick is the per-round delivery cap when a channel has no rate limit configured.
	// It exists to prevent "a channel configured as unlimited + one scan producing thousands of vulnerabilities" from
	// dragging a single round of the loop into a long blocking stall.
	notifyUnlimitedBurstPerTick = 50
	// notifyMaxSendsPerChannelPerTick is the max number of deliveries per channel per round.
	//
	// This cap is derived backwards from the **lease duration**: claiming rows stamps them with a lease (notifyLease = 3 minutes),
	// if the number of serial deliveries in one round is so large that the worst-case time exceeds the lease, the lease expires before the later rows finish sending.
	// Within a single process it does not matter (Run is a single goroutine running serially, ticks never re-enter), but when **two
	// processes connect to the same database**, the peer reclaims rows whose leases expired and resends them, and also
	// double-increments attempts, marking rows failed while the original process is still delivering them.
	//
	// Value: 3-minute lease / 30-second single-send timeout = 6 **exactly fills the lease** with zero margin,
	// so it must not be used; 5 keeps the worst-case time at 150 seconds, leaving a 30-second margin. This relationship is pinned by
	// TestNotifyTickBudgetFitsWithinLease -- changing notifyLease,
	// notifySendTimeout, or this value will fail that assertion.
	notifyMaxSendsPerChannelPerTick = 5
	// notifySendTimeout is the timeout for a single delivery. It also determines the value of the previous constant,
	// their product must not exceed notifyLease; see TestNotifyTickBudgetFitsWithinLease.
	notifySendTimeout = 30 * time.Second
)

// notifyBackoff is the backoff sequence for failed retries, indexed by attempt count.
// The 3 attempts (including the first) correspond to db.MaxNotifyAttempts; both must be changed together.
var notifyBackoff = []time.Duration{
	time.Second,
	5 * time.Second,
	30 * time.Second,
}

// Notifier is the delivery engine for vulnerability notifications.
//
// It runs side by side with the Scheduler as an independent goroutine (see server.New). It deliberately does not reuse
// the Scheduler's tick: the real-time requirement of notifications (3 seconds) differs from the business cadence of triggers,
// and failures of the two must not implicate each other -- a stuck notification pipeline must not affect agent triggering.
type Notifier struct {
	s  *Server
	pg *db.DB

	// mu guards buckets. With few channels and low contention, a single mutex is enough,
	// not worth introducing a finer-grained structure for it.
	mu      sync.Mutex
	buckets map[int64]*notifyBucket
}

// notifyBucket is the token bucket for a single channel.
//
// A token bucket is used instead of a "count-per-minute then reset" sliding window because the latter's boundary effect is terrible:
// sending 20 at the end of a window and 20 more the next instant looks like 40 within one second to the platform,
// triggering rate limiting; a token bucket refills at a constant rate, naturally avoiding such bursts.
type notifyBucket struct {
	tokens   float64
	lastFill time.Time
}

func newNotifier(s *Server) *Notifier {
	return &Notifier{s: s, pg: s.m.pg, buckets: map[int64]*notifyBucket{}}
}

// Run loops until ctx ends. Started once by server.New.
func (n *Notifier) Run(ctx context.Context) {
	if n.pg == nil {
		return
	}
	t := time.NewTicker(notifyTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.step(ctx)
		}
	}
}

// step runs one round: first dispatch new events, then deliver due tasks.
//
// Failure of any step is only logged, never interrupting the loop -- a failure of the notification system must never escalate into a process-level problem.
// Each tick is independent; the next round retries naturally.
func (n *Notifier) step(ctx context.Context) {
	if !n.enabled() {
		return
	}
	if _, _, err := n.pg.FanOutPendingEvents(ctx, notifyFanOutPerTick); err != nil {
		log.Printf("[notify] failed to dispatch event: %v", err)
		return
	}
	channels, err := n.pg.ListNotificationChannels(ctx)
	if err != nil {
		log.Printf("[notify] Failed to read channel: %v", err)
		return
	}
	baseURL := n.publicBaseURL()
	for _, ch := range channels {
		if !ch.IsEnabled() {
			continue
		}
		// The token bucket is metered in **message count** (equivalent to HTTP request count), not vulnerability count.
		// In real-time mode both are the same (one vulnerability per message); in summary mode a whole batch of vulnerabilities is merged into one message
		// A single message, so only one token is consumed.
		//
		// Both modes first ask the token bucket, then claim against the quota--the order must not be reversed, otherwise deliveries held back by rate limiting would have already consumed retry counts.
		// Delivery already consumed retry attempts.
		now := time.Now()
		if ch.Mode == db.NotifyModeDigest {
			tokens, claimLimit := digestTickPlan()
			if n.takeTokens(ch.ID, ch.RatePerMin, tokens, now) <= 0 {
				continue
			}
			n.stepDigest(ctx, ch, claimLimit, baseURL)
			continue
		}
		allow := n.takeTokens(ch.ID, ch.RatePerMin, notifyMaxSendsPerChannelPerTick, now)
		if allow <= 0 {
			continue
		}
		n.stepRealtime(ctx, ch, allow, baseURL)
	}
}

// digestTickPlan returns this round's token consumption and batch-size upper bound for digest channels.
//
// The two return values are **two different dimensions**, which is exactly why they are separate functions:
//
// - tokens is the message count. One batch of vulnerabilities is composed into one message with one HTTP request, so it is always 1.
// rate_per_min therefore still applies to digests (at most this many digest messages per minute).
//   - claimLimit is the maximum number of vulnerabilities this batch can hold. It is only bounded by the memory ceiling, independent of the request budget.
//
// In the past, to make rate_per_min apply to digests, the per-round request budget was [...]
// (notifyMaxSendsPerChannelPerTick, derived back from the lease) passed straight down as the batch size.
// The consequence: a channel with rate_per_min=20 refills only 1 token within a 3-second tick, so each digest...
// A message holding only 1 vulnerability -- the digest degrades into "realtime push with digest copy", and readers receive a stream of
// "1 new vulnerability in the last 30 minutes", while db.MaxDigestBatchSize is never reachable.
//
// This symptom is hard to catch in end-to-end tests (existing cases all manually pass a large enough limit to
// stepDigest, bypassing the quota computation inside step), so the decision is centralized here [...]
// Pinned directly by TestDigestTickPlanDecouplesBatchSizeFromSendBudget.
func digestTickPlan() (tokens, claimLimit int) {
	return 1, db.MaxDigestBatchSize
}

// stepRealtime claims and delivers a channel's real-time tasks, one message per vulnerability.
func (n *Notifier) stepRealtime(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	deliveries, err := n.pg.ClaimRealtimeDeliveries(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] Failed to claim real-time delivery channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("Channel type %q not registered", ch.Kind))
		return
	}
	for _, dl := range deliveries {
		msg, err := n.renderSingle(ctx, dl, baseURL)
		if err != nil {
			// Render failure is a local data problem; retrying will not help.
			_ = n.pg.FailDeliveries(ctx, []int64{dl.ID}, err.Error())
			continue
		}
		n.send(ctx, channel, cfg, msg, []*db.NotificationDelivery{dl})
	}
}

// stepDigest aggregates a channel's pending deliveries into one message when the batch is due.
func (n *Notifier) stepDigest(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	window := n.digestInterval()
	due, err := n.pg.DigestBatchDue(ctx, ch.ID, window)
	if err != nil {
		log.Printf("[notify] Failed to judge the summary batch channel=%d: %v", ch.ID, err)
		return
	}
	if !due {
		return
	}
	deliveries, err := n.pg.ClaimDigestBatch(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] Failed to claim the summary batch channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("Channel type %q not registered", ch.Kind))
		return
	}
	msg, included, err := n.renderBatch(ctx, deliveries, baseURL, int(window.Minutes()))
	if err != nil {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), err.Error())
		return
	}
	// Deliveries whose snapshot is broken and never made it into a message must be explicitly marked failed. Without this they would be left outside `included`--neither in the message nor in the failure list
	// Beyond included, [deliveries] go neither into the message nor the failure list -- on successful send their status will be [...]
	// The subsequent batch-marking is missed, stuck in sending forever until the lease expires and it gets re-claimed repeatedly.
	if skipped := excludeDeliveries(deliveries, included); len(skipped) > 0 {
		reason := "Event snapshot cannot be parsed; this vulnerability cannot be rendered into a message"
		if fErr := n.pg.FailDeliveries(ctx, deliveryIDs(skipped), reason); fErr != nil {
			log.Printf("[notify] Mark bad-snapshot deliveries as failed channel=%s ids=%v: %v", ch.Kind, deliveryIDs(skipped), fErr)
		}
		log.Printf("[notify] Skipping %d deliveries with unparseable snapshots channel=%d", len(skipped), ch.ID)
	}
	// Only hand off to send those that made it into the message: included[i] corresponds strictly to msg.Items[i],
	// send relies on this mapping to land "the channel report fit the first K items" onto the correct delivery rows.
	n.send(ctx, channel, cfg, msg, included)
}

// send dispatches and transitions state by result.
//
// Deliveries in the same batch (dozens in digest mode) share one send result: either delivered, or the whole batch retried.
// No per-item retry--a summary message is a single message; resending part of it would corrupt the batch semantics.
//
// The only exception is **segmentation caused by the channel length limit**: the channel report actually fit only the first K items,
// Items from K+1 onward must stay for the next batch instead of being marked successful together. Otherwise the truncated [...]
// Those vulnerabilities appear neither in the messages nor in the failure list \u2014 gone completely.
func (n *Notifier) send(ctx context.Context, channel notify.Channel, cfg map[string]any, msg notify.Message, deliveries []*db.NotificationDelivery) {
	// Cap each delivery round so one stuck channel cannot hold up all remaining channels in the round.
	sendCtx, cancel := context.WithTimeout(ctx, notifySendTimeout)
	defer cancel()
	delivered, err := channel.Send(sendCtx, cfg, msg)
	if err == nil && delivered > 0 {
		if delivered > len(deliveries) {
			// The count reported by a channel cannot exceed the delivered count; if it does, the rendering layer miscalculated,
			// Treat as fully delivered and log the problem \u2014 still better than corrupting the records.
			log.Printf("[notify] channel reported delivered count %d exceeding dispatched count %d channel=%s, treating as fully delivered",
				delivered, len(deliveries), channel.Kind())
			delivered = len(deliveries)
		}
		sent, rest := deliveries[:delivered], deliveries[delivered:]
		if err := n.pg.MarkDeliveriesSent(ctx, deliveryIDs(sent)); err != nil {
			log.Printf("[notify] failed to mark as delivered channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(sent), err)
		}
		if len(rest) > 0 {
			// This message has hit the channel length cap: the rest go straight back to the queue for the next tick to continue sending.
			// Use DeferDeliveries rather than RescheduleDeliveries -- this is not a failure,
			// Should not consume the retry budget (it was already optimistically +1 at claim time, and will be subtracted back there).
			if err := n.pg.DeferDeliveries(ctx, deliveryIDs(rest),
				fmt.Sprintf("This message has reached the channel length limit; only the first %d entries are delivered, the rest are left for the next batch", delivered)); err != nil {
				log.Printf("[notify] segmented continued-delivery enqueue failed channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(rest), err)
			}
		}
		return
	}
	if err == nil {
		// The channel neither errored nor reported how many entries were delivered. Treat as failure (with backoff),
		// So this delivery is not claimed repeatedly yet never marked done.
		err = fmt.Errorf("Channel did not report delivered count (delivered=%d)", delivered)
	}

	// Failure handling is decided **per item**, not by the max attempt count of the whole batch.
	//
	// It used to be `if maxAttempts(deliveries) >= MaxNotifyAttempts` to condemn the whole batch, but within a batch
	// Attempt counts differ per item: an old delivery already retried twice (attempts=2) drags the rest of the same batch
	// Brand-new deliveries (attempts=1) get dragged into failed together -- new vulnerabilities are permanently lost without using a single retry,
	// Exactly contrary to the original intent of not letting old rows drag new rows down.
	permanent := notify.IsPermanent(err)
	var failIDs, exhaustedIDs []int64
	byDelay := map[time.Duration][]int64{}
	for _, dl := range deliveries {
		switch {
		case permanent:
			failIDs = append(failIDs, dl.ID)
		case dl.Attempts >= db.MaxNotifyAttempts:
			exhaustedIDs = append(exhaustedIDs, dl.ID)
		default:
			delay := notifyBackoff[min(dl.Attempts, len(notifyBackoff)-1)]
			byDelay[delay] = append(byDelay[delay], dl.ID)
		}
	}

	if len(failIDs) > 0 {
		if fErr := n.pg.FailDeliveries(ctx, failIDs, err.Error()); fErr != nil {
			log.Printf("[notify] failed to mark failure status channel=%s ids=%v: %v", channel.Kind(), failIDs, fErr)
		}
	}
	if len(exhaustedIDs) > 0 {
		reason := fmt.Sprintf("Still failing after %d retries: %s", db.MaxNotifyAttempts, err)
		if fErr := n.pg.FailDeliveries(ctx, exhaustedIDs, reason); fErr != nil {
			log.Printf("[notify] failed to mark failure status channel=%s ids=%v: %v", channel.Kind(), exhaustedIDs, fErr)
		}
	}
	// Reorder grouped by delay: with only 3 backoff tiers the group count is naturally small, so no need to send once per item
	// UPDATE (that would make a 500-item batch produce 500 round trips).
	for delay, group := range byDelay {
		if rErr := n.pg.RescheduleDeliveries(ctx, group, delay, err.Error()); rErr != nil {
			log.Printf("[notify] Failed to reorder deliveries channel=%s ids=%v: %v", channel.Kind(), group, rErr)
		}
	}
	if len(failIDs)+len(exhaustedIDs) > 0 {
		log.Printf("[notify] delivery failed channel=%d kind=%s permanent failures=%d retries exhausted=%d pending retries=%d: %s",
			deliveries[0].ChannelID, channel.Kind(), len(failIDs), len(exhaustedIDs), len(byDelay), err)
	}
}

// excludeDeliveries returns those in all that are not in keep (compared by pointer identity).
// Used to find deliveries that "failed to make it into the message" -- they must be disposed of explicitly, not left in a gray zone.
func excludeDeliveries(all, keep []*db.NotificationDelivery) []*db.NotificationDelivery {
	inKeep := make(map[*db.NotificationDelivery]bool, len(keep))
	for _, dl := range keep {
		inKeep[dl] = true
	}
	var out []*db.NotificationDelivery
	for _, dl := range all {
		if !inKeep[dl] {
			out = append(out, dl)
		}
	}
	return out
}

// adapt fetches the channel implementation and parses its config.
// Returning ok=false means the type is unregistered; the delivery should be marked failed directly instead of retried forever.
func (n *Notifier) adapt(ch *db.NotificationChannel) (notify.Channel, map[string]any, bool) {
	channel, ok := notify.Get(ch.Kind)
	if !ok {
		return nil, nil, false
	}
	var cfg map[string]any
	if len(ch.Config) > 0 {
		// On config-parse failure, hand back an empty map: the channel's own Validate will report "which field is missing",
		// That error guides users to a fix better than a JSON parse error does.
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return channel, cfg, true
}

// renderSingle renders a single vulnerability message.
func (n *Notifier) renderSingle(ctx context.Context, dl *db.NotificationDelivery, baseURL string) (notify.Message, error) {
	snap, err := parseSnapshot(dl)
	if err != nil {
		return notify.Message{}, err
	}
	item, err := n.itemFor(ctx, snap, baseURL)
	if err != nil {
		return notify.Message{}, err
	}
	return notify.Message{Items: []notify.Item{item}, HomeURL: baseURL}, nil
}

// renderBatch renders the digest message. Snapshots are parsed one by one -- a broken one only skips that item,
// Don't let it drag down the whole digest batch.
//
// The return value included corresponds **strictly one-to-one** with msg.Items (the i-th delivery <-> the i-th entry).
// This mapping is a hard requirement: the caller determines the first K deliveries by "the channel reported fitting the first K"
// mark as delivered. If a bad snapshot is skipped here without removing the skipped delivery from included,
// The indices would misalign -- bad entries that should fail would be marked delivered, while good entries would be misjudged as undelivered.
// The broken ones are explicitly marked failed by the caller, see stepDigest.
func (n *Notifier) renderBatch(ctx context.Context, deliveries []*db.NotificationDelivery, baseURL string, windowMinutes int) (notify.Message, []*db.NotificationDelivery, error) {
	items := make([]notify.Item, 0, len(deliveries))
	included := make([]*db.NotificationDelivery, 0, len(deliveries))
	for _, dl := range deliveries {
		snap, err := parseSnapshot(dl)
		if err != nil {
			// Bad snapshots enter neither the message nor included--its handling is the caller's responsibility
			// (Explicitly marked as failed instead of slipping through mixed in with "delivered").
			log.Printf("[notify] skipping unparseable snapshot in digest batch delivery=%d: %v", dl.ID, err)
			continue
		}
		item, err := n.itemFor(ctx, snap, baseURL)
		if err != nil {
			return notify.Message{}, nil, err
		}
		items = append(items, item)
		included = append(included, dl)
	}
	if len(items) == 0 {
		return notify.Message{}, nil, fmt.Errorf("All %d deliveries in the digest batch failed to parse", len(deliveries))
	}
	return notify.Message{
		Items:         items,
		Batch:         true,
		WindowMinutes: windowMinutes,
		HomeURL:       baseURL,
	}, included, nil
}

// itemFor renders an event snapshot into a push-ready entry, also resolving the asset name and detail backlink.
func (n *Notifier) itemFor(ctx context.Context, snap notify.Snapshot, baseURL string) (notify.Item, error) {
	assets, err := n.pg.NotificationAssetNames(ctx, snap.AssetIDs)
	if err != nil {
		// Asset-name resolution failure should not block pushes: missing a name is far less serious than missing the notification,
		// Just one asset line missing from the message.
		log.Printf("[notify] Failed to parse asset name finding=%d: %v", snap.FindingID, err)
	}
	item := notify.Item{
		FindingID:  snap.FindingID,
		Name:       snap.Name,
		VulnClass:  snap.VulnClass,
		Severity:   snap.Severity,
		Summary:    snap.Summary,
		Assets:     assets,
		FromStatus: snap.FromStatus,
		ToStatus:   snap.ToStatus,
	}
	if baseURL != "" {
		// For the detail-page route see web/src/app/(main)/function/findings/detail/page.tsx,
		// It reads the vulnerability id from query param id.
		item.DetailURL = fmt.Sprintf("%s/function/findings/detail?id=%d", baseURL, snap.FindingID)
	}
	return item, nil
}

// takeTokens takes **at most want** tokens from the channel token bucket, returning the number actually taken.
//
// One token = one message (one HTTP request). In real-time mode the caller passes as many as it needs;
// In digest mode, a whole batch of vulnerabilities sends only one message; pass 1.
//
// Bucket capacity is the per-minute cap for the channel, refilled at a constant rate. ratePerMin<=0 means no rate limiting,
// Return a finite but sufficiently large value to keep a single round from being stalled by unbounded backlog.
//
// This want cap is required: without it the whole bucket could only be drained at once, while the caller itself has a per-round cap,
// Excess tokens taken can neither be used nor survive until the next refill -- they vanish into thin air; the saved-up burst capacity stays forever unreachable,
// Even rounds with no pending deliveries still deduct one.
func (n *Notifier) takeTokens(channelID int64, ratePerMin, want int, now time.Time) int {
	if want <= 0 {
		return 0
	}
	if ratePerMin <= 0 {
		return min(want, notifyUnlimitedBurstPerTick)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	b := n.buckets[channelID]
	if b == nil {
		b = &notifyBucket{tokens: float64(ratePerMin), lastFill: now}
		n.buckets[channelID] = b
	}
	// Replenish by elapsed real time, at a rate of ratePerMin/60 per second.
	if elapsed := now.Sub(b.lastFill).Seconds(); elapsed > 0 {
		b.tokens = minF(float64(ratePerMin), b.tokens+elapsed*float64(ratePerMin)/60)
		b.lastFill = now
	}
	// Add a tiny epsilon before rounding: the token count accumulates in floating point, so when refilled in two passes
	// 0.5 + 0.5 can yield 0.9999999999, which a direct int() would truncate to 0 --
	// A mathematically full bucket yielding no token. 1e-9 is far smaller than one token and will not let a genuine deficit slip through.
	take := min(int(b.tokens+1e-9), want)
	if take <= 0 {
		return 0
	}
	b.tokens -= float64(take)
	return take
}

// enabled reads the master switch.
func (n *Notifier) enabled() bool {
	return n.pg.GetBool(settingNotifyEnabled, true)
}

// publicBaseURL returns the external address used for back-links, with the trailing slash removed.
func (n *Notifier) publicBaseURL() string {
	v, ok, err := n.pg.GetSetting(settingNotifyPublicBaseURL)
	if err != nil || !ok {
		return ""
	}
	return trimTrailingSlash(v)
}

// digestInterval returns the digest period, falling back to the default when invalid or unconfigured.
func (n *Notifier) digestInterval() time.Duration {
	v, ok, err := n.pg.GetSetting(settingNotifyDigestMinutes)
	if err != nil || !ok {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	m := 0
	if _, err := fmt.Sscanf(v, "%d", &m); err != nil || m <= 0 {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	return time.Duration(m) * time.Minute
}

// parseSnapshot parses the snapshot of the event corresponding to a delivery.
func parseSnapshot(dl *db.NotificationDelivery) (notify.Snapshot, error) {
	var snap notify.Snapshot
	if len(dl.Snapshot) == 0 {
		return snap, fmt.Errorf("event snapshot for delivery %d is empty", dl.ID)
	}
	if err := json.Unmarshal(dl.Snapshot, &snap); err != nil {
		return snap, fmt.Errorf("Failed to parse event snapshot of delivery %d: %w", dl.ID, err)
	}
	if snap.Kind == "" {
		// The event type follows the event behavior; the copy in the snapshot may have been written by an older version.
		snap.Kind = dl.EventKind
	}
	return snap, nil
}

func deliveryIDs(deliveries []*db.NotificationDelivery) []int64 {
	out := make([]int64, 0, len(deliveries))
	for _, dl := range deliveries {
		out = append(out, dl.ID)
	}
	return out
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
