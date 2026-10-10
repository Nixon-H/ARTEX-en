package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// This file handles claiming and status transitions of delivery tasks.
//
// Claiming uses a "lease" rather than a long transaction: rows are marked sending and next_attempt_at is pushed into the future as
// the lease expiry time; network delivery happens after the transaction commits. That way no DB lock is held during delivery --
// Network requests can take seconds (client timeout 15s); holding row locks that long would stall other writes on the same DB.
//
// The cost is that if the process crashes mid-delivery, rows stay in sending. That is **self-healing**: once the lease expires
// next_attempt_at falls into the past, the next claim round picks the same row up again (see ... in the claim condition
// state IN ('pending','sending')). The retry counter was already +1 at claim time, so a crash does not cause
// no infinite retries -- after MaxNotifyAttempts chances are used up it lands in failed awaiting manual handling.

// MaxNotifyAttempts is the max attempts per delivery (including the first).
// Defined here rather than in the delivery engine: it is the state machine's own policy; the engine merely executes.
const MaxNotifyAttempts = 3

// MaxDigestBatchSize is how many deliveries one digest batch merges at most.
//
// It exists for resource reasons: if tens of thousands of findings are swept up in one digest cycle (entirely possible -- one full scan
// ... would do); without a cap, claiming would read all rows into memory and render one oversized message,
// only to have most of it cut off by the channel's length cap -- wasting memory and **silently losing** the truncated findings.
// With the cap set, the overflow stays in the DB as the next batch and goes out naturally next cycle, nothing lost.
//
// Rationale for 500: after rendering into a message it is still a "readable-content" magnitude within WeCom's 4096-byte cap;
// A larger value only moves the truncation point further back.
const MaxDigestBatchSize = 500

