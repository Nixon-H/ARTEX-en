# `/btw` Verification Records

Date: 2026-09-10. Branch: `codex/btw-side-question`. Baseline: `8dae851b9b622f2ff2631f332fde9719d0b16fba`.

use an isolated PostgreSQL Test database and data directory; real model credentials are only injected into an isolated test environment, Not written into code or this record, also No modificationsproduct default model.Go 1.26.3, norma v0.3.6, Next.js 16.2.9.

The actual model conversations, returned objects, engineering assertions, and Qwen's original review text are saved in [validation-2026-09-10.json](validation-2026-09-10.json), which contains no API credentials.

## Engineering Checks

| scope | result | Evidence |
| --- | --- | --- |
| structured messages, deep copy of tool parameters | pass | `TestCheckpointDeepCopyAndBoundaries` |
| summary / Compression requests do not overwrite ..., full replies and final-state publishing, Partial replies excluded | pass | `TestCheckpointDeepCopyAndBoundaries`, `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| actual model-pool member identity | pass | `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| tool pairing, 20 group replay, budget trimming and overflow errors | pass | `TestBuildRequestCompactionToolPairingAndBudget` |
| Main and side channels run in parallel, isolated bidirectional cancellation | pass | blocking Provider, `TestMainSideConcurrencyAndIndependentCancellation` |
| no tool execution, streaming / non-streaming, usage already incurred at failure time | pass | `TestServiceNoToolsAndUsageOnFailure` |
| real norma ChatAgent + local Read tool, main transcript / Activity isolation | pass | `TestSideActualChatCheckpointToolResultAndTranscriptIsolation`, Streaming and non-streaming subcases |
| persistence, pagination, idempotent, restarts preserve partial answers | pass | `TestSideHistoryIdempotencyPagingAndRecovery` |
| clear vs late-write race, parent resource deletion, version comparison | pass | `TestSideClearLateWritersAndDeletedParent` |
| MainAgent / Worker archive and restore, v1/v2/v3 | pass | `TestSideTaskArchiveVersions` |
| Three parent interfaces, auth, resource ownership, Worker soft delete | pass | `TestSideHTTPGlobalLimitTaskWorkerAndDeletion`, `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| busy main sessions support side-channel (operation), independent SSE reconnect / disconnect, Cancel, Clear | pass | `TestSideHTTPBusyIsolationClearAndReconnect` |
| Per parent session 1 / global 4 concurrency | pass | two `TestSideHTTP...` test case |
| snapshots persisted before admission, resume questions after restart, Old sessions cannot forge snapshots | pass | `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| refuse to continue when cached profile deleted or model changed | pass | `TestSideRejectsDeletedOrChangedCachedProfile` |
| cancel before archiving and wait for the final answer and usage to persist | pass | `TestSideTaskDrainPersistsBeforeArchive` |
| streaming consumers that cancel early record usage once with side-channel attribution | pass | `TestSideUsageRecordedOnceOnConsumerCancellation` |
| auto-restored on restart Worker / deadline runtime keeps publishing new snapshots | pass | `TestSideRestoredWorkerRuntimePublishesNewCheckpoint` |
| related packages race check | pass | The following commands |
| TypeScript And the production build | pass | `npx tsc --noEmit`, `npm run build` |
| new frontend module Biome | pass | `biome check`, 3 new modules added |

After configuring `ARTEX_PG_DSN` in a separate disposable database, the automated checks can be reproduced (do not point at the production database):

```sh
go test -race ./agent ./db ./server ./sidequestion ./llmrec ./llmpool \
  -run 'Test(Side|Checkpoint|Snapshot|BuildRequest|Service|MainSide|CaptureRun|TaskArchive|CompleteForwards|StopIntent|CancelIntent)' -count=1
cd web
npx tsc --noEmit
npx biome check src/lib/side-questions.ts src/hooks/use-side-questions.ts src/components/side-question-workspace.tsx
npm run build
```

The full Go regression is not all green: the `server` package has two pre-existing tests failing at the temp-directory cleanup stage, both reporting `TempDir RemoveAll ... directory not empty`:

- `TestInheritedActivityDetailAndRelationDeletion`
- `TestTaskMetadataPatchReturnsRenameAndPin`

Exporting the source from the above unmodified baseline and re-running the `server` package in the same isolated environment reproduces these two cleanup failures. The baseline run additionally shows a target node count assertion failure in `TestCoreTaskLifecyclePG`; the final modified `server` regression does not have that assertion failure. Other packages pass, and this round's side-path cases and race checks pass. Baseline issues were not marked as accepted for this round, nor were pre-existing assertions modified to hide issues.

