# Changelog

All notable changes to this project are documented in this file, following the format of [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/).

## [Unreleased]

## [0.3.15] - 2026-10-07

### Interception

#### New Features

- **New built-in interception rule "delete-type API paths"**: the previously built-in HTTP destructive rules only matched the DELETE **method**, while most apps' delete endpoints can be triggered with GET/POST, so calls like `curl 'http://t/api/user/delete?id=1'` would actually delete target data. A new `deny` rule now covers `/delete /del /remove /unlink /erase /destroy` (the verb must be followed by a separator, so `/delivery`, `/details`, `/delta` are not falsely blocked). Existing instances get it after upgrading, and it can be disabled or deleted under "System -> Command Interception".
- **Token usage of model-fallback approval is now viewable separately**: approval calls previously had no independent metering, so how much they cost was guesswork. They are now metered separately, with a new usage card below the "Model Fallback Approval" toggle under "System -> Command Interception" -- cumulative counts of calls, input, output, cache reads and cache writes, plus a mini bar chart of daily usage over the last 30 days.

### Vulnerability Push

#### New Features

- **Vulnerability findings can now be pushed to IM**: a new "System -> Notification Push" page can push scan findings to six channel types: **DingTalk, Feishu, WeCom, generic Webhook, Telegram, and email**. Any number of instances can be configured per type, each with its own enable/disable, rate-limit, and filter rules.
- **Delivery timing comes in two modes: real-time and digest**: real-time mode sends each hit immediately; digest mode merges findings into one message on a global cadence (default 30 minutes), starting with the count of new findings and their severity distribution. To get "critical in real time, everything else digested", create two channels and configure them separately.
- **Filter rules support four dimensions**: minimum severity, task/asset scope restriction, vulnerability-type keyword inclusion and exclusion (exclusion wins), and whether to receive remediation-status changes (off by default). Messages carry a "View Details" button whose target address comes from the global "callback address" setting; left blank, no button is attached.
- **Delivery history and manual resend**: lists each delivery's status, attempt count, failure reason, and owning channel, filterable by channel and status; failed items can be resent with one click (resending resets the retry counter).
- **Masked echo-back of channel credentials**: webhook URLs, signing secrets, bot tokens, SMTP passwords, etc. are only echoed back as masked values with trailing-character hints; submitting them back unchanged means "do not modify", while clearing the input deletes the field.

#### Fixed Issues

> This section is the outcome of a security audit round performed after the first version of the feature was completed; each item was first reproduced, then fixed, then covered with regression tests.

- **Fixed mask bypass via "change the target address, keep the credentials" (severe)**: config merging kept the stored value for any "unmentioned keys", so changing only the address while staying silent about credentials made the server send the real stored credentials to an arbitrary address, completely silently. Now, once the target address changes, every credential field must explicitly declare itself -- provide a new value, or explicitly leave it blank to indicate it is no longer needed.
- **Fixed zero escaping of untrusted content in markdown-based channels**: vulnerability titles and summaries come from model output and asset URLs come from scan results, so a crafted title would render as a clickable external link in DingTalk / WeCom / Feishu, while image syntax would be fetched by the client at render time, leaking the reader's IP. All channels now apply single-line collapsing + markdown metacharacter escaping, with the escaping happening at each channel's rendering exit point rather than in the shared title function.
- **Fixed the SSRF surface of delivery addresses**: previously loopback, intranet, and cloud-metadata addresses were all deliverable, and the first 200 bytes of a failure response body flowed into `last_error` and were echoed back by delivery history -- forming a semi-blind read primitive. Defenses are now applied at **dial time**, covering DNS rebinding, and cross-host redirects are rejected; loopback and link-local addresses require explicitly setting `ARTEX_NOTIFY_ALLOW_LOCAL=1`, while RFC1918 private networks remain allowed (self-hosted intranet SMTP relays are common).
- **Fixed push credentials leaking through error messages**: DingTalk's `access_token`, WeCom's `key`, and Telegram's bot token all live in URLs, and on delivery failure the full URL flowed into four places: `last_error`, delivery history, server logs, and frontend messages. All are now redacted, keeping only `scheme://host` plus the underlying cause.
- **Fixed silent loss from marking a whole batch delivered after a digest message was truncated**: truncated vulnerabilities appeared neither in the message nor in the failure list, yet delivery history showed success. Batching is now done **per whole message**: only the items packed into the current message are marked, the rest return to the queue for the next message without consuming retry counts, and the message header states how many more items will continue in the next message.
- **Fixed a typo in the minimum-severity threshold silently disabling the filter**: when `min_severity` was misspelled (e.g. `hgih`), the unknown severity had ordinal 0 and the check degraded to always-true -- users believing they only received critical findings actually had every vulnerability flooded into the group chat. Values are now validated on the write path with the allowed values listed.
- **Fixed `rate_per_min = 0` (unlimited) being unreachable**: the persistence layer quietly rewrote an explicit 0 to the default, so operators thought they had lifted the rate limit while still being throttled. The default is now filled in at the API layer only when the field is omitted.
- **Fixed digest channels completely bypassing the token bucket**: `rate_per_min` previously had no effect on digest mode. The number of items claimed per round is now constrained by both the round's quota and an in-memory upper bound.
- **Fixed failure handling judging by the batch's maximum attempt count, dragging new deliveries down with old ones**: one old delivery retried twice would drag brand-new deliveries in the same batch into failed status, permanently losing new vulnerabilities without a single retry. Decisions are now made per item.
- **Fixed deliveries with unparseable snapshots in a digest batch being silently marked successful**: such entries were skipped at render time but counted as successful with the whole batch. They are now explicitly marked failed with a reason.
- **Fixed per-round delivery counts of a single channel potentially exceeding the lease duration**: in multi-instance deployments, rows whose leases expired could be re-claimed, sent repeatedly, and have their attempt counts double-incremented. The per-round cap is now derived backwards from "lease / single-attempt timeout", with an assertion pinning the relationship between these constants.
- **Fixed Telegram truncation possibly cutting an HTML entity in half**: truncation avoided splitting tags but not fragments like `&amp`, and the parser could then reject the **entire** message. Both are now avoided.
- **Fixed the email channel treating transient SMTP failures as permanent**: greylisting `450` and other 4xx responses are temporary rejections that should be retried later, but were previously judged fatal -- servers with greylisting enabled would fail every push on its first attempt. Reply codes are now distinguished by their first digit: 4xx is retryable, 5xx is a permanent failure.
- **Fixed submitting nested structures actually writing the mask literal into storage**: object fields like `webhook.headers` can only be masked or submitted as a whole -- stuffing the mask sentinel inside the object would be stored as a real value, silently breaking authentication. Such submissions are now explicitly rejected.

#### Design Notes

- **Writing vulnerabilities uses a single blind INSERT in the transaction**: `notification_events` is written in the same transaction as the vulnerability, so commit guarantees atomic consistency of both. This INSERT deliberately reads neither the channel table nor user filter rules, and is wrapped in a `SAVEPOINT` -- otherwise a single misconfigured filter condition could abort the transaction and prevent a critical vulnerability from being stored.
- **Delivery uses lease-based claiming instead of long transactions**: after claiming with `FOR UPDATE SKIP LOCKED`, rows are set to `sending` with `next_attempt_at` pushed into the future as the lease; no database lock is held during delivery. Rows left behind by crashed processes are re-claimed after lease expiry -- self-healing without infinite retries.
- **Rate limiting does not consume retry budget**: the round's quota is first computed from the token bucket, and only that many items are claimed. Reversing the order would make deliveries blocked by rate limiting burn one attempt each, exhausting the three-attempt budget on pure waiting before landing in failure.

### Test Infrastructure

#### Fixed Issues

- **Fixed permanent asset residue caused by cleanup ordering in the company-ICP-ownership test case**: `defer d.Close()` ran first when the function returned, with `t.Cleanup` after it, so all cleanup statements landed on an already-closed connection; the errors were then discarded, leaving test assets and companies permanently in the database and making other asset-count-assertion cases fail mysteriously. Closing the connection now also goes through `t.Cleanup` and is registered first, while cleanup failures are made visible.

### Traffic

#### New Features

- **Traffic list supports content search, path / status-code / length filtering, and column sorting**: the filter row supports filtering by **response content** (via full-text index), **path**, **status code** (exact values or buckets like `2xx`), and **response length ranges**; table headers can sort by time, status code, and response length, with sort preferences stored locally. Two new indexes take effect automatically after upgrading existing instances; the Agent tool's query contract is unchanged.
- **Traffic list gains "Clear All"**: deletes all traffic records at once (ignoring current filters) and cleans up history host directories no longer referenced by the index, followed by a full compaction reporting how much space was actually freed. Traffic evidence attached to vulnerabilities lives in a separate evidence store and is unaffected.

#### Fixed Issues

- **Fixed disk space not being freed after deleting traffic**: the index database was created without `auto_vacuum`, so the file never shrank; and the full-text index is `contentless_delete`, where `DELETE` only writes tombstones without reclaiming the original postings -- deleting traffic actually made the index bigger (measured: capturing 6MB of body text -> 16MB index, still 16MB after deleting everything). Newly created index databases now enable `auto_vacuum=incremental`, and after each deletion a background process performs full-text index merges and incremental reclamation in chunks, gradually returning space to the system (same scenario drops back to 104KB); reclamation releases the write lock between chunks and does not block traffic recording.

