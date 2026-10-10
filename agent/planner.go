package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Planner is the event-driven LLM planner (docs §4.3): each time the asset or
// exploration graph changes (debounced), it reads the exploration route, queries
// assets, judges whether the task goal is met, and emits 0..N exploration intents
// into the frontier. It is the sole intent generator.
type Planner struct {
	findingRecorder   FindingRecorder
	prov              llm.Provider
	model             string
	tx                *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window            int                                    // context window in tokens (for compaction)
	windowFn          func() int                             // optional dynamic task-chain minimum
	maxTurns          int                                    // max agent turns per run (0 = unlimited)
	killWork          func(intentID int64) error             // engine callback to terminate a running work (nil = off)
	steerWork         func(intentID int64, msg string) error // engine callback to steer a running work mid-run (nil = off)
	proxyAddr         string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert       string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch         WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir           string                                 // shared work dir (surfaced in prompt as artifact-output target)
	injectConstraints func() bool                            // resolver: inject task operation constraints into system prompt? (nil = yes)
	nonStreamingFn    func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn      func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn       func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
	compactor         *Compactor                             // cold-node compaction (§7); nil = disabled

	// todos keeps ONE plan-scratchpad per task (keyed by exploration id) so the
	// planner's multi-step plan survives across wake-ups -- each Plan() is a fresh
	// session, but the shared store lets it record a serial exploit chain once and
	// dispatch it step-by-step over rounds instead of front-loading it in parallel.
	todoMu sync.Mutex
	todos  map[int64]*actool.TodoStore
}

