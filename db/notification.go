package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Autumn-27/artex/notify"
)

// This file is the channel-config and event layer of IM push. For delivery claiming and state transitions, see
// db/notification_delivery.go.
//
// Two invariants; keep them when modifying this file:
//
//  1. The vulnerability-writing transaction (RecordFindingTx) only calls InsertNotificationEventTx for a single blind insert,
//     reads no notification-related tables and does no filter matching. Any read introduced here could, because
// Misconfigured filter conditions by the user contaminating or even aborting the vulnerability write transaction.
//  2. Filter matching never errors: malformed configs are uniformly treated as "matched" (see notify.Match). Better to over-push,
// Must not miss any push notification.

// ErrNotificationChannelNotFound: channel does not exist.
var ErrNotificationChannelNotFound = errors.New("Notification channel does not exist")

// Delivery status.
const (
	NotifyStatePending = "pending" // pending
	NotifyStateSending = "sending" // Already claimed by some dispatcher, lease not yet expired
	NotifyStateSent    = "sent"    // Delivered
	NotifyStateFailed  = "failed"  // After retries are exhausted or on permanent failure, can be resent manually
	NotifyStateSkipped = "skipped" // Channel is disabled; no longer sending
)

// Push mode.
const (
	NotifyModeRealtime = "realtime"
	NotifyModeDigest   = "digest"
)

// ValidNotifyMode allowlist-validates the push mode (same rationale as findings.status: no DB CHECK,
// (for future extensibility).
func ValidNotifyMode(m string) bool {
	return m == NotifyModeRealtime || m == NotifyModeDigest
}

// NotificationChannel is a channel instance config. Config and Filter keep their raw JSON,
// Parsing is delegated to the notify package -- the db layer does not understand these fields' meanings.
type NotificationChannel struct {
	ID     int64           `json:"id"`
	Name   string          `json:"name"`
	Kind   string          `json:"kind"`
	Mode   string          `json:"mode"`
	Config json.RawMessage `json:"config"`
	Filter json.RawMessage `json:"filter"`
	// Enabled is a pointer to distinguish "field not passed" from "explicit false" --
	// The frontend toggle control only submits modified fields.
	Enabled    *bool     `json:"enabled,omitempty"`
	RatePerMin int       `json:"rate_per_min"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// IsEnabled returns whether the channel is enabled; a nil Enabled (not loaded) is treated as enabled.
func (c *NotificationChannel) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// NotificationEvent is one event fact.
type NotificationEvent struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	FindingID int64           `json:"finding_id"`
	Snapshot  json.RawMessage `json:"snapshot"`
	CreatedAt time.Time       `json:"created_at"`
}

const notificationChannelCols = `id, name, kind, enabled, config, mode, filter, rate_per_min, created_at, updated_at`

func scanNotificationChannel(sc interface{ Scan(...any) error }) (*NotificationChannel, error) {
	var c NotificationChannel
	var enabled bool
	if err := sc.Scan(&c.ID, &c.Name, &c.Kind, &enabled, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	c.Enabled = &enabled
	return &c, nil
}

// ListNotificationChannels returns all channel instances, enabled ones first, ties ordered by id.
// Sorting lives in SQL so the UI and the dispatcher see the same stable order.
func (d *DB) ListNotificationChannels(ctx context.Context) ([]*NotificationChannel, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels
ORDER BY enabled DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		c, err := scanNotificationChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// NotificationChannelByID fetches a single channel.
func (d *DB) NotificationChannelByID(ctx context.Context, id int64) (*NotificationChannel, error) {
	row := d.QueryRowContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels WHERE id=$1`, id)
	c, err := scanNotificationChannel(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotificationChannelNotFound
	}
	return c, err
}