> Upgrade note: `auto_vacuum` can only be set at database creation time, so **existing instances' index databases are still in the old mode**, where incremental reclamation is a no-op (a notice line is printed at startup). After upgrading, tombstone growth on such databases has stopped, and the already-occupied volume can be reclaimed with one "Clear All" -- that step converts the database to incremental-reclamation mode, after which routine deletions take effect on their own.

### LLM

#### Fixed Issues

- **Fixed custom session headers not taking effect on the "one-shot LLM call" path**: target decomposition (round 0) and cold-node compression are one-shot calls without a transcript store, so the context carries no session id and the gateway side cannot read that header -- for endpoints like opencode zen that return a hard 400 when the header is missing, this showed up as "round 0 fails while subsequent planner rounds work perfectly". Both paths now explicitly carry a per-exploration stable session id, which also lets token usage be correctly attributed to the corresponding exploration (previously recorded nowhere).

### Tools

#### Fixed Issues

- **Fixed output truncation not applying to MCP and custom tools, letting large results flood the context whole**: MCP tool returns previously bypassed truncation entirely, while HTTP-type custom tools received an empty tool context and always used a hardcoded 6000 characters instead of the session-configured limit. Both paths now go through `actool.Capture`: truncating at the session's `MaxOutputChars` (default 30000), and when `ToolOutputDir` is configured, spilling the full output to disk with only the leading fragment and a file pointer kept in the message.
- **Upgraded norma to v0.4.3**: the harness layer uniformly executes `CaptureOnce`, so no tool (including ones added in the future) can blow up the context by missing truncation again.

### Deployment

#### Fixed Issues

- **Fixed the activity stream failing to connect behind reverse-proxy deployments**: the SSE address was unconditionally assembled as `${hostname}:8787`, so in deployments proxied to 443 the page loads from `https://domain/` while SSE points at port 8787, which is not open to the public internet -- showing up as the activity stream reconnecting endlessly. It is now distinguished by `NODE_ENV`: production defaults to same-origin, with only `next dev` keeping `:8787` to bypass Next.js's buffering of SSE; the `NEXT_PUBLIC_SSE_BASE` override is retained. The README now includes reverse-proxy deployment notes.

### Discovery

#### Fixed Issues

- **Fixed the "by asset" view's asset list overflowing with no scrollbar**: the outer card only had `max-height` without a definite height, so the scroll viewport's percentage height could not resolve -- with many assets the list either burst out of the card or was cut off with no way to scroll. The height cap is now applied directly on the asset tree's native scroll container, adapting to window height.

### Database

#### Fixed Issues

- **Capped the connection pool**: `database/sql` places no limit on how many connections the application itself may open -- when no idle connection is available in the pool it creates new ones unconditionally, all the way up to PostgreSQL's `max_connections` (default 100) before being refused. The pool is now capped at 32; excess queries wait for an idle connection instead, appearing as slower rather than erroring under the same load. If `max_connections` has been tuned down, lower this accordingly.
- **Improved connection reuse**: `MaxIdleConns` defaults to 2, so concurrently-used connections beyond 2 are closed right after use and the next request must redo the TCP + PostgreSQL authentication handshake. The idle cap is now raised to match the total cap, with a 30-minute connection lifetime and 5-minute idle eviction added, letting the pool naturally rotate out long-lived bad connections.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.14] - 2026-09-24

### Task List

#### New Features

- **Task list gains a "Vulnerabilities" column**: shows counts of Critical / High / Medium / Low severities, with non-zero bands colored by severity -- the vulnerability scale and distribution of each task visible at a glance.

#### Changed Features

- **"Description / Target" columns narrowed**: overlong content is ellipsized with full text on hover, reducing long text squeezing the list's horizontal space.

### Exploration Graph and Planning Overview

#### Changed Features

- **Trimmed the planning overview (graph_overview) and capped list sizes**: recent facts, finished intents, pending intents, cold-zone summaries, and confirmed-vulnerability details now show "latest window + count fallback", with unshown items queryable on demand; significantly reduces per-round LLM context size and avoids context bloat in long tasks (the related task overview's cold zone is rate-limited too).

#### Fixed Issues

- **Eliminated the reverse duplicate edge between digest and finding in the exploration graph after collapsing cold nodes**.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.13] - 2026-09-19

### Asset Interception

#### New Features

- **New global asset interception rules (blocklist)**: supports exact and fuzzy matching of domains / IPs / URLs and CIDR ranges, with rule CRUD and enable/disable; managed under "System -> Asset Interception". Ships with built-in fuzzy blocking of government (`.gov` / `.gov.cn`) and education (`.edu` / `.edu.cn`) sites by default.
- **Asset interception hooked into the execution chain**: before dispatching intents (`add_intent`) and inserting assets (`insert_assets`), the Agent checks target assets against interception rules -- intents hitting an interception rule are not dispatched, assets hitting one are not inserted, and the Agent receives the asset info plus the interception reason in return.
- **New task-level block / allow (allowlist) rules**: independent of global rules and effective only for the current task, evaluated as "block first, then allow" -- a block hit forbids; with no block hit but task-configured allow rules that none match, testing is "not allowed"; with no allow rules configured, the allowlist is inactive. They can be entered when creating a task, or added/edited/enabled/disabled in the task detail's "Overview" tab.
- **Task templates support preset categories and task-level block/allow rules**: templates can save a task category plus a set of task-level rules, which are carried into the new-task form when the template is applied.

### Operations Review

#### Fixed Issues

- **Tightened the model-judge output protocol to reduce truncation-caused false passes**: the verdict explanation (comment) cap was lowered from 500 Chinese characters to 120, with "JSON only, no preamble or code fences" enforced -- preventing verdicts truncated by `MaxTokens` from failing to parse and thereby being passed under the model-failure policy (fail-open).

### Task Archiving

#### Fixed Issues

- **Archiving skips symlinks instead of failing the whole package**: the archive format end-to-end supports only regular files and directories, so any symlink in the working directory previously failed the entire task archive. Symlinks are now skipped with a log entry while remaining files archive normally (links are not followed, never escaping the directory tree).

### Accounts and Compliance

#### New Features

- **New "Terms of Use and Disclaimer" dialog before login**: login requires checking the box to agree first.

### Licensing and Dependencies

#### Changed Features

- **The project adopts the AGPL-3.0 open-source license**, with the README's license and disclaimer notes completed.
- **Upgraded norma to v0.4.1**.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.12] - 2026-09-17

### Exploration Broadcast Board

#### New Features

