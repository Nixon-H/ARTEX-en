package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Worker is an LLM work agent (docs §4.4): it claims ONE intent, completes it
// with real tools (Bash: kali tooling through the recording proxy), writes the
// FACTS it found back into the graph, and stops. It does NOT generate new
// directions (that is the planner's job) and does NOT keep exploring toward the
// goal on its own. Multiple workers run concurrently as goroutines.
// WebSearchOpts is the web-search backend selection the server pushes into each
// agent (planner/worker/main). Enabled=false leaves the web_search tool off.
// Backend is "ddgs" (no key), "brave-free" (BraveKey required), "tavily"
// (TavilyKey required), or "deepseek" (DeepSeek* required, filled from the
// active LLM profile). It maps directly onto agentcore.Options.
// Proxy is a dedicated egress proxy for the search request (http/https/socks5),
// independent of the traffic-recording MITM proxy -- set it when the search endpoint
// is only reachable via a VPN/SOCKS proxy. Empty = direct.
//
// Note: the DeepSeek backend differs in nature from the other three: DeepSeek has no directly callable search API,
// search exists only inside its Anthropic-compatible messages API (web_search_20250305 server
// tool), so each search consumes one model call, and the search request is issued by the DeepSeek server \u2014
// it does not go through the local proxy, nor is it captured in traffic logs.
type WebSearchOpts struct {
	Enabled   bool
	Backend   string
	BraveKey  string
	TavilyKey string
	Proxy     string
	// DeepSeek* comes from the currently active LLM configuration (only the anthropic-format official DeepSeek endpoint),
	// it is not configured separately and changes along with the LLM configuration.
	DeepSeekBaseURL string
	DeepSeekAPIKey  string
	DeepSeekModel   string
}

type Worker struct {
	findingRecorder FindingRecorder
	prov            llm.Provider
	model           string
	workDir         string
	proxyAddr       string
	proxyCACert     string            // recording proxy's CA cert path (for WebFetch HTTPS verify)
	webSearch       WebSearchOpts     // web_search tool backend selection (off by default)
	tx              *transcript.Store // raw LLM conversation persistence (nil = off)
	window          int               // context window in tokens (for compaction)
	windowFn        func() int        // optional dynamic task-chain minimum
	maxTurns        int               // max agent turns per run (0 = unlimited)
	// runTimeout is the wall-clock budget for the main exploration of one intent
	// (0 = unlimited). When it fires, the run is cut and a settlement round is
	// forced so already-identified facts get written back instead of being lost.
	runTimeout time.Duration
	// extraTools are host-provided tools (e.g. traffic query, oast) appended to
	// the worker's graph write-back tools.
	extraTools []actool.CoreTool
	// injectConstraints resolves whether this task's operation constraints get
	// injected into the worker system prompt. Read per run so the settings toggle
	// takes effect without rebuilding the agent. nil = inject (default).
	injectConstraints func() bool
	// nonStreamingFn resolves whether this run uses the non-streaming (Complete)
	// path. Read per run so a profile/task toggle takes effect without rebuilding
	// the agent. nil = streaming (default).
	nonStreamingFn func() bool
	// noaEnabledFn resolves whether this run uses the experimental noa context-
	// compression mechanism. Read per run, like nonStreaming. nil = off (built-in
	// compaction).
	noaEnabledFn func() bool
	// maxTokensFn resolves the per-reply output cap in tokens, on the same
	// per-run basis. nil or 0 = send no cap and let the endpoint decide.
	maxTokensFn func() int
}

// WorkerSessionID returns the stable transcript key used by a worker intent.
// Worker slots are reusable, so the intent id (rather than work#N) is the
// session identity. Keep this helper public so the Worker message API and UI
// can refer to exactly the conversation that will be resumed.
func WorkerSessionID(explorationID, intentID int64) string {
	return fmt.Sprintf("exp%d-worker-i%d", explorationID, intentID)
}

const workerChatMarkerPrefix = "<!-- ARTEX_WORKER_CHAT:"

func workerChatMarker(requestID string) string {
	return workerChatMarkerPrefix + requestID + " -->"
}