// NotificationDelivery is one delivery job, carrying the channel config and event snapshot needed for rendering.
type NotificationDelivery struct {
	ID            int64           `json:"id"`
	EventID       int64           `json:"event_id"`
	ChannelID     int64           `json:"channel_id"`
	State         string          `json:"state"`
	Attempts      int             `json:"attempts"`
	NextAttemptAt time.Time       `json:"next_attempt_at"`
	LastError     string          `json:"last_error"`
	BatchID       *int64          `json:"batch_id,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	SentAt        *time.Time      `json:"sent_at,omitempty"`
	Snapshot      json.RawMessage `json:"snapshot,omitempty"`
	// Jointly-loaded rendering context, excluded from JSON (the server layer assembles the DTO).
	Channel *NotificationChannel `json:"-"`
	// FindingID/EventKind are carried over from the event, letting the history list jump straight to finding details.
	FindingID int64  `json:"finding_id,string"`
	EventKind string `json:"event_kind"`
	// ChannelName/ChannelKind are redundant fields for list display, sparing the frontend a second query.
	ChannelName string `json:"channel_name"`
	ChannelKind string `json:"channel_kind"`
}

const notificationDeliveryCols = `d.id, d.event_id, d.channel_id, d.state, d.attempts, d.next_attempt_at,
       d.last_error, d.batch_id, d.created_at, d.sent_at`

// joinedDeliveryQuery is the unified read shape for a delivery row: delivery + event snapshot + channel config.
// Rendering one message requires all three; fetching them separately would cost three round trips.
const joinedDeliveryQuery = `SELECT ` + notificationDeliveryCols + `,
       e.snapshot, e.kind, e.finding_id,
       c.id, c.name, c.kind, c.enabled, c.config, c.mode, c.filter, c.rate_per_min
FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id
JOIN notification_channels c ON c.id = d.channel_id`

func scanNotificationDelivery(sc interface{ Scan(...any) error }) (*NotificationDelivery, error) {
	var (
		dl        NotificationDelivery
		lastErr   sql.NullString
		batchID   sql.NullInt64
		sentAt    sql.NullTime
		snapshot  []byte
		eventKind string
		channel   NotificationChannel
		chEnabled bool
	)
	if err := sc.Scan(&dl.ID, &dl.EventID, &dl.ChannelID, &dl.State, &dl.Attempts, &dl.NextAttemptAt,
		&lastErr, &batchID, &dl.CreatedAt, &sentAt,
		&snapshot, &eventKind, &dl.FindingID,
		&channel.ID, &channel.Name, &channel.Kind, &chEnabled, &channel.Config, &channel.Mode, &channel.Filter, &channel.RatePerMin); err != nil {
		return nil, err
	}
	dl.LastError = lastErr.String
	if batchID.Valid {
		dl.BatchID = &batchID.Int64
	}
	if sentAt.Valid {
		dl.SentAt = &sentAt.Time
	}
	dl.Snapshot = json.RawMessage(snapshot)
	dl.EventKind = eventKind
	dl.ChannelName = channel.Name
	dl.ChannelKind = channel.Kind
	channel.Enabled = &chEnabled
	dl.Channel = &channel
	return &dl, nil
}

// claimQuery describes one claim: first select candidates per sel and lock them, then set them to sending and
// extend the lease. The lease slot in sel is filled with a $n placeholder by the caller, which passes the argument itself.
type claimQuery struct {
	sql  string
	args []any
}

// ClaimRealtimeDeliveries claims a batch of due realtime deliveries for one channel, at most limit rows.
//
// Deliberately claims per **single channel** instead of "claim a global batch then cherry-pick": the rate-limit gate in the delivery engine is maintained per channel,
// so only by first knowing how many messages this channel may still send this round and then claiming that many rows will rate limiting avoid consuming
// retry attempts. Done the other way around -- claim first, discard later -- rows blocked by rate limiting would already have consumed one attempts count,
// the 3-attempt budget would be burned purely on waiting, and the rows would finally land in failed.
//
// The condition includes "sending rows whose lease has expired" -- that is the crash self-healing path. lease must be significantly larger than a single
// delivery's worst-case latency (the channel HTTP client times out after 15 seconds); otherwise the same row would be delivered by two dispatchers
// concurrently. Disabled channels are also blocked here: the disable operation has already marked the existing deliveries as skipped,
// and this adds one more barrier so no row slips through when disabling races with claiming.
func (d *DB) ClaimRealtimeDeliveries(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	return d.claimDeliveries(ctx, lease, claimQuery{
		sql: `SELECT dd.id FROM notification_deliveries dd
JOIN notification_channels c ON c.id = dd.channel_id
WHERE dd.channel_id = $1 AND dd.state IN ($2,$3) AND dd.next_attempt_at <= now()
  AND c.enabled AND c.mode = $4
ORDER BY dd.next_attempt_at, dd.id
FOR UPDATE OF dd SKIP LOCKED
LIMIT $5`,
		args: []any{channelID, NotifyStatePending, NotifyStateSending, NotifyModeRealtime, limit},
	}, nil)
}

// DigestBatchDue reports whether this channel has accumulated one due batch: pending deliveries exist, and **the oldest one**
// is already as old as the digest interval.
//
// The criterion is the oldest delivery's age, not the wall clock: that way a newly created channel won't, merely by aligning to the hour,
// immediately emit a "digest" containing a single entry, nor will a long-backlogged batch wait another round in vain.
//
// It is separate from ClaimDigestBatch because the semantics differ: this function only answers "should it be sent",
// while claiming takes **all** of the channel's pending rows (including those not yet old enough) -- otherwise one interval
// would be split into multiple messages and the digest would lose its meaning.
func (d *DB) DigestBatchDue(ctx context.Context, channelID int64, minAge time.Duration) (bool, error) {
	var due bool
	err := d.QueryRowContext(ctx, `SELECT EXISTS (
  SELECT 1 FROM notification_deliveries d
  JOIN notification_channels c ON c.id = d.channel_id
  WHERE d.channel_id = $1 AND d.state IN ($2,$3) AND c.enabled
  GROUP BY d.channel_id
  HAVING min(d.created_at) <= now() - make_interval(secs => $4)
)`, channelID, NotifyStatePending, NotifyStateSending, int64(minAge.Seconds())).Scan(&due)
	return due, err
}

// ClaimDigestBatch claims a channel's currently due pending deliveries as one digest batch,
// at most MaxDigestBatchSize rows per batch.
//
// All deliveries in the same batch share a batch_id, using the smallest id in the set as the batch number (stable, readable,
// no extra sequence needed). Retries use COALESCE to keep the original batch number, so that "these N rows were sent together"
// still holds after multiple retries.
//
// Takes the first N rows in ascending id order rather than at random: the earliest-produced deliveries go out first, so a backlog never sees
// starvation where "new vulnerabilities go out first and old vulnerabilities queue at the back forever".
func (d *DB) ClaimDigestBatch(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	// limit is a **memory upper bound**; callers pass MaxDigestBatchSize, with another clamp applied here
	// to guard against a caller passing in a larger value.
	//
	// Deliberately does not accept the "rate-limit quota" as the batch size: rate limiting is counted in messages -- one batch sends only
	// one message and consumes one token, deducted by the server layer's takeTokens -- which is a different dimension from "how many
	// vulnerabilities fit in one batch". Once, to make rate_per_min take effect on digests, the per-round
	// request budget was passed in as the batch size; as a result, a channel with rate=20/min packed only 1 vulnerability per batch,
	// and digests degenerated into realtime pushes with digest copy. To change rate limiting, change takeTokens' want;
	// do not touch this.
	if limit > MaxDigestBatchSize {
		limit = MaxDigestBatchSize
	}
	out, err := d.claimDeliveries(ctx, lease, claimQuery{
		sql: `SELECT dd.id FROM notification_deliveries dd
JOIN notification_channels c ON c.id = dd.channel_id
WHERE dd.channel_id = $1 AND dd.state IN ($2,$3) AND dd.next_attempt_at <= now() AND c.enabled
ORDER BY dd.id
FOR UPDATE OF dd SKIP LOCKED
LIMIT $4`,
		args: []any{channelID, NotifyStatePending, NotifyStateSending, limit},
	}, func(tx *sql.Tx, ids []int64) error {
		batchID := ids[0]
		for _, id := range ids {
			if id < batchID {
				batchID = id
			}
		}
		ph, idArgs := placeholders(2, ids)
		_, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET batch_id = COALESCE(batch_id, $1)
WHERE id IN (`+ph+`)`, append([]any{batchID}, idArgs...)...)
		return err
	})
	return out, err
}