// SaveNotificationChannel creates or updates a channel.
//
// On update, only fields explicitly given by the caller are overwritten (non-nil / non-empty), so the frontend can submit a partial
// edit drawer form without sending back the config fields it never displayed -- sending them back would instead cause
// the accident of "masked values overwriting the real secret".
func (d *DB) SaveNotificationChannel(ctx context.Context, c *NotificationChannel) (int64, error) {
	if c.Mode == "" {
		c.Mode = NotifyModeRealtime
	}
	// Here 0 is deliberately left untouched: 0 is a legal config meaning "unlimited".
	//
	// It used to read `if c.RatePerMin <= 0 { c.RatePerMin = <default> }`, intending "when unspecified,
	// ... give it a safe default", but that also swallows "explicitly set to 0" -- docs, UI hints, and
	// takeTokens interprets 0 as unlimited everywhere, except here it was quietly rewritten to 20 (DingTalk/WeCom/Telegram)
	// or 100 (Feishu), while the operator thinks rate limiting was lifted but is actually throttled at 20/min with no hint at all.
	//
	// Only the caller knows "unspecified" from "explicit 0" (absent field in the request body vs an explicit 0),
	// So the default is filled in by the server layer when the field is absent, see notifyCreateChannel.
	if c.RatePerMin < 0 {
		return 0, errors.New("Rate limit cannot be negative")
	}
	if c.Config == nil {
		c.Config = json.RawMessage(`{}`)
	}
	if c.Filter == nil {
		c.Filter = json.RawMessage(`{}`)
	}
	enabled := c.IsEnabled()

	if c.ID == 0 {
		var id int64
		err := d.QueryRowContext(ctx, `INSERT INTO notification_channels(name,kind,enabled,config,mode,filter,rate_per_min)
VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
			c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin).Scan(&id)
		return id, err
	}
	res, err := d.ExecContext(ctx, `UPDATE notification_channels
SET name=$2, kind=$3, enabled=$4, config=$5, mode=$6, filter=$7, rate_per_min=$8
WHERE id=$1`,
		c.ID, c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotificationChannelNotFound
	}
	return c.ID, nil
}

// SetNotificationChannelEnabled toggles enable/disable.
//
// When disabling a channel, also mark its unsent deliveries skipped: otherwise after re-enabling
// ... would suddenly receive a batch of stale findings "backlogged while disabled", long past their freshness and easily mistaken for new ones.
func (d *DB) SetNotificationChannelEnabled(ctx context.Context, id int64, enabled bool) error {
	return d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE notification_channels SET enabled=$2 WHERE id=$1`, id, enabled)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotificationChannelNotFound
		}
		if !enabled {
			if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET state=$2, last_error=$3
WHERE channel_id=$1 AND state IN ($4,$5)`,
				id, NotifyStateSkipped, "Channel disabled", NotifyStatePending, NotifyStateSending); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteNotificationChannel deletes a channel. Its delivery history is cascade-deleted via foreign key
// (Channel configs are all gone, history cannot be interpreted.)
func (d *DB) DeleteNotificationChannel(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `DELETE FROM notification_channels WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotificationChannelNotFound
	}
	return nil
}

// RecordNotificationEventTx writes one push event on a **best-effort** basis inside the caller's transaction.
//
// This is the only notification-related change on the finding-write path: a single INSERT that reads no tables and knows no channels,
// No filtering runs. Commit guarantees "finding persisted" and "push job exists" atomically consistent,
// There is no window where a commit succeeds but the message is never enqueued and lost forever.
//
// Two key design decisions, neither written casually:
//
//  1. **Why SAVEPOINT**: in PostgreSQL, any statement error inside a transaction puts the whole transaction into
//     aborted state, after which all statements (including COMMIT) fail. So "ignore this INSERT
//     ... an error and let the caller keep committing" is impossible in PG -- unless a savepoint isolates the error within
//     ... on this one statement. Without a savepoint, the only option left is "roll back the whole transaction".
//
//  2. **Why rolling back the whole transaction is wrong**: push is a convenience feature; the finding record is the product itself. A notification
//     ... table problems (unmigrated old DB, transient disk failure) must not block a critical finding from being stored. So it is isolated here
//     error, log it, and return false, letting the finding write commit as usual -- at the cost of dropping this one push.
//     Returning bool rather than error is deliberate: callers should not treat it as an error affecting write success.
func RecordNotificationEventTx(ctx context.Context, tx *sql.Tx, kind string, findingID int64, snap notify.Snapshot) bool {
	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[notify] failed to serialize push event finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT notify_event`); err != nil {
		log.Printf("[notify] failed to establish savepoint finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3)`,
		kind, findingID, string(raw)); err != nil {
		log.Printf("[notify] failed to write push event finding=%d (finding record unaffected): %v", findingID, err)
		// Roll back to the savepoint to rescue the transaction from the aborted state.
		if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT notify_event`); rbErr != nil {
			log.Printf("[notify] failed to roll back to savepoint finding=%d: %v", findingID, rbErr)
		}
		return false
	}
	// Release the savepoint to avoid accumulating useless savepoints in a long transaction.
	_, _ = tx.ExecContext(ctx, `RELEASE SAVEPOINT notify_event`)
	return true
}

// AddNotificationEvent is the standalone-transaction version of InsertNotificationEventTx, for callers not
// ... call sites inside an existing transaction (e.g. a channel's "send test message", which has no real finding).
func (d *DB) AddNotificationEvent(ctx context.Context, kind string, findingID int64, snap notify.Snapshot) (int64, error) {
	raw, err := json.Marshal(snap)
	if err != nil {
		return 0, fmt.Errorf("Failed to serialize notification event snapshot: %w", err)
	}
	var id int64
	err = d.QueryRowContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3) RETURNING id`,
		kind, findingID, string(raw)).Scan(&id)
	return id, err
}