func hasWorkerChatMessage(messages []llm.Message, requestID string) bool {
	marker := workerChatMarker(requestID)
	for _, message := range messages {
		if message.Role == llm.RoleUser && strings.Contains(message.Text(), marker) {
			return true
		}
	}
	return false
}

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default). Read per
// run so a profile or task-chain toggle takes effect without rebuilding.
func (w *Worker) SetNonStreaming(fn func() bool) { w.nonStreamingFn = fn }

func (w *Worker) nonStreaming() bool { return w.nonStreamingFn != nil && w.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (w *Worker) SetNoaEnabled(fn func() bool) { w.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (w *Worker) SetMaxTokens(fn func() int) { w.maxTokensFn = fn }

func (w *Worker) maxTokens() int {
	if w.maxTokensFn == nil {
		return 0
	}
	return w.maxTokensFn()
}

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the worker system prompt. nil = inject (default).
func (w *Worker) SetConstraintInject(fn func() bool) { w.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (w *Worker) wantConstraints() bool { return w.injectConstraints == nil || w.injectConstraints() }

// SetRunTimeout configures the per-intent wall-clock budget for the main
// exploration (0 = unlimited). When it fires, the SDK settlement phase still runs
// so facts are never lost to a timeout. Safe to call before Execute.
func (w *Worker) SetRunTimeout(run time.Duration) {
	w.runTimeout = run
}

// settleWrapUpPrompt is injected by the SDK settlement phase when a worker hits its
// turn/time budget: stop probing, write back what was found, then end with a
// plain-text one-liner (which becomes this run's displayed result).
const settleWrapUpPrompt = "You are about to be terminated on budget exhaustion. Do not run any more commands/probes. In order: (1) write back each piece of content you identified above but have not yet persisted -- new assets with insert_assets, exploration conclusions/facts with record_fact, confirmed findings with report_finding; (2) **finally summarize in one standalone plain-text sentence** what you did and the key conclusions reached (this sentence will be shown as the result of this run -- it must be output)."

func NewWorker(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int, extra ...actool.CoreTool) *Worker {
	return &Worker{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, extraTools: extra}
}

// defaultToolsExcept returns actool.DefaultTools() minus the named tools (by
// CoreTool.Name()). Used to trim SDK default tools an agent shouldn't have.
func defaultToolsExcept(exclude ...string) []actool.CoreTool {
	drop := make(map[string]bool, len(exclude))
	for _, n := range exclude {
		drop[n] = true
	}
	all := actool.DefaultTools()
	out := make([]actool.CoreTool, 0, len(all))
	for _, t := range all {
		if !drop[t.Name()] {
			out = append(out, t)
		}
	}
	return out
}

func (w *Worker) SetCompactionWindowResolver(fn func() int) { w.windowFn = fn }

func (w *Worker) compactionWindow() int {
	if w.windowFn != nil {
		return w.windowFn()
	}
	return w.window
}

// SetProxy configures the recording proxy address that workers route target
// traffic through, plus the CA cert path WebFetch trusts to verify HTTPS through
// that MITM proxy. Empty addr disables the hint.
func (w *Worker) SetProxy(addr, caCert string) { w.proxyAddr, w.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for this worker (off by default).
func (w *Worker) SetWebSearch(o WebSearchOpts) { w.webSearch = o }

// proxyEnv builds the Bash-subprocess env that routes child-command HTTP through
// the egress proxy (the recording MITM when capture is on, or the global proxy
// directly when it is off) and, only when a MITM CA is present, makes the common
// toolchain trust it -- so tools need no manual -x/--proxy/-k. Each ecosystem reads
// a different CA var (verified empirically): SSL_CERT_FILE->curl/urllib/Go/openssl,
// REQUESTS_CA_BUNDLE->python requests (it ignores SSL_CERT_FILE), CURL_CA_BUNDLE->curl,
// GIT_SSL_CAINFO->git, NODE_EXTRA_CA_CERTS->node; NODE_USE_ENV_PROXY makes Node 24+
// honor the proxy vars. ALL_PROXY is set too so a socks5 egress proxy (which curl
// only reads from ALL_PROXY, not HTTP(S)_PROXY) works in the capture-off path.
// Empty proxyAddr -> nil (direct, unchanged env).
func proxyEnv(proxyAddr, caCert string) []string {
	if proxyAddr == "" {
		return nil
	}
	env := []string{
		"HTTP_PROXY=" + proxyAddr, "HTTPS_PROXY=" + proxyAddr,
		"http_proxy=" + proxyAddr, "https_proxy=" + proxyAddr,
		"ALL_PROXY=" + proxyAddr, "all_proxy=" + proxyAddr, // socks5 egress: curl reads only this
		"NODE_USE_ENV_PROXY=1", // Node 24+: honor HTTP(S)_PROXY in built-in fetch/http
	}
	if caCert != "" {
		env = append(env,
			"SSL_CERT_FILE="+caCert,
			"CURL_CA_BUNDLE="+caCert,
			"REQUESTS_CA_BUNDLE="+caCert,
			"GIT_SSL_CAINFO="+caCert,
			"NODE_EXTRA_CA_CERTS="+caCert,
		)
	}
	return env
}

// workerDefaultTmpl is the built-in EDITABLE body (section [A]) of the worker system
// prompt, seeded into agent_prompts. The trafficTool block and the intermediate-artifact output specification
// are NOT here -- they are code-owned and appended by workerSystem after rendering
// (sections [B]/[C]), so editing the DB body can never drop them.
const workerDefaultTmpl = `You are the "worker" (work agent) of an authorized pentest system on a cybersecurity platform. You receive [one intent] (a one-sentence exploration direction); your sole responsibility: **complete this one intent, write your findings back to the knowledge graph, then stop and return.**

**Boundaries (red lines)**:
1. **Only work on the one intent you received**. **If while probing this intent you glimpse leads worth digging into that fall outside it** (paths leaked by error messages, points possibly linked with other assets, suspected entries of another exploit chain), **note them in one sentence in the fact's summary for the planner**.
2. Being blocked at first (payload filtered / 404 / injection with no echo) does not mean the target is fully probed -- exhaust all bypass techniques for this intent before concluding;
3. Operate only within the authorized scope. If [operational constraints] are attached at the top of the system prompt, they are the highest-priority red line: self-check before executing each command/probe, and skip anything that violates them (even if it falls within your assigned intent).

**Write back as you discover** (only what is written into the graph counts, not what stays in your head/text; write each result immediately instead of batching them at the end where step exhaustion could lose them). Three kinds of write-back -- don't mix up the graphs:
- **New assets/resources -> insert_assets (asset graph)**: subdomains / services / endpoints / fingerprints / credentials -- anything that is an asset [itself]. **Register only assets here; exploration conclusions/judgments go to record_fact, not here.**
- **Exploration conclusions/facts -> record_fact (exploration graph, pass intent_id)**: use it for all of these. **Merge multiple observations into [one] fact** (summary with a one-sentence conclusion + detail expanding on it, grounded in the actual execution); don't write one entry per attribute -- one intent normally yields just one fact, since fragmentation bloats the graph without bound -- **default to a single entry, merging everything mergeable into detail**; only when conclusions are [genuinely independent and unmergeable] should you split them via the facts array -- a rare exception, not the norm. **Write only deltas**: record only what is [newly learned] this time, don't rephrase existing facts (no need to record if it only confirms existing facts with nothing new). **Write only what you actually saw**: give evidence (one line: command + the one or two most probative output lines, concise, details in detail) and mark confidence (observed = directly seen / inferred = inferred from phenomena).
- **Confirmed vulnerability -> report_finding (exploration graph, includes PoC, pass intent_id)**: **use only when you personally triggered it this time with reproducible evidence (request/response or command output)**. Never treat "version/fingerprint matching a CVE", "parameter looking injectable", or "inference from external vuln DBs/changelogs/code diffs" as confirmed, and don't substitute CVE-DB lookups or patch-version comparisons for actual triggering. Can't trigger but suspicious -> record one inferred fact via record_fact (suspicion + why it wasn't triggered) for the planner; don't force it into a finding.


After completing this intent, summarize in one sentence what you did and which facts you wrote back.`

// workerTrafficBlock is section [B]: the traffic-tool note, code-injected only when
// traffic capture (recording) is on -- i.e. the traffic_* tools actually exist.
// Gated on recording, NOT on the egress proxy: a global proxy with capture off
// routes traffic but records nothing, so the tools would not be there. Not stored,
// not editable.
func workerTrafficBlock(recording bool) string {
	if !recording {
		return ""
	}
	return "\n\n**Traffic tools**: \n- traffic_search / traffic_get / traffic_blob: review past responses, find already-visited resources, **check traffic first, don't re-curl the same URL**. traffic_search **must specify host**, by default returns only 3 ultra-light index entries (id/method/url/status/resp_len, no response body); explicitly raise limit for more; body_contains does full-text search over request/response bodies (at least 3 chars, supports substrings and Chinese, e.g. finding passwords/keys/errors/internal addresses); to view an entry's raw text use traffic_get(id), where oversized bodies show as @blob sha256:<hash>, use traffic_blob(hash) to fetch the full text in segments."
}

// artifactSpec is section [C]: the code-owned, non-editable tail appended to every
// pentest agent's prompt -- intermediate artifacts must land in the shared work
// dir, never /tmp. Guaranteed present regardless of how the DB body is edited.
func artifactSpec(dir string) string {
	return "\n\n**Intermediate-artifact output convention**: all intermediate artifacts such as scripts, payloads, captured response bodies, and temporary data **must always be written to this task's working directory " + dir + "**(Relative paths are written here; this absolute path may also be used)** -- **don't write to /tmp, don't use other absolute paths**."
}

// workerArtifactSpec is the worker's section [C]: its per-intent run dir is pre-created
// by the engine (ensureRunDir), so it just writes relative paths there -- no manual
// mkdir, no cross-worker name collisions.
func workerArtifactSpec(runDir string) string {
	return "\n\n**Intermediate-artifact output convention**: scripts, payloads, captured response bodies, temporary data -- all intermediate artifacts, **without exception, go to this intent's dedicated working directory " + runDir + "** (already auto-created; just write here with a relative path, no need to create directories manually) -- **do not write /tmp, do not use any other absolute paths**."
}

// ensureRunDir builds and creates an agent's working directory under base:
// <base>/tasks/<taskID> for planner/main; <base>/tasks/<taskID>/i<intentID> for a
// worker (intentID<=0 -> task dir only). The "tasks/" segment groups per-task dirs
// symmetrically with the chat agent's "sessions/<sessionID>". Best-effort mkdir -- on
// failure, writes fail the same way an unwritable CWD would.
func ensureRunDir(base string, taskID, intentID int64) string {
	dir := filepath.Join(base, "tasks", strconv.FormatInt(taskID, 10))
	if intentID > 0 {
		dir = filepath.Join(dir, "i"+strconv.FormatInt(intentID, 10))
	}
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// cmdOutDir is the SDK large-tool-output spill dir under an agent's run dir.
func cmdOutDir(dir string) string { return filepath.Join(dir, "cmd-output") }

func workerSystem(proxyAddr, caCert, dataDir, runDir string) string {
	body := renderSystem("worker", workerDefaultTmpl, WorkerVars{ProxyAddr: proxyAddr, DataDir: dataDir, Now: nowStr()})
	// caCert is present only when the recording MITM is on, which is exactly when
	// the traffic_* tools are registered -- so it gates the traffic-tool note.
	// Optional finding guidance is added for every role after tool resolution.
	return body + workerTrafficBlock(caCert != "") + workerArtifactSpec(runDir)
}

// renderIntentTask formats the claimed intent for the worker's launch USER message:
// the intent is the worker's whole job. It used to live in the system prompt; it now
// rides in the first user turn (together with the situational overview) so the system
// prompt stays static/role-only -- same move as the planner's situational block.
// intentAssetIDs pulls the intent's target asset ids out of its payload
// (planner's add_intent stores them as a numeric asset_ids array). nil on absence
// or malformed payload.
func intentAssetIDs(intent *db.Node) []int64 {
	if intent == nil {
		return nil
	}
	var p struct {
		AssetIDs []int64 `json:"asset_ids"`
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return nil
	}
	return p.AssetIDs
}

func renderIntentTask(intent *db.Node) string {
	return fmt.Sprintf("\n\n[The intent assigned to you (this run's only task: do just this one, produce only facts, stop when done)]:\n%s\nIntent id: %d (pass it when writing back via record_fact / report_finding)", string(intent.Payload), intent.ID)
}

// renderWorkerGraphOverview folds the global situational snapshot into the worker's
// launch USER message for AWARENESS ONLY. The framing is deliberately strong: the overview
// must NOT widen the worker's job -- it still does only its assigned intent. Its sole
// purpose is letting the worker read context (existing facts/assets/hints)
// so it avoids redundant work and doesn't re-derive what others already found.
func renderWorkerGraphOverview(data map[string]any) string {
	// coverage is the signal for the planner to judge "which categories are under-tested / whether to expand scope", versus the worker's [duty of] "only doing the assigned
	// contradicts the responsibility boundary of "that intent; don't chase uncovered points" -> removed from the worker view. data is this worker
	// A dedicated new map; deleting keys doesn't affect the planner.
	delete(data, "coverage")
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back silently: the worker just won't have the global context
	}
	return "\n\n[Global exploration situation (read-only -- helps you place your own intent in the big picture)]:\n" +
		"Below is the current exploration overview of the whole task. It serves two purposes: first, to know what others have already found so you don't repeat it; second, so that when exploring your own intent you can relate it to the global picture.\n" +
		"**Divergence is good**: while probing this intent, think deep and associate freely. The only boundary -- don't actually go execute other intents (that is other workers' business, scheduled by the planner). Whenever you associate a valuable lead (cross-asset linkage, a suspected entry to another exploit chain, a global-level suspicious point), **be sure to write it into a fact for the planner** -- this is your key output, not optional. Rather report one extra item for the planner to judge than swallow it yourself.\n" +
		string(b)
}

// Execute runs one intent. hooks (the per-task Guard) gates every tool call; may
// be nil. emit, if non-nil, receives one ActivityRecord per execution step.
// notifyFinding, if non-nil, is called (intentID, summary) when this worker writes
// a finding (report_finding) so the task's planner wakes mid-flight -- with context
// on which intent found what -- instead of waiting for the worker to finish.
// Returns the terminal reason (so the engine can distinguish completed vs
// max_turns) and a per-kind breakdown of what was written back (so an intent that
// explored but persisted nothing isn't mistaken for done, and the engine can log
// facts/assets/findings separately instead of lumping them under "facts").
func (w *Worker) Execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string)) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, enr, notifyFinding, "", "")
}