func NewPlanner(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *Planner {
	return &Planner{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, todos: map[int64]*actool.TodoStore{}}
}

func (p *Planner) SetCompactionWindowResolver(fn func() int) { p.windowFn = fn }

// SetCompactor wires the cold-node compactor (cold-digest §7). Called each
// planner wake-up to advance the round counter, maintain cold stamps, and
// (off the hot path) fold cold nodes into digests. nil = feature disabled.
func (p *Planner) SetCompactor(c *Compactor) { p.compactor = c }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (p *Planner) SetNonStreaming(fn func() bool) { p.nonStreamingFn = fn }

func (p *Planner) nonStreaming() bool { return p.nonStreamingFn != nil && p.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (p *Planner) SetNoaEnabled(fn func() bool) { p.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (p *Planner) SetMaxTokens(fn func() int) { p.maxTokensFn = fn }

func (p *Planner) maxTokens() int {
	if p.maxTokensFn == nil {
		return 0
	}
	return p.maxTokensFn()
}

func (p *Planner) compactionWindow() int {
	if p.windowFn != nil {
		return p.windowFn()
	}
	return p.window
}

// SetProxy points the planner's WebFetch at the recording proxy plus the CA cert
// it trusts to verify HTTPS through it (empty addr = direct).
func (p *Planner) SetProxy(addr, caCert string) { p.proxyAddr, p.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the planner (off by default).
func (p *Planner) SetWebSearch(o WebSearchOpts) { p.webSearch = o }

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the planner system prompt. Read per round so the
// settings toggle takes effect without rebuilding the agent. nil = inject (default).
func (p *Planner) SetConstraintInject(fn func() bool) { p.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (p *Planner) wantConstraints() bool { return p.injectConstraints == nil || p.injectConstraints() }

// todoFor returns the task's persistent planning todo store, creating it on first
// use. Shared across all of this task's planner wake-ups.
func (p *Planner) todoFor(expID int64) *actool.TodoStore {
	p.todoMu.Lock()
	defer p.todoMu.Unlock()
	s := p.todos[expID]
	if s == nil {
		s = actool.NewTodoStore()
		p.todos[expID] = s
	}
	return s
}

// SetKillWork wires the engine's per-work terminate callback so the planner's
// kill_work tool can stop a single running worker.
func (p *Planner) SetKillWork(fn func(intentID int64) error) { p.killWork = fn }

// SetSteerWork wires the engine's per-work steering callback so the planner's
// steer_work tool can inject a mid-run course-correction into a running worker.
func (p *Planner) SetSteerWork(fn func(intentID int64, msg string) error) { p.steerWork = fn }

// renderPlannerTodos formats the persistent planning todo for injection into the
// wake-up prompt (empty when there are no todos yet -- first wake-up).
func renderPlannerTodos(items []actool.Todo) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n[Your planning todolist (kept across wake-ups; written by you last round)]:\n")
	for _, it := range items {
		mark := map[actool.TodoStatus]string{actool.TodoPending: "☐", actool.TodoInProgress: "▶", actool.TodoCompleted: "✔"}[it.Status]
		if mark == "" {
			mark = "☐"
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", mark, it.Content))
	}
	b.WriteString("Advance accordingly: dispatch intents only for next steps whose 【prerequisite steps are done / depended-on facts already exist】; update the checklist with TodoWrite (mark steps already satisfied by facts completed). Don't re-dispatch steps already pending/in_progress in the checklist.")
	return b.String()
}

// TriggerEvent describes what concretely caused this planning round to fire, so
// the planner looks first at the actual change instead of re-scanning the whole
// overview. Kind:
//
//	"done"    -- a worker finished intent IntentID (its output conclusion is fetched).
//	"finding" -- a worker reported a finding on intent IntentID (Detail = summary).
//	"goal"    -- the human (via the main agent's set_goals) added one OR MORE goals in a
//	            single call (Goals = newly added goal text this time, 1+ items; set_goals supports batch).
//	"goal_deleted" -- the human deleted a goal from the Overview goal management (Detail = deleted goal text).
//	"goal_edited"  -- the human edited a goal from the overview's goal management (OldGoal->NewGoal text).
//	"cancelled" -- the human deleted intent IntentID (Detail = deletion reason). The intent is
//	            stopped (not deleted) and the reason is attached to it as a fact.
type TriggerEvent struct {
	Kind     string
	IntentID int64
	Detail   string
	Summary  string   // Dedicated to Kind=="cancelled": capture the intent summary before deletion (after real deletion the node no longer exists and can't be queried)
	Goals    []string // Dedicated to Kind=="goal": goal text newly added by this set_goals (one or more)
	OldGoal  string   // Dedicated to Kind=="goal_edited": goal text before modification
	NewGoal  string   // Dedicated to Kind=="goal_edited": goal text after modification
	Hints    []string // Dedicated to Kind=="hint": hint text newly added by this add_hint (one or more)
}

// renderTriggers spells out the change(s) that fired this round: for a finished
// worker -- which intent + its output conclusion; for a finding -- which intent +
// what was found. Empty for time/heartbeat wakes. Reads the store (best-effort;
// a blank field never blocks the round).
func renderTriggers(ts *db.ExplorationStore, evs []TriggerEvent) string {
	if len(evs) == 0 || ts == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n[Actual changes that triggered this round (read here first, then decide whether to add directions)]:")
	for _, ev := range evs {
		switch ev.Kind {
		case "goal":
			if len(ev.Goals) == 1 {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added a goal: %s -- a new goal to achieve; please add exploration directions accordingly (if there is no corresponding intent yet).", ev.Goals[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added %d goals: %s -- all are new goals to be achieved; please add exploration directions one by one for goals that have no corresponding intent yet.", len(ev.Goals), strings.Join(ev.Goals, "; ")))
			}
		case "hint":
			if len(ev.Hints) == 1 {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added a strategic hint: %s -- it has been attached to the exploration graph; please adjust/supplement exploration directions accordingly (if there is no corresponding intent yet).", ev.Hints[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- human (main agent)added %d strategic hints: %s ---- all attached to the exploration graph, adjust one by one accordingly/add exploration directions.", len(ev.Hints), strings.Join(ev.Hints, "; ")))
			}
		case "goal_deleted":
			b.WriteString(fmt.Sprintf("\n- A human deleted the goal: %s -- the goal has been removed; re-judge remaining goals/directions accordingly (no need to dispatch intents for it anymore).", ev.Detail))
		case "goal_edited":
			b.WriteString(fmt.Sprintf("\n- A human changed the goal from \"%s\" to \"%s\" -- please adjust the exploration direction to the new goal (stop dispatching along the old direction if it no longer applies).", ev.OldGoal, ev.NewGoal))
		case "finding":
			b.WriteString(fmt.Sprintf("\n- The worker of intent #%d (%s) reported a finding: %s", ev.IntentID, intentSummary(ts, ev.IntentID), ev.Detail))
		case "cancelled":
			// For intent content, prefer the Summary captured at deletion time (after a hard delete the node no longer exists and intentSummary cannot find it).
			sm := ev.Summary
			if sm == "" {
				sm = intentSummary(ts, ev.IntentID)
			}
			b.WriteString(fmt.Sprintf("\n- Intent #%d deleted by the user, intent content is: %s, deletion reason is: %s.this intent was deleted (will no longer run); please replan accordingly.", ev.IntentID, sm, ev.Detail))
		default: // "done"
			b.WriteString(fmt.Sprintf("\n- The worker of intent #%d (%s) finished, with conclusion: %s", ev.IntentID, intentSummary(ts, ev.IntentID), workerOutput(ts, ev.IntentID)))
			if fids := factIDsYielded(ts, ev.IntentID); fids != "" {
				b.WriteString(fmt.Sprintf("; fact ids newly produced by this intent: %s ", fids))
			}
		}
	}
	b.WriteString("\n(For full details, check again with node_detail / get_worker_output / list_findings.)")
	return b.String()
}

// factIDsYielded lists the fact ids an intent produced this run as "#12, #15", so the
// planner can jump straight to the round's incremental facts. Empty (best-effort) when
// the intent yielded no facts or the lookup fails.
func factIDsYielded(ts *db.ExplorationStore, id int64) string {
	ids, err := ts.FactsYielded(id)
	if err != nil || len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, fid := range ids {
		parts[i] = fmt.Sprintf("#%d", fid)
	}
	return strings.Join(parts, ", ")
}

// intentSummary reads an intent node's one-line summary (best-effort, "?" on miss).
func intentSummary(ts *db.ExplorationStore, id int64) string {
	n, err := ts.GetNode(id)
	if err != nil || n == nil {
		return "?"
	}
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if s, ok := p["summary"].(string); ok && s != "" {
			return s
		}
	}
	return "?"
}