- **New "Exploration-Trace Broadcast Board" on the task detail page** (#144): presents the task's exploration nodes (origins/goals/intents/facts/vulnerabilities/hints/compactions) as a timeline feed, with filtering by type, keyword search, ascending/descending order, pagination, and auto-refresh. While on the newest-first first page it acts as the live position and refreshes every round; once scrolled away it only accumulates an unread count without disturbing the current reading, with one-click return via "N new broadcasts · Back to latest". Entries are grouped by day, so long tasks paged across days still show "which day this happened".
- **Each broadcast-board row shows the node id** (#145), making it easy to cross-reference the exploration-trace graph and locate specific nodes.
- **Expanding a broadcast-board node reveals its upstream/downstream links and anchored assets** (#147): expanding a broadcast entry shows the node's upstream (where it came from) / downstream (what it produced) relationships, with hover popups on related entries showing the corresponding node's card (type/status/source/time/summary/payload snippet); the node's anchored assets are listed alongside (type tags + recognizable text). The related data ships with the broadcast page, so expanding issues no extra requests.
- **Broadcast-board search supports filtering by node id** (#150): besides "content / source", the search box now also matches node ids -- typing a plain number or the displayed "#41" form locates the corresponding node precisely.

### Intent Management

#### New Features

- **Intent deletion supports "soft delete / hard delete" modes** (#149): pending, running, and paused intents can all be deleted, with a reason required; the mode is chosen in the confirmation dialog.
  - **Soft delete (default)**: the intent is marked "deleted" with the reason recorded in a dedicated field, keeping the intent node and all of its outputs and lineage.
  - **Hard delete**: physically removes the intent plus its exclusively-owned descendant nodes ("supported only by it", cascaded along output/intent chains down to the leaves), avoiding orphaned data; the deleted intent's token accounting is archived under its original dates; shared nodes (still referenced by other intents), goals, and task root facts are always kept, and the hard-delete dialog shows the expected number of cascade-affected nodes.

  Both modes notify the planner that "this intent was deleted by the user + reason" so it can replan accordingly.

### Operations Review

#### New Features

- **Approval records support filtering by status and decision source** (#139): approval records can be filtered by approval status and decision source to locate target records quickly.

#### Fixed Issues

- **Pending approval requests load independently of history pagination** (#133): pending requests are no longer affected by history-list pagination and stay fully visible while browsing history.

### Agent

#### Changed Features

- **Worker role description changed to a generic cybersecurity-platform wording** (#138).

#### Fixed Issues

- **Fixed cold-node compaction never triggering**: wired the per-task planner up to the Compactor -- cold-node compaction (cold-digest) previously never ran because it was never connected, and is now restored.

### Chat

#### New Features

- **Chat supports @-mentioning multiple record types with scroll pagination** (#135): multiple record types can be @-mentioned in chat, and mention candidates support scroll-based paged loading.

#### Fixed Issues

- **Long message bubbles are constrained inside the session panel** (#137): overlong message bubbles no longer overflow the session area.
- **Fixed file upload reporting "missing upload file" when a new conversation was not yet created**: the Composer's file picker now snapshots the `FileList` into an array before clearing the input and then invokes the callback; previously, draft state required asynchronously creating the session first, and by the time execution resumed the input-bound `FileList` had been cleared, leaving the upload without its file field and the backend returning 400.

### Traffic

#### Fixed Issues

- **Traffic-recording proxy listens on 127.0.0.1 by default** (#129, #130): avoids exposing the proxy on other network interfaces under the default configuration.

### Web

#### Fixed Issues

- **Fixed the static-export task list page losing its global header**.
- **Added missing demo traffic mocks for the vulnerability detail page, fixing a blank screen**.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)
- [@dingpotian](https://github.com/dingpotian)

## [0.3.11] - 2026-09-15

### Operations Review

#### New Features

- **Approval records support source tracing** (#125): clicking an approval record's "source" jumps back to the tool execution that triggered that approval -- opening the original full session, auto-paging to the target position, expanding the command and result in place, and centering/highlighting it in the scroll area, while keeping surrounding messages readable; auto-correction stops once the user scrolls manually. Across plain conversations, task Workers, planners, and main-Agent segmented sessions, precise location is achieved via persisted `tool_use_id` plus task mapping (new in-scope call-ID index), with an explicit notice instead of jumping elsewhere when data is missing, duplicated, or ambiguous. A clear notice is shown when the session was deleted or the task is archived/records are missing.
- **Approval records support pagination** (#115).

#### Changed Features

- **Streamlined model-review input** (#124, #125): the model review (the LLM judge used when no rule matches) input is narrowed to a versioned JSON of "this one complete tool call + explicitly selected brief background + local working directory". Background only takes real user messages (the current user message of chat / the task main Agent); Workers no longer attach intent summaries, and background inherited from parent Agents is cleared; planner and auto-triggered sessions do not fabricate user messages. Task descriptions, goals, operation constraints, global exploration posture, historical calls, and full Worker intents are no longer attached -- these are still used as before for Agent execution and standalone session audits, just not fed into operation review. All verdicts must output JSON `decision`/`comment`, with explanations covering "actual operation, post-success consequences, matched rules"; instructions inside parameters or background cannot change the review policy. Snapshots plus fingerprints of the actual model input are saved, old-version snapshots are kept and version-tagged, and old inputs are never backfilled with current data.

#### Fixed Issues

- **Model verdicts wrapped in code blocks no longer silently pass** (#126): when the model-returned verdict JSON is wrapped in ``` fences, parsing previously failed and the verdict was silently passed through the model-failure policy; fences are now stripped before parsing, and the configured failure policy applies only if parsing still fails.

### Agent

#### New Features

- **New experimental noa context compaction** (norma upgraded to v0.4.0): an opt-in experimental feature in system settings, off by default. When enabled, noa (model-driven context compaction) takes over context compaction for the four Agent types -- main Agent, planner, Worker, and chat -- replacing the built-in compaction; the pre-compaction source text is persisted centrally under `<workDir>/noa/<sessionID>/` (not scattered across task directories), one globally-unique directory per session ID. On integration failure it automatically falls back to built-in compaction without interrupting real tasks; the switch is read once per run, and toggling only affects subsequently started runs.

#### Fixed Issues

- **Exploration-graph tools now reject calls without task context instead of crashing on nil**: calling exploration-graph tools outside a task context returns a clear error instead of crashing on an empty-store dereference.

### MCP

#### New Features

- **Support for legacy SSE MCP servers** (#117): compatible with MCP servers that only provide legacy SSE transport.

### Traffic

#### Fixed Issues

- **Traffic search matches record hosts port-sensitively** (#114): host matching in `traffic_search` now includes the port, preventing same-host records on different ports from interfering with each other.
- **A `traffic_search` description-migration failure no longer aborts subsequent reporter migrations**: a single migration failure is isolated and does not affect subsequent migrations.

### Web

#### Changed Features

- **Separators added to task retest-vulnerability options** (#122): dividers between retest-vulnerability choices for clearer visuals.

#### Fixed Issues

- **Fixed the demo task detail page crashing entirely**: in mock mode, the task-detail "Sessions" tab fetches `GET /api/tasks/<id>/side-questions` (side-question history), but the mock handler had no such route, and the fallback reader returned `[]` for any "path ending in s as a collection", leaving `data.items` as `undefined`; the side hook's `merge()` then threw `TypeError: t is not iterable` while iterating it. The exception occurred inside the `setItems` updater, was rethrown by React at render time so the caller's `catch` could not intercept it, and the whole page fell to the error boundary showing "This page couldn't load". The mock handler now explicitly returns empty side history, and `sideAPI.history` normalizes its return value (non-array `items` always coerced to `[]`) as defense in depth.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)

## [0.3.10] - 2026-09-13

### Networking

#### New Features

- **Network search adds an official DeepSeek source**: directly reuses the currently active LLM configuration. It differs from the other three sources in nature -- DeepSeek has no directly callable search API; search exists only inside its Anthropic-compatible interface (the `web_search_20250305` server-side tool) and is executed by the DeepSeek server. So it **only supports official DeepSeek endpoints + the Anthropic protocol** (OpenAI-protocol endpoints reject server-side tools outright), each search **costs one extra model call**, requests **bypass the search egress proxy** and **are not recorded in traffic capture**, and results carry **only titles and links** (no summaries -- fetch body text via WebFetch when needed). The settings page documents these limits but **does not validate or block**, leaving users to confirm eligibility themselves, with a "test search" button for a live verification run.

### Agent

#### New Features

- **Workers gain cross-work review capability**: new `search_all_worker_traces` (globally search hit steps across all works' execution traces in this task by keyword, no need to know intent_id first) and `get_worker_trace` (after pinning a work, list its step stream, search in place by keyword, and fetch full content by step_id), for reusing observations seen in other works but never written into facts, avoiding duplicated effort.
- **`node_detail` handed down to workers**: together with the review tools above, a worker holding an intent_id / node id can directly query that node's full details.
- **Planning rounds triggered by `add_hint` are now explicitly announced**: previously new hints were only folded into the posture overview for the planner to discover on its own; now each `add_hint` records a "user added N strategic hints: ..." trigger for the planner (batched as one entry, not one per hint), with the planner explicitly told "this round was triggered by new hints" and shown the hint contents directly.

#### Changed Features

- **Global posture prompt switched to an "unleashing" tone**: encouraging divergent exploration and timely cross-intent clue reporting instead of converging too early.
- **Streamlined worker boundary wording**: a first blockage does not mean fully probed -- focus on "exhausting this intent's bypass options before concluding".
- **`insert_assets` drops the per-asset `related` parameter**: its only effect was deciding whether to add the asset to this task's scope, yet the value was never persisted (re-registering the same asset voided it, and the UI never showed who was judged irrelevant) -- it just made the model perform one more unpreserved judgment.
- **Test scope (`task_scope`) no longer affected by the asset-coverage switch**: automatic scope admission in `insert_assets` (`source='auto'`) always runs whether or not coverage is enabled, and `add_task_scope` is always offered to the planner / in-task main Agent / goal-decomposition Agent. Scope is the task's authorization boundary and the filter baseline for asset queries; the coverage switch only decides whether to use it as the denominator for metrics, not whether to accumulate scope itself. Previously, disabling coverage broke both the `auto` and `agent` write paths, leaving `task_scope` with only manually added UI rows. `list_untested_assets` stays hidden when coverage is off (it is inherently a coverage view).

#### Fixed Issues

- **Fixed cross-task asset leakage** (#59): `list_assets` previously hardcoded the task id to 0 and queried the whole shared asset library bare, with the model unable to express scope filters -- so assets from other tasks (especially IPs) were mistaken for this task's targets, skewing test direction. It now returns only assets inside **this task and directly linked tasks'** test scope (`task_scope`), matched by **ownership** rather than literal values: a root domain in scope covers all its subdomains/services/APIs, a CIDR covers in-range hosts and services; out-of-scope assets cannot be fetched even by id. Non-task contexts (Auto/pentest) with no scope to rely on still fall back to the full library. Also fixed the ownership blind spot where direct-IP hosts (e.g. `http://1.2.3.4/api`) could not be matched by CIDR scope. The UI "Test Assets" view (filtered by task producer) is unaffected.
- **Fixed worker cross-work review tools being accidentally unbound**: after `search_all_worker_traces` / `get_worker_trace` (plus `node_detail`) were added to the worker default toolset, an old "narrow the worker tool surface" migration unbound them again at startup, leaving workers without these tools in practice. They have been removed from that unbind list, and libraries that already ran the old migration get a one-time rebind to workers.
- **Fixed occasional `/btw` side-question persistence failures**: strips JSONB-unsupported NUL (`\u0000`) escapes before the side_question checkpoint is written, avoiding write errors on content containing that character.

### Triggers

#### Fixed Issues

- **Merged trigger sessions dedupe task descriptions/goals per task**: on multi-task merged triggers, the same task's description/goals are no longer pasted into the message repeatedly, avoiding long goals stacking up and blowing out context.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.9] - 2026-09-11

### Agent

#### New Features

- In-task main Agent supports **multi-session**: sessions can be created, switched, and context-reset independently, each fully interactive.
- Support for persistent `/btw` **side questions**: follow-ups without interrupting the main thread, persisted to the database, still there after restart.
- `spawn_task` adds `source_task_ids`, letting new tasks **read-only inherit** assets and findings from source tasks.

#### Changed Features

- Worker cross-engagement memory disabled and its default tool surface narrowed: reading context and cross-work retrospectives are planner duties; workers handle single-intent execution and write-back only.
- Streamlined worker default prompts; `get_worker_output` drops the `terminated` and `worker_name` fields; streamlined `insert_assets` / `list_assets` descriptions.

#### Fixed Issues

- Fixed works ending early on **idle rounds**: sometimes the model spends a whole round outputting only thinking -- no text, no tool calls -- which the harness sees as a natural finish (`end_turn` with no `tool_use`) and closes out as `completed` with an empty summary -- an unfinished intent cut off mid-way, showing as "task ended normally but with no text summary". None of the five LLM-retry layers could catch it: it is not an error, the same-provider safety-window retry only triggers on stream failures, the circuit breaker treats `err == nil` as success outright, intent reruns only recognize `model_error`; the SDK's empty-response retry cannot catch it either, since that layer judges emptiness by "whether any event was ever yielded", and thinking deltas are events themselves. Now the work Stop hook recognizes such rounds and injects a resume instruction so the model continues from its already-produced thinking -- deliberately not "resending as-is", since such idling is usually determined by prompt and context shape (stable behavior, not random sampling noise), and resending would only make the model think it all over again.
- Reuses the **empty-response retry count** in the LLM page's "Retry & Backoff" section (both answer "the model finished without producing any substantive content", differing only in criteria and mechanism), Default 2 time(s), fill in `-1` to disable, reverting to the previous "idle winds down" behavior. It is a **total** cap for one intent rather than a consecutive count: the harness itself already hard-limits consecutive idling to a single push, and the quota only refreshes once a genuine tool round has actually occurred, so this number blocks "tool -> idle -> push -> tool -> idle" loops from draining the intent budget. On trigger, the log records `[work <worker> · #<Intent>] idle round...inject continuation instruction (n/N)`. See design in `docs/LLM-retry-design.md` §1.1.
- Fixed `/btw` context budgeting and input layout in long conversations; side-request ID generation is downgraded in non-secure contexts (non-HTTPS access).

### Traffic

#### New Features

- Vulnerabilities support **linking multiple traffic evidence items**, with sorting, annotations, and role-based distinction (request/response/corroborating).
- The report agent **automatically links** relevant traffic evidence before writing the report.
- Added an agent-level **traffic-binding toggle**, and completed evidence handoff between agents.

### Tasks

#### New Features

- Support for **session-level vulnerability retesting**: retests run in a dedicated agent session, with running status shown in the list and detail views.

### Interception

#### New Features

- Approval records gain **detail and execution auditing**: tool request context, model/rule preliminary verdicts, execution output, and parameter fingerprints are now viewable.

### Assets

#### New Features

- Task test assets support **DSL search**.

### LLM

#### Fixed Issues

- Fixed test connections not re-sending the custom session header, which caused opencode zen to return 400.

### Networking

#### New Features

- MCP HTTP transport supports **skipping TLS certificate verification**, making it easier to connect to services with self-signed certificates.

### UI

#### New Features

- The session list is grouped by agent, with expand/collapse and independent pinning; conversations can be filtered by agent.
- The attack-chain graph renders compressed (digest) nodes and collapses their members.

#### Fixed Issues

- Fixed the blank screen caused by out-of-sync login credentials.
- Interception prompt copy now explicitly marks it as platform control, so it is no longer misread as target-side defense.

### Deployment and Updates

#### New Features

- Support for **one-click in-page updates**: a new version-and-updates card on the system settings page, with a top-bar prompt when a new version is available. The flow is: download the release package, verify `SHA256SUMS`, smoke test, stage, then exit and let the watchdog script relaunch to complete the swap, with the page auto-refreshing.
- New watchdog launch scripts `start.sh` / `start.bat` as the official startup entry (included in both the release package and the Docker image; `install.sh` is unchanged): they decide whether to relaunch based on the exit code and forward SIGTERM to artex. Verification and swap logic lives in Go; the scripts stay foolproof.
- Automatic fallback on failure: if verification or the smoke test fails, the new package is discarded and the current version keeps running; if the new version fails to start 3 times in a row, it rolls back to the previous version. The settings page also offers manual rollback (note that the database schema is not rolled back).
- Updates only trust the GitHub domain over mandatory HTTPS; the release source is not configurable, and dev builds disable one-click updates. GitHub query results are cached for 30 minutes so the top-bar check does not exhaust the API quota.

#### Known Limitations

- Under Docker only the binary is swapped, not the image: the toolchain is not upgraded along with it, and recreating the container reverts to the image-bundled version -- still use `docker compose pull artex` when needed.
- The `skills/` directory from the release package is not synced, so newly added built-in skills in a new version do not take effect automatically.
- Updating restarts the service and interrupts running tasks.

### Dependencies

#### Changed Features

- Upgraded norma to v0.3.7.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)

## [0.3.8] - 2026-09-09

### LLM

#### New Features

- The LLM page adds a "Retry & Backoff" tab: the **retry count**and**interval** of all five retry layers are configurable. A single model call's failure passes through the five layers, innermost to outermost: connection retries (SDK, connection reset / timeout / 429 / 5xx before streaming starts), empty-response retry (SDK, ended normally but with no content, only openai format), same provider safety-window retries (replaying an interrupted stream before any output was delivered to the caller), polling circuit breaker (cooldown skip once consecutive failures reach the threshold), and intent rerun (after a worker winds down under the pretext of model_error, the entire intent is rerun) — an outer layer only runs after the inner layer is exhausted. Each layer has two knobs with unified semantics: leave blank = use the original default count and exponential backoff; fill in a count to use that count; fill in an interval to swap exponential backoff for a fixed interval; fill in -1 = disable this layer's retry. The first three layers follow the endpoint and can override the global defaults per field in each model config (pinning only the interval still inherits the global count); the circuit breaker and intent rerun have process-level semantics with only one global instance. Takes effect hot after saving, no restart needed. Leaving everything blank keeps current behavior, and legacy database upgrade stays byte-for-byte unchanged (new columns default to 0, missing settings keys mean all defaults). Connection tests deliberately exclude these parameters----it has a 30s hard timeout, and stacking user-configured retries on top would only turn working endpoints into timeout failures. See design in `docs/LLM-retry-design.md`.
- Each LLM config supports a custom **session header** (`session_header_key`): when non-empty, every LLM request carries that HTTP header whose value is the currently running session id (chat sessions use `conv-<id>`, workers use `exp<x>-worker-i<intent>`, etc.), for gateways that do prompt caching / sticky routing keyed on a session-id header. The session id is injected from the request context, so no norma change is needed, and the same shared provider can emit different header values per session. Existing databases get a migration, and the frontend LLM config dialog gains a new input field.

#### Fixed Issues

- Fixed saving an LLM config failing with `23502` when the session-header field was left empty: the column is `NOT NULL DEFAULT ''`, but an erroneous `NULLIF($n,'')` wrapper turned an unfilled session header into NULL and tripped the not-null constraint; the parameter is now passed directly with empty-string semantics.

### Agent

#### New Features

- Wall-clock timeouts now **finish in place**: with norma v0.3.6, when `MaxDuration` expires the running tools are interrupted and the run continues for the configured wind-down rounds on a live ctx (writing back identified content + a summary, with a final state of timeout), instead of relying on each worker/planner's `maxDur+90s` external hard ctx to kill a stuck run outright as `aborted_tools`. Chat goes through the same harness and automatically gets the same behavior; mainagent has no `MaxDuration` and is unaffected.
- Stall guard: when the planner wakes on a heartbeat / no-change wakeup with no open or running intents left in the whole graph, its opening statement becomes a stall alert, explicitly stating that no worker is running and no direction is queued -- this round must produce one or more non-duplicate new intents (producing 0 intents is not allowed).

#### Changed Features

- Worker prompt refactor: the intent / launch directives / anchored-asset raw JSON move into the system prompt, rebuilt every round and never compacted away, so resumed runs no longer depend on the transcript's first message surviving; the launch user message is slimmed down to just the global-situation overview (degradable, stale-tolerant). The cost is per-intent data mixed into system, losing cross-intent cache reuse -- a deliberate trade-off for never losing the intent.
- Slimmed down the planner / worker default bodies and fixed several field-tested issues: the planner now distinguishes `recent_done` by state (for blocked/exhausted, check the trace before deciding -- neither treating them as dead ends nor blindly rerunning), phrases negative conclusions as observations / open questions rather than verdicts and consults evidence before accepting them, must produce output when the goal is unmet and no open/running intents remain (a hard floor), and prefers depth over coverage; worker negative conclusions only state observations + tentative readings with verdict authority resting with the planner, and cross-intent leads go into the fact's summary for the planner instead of being pursued directly. Reseed appends the new defaults as a new version and switches to it; user customizations / old versions stay in history and can be rolled back.
- Narrowed the work-agent default toolset and asset write-back hints: workers only execute and write back a single intent, while reading context / cross-work review is the planner's job, so `list_facts` / `node_detail` / `list_companies` and cross-work retrieval (`search_all_worker_traces` / `list_worker_traces` / `get_worker_trace`) are removed from their default tools, keeping only `list_findings` (dedup before reporting a vulnerability) + `add_finding` / `record_fact` + `insert_assets` / `list_assets`; stale field notes conflicting with the `insert_assets` schema (`type=tech`/`on_url`/`props`) are removed from the prompts. Existing databases get a one-time migration stripping the corresponding bindings; the planner/main bindings of the same names are untouched.

### Exploration Graph

#### New Features

- **Exploration-graph cold-node compaction (cold-digest)**: old, long-inactive intents / facts are folded into digest nodes for display in `graph_overview`, with the original nodes permanently retained and fully restorable by id (lossless storage, presentation-only compaction, reversible folding). Hot/cold classification uses reverse reachability + any-live-branch-counts-as-hot, with R=6 rounds of debounce and connected-component grouping; background compaction (minor folds uncovered cold blocks, major re-presses from source merging fragments) includes activity re-verification and cooldown exclusion, never runs on the hot path and never overwrites revived nodes. The overview side exposes `cold_digests` and the asset-indexed `cold_index`, with `expand_digest` / `expand_index` handling restoration. Linked / inherited tasks' overviews reuse their own folded views, and `expand_digest` supports cross-task read-only restoration. `expand_digest` / `expand_index` are granted only to the planner and main agent, not to workers. All with migrations for existing databases.
- `graph_overview` now outputs the full `finding_list`: findings are the highest-value product of a task and usually few per task, so they are now included in full in the overview (unlike facts, which only show the recent window), letting the planner/worker see all confirmed vulnerabilities at a glance every round without calling `list_findings`. Each entry is condensed to `{id, summary, evidence?, from_intent?, assets?}`, with affected assets given as human-readable content (url / domain / ip:port) rather than bare ids.

#### Changed Features

- `graph_overview` no longer flattens `hosts`: the coverage block drops the host list (in large-scope tasks up to 500 host strings were carried every round with limited value for planning decisions), keeping only `host_count`, with specific hosts looked up via `list_assets` as needed. A new top-level `done_intents_total` (total finished intents) sits alongside `recent_done_intents` truncated to 15 or fewer, so the planner knows whether truncation occurred when deduping.

### Tools

#### Changed Features

- `add_company_scope` default binding moved from worker to planner: defining enterprise asset scope is the planner / main-controller / Auto's job, while workers only execute exploration. Fresh-database seeds default-bind to mainagent/planner/auto; existing databases get a one-time migration.
- Planner default-binds `list_assets`: it now has both `list_assets` (DSL full-library search) and `list_untested_assets` (untested within scope); existing databases get a one-time backfill that does not override user unbindings.

### Tasks

#### New Features

- New Running Workers column in the task list: counts the intent nodes with `state='running'` under the task, using exactly the same definition as the Running Workers figure in the task detail overview; shows 0 when no workers are running.

### Networking

#### New Features

- New **global egress proxy** setting: all target traffic can exit through one unified global proxy (http/https/socks5, with `user:pass` support). When traffic capture is on, it serves as the upstream of the MITM recording proxy (traffic is still recorded, then exits via the proxy -- both intercept and passthrough paths go upstream, never leaking the source IP); when capture is off, it is injected directly into the agent's bash environment and WebFetch (`proxyEnv` gains `ALL_PROXY` for socks5 support). The setting lives in the settings KV table (no migration needed), with a new Global Proxy card on the frontend system settings page, independent of the web-search proxy and the LLM proxy.

### UI

#### Fixed Issues

- Fixed misleading intent status labels: `exhausted` is now Budget exhausted instead of Exhausted (it actually means the step / time budget cut the run short with only partial results written back, not that the direction was fully explored), `blocked` is now Execution error instead of Blocked (it actually means model / API / network failures exhausted their retries and the intent was barely explored, not target / WAF interception), and added the previously missing `stopped` / Stopped (a work manually stopped by the user).

### Dependencies

#### Changed Features

- Upgraded norma to v0.3.4: MCP tool output gains truncation and spill-to-disk (see `651b961`); v0.3.6 will later support in-place wall-clock wind-down.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.7] - 2026-08-31

### LLM

#### New Features

- Model configs gain an output cap plus a choice of which request field name carries it: the cap limits how many tokens a single reply may generate at most, sent with every request, where 0 (default) = the field is not sent and the server default applies; it is a different thing from context window -- the latter is the model's total capacity, used only locally to compute the compaction threshold and never sent in requests. The field-name switch is only meaningful for the `openai` (Chat Completions) format: empty (default) sends `max_tokens`, which is the only field the vast majority of compatible gateways recognize; official OpenAI reasoning models (o-series / GPT-5) conversely only recognize `max_completion_tokens` and fail outright with `unsupported_parameter` on receiving `max_tokens`, so such endpoints must be switched to the new field manually. The other two formats each fix their own field name (Anthropic uses `max_tokens`, the Responses API uses `max_output_tokens`), so for non-openai formats the switch is greyed out and cleared on save. It also closes a previously unwired loop: the planner / worker / chat / main agent / target-decomposition never set an output cap anywhere -- the openai family sent nothing at all and Anthropic relied on the SDK's 8192 fallback, so filling in a number without this wiring would never take effect; the value is now resolved per round like streaming output, taking effect on the next round after a failover switch. Existing databases behave exactly as before after upgrade (new columns default to 0 with an empty field name).
- Clicking Test in the backend now logs each HTTP attempt's status code and the gateway's raw response body into the server log (bodies trimmed to 4K), easing diagnosis of 401s, quota messages, empty frames, HTML-page responses and the like -- no longer leaving only the UI-collapsed ok / err. This logging does not depend on the LLM logging switch.

#### Changed Features

- The task LLM config chain is now editable in any state, no longer restricted to running / paused / chain exhausted: after a task ends (done / failed / timeout) the main-agent conversation still uses this chain, and when a model on it broke there was previously no way to change it and continue interacting. The two terminal-state guards (backend HTTP and DB transaction) are both removed, and the frontend dialog opens editing and saving for terminal tasks too. Saving a terminal task no longer reopens quota-blocked intents (those intents would be moved to open, where no worker executes them and they no longer qualify for intent rerun); to keep running, still use intent rerun / new targets, which pull the task back into the running state.

### Agent

#### Changed Features

- Messaging a running worker to redirect it now reuses the existing pause / resume + transcript resume mechanism, behaving the same as main-agent conversations: previously it went through a home-grown intervention persistence protocol touching the scheduling barrier, recovery flow, and a dozen-plus activity-query filters. Messages are now injected via the next round's input, and the intent runs directly in its dedicated goroutine -- free of the 3-slot worker-pool limit, starting as soon as it is sent; the frontend keeps the message box and restores the Continue directly button, with delivery carried over SSE. No DB schema change. Trade-offs: messages live only in memory with no crash recovery, and sending a message may momentarily push the task one worker over its concurrency limit (a low-frequency scenario, acceptable).

### Tasks

#### Fixed Issues

- Fixed out-of-memory (OOM) crashes when archiving large tasks: archiving now streams the snapshot to disk instead of reading the whole task into memory; it also closes three recovery gaps on the cold-archive path -- on modern installs traffic lives only in SQLite, and a crash between the PostgreSQL commit and the SQLite commit used to leave an archived task's traffic stranded in hot storage with no way to recover; staging logs are now always written with or without a history directory, and a missing `journal.json` is treated as disposable.
- Fixed system lag with many tasks: loading conversations and loading the UI showed noticeable delays. Queries for the task list, task context, and exploration records are optimized together, and the frontend dashboard, conversation page, and task detail page now issue fewer duplicate requests.

### Assets

#### Changed Features

- Removed the max 256 rules limit on enterprise asset scope: enterprises entering scope IP by IP / domain by domain easily hit this cap and then had to split into multiple enterprises, while more rules do not actually make scope slower. The cap is removed in all three places (frontend, backend, and demo mock); each rule is still limited to 1024 characters, and the 2 MiB request-body cap stays as a backstop (roughly forty to fifty thousand rules).

### Skills

#### Fixed Issues

- Fixed skill-archive uploads failing with upload failed: zip: unsupported compression: Go's standard library only ships Store / Deflate decompressors, so bzip2 or Zstandard archives written by compression tools at non-default levels could never be extracted. Both decompressors are now added (pure-Go implementations, no new external dependencies); genuinely unsupported methods like Deflate64 / LZMA / XZ / PPMd, as well as encrypted archives, now produce a Chinese-language message before extraction naming exactly which file uses which compression method and how to repack it, instead of surfacing the raw English error.
- Fixed skill filename validation rejecting Chinese filenames as illegal: path validation used to be an ASCII allowlist of `[A-Za-z0-9-_./]`, so a package containing even one Chinese-named file (e.g. `reference/description.md`) failed the whole upload with "archive contains illegal paths". Changed to a Unicode denylist: filenames in any language and spaces are allowed, while control characters, invalid UTF-8, zero-width and bidi control chars (RLO filename spoofing), `\ % # ? * : " < > |` and `..` / absolute paths / empty path segments are still rejected; path-traversal protection behavior is unchanged. Skill names are likewise opened up - ASCII is still limited to lowercase letters, digits and hyphens (per the agentskills.io spec), while non-ASCII letters such as Chinese may be used directly as skill names, but spaces, dots and path separators are not accepted.
- Fixed Chinese-named archives packed by Windows compression tools coming out garbled or rejected wholesale: such zips do not set the UTF-8 flag and store filenames in GBK, so they are now fallback-decoded as GBK before path validation. Quoted frontmatter like `name: "Chinese skill"` also resolves the skill name correctly now.

### UI

#### New Features

- Added a new "All Findings" flat view on the Findings page and made it the default, switchable with the original "Group by Task" view via header tabs: the grouped view requires expanding each task to see vulnerabilities, which gets in the way when you just want to scan the list across tasks. The flat view is one large cross-task table (10/20/50/100 rows per page) that behaves exactly like the grouped view -- checkbox export, inline expansion for evidence and full reports, inline editing of name/category/severity, triage-status changes, dig-deeper, delete. Both views share the stat cards and filter bar, switching views keeps filters, and the current view plus filter selections are persisted locally; polling only hits the current view, and inline edits are written to both views' caches so you never see stale data after switching.
- Added a "By Asset" view on the Findings page: an asset tree on the left (enterprise -> root domain / IP -> subdomain -> service -> endpoint; the enterprise level only appears when assets actually have an owner), with findings under the selected node's entire subtree on the right, sharing the same table and filters as the other two views. The tree only contains assets that have findings, with ancestor chains completed on demand (upper levels are still reconstructed when a finding hangs only off the deepest endpoint); node counts are subtree-aggregated and deduplicated by finding; findings from deleted assets or with no asset association go into the "Unassociated" bucket. Unlike the other two views, the asset view does not poll -- it queries only on entering the view, changing filters, adding/editing/deleting findings in-page, or clicking the refresh button on the tree; the left tree is a navigation structure that doesn't need recomputing every 5 seconds. Each tree level shows only the increment relative to its parent (subdomains strip the root-domain suffix, services show `https :443`, endpoints show only the path), with full values in hover tooltips and breadcrumbs; when a level has too many nodes, the endpoint/service levels are dropped for the whole level with a notice, while counts still roll up to upper levels.

#### Fixed Issues

- Fixed session transcripts being squeezed and cut off in mobile session details: on narrow screens the session list collapses to give width back to the transcript itself.

### Dependencies

#### Changed Features

- Upgraded norma v0.3.2 -> v0.3.3: added `Config.MaxTokensField`, allowing OpenAI Chat Completions output limits to be sent as `max_completion_tokens` (reasoning models only accept this key). The two keys are mutually exclusive -- only one is sent; the default remains `max_tokens`.
- Upgraded `golang.org/x/mod` v0.37.0 -> v0.40.0, fixing two dependency security advisories.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)
- [@begininvoke](https://github.com/begininvoke)

## [0.3.6] - 2026-08-27

### LLM

#### Fixed Issues

- Fixed the "non-streaming" toggle reverting to "streaming" after reopening (#69): the config-list endpoint DTO was missing the `streaming` field, so responses never returned it and the frontend fell back to the default streaming whenever it read `undefined`; writes and DB storage were fine all along -- the value just couldn't be read back. The field was added back to the DTO (without `omitempty`, so `false` must also appear in responses).
- Test-connection now verifies the model actually replies, and tests using the config's real send/receive mode (#65): previously it only checked whether HTTP succeeded, so requests that went through but got zero reply from the model (reasoning burning through the budget / body swallowed by safety policies / compat layer dropping `content`) still reported "connection successful", contradicting the "won't talk" behavior seen in sessions; and tests always ran streaming, so a non-streaming config was actually testing a different channel. Now empty replies are an outright failure, success messages show the model's reply, and the current streaming toggle is carried into the test, keeping "test passed" consistent with "session works".

### Agent

#### Changed Features

- `list_facts` is now paginated + keyword-filtered to avoid blowing up context with a single call when there are many facts (#74): returns the latest 20 by default, with `limit` (max 100) / `before` cursor / `q` summary-keyword support, returning `{facts, total, has_more, next_before}`; overlong single summaries are truncated by character count (full text still via `node_detail`). Worker / planner prompts updated to the paginated semantics. Existing DBs get the new parameter schema flushed into the tool-catalog table via a one-time migration (`SeedTool` insert-only on first seed, otherwise the tool-management page shows "no parameters").

### Tools

#### New Features

- Added tool-call count statistics to the "Tool Executions" page (#72): a "Stats" button in the toolbar opens a dialog showing per-tool call counts, share, and failure counts (sorted by count descending); it follows the list's task / keyword filters and aggregates the whole result set rather than the current page, fetching only when the dialog opens.

### Dependencies

#### Changed Features

- Upgraded norma v0.3.1 -> v0.3.2: fixed silently dropped reasoning/refusal in OpenAI-family responses (Responses gains `reasoning_text`), triple-name dedup of the reasoning field, bounded retries on empty responses (zero content blocks), and gateway 400s caused by compression boundaries severing tool pairings (also repairing orphaned pairs already baked into stored transcripts).

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.5] - 2026-08-25

### LLM

#### New Features

- Each LLM config now supports streaming/non-streaming switching (streaming by default): on uses streaming SSE; off uses true non-streaming (`stream:false`, full JSON returned at once), working around gateways with poor SSE implementations (empty frames, dropped reasoning frames) at the cost of live progress and live token counts. worker / planner / mainagent / chat / goals all read the currently active config dynamically; the `llmpool` / `llmrec` / task-runtime three-layer provider wrappers are all non-streaming-compatible; `llm_profiles` gains a `streaming` column with `ALTER` backfill for old DBs (default `true`, old configs unaffected).
- Support for LLM configs in OpenAI Responses API format: each config gains a third format `openai-responses` (hitting `POST /v1/responses`), alongside Chat Completions / Anthropic; `BaseURL` normalization, default model `gpt-5`; the `llm_profiles` `format` constraint gains `openai-responses` with an idempotent migration for old DBs; the frontend format dropdown gains "OpenAI (Responses API)". Depends on norma v0.3.1 (including the `reasoning_content` pass-through fix).
- Record raw LLM request/response HTTP text: capture the real wire body at the HTTP transport layer, preserving tool schemas, `tool_use` blocks, and raw SSE frames invisible in the normalized view; each attempt of norma's internal retries is kept separately; `llm_records` gains `raw_request` / `raw_response` columns with `ALTER` backfill for old DBs; the recording page's detail panel gains a "raw text" view toggle and request/response copy buttons (with an `execCommand` fallback for non-secure contexts).

### Conversations

#### Changed Features

- Session-list multi-select changed to a "multi-select" mode switch: the default list no longer shows per-row checkboxes (cleaner look); the header now shows an "N total + Multi-select" button; clicking "Multi-select" enters selection mode (checkboxes, select-all, batch delete), "Done" exits and clears selection, and batch delete auto-exits on full success (stays in selection mode on failure for easy retry); single-item rename / pin / delete still go through each row's ⋯ menu.

### UI

#### Changed Features

- Task management, session operations, and traffic-view experience improvements (#57): task-list column sort preferences are now persisted, inline rename is triggered directly by an icon (no longer via menu), and drawer (sheet) interactions plus traffic-view details are polished.

#### Fixed Issues

- Worker asset tags now show only domains and IPs, and empty-tag cases are handled correctly.

### Agent

#### New Features

- Planner goal adjudication gains a "quantitative acceptance check": when a goal carries quantifiable acceptance criteria (asset test coverage reaching X%, capturing N flags, obtaining some permission), `graph_overview` measured values (`coverage.pct`, findings counts, etc.) must be verified before `prove_goal`; if measured values fall short, `prove_goal` is forbidden and gap-filling intents must be dispatched instead -- "mostly done / substantially achieved" is not grounds for marking met early. Fixes goals requiring 100% coverage being judged complete at only 40% measured.

#### Changed Features

- Rewrote the planner prompt's criteria for "0 intents this round": the original text described 0 intents as "the most common, most important principle", causing the planner to stop early while goals were unmet and unscanned surfaces remained in scope. Now 0 intents should only be produced in two specific cases -- ① every direction considered is already covered by open/running/recent_done intents; ② the next step depends on output from currently running work that hasn't arrived yet (wait for it to finish and the graph to update, then plan on the next wake-up). With the reverse constraint added: when there genuinely are uncovered directions not depending on running work, or goals are unmet with unscanned surfaces remaining, don't hold back just because "0 intents is common".
- Raised the worker prompt's "evidence bar for negative conclusions": for negative conclusions that could make the planner abandon a whole direction ("not injectable / port closed / no login entry"), all reasonable means within the intent must be exhausted first (different encodings/params/paths/methods) before concluding; with means untried or weak evidence, always mark `confidence=inferred`, avoiding welding a whole route shut with a rash `observed` negative (early-task wrong negatives are especially prone to steering the task off course with no self-recovery).
- The above planner / worker default-prompt changes are appended as new versions and switched to current via one-time migrations (`reseedPlannerPrompt` / `reseedWorkerPrompt`, each guarded by a settings flag); old versions remain in version history, and users who customized prompts can restore from the version records.

### Operations

#### New Features

- Added `reset-password.sh` to reset the admin (fixed username `ARTEX`) password: supports local / docker deployments, with connection info explicitly specified or auto-read from `--dsn` / `$ARTEX_PG_DSN` / `config.json`; generates a backend-login-compatible bcrypt hash with `pgcrypto` inside the DB and writes it back to `settings.auth.password_hash`, no service restart needed after reset. The password is passed via environment variable, never enters the process argv, and is escaped against injection.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)

## [0.3.4] - 2026-08-24

### UI

#### New Features

- Checking multiple tasks in the task list now allows batch category changes, with the target category supporting "Uncategorized" to move tasks out of their current category; the whole batch is written in one transaction, and the only possible failures are tasks deleted after being checked.

#### Changed Features

- New-task "task category" changed from a dropdown to a searchable input: typing filters existing categories, and a name not in the library is created and selected instantly on Enter (or via the "Create" item in the dropdown), with the selection shown as a removable tag; still limited to a single category.

### Conversations

#### Fixed Issues

- Fixed messages being blocked by "LLM not configured, cannot chat" when a session had its own LLM config selected but no globally active config. The pre-send check only looked at the globally active config, while actual execution prefers the session-selected config -- the two resolution paths disagreed; both now use the same logic (session/agent binding first, global fallback).
- The cannot-chat hint is now differentiated by state: no configs at all prompts "add a config", configs-but-none-active prompts "activate one or assign one to this session", instead of always showing "not configured", making it easier to pinpoint.

### Agent

#### New Features

- In tasks whose goals are all met, intents dispatched by the main agent via `add_intent` can be picked up and executed directly by workers, stopping when done: the planner no longer runs (tasks with no open goals don't enter planning), avoiding it re-judging goals as met and cancelling just-dispatched intents; once the frontier drains, the task returns to completed. Before dispatching, the main agent asks the user whether to register the intent content as a formal goal, in which case the task resumes normal autonomous planning.

#### Changed Features

- `get_worker_trace` / `get_task_worker_trace` no longer error outright when `step_ids` exceeds the per-call cap (5): instead they return full content for the first 5 steps and report via `returned_step_ids` / `omitted_step_ids` / `notice` which were fetched, which were left out, with a hint that "no further fetching is needed if this suffices". Duplicate and invalid ids are deduplicated/removed before counting.

#### Fixed Issues

- The task list is now sorted by task id descending (newest first), replacing creation-time ordering. Multiple tasks created in the same instant share identical timestamps with unstable ordering, which combined with 10-second polling and random in-memory map traversal made them swap positions frequently in the list.

### Assets

#### Fixed Issues

- Fixed enterprise-ownership recalculation being crashed by a single dirty row: when `assets.ip` holds a hostname (written by an agent or the asset API), `a.ip::inet` throws `22P02`, failing and rolling back enterprise asset-range add/edit and enterprise deletion entirely. Switched to the safe cast `try_inet()`, skipping invalid values instead of aborting the whole statement.
- Unparsable `ip` values are no longer silently skipped: after saving enterprise scope, an explicit notice reports how many assets have an `ip` that isn't a valid IP (with specific ids and values), warning that IP/CIDR rules won't cover them; the same info is written to server logs, covering paths with no frontend response (enterprise deletion, scopesentry sync, agent writes).
- Task test-scope IP/CIDR matching switched to `try_inet()` too, replacing the old charset-only regex guard (all-hex-letter hostnames like `abc.def` previously slipped through and triggered the same error).

#### Changed Features

- The `ip` field of `ip`, `service`, and `endpoint` assets no longer accepts hostnames: `insert_assets` and the asset API return per-item errors carrying `index` plus a direct remediation (use `type=subdomain` with `domain`, or resolve A/AAAA records first), so agents can self-correct and re-insert. Other assets in the same batch are unaffected and stored as usual.

### Contributors

- [@neouks](https://github.com/neouks)

## [0.3.3] - 2026-08-23

### Worker

#### New Features

- Running workers support per-item pause, resume, and cancel; pausing preserves intents, facts, and findings, while cancelling transactionally cleans up the current intent and its direct products after execution exits.
- Added the `paused` intent state, execution fences, and named termination reasons to prevent late blackboard writes after pause, cancel, or task deletion.
- Added an optional task concurrency cap; new, resumed, finding dig-deeper, and queue backfill entries all go through a single persistent FIFO admission path.

#### Changed Features

- Worker single-run wall-clock default raised from 600s to 1200s; timed-out tasks restart timing on their next real execution after revival.
- Real execution states of worker, planner, and main agent now uniformly drive the task status badge, fixing workers showing "idle" while running.
- Worker sessions keep per-item controls and a current-session token summary, with the model name moved to the right of the current session title.

#### Removed Features

- Removed worker multi-select, select-all, and batch pause/resume UI, along with the worker batch-control APIs and mock contracts.
- Removed the token badge and tooltip from worker list rows; the underlying full token ledger and task-aggregation APIs remain.

### LLM

#### New Features

- Tasks support an ordered LLM config chain; on clearly identified provider quota exhaustion, execution automatically switches to the next config, persisting the current config, exhaustion state, and error summary.
- Running and paused tasks support editing the full config chain, reordering, and manually switching the current config, effective from the next LLM call.
- Auto-switches, manual switches, and full-chain exhaustion are written as structured system activities and surface deduplicated alerts through the task activity stream.
- Added a task-role model resolution API that uniformly resolves which config and model the main agent, planner, and worker will use on their next call.
- Added an optional global LLM pool supporting configured call order, per-model failure fallback, health status, cooldown recovery, and manual reset.
- Added an always-on LLM usage ledger aggregating input, output, and cached tokens by task, session, model, and config.

#### Changed Features

- Goal decomposition, planner, worker, and main agent share the task-level LLM runtime (resolution priority below).
- Failover only responds to explicit quota, balance, or billing errors; ordinary rate-limit, auth, network, server, and context errors no longer trigger spurious switches.
- The LLM config page is now a config-card list with right-side drawer editing, plus pool polling order, priority, exclusions, health status, and recovery actions.
- Chain-exhaustion info now wraps on narrow screens; the current model moved from a session-list icon to a text label beside the current session title with a full tooltip.
- The per-role LLM resolution order within tasks is now "agent binding -> task config chain -> global": roles with an explicitly bound model always run on that model, unbound roles fall to the task chain, and fall to global only when the task chain is empty.
- An empty outbound proxy now means direct connection, no longer falling back to `HTTP_PROXY` / `HTTPS_PROXY` environment variables (explicit `ARTEX_LLM_PROXY` is unaffected); proxy input supports credentialed `socks5://user:pass@host:port`.

#### Fixed Issues

- Transient streaming failures before any output is produced (pre-submit) now safely back off and retry on the same provider, sharply reducing "died after one or two tools, no summary, terminal `model_error`" cases; quota exhaustion, overlong context, and deterministic 4xx errors are not retried and remain handled by failover and compaction recovery respectively.

#### Removed Features

- Removed the LLM icon-style markers in the session list to avoid confusion with Worker status icons.

### UI

#### New Features

- Tasks support creating, renaming, deleting, and filtering global categories; new tasks can select a category directly.
- Added global task-template CRUD and a right-side management drawer; new tasks can load preset descriptions and objectives, and current content can be saved as a template.
- New tasks can link multiple source tasks and multiple enterprise asset scopes; enterprise scopes are auto-recognized (domains, URLs, IPs, CIDRs, ICP filings, enterprise keywords) from a single multi-line text box.
- Task test assets support real-time adding and removal, recording sources such as manual, enterprise, inherited, or Agent-discovered; the current test asset and source summary are shown next to the Worker session title.
- The findings page groups by task with independent in-group pagination, and supports writing descriptions for vulnerabilities and creating high-priority deep-dive intents.
- Conversations support rename, pin, unpin, and delete; the task list supports batch pause/resume on the current page.
- Traffic details and tool-execution details moved to a right-side drawer; HTTP messages backfill `Host` and highlight the request line, status codes, headers, JSON, and markup-language bodies.
- Added "Details" and "Pause/Resume" buttons to the task list action column; the session send keybinding is configurable in system settings (`Enter` / `Cmd+Enter`, etc.), and a proxy input was added to the web search area.
- The badge right of the session title now shows the LLM configuration name, with the model ID moved to a hover tooltip; tasks can have an optional name, and the task list adds a name column plus search (falling back to the description when empty).
- The skills page adds usage counts, last-used times, and missing-dependency stats to help locate ineffective or uninstalled skills.

#### Changed Features

- Task titles are now focusable detail links; task assets, enterprise assets, finding task groups, and in-group vulnerabilities use real server-side pagination, stable sorting, and exact totals.
- Adding an enterprise now uses a right-side drawer, with scope entry unified as multi-line text offering real-time recognition, validation, and type preview.
- The Main Agent input supports auto-grow, `Enter` to send and `Shift+Enter` for newlines, and avoids accidental sends during Chinese IME composition.
- Agent preview, task reports, and related details now share Markdown rendering; delete confirmations, long errors, and mobile drawer widths were fixed uniformly.
- The task-template selector displays and searches by template name and submits by template ID; the system default font size was restored, and the app version is shown uniformly as `0.3.3`.
- Task totals and current-session tokens highlight only input, cache-read, and output values; the send button uses a simpler up-arrow icon.
- The task overview test scope shows enterprise names instead of IDs.

#### Fixed Issues

- Ordinary text containing dots is no longer misclassified as an ICP filing number.
- Fixed duplicate keys rendered in the Agent editor variable list when variable-catalog names collide with global runtime variables (e.g. `{{.Now}}`).

#### Removed Features

- Removed the standalone "Enter" button on task cards; entering details is now uniformly done by clicking the task title.
- Removed the dashboard "New Task" button, the Logo URL field when adding an enterprise, and the vulnerability-count badges at the top of the findings page and in task groups.
- Reverted the global 10% font-size increase, and removed the Worker selector, model icons, and per-row token stats from the session list.

### Agent

#### New Features

- New tasks can directly link multiple existing tasks, inheriting objectives, facts, vulnerabilities, completed intents, asset scopes, and blackboard context read-only in real time from direct source tasks.
- Blackboard read tools support on-demand queries of source-task nodes, facts, vulnerabilities, and execution traces; inherited nodes carry source markers and all write tools refuse to modify them.
- Enterprise-scope tools support domains, URLs, IPs, CIDRs, ICP filings, and enterprise keywords; keywords serve only as Agent scope hints and do not participate in automatic asset attribution.
- The vulnerability deep-dive action creates a high-priority manual Worker intent in the original task with an asset anchor and a `derived_from` edge.
- Added an "Objective Management" card to the overview for manual viewing, adding, editing, and deleting objectives; additions/edits notify the Planner and revive the task, while deletion hard-deletes the objective node (cascading edges and anchors) without revival.
- The Main Agent gets the `steer_work` tool by default, which can inject course-correction instructions into running Workers in real time without interruption or progress loss (intent ownership by this task is still verified).

#### Changed Features

- Source-task intents do not enter the new task's frontier; the new task keeps independent exploration, execution queues, working directories, and conversation history.
- Deleting a running intent no longer destroys data: it transitions to `stopped`, requires a deletion reason, attaches the reason as a fact to the intent and writes it into the payload, and the planner perceives it as `cancelled` (intent content and reason are retained).
- Main Agent conversations are now driven by the server-side activity stream, with input recoverable after send failures; opening a session auto-scrolls to the bottom and keeps the last reply visible after lazy-loading details.
- Pausing a task terminates current Main Agent, Planner, and Worker calls, but does not prevent the user from actively starting new Main Agent orchestration sessions while paused.
- Cancellation, closing, and streaming interruptions uniformly preserve the real termination reason, generated content, run rounds, elapsed time, tokens, and unreturned tool calls.

#### Removed Features

- Removed Main Agent client-side optimistic message echo to avoid duplicate messages and cross-session leakage under pause, failure, or concurrent activity.

### Constraints

#### New Features

- The objective-decomposition phase extracts operational constraints (allow/deny) before splitting objectives, with the Main Agent able to supplement them at runtime; constraints are injected into Planner and Worker system prompts at the highest priority, and post-debiasing diversity and surface-expansion exploration explicitly obey constraints.
- Added a "Constraint Management" card with add/edit/delete APIs to the overview; the constraint injection scope can be toggled separately for Planner / Worker (both on by default, read each round, effective immediately). The new `task_constraints` table uses `CREATE TABLE IF NOT EXISTS`, auto-created for existing databases.

### Interception

#### New Features

- Command interception adds model-based fallback approval beyond regex/string rules: when no rule matches, the model makes a semantic `ALLOW`/`ASK`/`DENY` decision, with configurable failure-fallback and approval-timeout actions; the interception page splits into "Interception Rules / Model Config" tabs, and model verdicts are prefixed with `[Model]` plus a reason.

#### Fixed Issues

- Hardened referee-output parsing to prevent a correct `DENY` verdict from being mistaken as allowed.

### Traffic

#### Changed Features

- Traffic recording now stores each whole exchange in SQLite (large bodies overflow to a hash-deduplicated blob bucket), with trigram full-text indexes on text bodies supporting arbitrary-substring and Chinese search; deletion collapses to a single SQL transaction, dropping from hours to milliseconds with no recording downtime. Added large-body streaming downloads and a `traffic_search` full-text parameter; legacy file-tree data needs no migration and remains readable, searchable, and deletable.

### Tasks and Assets

#### New Features

- Added an "asset coverage feature" toggle when creating tasks (on by default): when off, coverage is neither computed nor shown, the situation map shows assets only, `task_scope` is not auto-accumulated, and related tools are removed from the Planner / Main Agent; enterprise association is unaffected by this toggle.
- `insert_assets` adds a per-asset `related` flag (default `true`): effective only when coverage is enabled; `false` puts the asset only into the shared asset library without counting toward this task's coverage (e.g. incidentally discovered neighboring sites or unrelated assets).
- Enterprise scopes and task test assets uniformly support domain, URL, IP, CIDR, ICP, and keyword text recognition; domains/IPs can create or reuse global assets, while CIDRs/ICP filings/keywords are kept as task-scope context.

### Build

- `build.sh --release` supports building Linux amd64/arm64, macOS amd64/arm64, and Windows amd64 in one run, producing a zip release package containing `skills/`, config examples, and a README.
- Uses the Go linker to strip debug info and produce the zip release package; UPX is now explicitly opt-in to avoid segfaults at startup from its self-extracting ELF on some Linux environments.
- The release workflow runs a startup smoke test on the Linux amd64 binary and ships `SHA256SUMS` with the release package.

### Contributors

- [@neouks](https://github.com/neouks)

## [0.3.2] - 2026-08-20

### Added

- New tasks can link multiple existing tasks, inheriting facts, vulnerabilities, completed intents, asset scopes, and blackboard context from direct source tasks read-only in real time; new tasks still use independent execution queues, working directories, and session history.
- Tasks support an ordered LLM configuration chain. When provider quota exhaustion is positively identified, it auto-switches to the next configuration, persisting the current configuration, exhaustion state, and structured audit activities.
- Running and paused tasks support editing the LLM configuration chain, reordering, and manually switching the active configuration; auto-switches, manual switches, and full-chain exhaustion are surfaced on the task details page.
- Running Workers support pause, resume, and cancel. Pausing preserves blackboard data; canceling transactionally cleans up the intent plus its directly produced facts, vulnerabilities, and execution records after the Worker stops writing.
- Deleting a task can optionally clean up associated assets, traffic, vulnerabilities, and on-disk task files, with added deletion barriers, concurrency protection, and auditable deletion stats.
- Added an optional task concurrency limit; new tasks beyond the limit queue in FIFO order and auto-start once a running slot frees up.
- Added task-asset pagination, company-asset pagination, task-category stats, and paginated-intent mock contracts.
- Added the `build.sh` single-binary build script, supporting static frontend export, asset embedding, cross-platform targets, and build-version injection.

### Changed

- Task-level explicit LLM configuration chains coexist with the existing global LLM pool; explicit chains keep strict quota-failover semantics, while tasks without an explicit chain follow Agent bindings and global configuration rules.
- The Main Agent input supports auto-growing multi-line input, `Enter` to send and `Shift+Enter` for newlines, avoiding accidental sends during Chinese IME composition.
- Planner, Worker, and Main Agent share the task-level LLM runtime; the task chain uses the smallest context window among candidate models as the safe compaction threshold.
- The task details page shows running/idle/paused states based on real LLM call status; source-task facts, vulnerabilities, intents, asset references, and graph nodes are uniformly source-marked and kept read-only.
- Agent preview, task reports, and related detail views share a common Markdown rendering component.
- LLM model configuration uses card-and-drawer interaction, with the model list independently scrollable inside the drawer.
- Task pausing no longer blocks Main Agent conversations: Main Agent orchestration sessions are independent of task pausing, so new messages can still be sent while paused (pausing only terminates the currently ongoing round).
- The default wall-clock duration of a single Worker run was adjusted from 600 seconds to 1200 seconds.

### Fixed

- Removed optimistic echo from the Main Agent console in favor of pure server-side data rendering, fixing garbled messages and cross-session content leakage when paused or when sends fail.
- Opening a Main Agent session scrolls to the bottom by default and fully shows the last reply: after the last reply's full content lazy-loads and expands, it auto-pins to the bottom instead of being pushed off-screen.
- Fixed Main Agent sessions still running after a task is paused, and the orchestrating Agent not stopping the Main Agent in sync when pausing a task.
- Fixed task status badges and action buttons being out of sync when tasks complete or when the Planner, Worker, or Main Agent is actually running.
- Fixed potential late blackboard writes or leftover files arising between Worker cancellation, task deletion, and concurrent writes.
- Fixed broken Markdown preview, long-text overflow in delete confirmations, mobile widths, and misaligned buttons on some task-detail views.
- Added named termination reasons to all Agent cancellation paths; activity details can show the canceling party, final state, run rounds, elapsed time, token usage, and unreturned tool calls.
- Fixed a race where the parent context could preemptively override the named `shutdown` reason on backend shutdown, and garbled Chinese activity summaries caused by byte-based truncation.
- Fixed cancellation events with partial streaming output losing the real termination reason, while preserving content generated before cancellation.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)

[Unreleased]: https://github.com/Autumn-27/ARTEX/compare/v0.3.10...HEAD
[0.3.10]: https://github.com/Autumn-27/ARTEX/compare/v0.3.9...v0.3.10
[0.3.9]: https://github.com/Autumn-27/ARTEX/compare/v0.3.8...v0.3.9
[0.3.8]: https://github.com/Autumn-27/ARTEX/compare/v0.3.7...v0.3.8
[0.3.7]: https://github.com/Autumn-27/ARTEX/compare/v0.3.6...v0.3.7
[0.3.6]: https://github.com/Autumn-27/ARTEX/compare/v0.3.5...v0.3.6
[0.3.5]: https://github.com/Autumn-27/ARTEX/compare/v0.3.4...v0.3.5
[0.3.4]: https://github.com/Autumn-27/ARTEX/compare/v0.3.3...v0.3.4
[0.3.3]: https://github.com/Autumn-27/ARTEX/compare/v0.3.2...v0.3.3
[0.3.2]: https://github.com/Autumn-27/ARTEX/compare/v0.3.1...v0.3.2