// FanOutPendingEvents expands undispatched finding events into delivery jobs per currently enabled channels,
// Return the number of events processed in this round and the number of newly created deliveries.
//
// The whole round runs in one transaction: events are claimed with FOR UPDATE SKIP LOCKED, so multiple processes can run concurrently
// ... each claim different rows (the archive queue in this project claims with the same technique, see
// db/task_archives.go's completeNextArchiveJob).
//
// Filter matching is deliberately done on the Go side rather than in SQL: a channel's filter is a JSONB of optional fields,
// Expressing the six-combination matching in SQL would make queries unmaintainable, while the channel count is "a handful configured by hand",
// Loading everything then comparing item by item in memory is faster and easier to test.
//
// Events matching no channel are likewise marked fanned_out -- otherwise they would stay in the to-dispatch set forever,
// Rescanned on every tick.
func (d *DB) FanOutPendingEvents(ctx context.Context, limit int) (eventCount, deliveryCount int, err error) {
	if limit <= 0 {
		limit = 200
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful commit

	channels, err := listEnabledNotificationChannelsTx(ctx, tx)
	if err != nil {
		return 0, 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, kind, finding_id, snapshot FROM notification_events
WHERE NOT fanned_out ORDER BY id FOR UPDATE SKIP LOCKED LIMIT $1`, limit)
	if err != nil {
		return 0, 0, err
	}
	var (
		events      []NotificationEvent
		parsedSnaps []notify.Snapshot
	)
	for rows.Next() {
		var ev NotificationEvent
		if err := rows.Scan(&ev.ID, &ev.Kind, &ev.FindingID, &ev.Snapshot); err != nil {
			rows.Close()
			return 0, 0, err
		}
		var snap notify.Snapshot
		// Snapshots are written by ourselves, so theoretically always parseable; a parse failure does not block the delivery flow,
		// But this event, with all fields empty, would be skipped by every channel having filter conditions -- better to push one fewer
		// Also prevents one bad row from blocking the whole queue.
		_ = json.Unmarshal(ev.Snapshot, &snap)
		// kind follows the inline row value: the copy in the snapshot is for rendering and may have been written by an older version.
		snap.Kind = ev.Kind
		events = append(events, ev)
		parsedSnaps = append(parsedSnaps, snap)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if len(events) == 0 {
		return 0, 0, tx.Commit()
	}

	type pending struct {
		eventID   int64
		channelID int64
	}
	var toInsert []pending
	for i, snap := range parsedSnaps {
		for _, ch := range channels {
			if !notify.Match(notify.ParseFilter(ch.Filter), snap) {
				continue
			}
			toInsert = append(toInsert, pending{eventID: events[i].ID, channelID: ch.ID})
		}
	}
	if len(toInsert) > 0 {
		var (
			vals []string
			args []any
		)
		for _, p := range toInsert {
			vals = append(vals, fmt.Sprintf("($%d,$%d)", len(args)+1, len(args)+2))
			args = append(args, p.eventID, p.channelID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notification_deliveries(event_id,channel_id) VALUES `+strings.Join(vals, ","), args...); err != nil {
			return 0, 0, err
		}
	}

	// Mark this round's events dispatched. Events matching no channel are marked too (see the function notes).
	ids := make([]string, 0, len(events))
	markArgs := make([]any, 0, len(events))
	for _, ev := range events {
		markArgs = append(markArgs, ev.ID)
		ids = append(ids, fmt.Sprintf("$%d", len(markArgs)))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notification_events SET fanned_out=true WHERE id IN (`+strings.Join(ids, ",")+`)`, markArgs...); err != nil {
		return 0, 0, err
	}
	return len(events), len(toInsert), tx.Commit()
}

// listEnabledNotificationChannelsTx fetches enabled channels inside a transaction. The count is small,
// No pagination and no cache -- caching would introduce the extra timing question of "when do config changes take effect".
func listEnabledNotificationChannelsTx(ctx context.Context, tx *sql.Tx) ([]*NotificationChannel, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, name, kind, config, mode, filter, rate_per_min
FROM notification_channels WHERE enabled ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		var c NotificationChannel
		if err := rows.Scan(&c.ID, &c.Name, &c.Kind, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// NotificationAssetNames resolves asset ids into short display names for use in push messages.
//
// Return order matches input order; length may be shorter than input (nonexistent ids are skipped). Keeping input order is
// so that the asset order in messages for the same vulnerability stays stable across deliveries -- otherwise in messages received after a retry
// a changed asset order would be misread as "the assets changed".
func (d *DB) NotificationAssetNames(ctx context.Context, ids []int64) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph, args := placeholders(1, ids)
	rows, err := d.QueryContext(ctx, `SELECT id, type, domain, ip, url, app_name, bundle_id FROM assets WHERE id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	labels := map[int64]string{}
	for rows.Next() {
		var (
			id                int64
			typ               string
			domain, ip, url   sql.NullString
			appName, bundleID sql.NullString
		)
		if err := rows.Scan(&id, &typ, &domain, &ip, &url, &appName, &bundleID); err != nil {
			return nil, err
		}
		labels[id] = assetDisplayName(typ, domain.String, ip.String, url.String, appName.String, bundleID.String)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ids))
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if label, ok := labels[id]; ok && label != "" {
			out = append(out, label)
		}
	}
	return out, nil
}