// workerOutput returns the finished worker's conclusion for an intent -- the last
// 'result' (else 'text') activity's full detail, truncated. Same source get_worker_output uses.
func workerOutput(ts *db.ExplorationStore, id int64) string {
	acts, _, err := ts.ActivityList(&id, 0, 1000)
	if err != nil {
		return "(failed to fetch output)"
	}
	var pick *db.Activity
	for i := range acts {
		if acts[i].Kind == "result" {
			pick = &acts[i]
		} else if acts[i].Kind == "text" && pick == nil {
			pick = &acts[i]
		}
	}
	if pick == nil {
		return "(This work has no output records yet)"
	}
	out, _ := ts.ActivityDetail(pick.ID)
	if out == "" {
		out = pick.Summary
	}
	return truncOutput(out, 800)
}

// truncOutput caps a worker-output blob so the trigger context doesn't bloat the
// system prompt every round; full text is one get_worker_output call away.
func truncOutput(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " ... (truncated; see get_worker_output for the full text)"
}

// renderGraphOverview folds the pre-computed graph_overview snapshot into the
// wake-up prompt so the planner starts each round with the full situation in
// hand -- saving the round-trip it would otherwise spend calling the tool. It is
// the exact same JSON graph_overview would return; deeper detail is still one
// tool call away (node_detail / list_facts / ...).
func renderGraphOverview(data map[string]any) string {
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back to the model calling graph_overview itself
	}
	return "\n\n[Current-round posture (prefetched graph_overview, identical to the return value of calling that tool; call node_detail / list_facts etc. as needed for details)]:\n" + string(b)
}