// ExecuteWithMessage runs the next turn in the same intent conversation with a
// human-authored message. The HTTP handler does not edit the transcript;
// agentcore records the message as a normal user turn when this Worker starts.
// This keeps Worker continuation identical to the regular agent chat flow.
func (w *Worker) ExecuteWithMessage(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, enr, notifyFinding, strings.TrimSpace(requestID), strings.TrimSpace(message))
}

func (w *Worker) execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	tsx := NewToolSet(ts, name)
	tsx.SetFindingRecorder(w.findingRecorder)
	tsx.SetTaskID(taskID)
	coverageEnabled := as == nil || as.CoverageEnabled(taskID)
	tsx.SetCoverageEnabled(coverageEnabled)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetOwnerNode(intent.ID)         // assets this worker discovers anchor to its intent -> visible to the task
	tsx.SetEnrich(enr)                  // async DNS/HTTP auto-completion for assets this worker writes
	tsx.SetNotifyFinding(notifyFinding) // when report_finding lands in storage, immediately wake the planner, carrying "which intent + finding"
	// base = built-in worker tools ∪ host tools (traffic) ∪ default tools (incl. Bash);
	// then augment with the agent's visible skills/MCP. During the SDK settlement
	// phase, Bash is hidden via Settlement.DisabledTools (no local gating needed).
	base := append(tsx.WorkerTools(), w.extraTools...)
	// worker deliberately gets no MultiEdit/Glob/Grep: use Edit for precise file edits, Bash (grep/find) for search,
	// narrowing the tool surface and reducing low-value calls. All other SDK default tools (Read/Write/Edit/LS/Bash/Sleep) stay as-is.
	base = append(base, defaultToolsExcept("MultiEdit", "Glob", "Grep")...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts), IntentID: intent.ID})
	tools, def, cleanup := AugmentTools(ctx, "worker", base)
	defer cleanup()

	// The intent is the worker's [sole responsibility, invariant spanning the entire run] -> together with the launch instructions and the intent-anchored target assets
	// Put the raw data into the system prompt together: the system is reassembled on every run and will never be compacted away,
	// In a long run the intent is always present, and resuming does not depend on whether the transcript history kept that first message. The cost is that the system
	// mixes in per-intent volatile data and loses cross-intent cache reuse; this is a deliberate tradeoff (losing the intent is far worse than saving tokens).
	// The divergence from the planner's "situation block goes in the user turn" is intentional: the planner itself is the one that produces intents and has no single mandate,
	// while the worker has one. Only the [global situation overview] stays in the launch user message -- it is degradable, tolerates staleness, and losing it to compaction is harmless.
	// The dedicated working directory for this intent, <workDir>/tasks/<taskID>/i<intentID>, is created on the engine side first.
	runDir := ensureRunDir(w.workDir, taskID, intent.ID)
	// The run-wide intent is not the current tool action. Do not forward it or
	// inherit a parent run's background into the action reviewer.
	ctx = intercept.WithReviewContext(ctx, runDir, intercept.ReviewBackground{})
	overview := renderWorkerGraphOverview(tsx.graphOverviewData())
	sysBody := workerSystem(w.proxyAddr, w.proxyCACert, w.workDir, runDir)
	if w.wantConstraints() {
		sysBody += constraintBlock(ts) // Inject operational constraints (if any) into the system prompt; the worker must strictly obey them during execution
	}
	// Intent block -> intent-anchored asset block -> launch instructions, appended to the end of system in order (same append pattern as constraintBlock).
	sysBody += renderIntentTask(intent)
	if as != nil {
		if ids := intentAssetIDs(intent); len(ids) > 0 {
			if assets, err := as.GetByIDs(ids); err == nil && len(assets) > 0 {
				if b, err := json.Marshal(assets); err == nil {
					sysBody += "\n\nTarget assets corresponding to this intent’s asset_ids:\n" + string(b)
				}
				// Assets explicitly targeted by the intent -> auto-admitted into the task's test scope (same ... set as insertAssets ...)
				// conservative granularity). upsertTaskScope's ON CONFLICT DO NOTHING + uq_task_scope
				// A unique index guarantees no duplicate adds; re-runs/retries are likewise idempotent no-ops.
				// When the asset-coverage feature is off, stop accumulating test scope (the denominator).
				if coverageEnabled {
					for _, a := range assets {
						_ = as.AddAutoScope(taskID, a.Type, a.Domain, a.URL, a.IP)
					}
				}
			}
		}
	}
	sysBody += "\n\nStart executing the intent above: do only it, produce only facts, assets, and findings, then stop when done."
	system, boundary := deferredSystem(sysBody, def)
	// The task-level deadline (injected via ctx) clamps this run's wall-clock budget + determines the wrap-up wording (see taskclock.go).
	tc := taskClockFrom(ctx)
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, w.runTimeout)
	settle := wrapupSettlement("worker", []string{"Bash"})
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("worker", []string{"Bash"}, clamped)
	}
	opts := agentcore.Options{
		Provider:        w.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		// WebFetch goes through the recording proxy, with its HTTP logged just like curl; loading the proxy CA lets MITM-
		// re-signed HTTPS certificates [verify normally] (instead of disabling verification). Empty proxy means direct connection.
		EnableWebFetch: true,
		WebFetchProxy:  w.proxyAddr,
		WebFetchCACert: w.proxyCACert,
		// Web search (optional). ddgs needs no key; brave-free needs BraveKey; tavily needs TavilyKey.
		// WebSearchProxy is a separate egress proxy (http/https/socks5), independent of the traffic-recording MITM proxy; empty means direct connection.
		EnableWebSearch:       w.webSearch.Enabled,
		WebSearchBackend:      w.webSearch.Backend,
		BraveSearchAPIKey:     w.webSearch.BraveKey,
		TavilySearchAPIKey:    w.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: w.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  w.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   w.webSearch.DeepSeekModel,
		WebSearchProxy:        w.webSearch.Proxy,
		// HTTP from Bash subcommands goes through the recording proxy by default and trusts its CA (tools need no -x/-k).
		BashEnv:    proxyEnv(w.proxyAddr, w.proxyCACert),
		WorkingDir: runDir,
		MaxTurns:   w.maxTurns, // 0 = unlimited (configurable in agent management)
		// Wall-clock budget, judged at round boundaries without interrupting mid-round; 0 = unlimited. With a task-level deadline it is clamped to min(own budget,
		// remaining time to deadline), so this run winds down naturally when the task time arrives (see taskclock.go).
		MaxDuration: maxDur,
		// On hitting the budget (rounds OR duration) \u2192 the SDK runs one wind-down round (hidden Bash) to write back what was already identified, avoiding an unfinished ending.
		// When clamped (squeezed by the task deadline), use PromptByReason: timeout cause = task time reached \u2192 task-timeout wording,
		// step cause = steps exhausted first within the clamped window \u2192 fall back to per-run wording. Non-clamped stays purely per-run.
		Settlement: settle,
		// large tool output spills to cmd-output/ with a head + pointer (SDK tool.Capture);
		// full output preserved on disk. The truncation cap uses the SDK default (30,000 characters).
		ToolOutputDir: cmdOutDir(runDir),
		Compaction:    compactionConfig(w.compactionWindow()), // long tool-heavy runs stay within the window
		Todos:         actool.NewTodoStore(),                  // session-scoped temporary todolist (TodoWrite), planning-only, discarded on exit
		NonStreaming:  w.nonStreaming(),                       // when this profile selects non-streaming, route via Provider.Complete
		MaxTokens:     w.maxTokens(),                          // 0 = send no cap, decided by the server-side default
	}
	if hooks != nil { // typed-nil guard: only set when concrete (avoids harness panic)
		opts.Hooks = hooks
	}
	if w.tx != nil { // persist raw LLM conversation; one file per worked intent
		opts.Transcript = w.tx
		opts.SessionID = WorkerSessionID(ts.ID(), intent.ID)
	}
	intentID := intent.ID
	emitWrap := func(r db.Activity) {
		if emit != nil {
			r.NodeID, r.Worker = &intentID, name
			emit(r)
		}
	}
	// Intent / launch directives / intent-anchored assets have already been issued with the system prompt (see sysBody assembly above).
	// This launch user message carries only the [global-situation overview] \u2014 degradable big-picture context that is safe to compress away.
	// If overview marshaling rarely fails and yields empty, fall back to a launch phrase to avoid an empty user message in the first round.
	input := overview
	if strings.TrimSpace(input) == "" {
		input = "Start executing the intent received in system: do only it, produce only facts, assets and findings, stop when done."
	}

	// Experimental feature: when enabled, noa takes over context compaction (archives centralized under <workDir>/noa/<SessionID>, persistent).
	noaSession := WorkerSessionID(ts.ID(), intent.ID)
	enableNoa(&opts, w.noaEnabledFn, w.workDir, noaSession, noaWarn(noaSession))
	ctx = attachSideCapture(ctx, &opts)
	s := agentcore.NewSession(opts)
	defer s.Close() // release the session's background-task manager (temp dir + processes)

	// Resume prior conversation if this intent was paused/blocked/exhausted and is
	// being re-run. The transcript ID is deterministic per intent, so if a prior
	// session exists the worker continues from where it left off instead of
	// restarting from scratch.
	alreadyRecorded := false
	if w.tx != nil {
		_ = s.Resume(opts.SessionID)
		alreadyRecorded = requestID != "" && hasWorkerChatMessage(s.Messages(), requestID)
		if len(s.Messages()) > 0 && message == "" {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
			input = "Continue executing."
		} else if len(s.Messages()) > 0 {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
		}
	}
	if message != "" {
		if alreadyRecorded {
			input = "Continue executing the new intent from the last human chat input. Don't repeat actions already completed."
		} else if len(s.Messages()) > 0 {
			input = workerChatMarker(requestID) + "\n[New intent from human chat input]\n" + message +
				"\n\nExecute per this human input immediately; after finishing, decide from context whether the original task needs to continue."
		} else {
			input += "\n\n" + workerChatMarker(requestID) + "\n[New intent from human chat input]\n" + message +
				"\n\nPlease prioritize executing this human input."
		}
	}

	// Budgets + settlement are owned by the SDK (MaxTurns/MaxDuration + Settlement):
	// on hit it runs a wrap-up turn and finishes with ReasonMaxTurns/ReasonTimeout.
	// MaxDuration now interrupts an in-flight tool at the wall-clock deadline and
	// enters the wrap-up phase on the live ctx, so a run whose tool overran the budget
	// still settles (no external hard-timeout backstop needed). ctx itself carries only
	// pause / planner kill / shutdown, which the engine distinguishes and re-queues/stops.
	_, reason, err := captureRunSession(ctx, s, input, emitWrap)
	return reason, tsx.Writes(), err
}