// assetDisplayName picks the most distinctive identifier per asset type.
// Falls back to an empty string; the caller decides how to render an "asset whose name is unavailable" -- this function invents no placeholder,
// Otherwise noise like "asset #42" would slip into push messages, and readers would mistake it for a real domain.
func assetDisplayName(typ, domain, ip, url, appName, bundleID string) string {
	pick := func(vals ...string) string {
		for _, v := range vals {
			if strings.TrimSpace(v) != "" {
				return v
			}
		}
		return ""
	}
	switch typ {
	case "root_domain", "subdomain":
		return domain
	case "ip":
		return ip
	case "app":
		return pick(appName, bundleID)
	case "service", "endpoint":
		return pick(url, domain, ip)
	default:
		return pick(domain, ip, url, appName)
	}
}

// SetFindingStatusWithNotify updates the vulnerability remediation status and registers a status-change
// push event in the same transaction.
//
// Returns from = pre-change status; found = whether the vulnerability exists; notified = whether the event was registered successfully.
//
// Three deliberate behaviors:
//   - No event is registered when the status did not actually change. The frontend drawer resubmitting the same value, or automated scripts
//     with idempotent replays, must not produce push noise.
//   - When the vulnerability does not exist, return found=false with no writes; the caller translates it into a 404.
//   - Event registration failure does not affect the status update (see the savepoint notes on RecordNotificationEventTx),
//     so when notified=false the status has already been changed successfully, and the caller must not raise an error because of it.
func (d *DB) SetFindingStatusWithNotify(ctx context.Context, id int64, status string) (from string, found bool, notified bool, err error) {
	err = d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		var txErr error
		from, found, _, notified, txErr = SetFindingStatusTx(ctx, tx, id, status)
		return txErr
	})
	return from, found, notified, err
}