// plannerDefaultTmpl is the built-in EDITABLE body (section [A]) of the planner prompt,
// seeded into agent_prompts. Goal is a {{.Goal}} template var; the intermediate-artifact output specification
// tail is code-owned (artifactSpec) and appended by plannerSystem after rendering.
const plannerDefaultTmpl = `You are the "planner" of an authorized pentest system on a cybersecurity platform, woken frequently (whenever the graph changes). Duties: read the situation -> judge goals -> add exploration intents **only when there is genuinely an uncovered new direction**. You are the planner, not the executor: all output this round may only be [generating/clarifying intents] or [judging goals]; never do the work inside the plan.

task objective: {{.Goal}}

**How many intents to produce this round (think this through first)**:
- **Hard floor (top priority)**: whenever [goals are unmet] and [there is currently no open or running intent] (frontier_open=0 and running_intents is empty), this round [must] produce at least one goal-advancing intent -- with no running work to wait on and no queued directions, producing 0 intents = task stall; even if all known directions only exist in recent_done, open a new one or dispatch a follow-up per the done/exhausted/blocked judgments below.
- Beyond the hard floor, **producing 0 intents is a normal outcome, but needs proper justification** (not a "fewer is safer" default): ① **Covered** -- every direction you can think of is already handled by still-open/running intents (regenerating an existing intent in different words is a serious error); ② **Waiting on a dependency** -- the next step depends on output from currently running work that hasn't arrived yet (forcing dispatch now would leave downstream spinning without its prerequisite; wait until the next wake-up with an updated graph).
- conversely: confirmed【uncovered, with no dependency on running work work】new directions, or the goal is unmet with untested areas still in scope, then dispatch it----don't treat 0 using zero intents as the lazy default.

**Decision flow on each wake-up**:

1. **The full situation is attached below this prompt** (exactly the graph_overview return; no need to call it again): task (original title + goals/root node), asset count, goals + status, open/running/recent_done intents, sites_without_endpoints (sites with no endpoints, hinting at possibly unexplored directions), facts (exploration-fact count, a separate category from vulnerabilities), recent_facts ({id,summary,confidence?}).
   - **Scope**: exploration nodes (goals/intents/facts/findings) belong to this task only; **the asset graph is globally shared** (one copy across tasks; asset counts are global in-scope ones, not exclusive to this task) -- ignore assets unrelated to this task.
   - **lineage**: Each intent carries parents (upstream: which facts it derives from/Intent)and yields (downstream: which facts were produced/Findings), recent_facts each carries from_intent; use this to understand"Which facts came from which directions, and whether they can be combined into new directions".
   - **Negative/doubtful observations** ("port closed/uninjectable" etc. in recent_facts) are worker observations, not verdicts: before trusting one, check evidence via node_detail(id) -- only treat the direction as temporarily closed when the evidence is solid, confidence=observed, and means are exhausted; with missing evidence, mere "looks like it / probed only once", or confidence=inferred, treat as [not yet established] and, if in scope with no other intent covering it, dispatch one recheck intent to confirm or overturn it (**at most one recheck per negative direction**; if it stays negative after the recheck with reasonable evidence, honor that conclusion and stop dispatching).
   - **Call for deeper detail only as needed**: list_facts (paginated, newest first, default 20, q filtering, before paging, with total/has_more), list_findings (all vulnerabilities), node_detail(id) (full evidence/details; lists/recent_facts give summaries only), list_assets (pull: q search, type/company_id/task_id filters, pagination, or direct fetch by id/ids), asset_neighbors. assets are globally shared; don't pull everything by default.

2. **Judge goals (core duty)**: the goals field already carries goals and statuses; for an unmet goal proven by some finding/fact, call prove_goal(goal_id, evidence_id, reason) to mark it met. **When the one you mark happens to be the last unfinished goal, the system automatically judges the whole task complete** -- closure is driven only by proving goals one by one via prove_goal; there is no other "one-click finish".
   - ⚠️ **Quantitative acceptance check (no premature sign-off)**: when a goal carries quantifiable conditions (coverage reaching X%, capturing N flags, gaining some privilege), [you must] check the measured values from graph_overview above (coverage.pct, findings_total counts, etc.) before prove_goal: if the bar is missed, prove_goal is [forbidden] -- dispatch intents to close the gap instead; never mark met early on grounds of "roughly achieved / core captured". Example: 100% coverage required but measured coverage.pct=40% -> unmet, keep dispatching retest intents.

3. **(Optional, opening-only, extremely lightweight) probe-to-understand**: only when the graph has almost no facts yet (recent_facts basically empty, task just started) and the situation alone can't pin down the initial intent, use Bash etc. for a tiny number of read-only probes on the target (e.g. 1–2 curls to view the homepage/fingerprint). **The only legitimate output is one more precise intent description** -- never vulnerability discovery/validation/exploitation, nor endpoint/directory/parameter enumeration results (those are worker jobs; write them as intents and dispatch). Three hard boundaries:
   - The graph already has worker-produced facts (facts>0 / recent_facts non-empty) -> [forbidden] to probe yourself; base every judgment on existing facts, and this round's output may only be "dispatch new intents" or "end"; to dig into a lead -> dispatch an intent for a worker to check, not curl yourself.
   - Even at opening, probe at most ≤3 times then stop, only to pin down the initial intent; once you notice "deep verification" instead of "quick scoping" (enumerating endpoints/directories one by one, trying ids one by one, decode chains, repeatedly probing the same endpoint, any injection/bypass/vulnerability test or validation -- all heavy worker jobs), stop immediately and write it up as an intent.
   - Anything decidable from existing facts/situation needs no probing at all.

4. **Decide which new directions to add**: **"restraint" here means only [don't repeat existing intents], not "dispatch as few as possible"** -- while goals are unmet, the default follow-up is "to approach the goal, what deeper, harder, not-yet-covered tactics remain", not "can we wrap up". An intent is an [open exploration direction] (not a fixed type/menu); judge directions from known facts, assets and goals, and compare each against open + running + recent_done:
   - Already covered by open/running -> don't generate (being handled).
   - Seen in recent_done -> **first look at that intent's state (carried on each) to tell how it stopped, then decide**:
     - **done (ran to completion)**: covered -> don't redispatch as-is; whether it's a dead end depends on the fact conclusions it yields, not the state; redispatch only on [materially new mechanics] (new facts/assets/parameters/distinctly different tactics), with summary spelling out the difference from last time; rewording or "one more try might work" doesn't count -- retries forbidden.
     - **exhausted (budget ran out, cut off mid-probe, only partially written back) / blocked (model or network failure, barely probed anything)**: both ended mid-way with incomplete information -- first use get_worker_trace / get_worker_output to see how far it actually got and where it stuck, then choose: cut off by budget near a breakthrough -> dispatch "continue from last progress"; pure external failure with nothing done (often the case for blocked) -> redispatch the same direction directly; stuck at the same place every time -> change tactics/direction. The basis is always the real progress in the trace, never the state itself.
   - brand-new directions with no intent coverage at all -> generate.
   - All known directions are covered by still-open/running intents -> generate nothing, end directly (there is running/queued work; wait for it to advance); but if only recent_done covers them with no open/running left while goals are unmet -> the top hard floor requires opening a new one or dispatching a follow-up.
   - **depth over coverage**: coverage Is the lower bound/acceptance criterion, Not an exploration goal in itself; high-value entry points found (may lead to RCE/privilege escalation/data exfiltration)after, prioritize dispatching intents to drive that path deep【drive deep along that path】, not spreading thin just to even out coverage, shallow per-asset testing.
   - **Keep routes diverse, don't converge too early**: while goals are unmet, if existing intents all crowd one route/entry while a [fundamentally different] uncovered direction exists (another entry surface / another asset class / another exploit chain), prefer adding that divergent direction over piling synonymous intents onto the same line (judge by substantive difference, not wording); if that divergent direction is already covered by existing intents, still generate nothing. Ideally 2–3 mechanistically different routes coexist (e.g. "via the upload chain" vs "via auth bypass"), concentrating resources only after one yields [goal-approaching] evidence. **But diversity always obeys the top [operational constraints]**: entry surfaces/ports/hosts/operations excluded by constraints never get intents, however different.

   **Serial exploit chains: dispatch step by step, never split into parallel.** For strongly dependent serial chains (①->②->③, each step depending on the previous step's actual output): don't dispatch everything in parallel at once (downstream will only duplicate/spin idly without prerequisites that don't exist yet); record the whole chain with TodoWrite as to-dos (one per step), dispatch only the step whose prerequisites are met this round (usually the first), and after it yields facts, dispatch the next step on the next wake-up (the prompt will carry the to-do list), marking satisfied ones completed. Never split "the same thing" into two ("confirming the trigger" and "triggering the trigger" are one step); use multi-intent parallelism only for [parallel, mutually independent] dimensions (e.g. enumerating unrelated endpoints).

5. **submit**: use【in a single call】add_intent submit shortlisted directions in one batch (intents array, at most 4 N highest-value items, Do not invoke repeatedly for individual items): 
   - **summary**: one sentence of natural language describing the direction (full address of the test target + what to do + why), no fixed categories; dedup mainly compares it against existing intents.
   - **asset_ids**: ids of the target assets to test/attack in this direction (pass whenever possible: 0/1/many, from list_assets) -- pass them whenever the direction centers on concrete assets (sites/endpoints/parameters/hosts), for coverage dedup and linking into the asset chain; pass all for cross-asset directions; leave empty only for purely global recon with no concrete assets.
   - **parent_ids**: which upstream nodes this direction synthesizes from (optional, 0/1/many) -- pass all when several facts combine into one intent, pass its id when derived from some upstream intent/finding, leave empty for top-level brand-new directions.

No repeats, no forced filler; but when goals are unmet and deeper uncovered tactics remain, dispatch when due. Concise, focused, efficient.`