// claimDeliveries executes "select + set sending and extend lease + read full rows", all inside one transaction.
// postClaim is an optional extra step (digest batches use it to write batch_id).
func (d *DB) claimDeliveries(ctx context.Context, lease time.Duration, cq claimQuery, postClaim func(*sql.Tx, []int64) error) ([]*NotificationDelivery, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful commit

	ids, err := selectForClaim(ctx, tx, cq.sql, cq.args...)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, tx.Commit()
	}
	// Sets sending and pushes next_attempt_at into the future: that future moment is the lease expiry time,
	// so "lease not yet expired" and "retry time not yet reached" share the same condition expression -- no new column needed.
	ph, idArgs := placeholders(3, ids)
	if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, attempts=attempts+1, next_attempt_at=now()+make_interval(secs => $2)
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateSending, lease.Seconds()}, idArgs...)...); err != nil {
		return nil, err
	}
	if postClaim != nil {
		if err := postClaim(tx, ids); err != nil {
			return nil, err
		}
	}
	out, err := loadDeliveriesTx(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func selectForClaim(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func loadDeliveriesTx(ctx context.Context, tx *sql.Tx, ids []int64) ([]*NotificationDelivery, error) {
	ph, args := placeholders(1, ids)
	rows, err := tx.QueryContext(ctx, joinedDeliveryQuery+` WHERE d.id IN (`+ph+`) ORDER BY d.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationDelivery{}
	for rows.Next() {
		dl, err := scanNotificationDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dl)
	}
	return out, rows.Err()
}

// MarkDeliveriesSent marks a batch of deliveries as delivered.
func (d *DB) MarkDeliveriesSent(ctx context.Context, ids []int64) error {
	ph, args := placeholders(2, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, sent_at=now(), last_error='' WHERE id IN (`+ph+`)`, append([]any{NotifyStateSent}, args...)...)
	return err
}

// RescheduleDeliveries returns a batch of deliveries to pending and postpones the retry time.
//
// Returning to pending instead of introducing a new intermediate state keeps "how many chances remain" expressed in exactly one place
// (MaxNotifyAttempts), so the state machine's branches don't swell with the retry policy.
func (d *DB) RescheduleDeliveries(ctx context.Context, ids []int64, delay time.Duration, errMsg string) error {
	ph, args := placeholders(4, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, next_attempt_at=now()+make_interval(secs => $2), last_error=$3
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStatePending, delay.Seconds(), truncateNotifyError(errMsg)}, args...)...)
	return err
}