// SetFindingStatusTx updates the vulnerability status and registers a status-change push event **inside the caller's transaction**.
//
// Extracted as a transaction-level function so all status-changing paths share one semantics -- previously only
// patchFinding used the notifying version, while **when a retest concludes "fixed"** (finding_retests
// the `UPDATE findings SET status=...` there) wrote straight to the DB, so channels configured with
// `on_status_change` never got pushes for such transitions: the status quietly changed in the UI,
// and ops would only notice when opening the platform.
//
// Returns from = pre-change status, found = whether the vulnerability exists, changed = whether the status really changed,
// notified = whether the event was registered successfully (registration failure does not affect the status update, see RecordNotificationEventTx).
func SetFindingStatusTx(ctx context.Context, tx *sql.Tx, id int64, status string) (from string, found bool, changed bool, notified bool, err error) {
	var (
		vulnclass, name, severity, summary string
		taskID                             sql.NullInt64
		assetIDs                           []byte
	)
	scanErr := tx.QueryRowContext(ctx, `SELECT vulnclass, name, severity, summary, task_id, asset_ids, status
FROM findings WHERE id=$1 FOR UPDATE`, id).
		Scan(&vulnclass, &name, &severity, &summary, &taskID, &assetIDs, &from)
	if scanErr == sql.ErrNoRows {
		return "", false, false, false, nil
	}
	if scanErr != nil {
		return "", false, false, false, scanErr
	}
	found = true
	if from == status {
		// No event is recorded when the status did not really change: resubmitting the same value or idempotent replays should not
		// Generates push noise.
		return from, true, false, false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE findings SET status=$2 WHERE id=$1`, id, status); err != nil {
		return from, true, false, false, err
	}
	var assets []int64
	_ = json.Unmarshal(assetIDs, &assets)
	notified = RecordNotificationEventTx(ctx, tx, notify.EventFindingStatusChanged, id, notify.Snapshot{
		Kind:       notify.EventFindingStatusChanged,
		FindingID:  id,
		TaskID:     taskID.Int64,
		VulnClass:  vulnclass,
		Name:       name,
		Severity:   severity,
		Summary:    summary,
		AssetIDs:   assets,
		FromStatus: from,
		ToStatus:   status,
	})
	return from, true, true, notified, nil
}

// NotificationStats are the overview counters at the top of the notifications page.
type NotificationStats struct {
	Channels     int   `json:"channels"`
	ChannelsOn   int   `json:"channels_on"`
	Pending      int   `json:"pending"`
	Failed       int   `json:"failed"`
	SentToday    int   `json:"sent_today"`
	BacklogAgeMS int64 `json:"backlog_age_ms"` // Age in milliseconds of the oldest pending delivery
}

// NotificationStatsSnapshot summarizes the notification system's health.
// BacklogAgeMS is the most direct indicator of "whether push is stuck" -- far more useful than the pending count,
// Because the difference between a backlog of 3 and a backlog of 3 can range from 3 seconds to 3 hours.
func (d *DB) NotificationStatsSnapshot(ctx context.Context) (*NotificationStats, error) {
	var s NotificationStats
	if err := d.QueryRowContext(ctx, `SELECT
    (SELECT count(*) FROM notification_channels),
    (SELECT count(*) FROM notification_channels WHERE enabled),
    (SELECT count(*) FROM notification_deliveries WHERE state IN ($1,$2)),
    (SELECT count(*) FROM notification_deliveries WHERE state=$3),
    (SELECT count(*) FROM notification_deliveries WHERE state=$4 AND sent_at >= date_trunc('day', now())),
    COALESCE((SELECT EXTRACT(EPOCH FROM (now() - min(created_at))) * 1000 FROM notification_deliveries WHERE state=$1), 0)::bigint`,
		NotifyStatePending, NotifyStateSending, NotifyStateFailed, NotifyStateSent).
		Scan(&s.Channels, &s.ChannelsOn, &s.Pending, &s.Failed, &s.SentToday, &s.BacklogAgeMS); err != nil {
		return nil, err
	}
	return &s, nil
}