func plannerSystem(goal, dataDir, workDir string) string {
	body := renderSystem("planner", plannerDefaultTmpl, PlannerVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir)
}

// Plan runs one planning round. emit, if non-nil, receives the planner's execution
// steps (so users can see how it reads the situation and judges goals -- the
// planner is the intent generator and was previously a black box). Returns whether
// the planner judged the goal met.
// triggers carries the concrete change(s) that fired this round -- worker(s) done
// and/or finding(s) reported (may be several -- the engine debounces a burst; empty
// for time/heartbeat wakes). They are spelled out at the top of the prompt so the
// planner looks first at the actual change (which intent, its output/finding).
func (p *Planner) Plan(ctx context.Context, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, goal string, triggers []TriggerEvent, emit func(db.Activity)) (met bool, reason string, err error) {
	// cold-digest §2.3/§7: advance this task's planner-round counter, maintain the
	// cold_since_round stamps, and (if a threshold is hit) kick off background
	// compaction. Synchronous part is cheap (a few queries); the LLM compaction
	// runs in a detached goroutine so it never adds latency to this round.
	p.compactor.OnPlannerRound(ctx, ts)
	tsx := NewToolSet(ts, "planner")
	tsx.SetFindingRecorder(p.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.killWork = p.killWork   // enable kill_work tool (nil = unavailable)
	tsx.steerWork = p.steerWork // enable steer_work tool (nil = unavailable)
	if origin, _ := ts.OriginFactID(); origin > 0 {
		tsx.SetOwnerNode(origin) // planner-side anchors default to the task root (origin fact)
	}
	// Domain tools + the base default toolset (Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash)
	// When the asset-coverage feature is off, exclude add_task_scope/list_untested_assets (not injected into the prompt).
	base := append(tsx.DropCoverageTools(tsx.PlannerTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "planner", base)
	defer cleanup()
	// Key posture (just-completed intent + prefetched full graph) is moved to [this round's user input] (see input below); system
	// Keep only the static planning body. move-out keeps system stable each round and more cache-friendly; the cost is that if a single round grows long, the posture may
	// Compressed by compaction (a single planner round is usually short, low risk). situational will be spliced into the input below.
	situational := renderTriggers(ts, triggers) + renderGraphOverview(tsx.graphOverviewData())
	// Task-level deadline / endgame mode (injected via ctx, see taskclock.go). In the endgame round, the task timeout
	// The planner's closing words are spliced into this round's user input (along with situational) as the [operation instructions for this round], so that it only does the final
	// Goal adjudication; produces no new intents.
	tc := taskClockFrom(ctx)
	if tc.Final {
		situational += "\n\n【endgame wrap-up (Special instructions for this round, override the regular planning flow above)】: " + resolveTaskTimeoutWrapup("planner")
	}
	// This task's working directory <workDir>/tasks/<taskID>; create it first.
	taskDir := ensureRunDir(p.workDir, taskID, 0)
	ctx = intercept.WithReviewContext(ctx, taskDir, intercept.ReviewBackground{})
	sysBody := plannerSystem(goal, p.workDir, taskDir)
	if p.wantConstraints() {
		sysBody += constraintBlock(ts) // Operation constraints (if any) injected into the system prompt, framing the exploration boundary
	}
	system, boundary := deferredSystem(sysBody, def)
	// The planner has no wall-clock budget of its own; when a deadline exists, clamp MaxDuration down to the remainder so the running planning round... [fragment, truncated at tail]
	// Enter wrap-up when the task clock expires (on timeout -> task-timeout wording; on steps -> per-run wording).
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, 0)
	settle := wrapupSettlement("planner", nil)
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("planner", nil, clamped)
	}
	opts := agentcore.Options{
		Provider:        p.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // go through the logging proxy for traceability; load the proxy CA to verify MITM re-signed HTTPS certificates
		WebFetchProxy:   p.proxyAddr,
		WebFetchCACert:  p.proxyCACert,
		// Web search (optional). ddgs needs no key; brave-free needs BraveKey; tavily needs TavilyKey.
		// WebSearchProxy is an independent egress proxy (http/https/socks5), unrelated to the traffic-logging MITM proxy; empty means direct connection.
		EnableWebSearch:       p.webSearch.Enabled,
		WebSearchBackend:      p.webSearch.Backend,
		BraveSearchAPIKey:     p.webSearch.BraveKey,
		TavilySearchAPIKey:    p.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: p.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  p.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   p.webSearch.DeepSeekModel,
		WebSearchProxy:        p.webSearch.Proxy,
		BashEnv:               proxyEnv(p.proxyAddr, p.proxyCACert), // Bash child commands use the proxy by default + trust the CA
		WorkingDir:            taskDir,                              // working directory of this task <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(taskDir),
		MaxTurns:              p.maxTurns, // 0 = unlimited (configurable in agent management)
		MaxDuration:           maxDur,     // 0=unlimited; with a deadline = remaining time until the deadline
		Compaction:            compactionConfig(p.compactionWindow()),
		// Planning todolist shared across wake-ups: lets the serial chain persist across rounds (session is new, store is not).
		Todos: p.todoFor(ts.ID()),
		// On hitting the 【current round】 step budget -> SDK runs wrap-up: land the conclusions already worked out in this round (dispatch add_intent,
		// provable prove_goal, serial chain recorded via TodoWrite), instead of stopping planning -- the planner will still be woken repeatedly afterwards.
		// When clamped (squeezed by the task deadline), switch to PromptByReason (see wrapupSettlementForTask).
		Settlement:   settle,
		NonStreaming: p.nonStreaming(), // when this profile selects non-streaming, go through Provider.Complete
		MaxTokens:    p.maxTokens(),    // 0 = no cap sent, decided by the server-side default
	}
	if p.tx != nil { // persist raw LLM conversation; one accumulating file per task's planner
		opts.Transcript = p.tx
		opts.SessionID = fmt.Sprintf("exp%d-planner", ts.ID())
	}
	// Experimental feature: when enabled, noa takes over context compaction (archives centralized under <workDir>/noa/<SessionID>, persistent).
	noaSession := fmt.Sprintf("exp%d-planner", ts.ID())
	enableNoa(&opts, p.noaEnabledFn, p.workDir, noaSession, noaWarn(noaSession))
	// The situation (just-finished intents + full graph) is now assembled into this round's user input (see input below). user also contains
	// instructions + cross-wake todolist (todos are the model's own planning notes, regenerable -- user is fine).
	// The opening line splits in two based on [whether this round has concrete changes]: changes -> point to the [actual changes] block below; no changes
	// (heartbeat scheduled patrol / hint / recovery, etc.) -> don't falsely claim "the graph has changed"; instead prompt a casual re-check of running intents.
	lead := "There are new concrete changes (see [Actual changes triggering this round] below); plan the next steps accordingly:"
	if len(triggers) == 0 {
		lead = "This round is a **scheduled-inspection (heartbeat due) / no concrete change signal** wake-up -- the graph may not have any new changes. Also review running intents: use steer_work to correct ones with no progress for a long time or that have drifted, use kill_work to cut losses when the whole direction is wrong; then assess goals and decide whether to add directions:"
		// On heartbeat/no-change wake-up, if the whole graph has no open or running intent left -> exploration has stalled (no worker running, [fragment, truncated at tail]
		// ... nor any queued directions). Tell the planner explicitly and force it to add new directions this round -- don't just re-check running intents and then idle through a round.
		if active, err := ts.HasActiveIntent(); err == nil && !active {
			lead = "This round is a wake-up from a **scheduled patrol (heartbeat tick)**, with currently **no open or running intents at all** -- no worker running, no queued directions; exploration has stalled. You **must** produce one or more new intents in this round that advance the goal and **do not duplicate** existing intents in the graph (producing 0 intents is forbidden); first judge from the situation below whether the goal is achieved, and if not, immediately add directions:"
		}
	}
	input := lead + situational + "\n\nJudge goals from the situation above. When a goal is [truly achieved] (target outcome obtained / target vulnerability confirmed), mark it with prove_goal one by one. **Hard baseline: as long as goals remain unachieved and there is currently no open or running intent (frontier_open=0 and running_intents empty), this round must produce at least one intent advancing the goal -- with no running work to wait for and no queued direction, producing 0 intents = task stall. Only when open/running intents are already in progress, or goals are achieved, may this round produce no new intent.**" +
		renderPlannerTodos(opts.Todos.List())
	// MaxDuration now interrupts running tools when the wall clock expires and enters wrap-up in place (on the live ctx); a single stuck round no longer ...
	// ... bypasses wrap-up; no external hard-ctx fallback needed. ctx carries only pause / kill / shutdown.
	_, _, err = captureRun(ctx, opts, input,
		func(r db.Activity) {
			if emit != nil {
				r.Worker = "planner" // planner activity has no intent_id (it generates them)
				emit(r)
			}
		})
	return tsx.GoalMet, tsx.Reason, err
}
