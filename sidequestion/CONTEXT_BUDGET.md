# Side Long Conversations and Context Budget

2026-09-11 fix.The original implementation treated the request ... JSON Character count treated directly as token, inheriting the main task's 32K output reservation, therefore HTML, JS early-rejects normal side questions when tool results are heavy.

## Open Source Implementation Checklist

- [Grok CLI side-channel context](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/agent/agent.ts#L739): extracts snippets from recent user and assistant text, with a character budget of about 2000 and at most 400 characters per entry. No continuous side-channel Q&A history is maintained on this path.
- [Grok CLI standalone request](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/utils/side-question.ts): independent cancellation signal; output limit of 2048 tokens when supported by the model; no tools provided.
- [Grok CLI main session compaction](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/agent/compaction.ts): estimates tokens, keeps recent content, merges new content into the old summary, and handles cross-turn truncation.
- [OpenCode session compaction](https://github.com/anomalyco/opencode/blob/b3f1a96c6dd7adeb28b36dd11add1998fc84d67b/packages/core/src/session/compaction.ts): full request estimation, output/buffer reservation, recent content plus rolling summary, tool-free summary requests; this implementation defaults to a recent budget of 8000 and a summary output limit of 4096 tokens.
- [OpenCode overflow recovery](https://github.com/anomalyco/opencode/blob/b3f1a96c6dd7adeb28b36dd11add1998fc84d67b/packages/core/src/session/runner/llm.ts): only attempts overflow recovery when assistant output has not yet started; the call after recovery does not re-enter the same overflow recovery path.

ARTEX borrows the practices of independent output budgets, recent content with rolling summaries, and limited recovery. It keeps norma v0.3.6's structured messages paired with tools, without copying Grok's text excerpts; it does not write OpenCode's main session compaction events into the ARTEX main transcript.

## Request Budget and Execution

- Messages follow the existing scheme norma Per content block of UTF-8 byte-based estimation plus (a margin) 4/3 margin; plus system-prompt overhead, tool schema plus message-envelope overhead.estimates, not exact model counts token count.
- side-channel output defaults to at most 8192 tokens, Nor exceeds the output cap already set in the main configuration.Configurable via the service environment variable `ARTEX_BTW_MAX_OUTPUT_TOKENS` settings 256–32768 upper limit; does not change the product default model or main settings service params.
- Input budget is the context window minus the output limit and safety margin; unknown windows use the platform default of 200K. The safety margin is 5% of the window, minimum 128, maximum 8192 tokens.
- Successful Q&A pairs are loaded in batches of at most 20 groups by increasing sequence number. At most 20 groups of original text are kept, with a token budget of at most 1/4 of the input budget and no more than 16K.
- overflow Q&A rolled into rolling summary.summaries carry historical provenance and context timestamps; past assistant answers are not new tool evidence, on conflict prefer the latest main snapshot.
- When the main context is still too long, only old messages in the copy are summarized, keeping at most 8K tokens of recent content; truncation points never split tool calls from their results. An oversized single group enters the summary as a whole.
- Summary input is chunked in a UTF-8-safe way according to the actual remaining window, with an output limit of 2048 tokens; empty summaries, truncation, tool calls, or exceeding the summary budget are never cached. A single side-channel pass allows at most 12 summary calls under the same 120-second timeout; hitting the limit fails explicitly instead of looping forever.
- if the model first returns context-overflow before emitting any text or tool calls, retry at most once after further shrinking; if the estimated size has not decreased, stop recovery immediately.other model errors and partial stream output don't trigger this recovery.
- all usage incurred so far, Including summaries, usage from failed and cancelled attempts, accumulated onto the same side-channel request.Provider still record zero when the provider returns no usage, don't pass estimates off as actual usage.

## Persistence and UI

`side_question_sessions.memory` store old Q&A summaries, covered ordinal, And the main-context summary cached by snapshot identity.`side_question_requests.context_info` records the preparation stage, actual replayed group count, summary usage and budget estimates.

save the summary only while the original request is still running and the cleanup version matches; clearing also purges the cache, late writes will not restore cleared data.new snapshots don't reuse old snapshot summaries.summary fields are saved with (v3 task archives) v3 saved with task archives; restoring old v3 fill in empty objects for missing fields, v1/v2 remain compatible.

POST Admit and return the request first, Preparation and compression run in the background, Holds neither the admission lock nor a DB transaction.SSE/history shows the preparation stage, organize the Q&A, compressed copy, answering phase; compression failure saved as the failed end state.frontend keeps the error state and restores the draft of this failed question, without floating notifications obscuring the input box; history polling no longer clears submit errors.

## Validation Records

- Replay of 19/20/21/50 groups, retention of old conclusions across 20 groups, summary cache reuse after restart: automated checks passed.
- overlong Chinese answers and code context, Chunked-request budget, tool pairing, snapshots unchanged, new-snapshot cache invalidation: auto-passed.
- summarization failed/Cancel/truncation/overlong/tool returned, clear race, call limit, only one overflow recovery, no retry for partial streams: auto-passed.
- independent PostgreSQL Pagination of, restart, v1/v2/v3 archive, summary cache and budget-metadata archive/restore, old v3 missing new fields, 20 N parent sessions share four concurrency slots: pass.
- Go candidate service build, frontend TypeScript check, Biome check of modified components, and Next.js production build in an isolated directory: passed.
- The built-in browser uses an isolated UI fixture, verifying the tidy-up stage, summary-range hints, draft recovery after failure, no floating error notifications, no horizontal overflow, and a clean console at 1280×720 and 390×844. Temporary fixtures have been removed.
- Read-only replay of the locally existing ... Worker snapshot, new budget check passed; for example Worker #3 's 293085 character snapshots no longer wrongly rejected by local char counting.This item made no external model calls.
- This run keeps data private Worker snapshots sent to Grok real-conversation test rejected by automated approval review, not executed, Not counted as a passing case.
- 2026-09-11 00:37 restarted the local backend at the user's request, reusing the original database, data directory, and login configuration. The running binary matches the candidate binary SHA-256; both the backend and frontend proxy `/api/health` return healthy.

Validation commands (using the isolated test library only):

```sh
go test -race ./sidequestion ./db ./server -run 'TestSide|TestCheckpoint|TestSnapshot|TestBuildRequest|TestService|TestMainSide|TestTaskArchive' -count=1
go build ./cmd/artex
npx tsc --noEmit
npm run build -- --webpack
```

The frontend production build uses an isolated copy to avoid overwriting the current preview's `.next`. The candidate service is located at `/private/tmp/artex-btw-budget-candidate`, copied to `/private/tmp/artex-btw-preview/artex` and started; the original binary is backed up as `artex.before-context-budget` in the same directory.