Next.js Pre-existing build output with multiple warnings lockfile / workspace root inferred warnings; build completed and all pages generated.

## Browser Checks

Using the Codex In-app Browser, connected to a standalone local Go service and a Next.js dev server. On desktop and a 390 × 844 narrow screen, the following manual automated operations were completed, checking screenshots and browser logs:

Typing `/btw` while a normal chat is running shows the main content and the side path at the same time; the desktop sidebar is normal.
Follow-up questions in succession; after the side path stops, the already generated portion is kept; the main flow continues.
Requests continue when the panel is closed, and the completed answer is restored after reopening; after a page refresh, an empty `/btw` restores history.
- narrow screen Drawer inputs, button, history and close actions work, no horizontal overflow.
- clearing requires a confirmation dialog, history disappears after clearing, main transcript main transcript and snapshots are kept.
- Tasks MainAgent With two Worker Ask separately and switch between them, Agent Tags and history stayed isolated.
The blocking local-model fixture keeps the Worker running; submitting `/btw` from the Worker's main input box and then stopping the side path, the Worker still shows live running status and its own pause button, and the side path saves the partial answer.
Browser error/warning logs are empty.

Controlled fixtures for precisely verifying concurrency timing, Does not depend on the real model's output speed.twice during debugging Worker runtime check formed no valid concurrency window (task has ended / answer ended early), fixed fixtures, redid, and passed; don't count these initial operations as valid passes.

## Real Model Conversations

Probed `grok-4.6` first, OpenAI-compatible endpoint `http://127.0.0.1:12580/tingly/openai`. The probe returned HTTP 200 with model name `grok-4.6` and `READY`, taking 2.82 seconds. Since the first choice was available, neither the Tingly `glm` nor the Zhipu `glm-5.3` fallback chain was enabled; these two fallback services were not verified this time.

| scenario | actual results |
| --- | --- |
| Query assets while the main session is running, Target, mark | return `redhaze.top`, read the homepage and summarize the target, `BTW-REAL-0910`; side channel completed, 16.97 seconds |
| main session asks for the tool evidence after loading the homepage | Correctly cited WebFetch 200, curl redirect 301 -> 302 -> 200, page title; 7.24 seconds |
| Side-question requirement Bash requirement to create test files via Bash | refused to execute, target file not created; 7.74 seconds |
| completed side channels don't alter the main context | main transcript SHA-256 Consistent with the primary activity record; Side tool execution count is 0 |
| actually stopped / restart Go Follow-up questions after a service restart | Keep the previous ... 3 side-channel history entries, answer assets directly from persisted snapshot, marks and titles, without re-running the main agent Agent |
| new sessions use Grok non-streaming config | Correctly answered the assets and `ATOMIC-0910`; return and save usage: input 11734, output 138, cache_read 11520 |

In the asset case, the main session used WebFetch and Bash/curl to read the public homepage, landing on `https://id.redhaze.top/home` with the title "Red Curtain Technology RedHaze Group · Global Integrated Group Portal". Bash staged the response in a local test file; no writes were executed to the remote end. This fact was verified separately from "the side path executed no tools".

Main transcript checksum: `e7e61f135a4a120954b539f357e8c4205d7d5cd7460dcaf3dc0fd066463e1d00`.

**Usage limitation:** Tingly's Grok streaming response returned no usage. A separate direct send of `stream_options.include_usage=true` verified HTTP 200, 12 data frames, and 0 usage frames. So the 0 in the streaming tests means the endpoint provided no usage and must not be read as unbilled. Non-streaming usage and the fixture's failure/cancellation usage were all saved correctly.

## Qwen Review

Review model `qwen-flash`, OpenAI-compatible endpoint `https://dashscope.aliyuncs.com/compatible-mode/v1`, HTTP 200. Provided with the first three real side-path conversations, the main-session tool basis, and the engineering assertions; returned `verdict: accept`, `concerns: []`, judging the answers consistent with the asset, marker, and page-read evidence and the side-path tool refusal compliant with the constraints. Review usage: prompt 6625, completion 312, total 6937.

This Qwen review did not cover the later-added service-restart and non-streaming tests. Qwen's "no writes" generalization was too broad: the main-session curl did create a local response temp file, as explicitly recorded above. Concurrency, zero tool execution, and transcript isolation are judged by the engineering assertions; the model review only assists in evaluating answer quality.