// DeferDeliveries returns a batch of deliveries to pending, immediately claimable again, and **undoes the attempt counted at claim time**.
//
// It has exactly one use: when a digest message is split for sending by the channel's length cap, entries that didn't fit in this message stay for the next batch.
// That is not a failure, so it must not consume retry budget -- attempts was already optimistically incremented by 1 at claim time,
// so it must be decremented back here. Otherwise a 500-row backlog split into 25 segments of 20 rows each
// would see its tail entries judged failed by MaxNotifyAttempts by the 3rd segment, though they never errored at all.
//
// GREATEST(...,0) covers the case where "someone manually re-sent, zeroing attempts, and execution reaches here again",
// keeping the count from going negative.
func (d *DB) DeferDeliveries(ctx context.Context, ids []int64, reason string) error {
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, attempts=GREATEST(attempts-1, 0), next_attempt_at=now(), last_error=$2
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStatePending, truncateNotifyError(reason)}, args...)...)
	return err
}

// FailDeliveries marks a batch of deliveries as finally failed, awaiting a manual re-send from the delivery history.
func (d *DB) FailDeliveries(ctx context.Context, ids []int64, errMsg string) error {
	// Placeholders start at $3: $1 is state, $2 is last_error.
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries SET state=$1, last_error=$2 WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateFailed, truncateNotifyError(errMsg)}, args...)...)
	return err
}

// RetryNotificationDelivery manually re-sends one delivery: resets it to pending, zeroes the retry count,
// effective immediately. Zeroing the count is deliberate -- a human clicking "re-send" means the cause of the earlier failures has been handled,
// so limiting it by the old count makes no sense.
func (d *DB) RetryNotificationDelivery(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$2, attempts=0, next_attempt_at=now(), last_error=''
WHERE id=$1 AND state IN ($3,$4)`, id, NotifyStatePending, NotifyStateFailed, NotifyStateSkipped)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("delivery %d does not exist or its current state does not allow resend", id)
	}
	return nil
}

// NotificationDeliveryFilter is the query filter for delivery history.
type NotificationDeliveryFilter struct {
	ChannelID int64
	State     string
	EventKind string
}

func (f NotificationDeliveryFilter) where() (string, []any) {
	var conds []string
	var args []any
	if f.ChannelID > 0 {
		args = append(args, f.ChannelID)
		conds = append(conds, fmt.Sprintf("d.channel_id=$%d", len(args)))
	}
	if f.State != "" {
		args = append(args, f.State)
		conds = append(conds, fmt.Sprintf("d.state=$%d", len(args)))
	}
	if f.EventKind != "" {
		args = append(args, f.EventKind)
		conds = append(conds, fmt.Sprintf("e.kind=$%d", len(args)))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// ListNotificationDeliveries returns delivery history paginated, newest first.
func (d *DB) ListNotificationDeliveries(ctx context.Context, f NotificationDeliveryFilter, page, pageSize int) ([]*NotificationDelivery, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 50
	}
	where, args := f.where()

	var total int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := fmt.Sprintf("%s%s ORDER BY d.id DESC LIMIT $%d OFFSET $%d",
		joinedDeliveryQuery, where, len(args)+1, len(args)+2)
	rows, err := d.QueryContext(ctx, q, append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*NotificationDelivery{}
	for rows.Next() {
		dl, err := scanNotificationDelivery(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, dl)
	}
	return out, total, rows.Err()
}

// truncateNotifyError truncates an error message to a length the column accepts. A channel's response body can be very long
// (especially when a generic webhook hits a self-hosted service); without truncation the history list payload bloats.
func truncateNotifyError(msg string) string {
	const max = 500
	if len(msg) <= max {
		return msg
	}
	// Back off along character boundaries, to avoid leaving half a UTF-8 character that the frontend renders as mojibake.
	cut := max
	for cut > 0 && !isUTF8Start(msg[cut]) {
		cut--
	}
	return msg[:cut] + "..."
}

func isUTF8Start(b byte) bool { return b&0xC0 != 0x80 }

// placeholders generates $n placeholder strings starting from start plus the corresponding args, for IN (...) use.
// E.g. start=3, ids=[7,8] -> "$3,$4", [7,8].
func placeholders(start int, ids []int64) (string, []any) {
	ph := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		ph = append(ph, fmt.Sprintf("$%d", start+i))
		args = append(args, id)
	}
	return strings.Join(ph, ","), args
}
