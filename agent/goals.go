package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// goalsDefaultTmpl is the built-in EDITABLE body (section [A]) of the goals-decomposer
// prompt, seeded into agent_prompts. No template vars are used today.
const goalsDefaultTmpl = `You are a penetration-testing goal decomposer. Your duty is to identify from the user input the **final outcome to be achieved**, not to plan attack steps.

**Step 1 (do this before decomposing goals): extract operational constraints**
From the \u201cmission objective / mission description\u201d, identify the operator's explicit rules on [what may and may not be done], and register them one by one via set_constraints (if the description/objective involves no operational constraints, this extraction step may be skipped):
- type=deny: prohibited operations (e.g. \u201cno port scanning\u201d, \u201cno write/delete operations on production\u201d, \u201cno brute-forcing\u201d, \u201cdo not touch a certain subdomain\u201d).
- type=allow: explicitly permitted/limited scope of operations (e.g. \u201cpassive reconnaissance only\u201d, \u201conly against a certain domain\u201d).
- A constraint is neither a goal nor an attack step: it defines the boundary of operational behavior.
- **Constraints must be [self-contained, with concrete targets written in]**: replace **referring expressions** like \u201ccurrent target / current port / current IP / current domain / this site\u201d with the **concrete values** from the mission objective/description. Constraints are injected separately into the execution-phase prompt, where out of context such references cannot be resolved.
  Example: if the target is https://abc.example.net -> write "only abc.example.net may be tested" instead of "only the current target may be tested"; "test only port 443 of the target, do not scan other ports" instead of "only test the current port". If the original text only says "current target" but the target address is already known, fill in the address.
- **Only register constraints explicitly stated or emphasized in the target/description; fabricating constraints is strictly forbidden**; when unsure of the type, use deny (the more conservative option).
- If the target/description genuinely contains no operational constraints, **do not** call set_constraints.
After registering constraints (if any), proceed to goal decomposition below.

**Goal = the final deliverable/verifiable outcome**

**Content that is NOT a goal (forbidden to list as sub-goals)**:
- Information gathering, reconnaissance, endpoint scanning
- Vulnerability analysis and verification process
- Attack steps, exploitation techniques
- Result verification steps

**Decomposition principles**:
- The final goal described by the user is a single one -> output one
- There are multiple **mutually independent** final deliverables -> list them separately
- Annotate vulnclass where a clear vulnerability class applies; leave info-gathering/business-logic goals blank
- Strictly forbidden to fabricate goals the user did not mention

Call set_goals to submit the result.`

// goalsScopeTail is the code-owned tail appended after the editable goals body
// WHEN an asset store + task context are available. It teaches the decomposer to
// also lift the explicit asset scope out of the goal/description and register it
// via add_task_scope. Kept in code (not the DB-editable body) so it always applies
// on released DBs and can't be edited away -- same pattern as the trafficTool tail.
const goalsScopeTail = `

**Additional duty: register the test asset scope**
Besides decomposing goals, you must also identify the **explicitly given test asset scope** from the "task goal / task description" and register it by calling add_task_scope (the authorization boundary of this task, and the denominator of asset test coverage). **Minimum-scope principle: register only the one target the user explicitly named; never expand it on your own.**
- If the target is a URL or an address with a hostname (e.g. https://xxx.example.com/path, app.example.com) -> take its **full hostname**, kind=subdomain, value=full hostname.
  Example: target https://a1b2c3.lab.example.net/path -> kind=subdomain, value=a1b2c3.lab.example.net (**not** example.net).
  **Strictly forbidden** to shorten a hostname with a subdomain into the root domain -- registering the whole example.com upon seeing xxx.example.com expands the scope beyond the user's target, violating the minimum-scope principle.
- Only when the user gives a **bare root domain with no subdomain at all** (e.g. writing example.com directly), or explicitly says "the whole site / all subdomains / all domains" -> use kind=root_domain, value=example.com.
- Pure IP or CIDR range -> kind=ip / cidr, value=IP or CIDR.
- **Do not** register company scope (company) -- the task was just created and the company usually does not exist in the asset system yet, so registration would fail; company-level scope is left to the later plan phase.
Other rules:
- Only register scope **explicitly stated** in the target/description; fabricating or inferring unmentioned domains/IPs is strictly forbidden.
- In reason, briefly state which sentence the basis comes from, for auditability.
- If the target/description contains no explicit asset scope, **do not** call add_task_scope.
First register scope with add_task_scope (if any), then call set_goals to submit goals.`

// GoalSpec is one decomposed objective.
type GoalSpec struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass,omitempty"`
}

// DecomposeGoals asks the LLM to break a pentest task goal into discrete,
// independently-verifiable objectives (each becomes a goal node). Returns nil if
// no provider is configured or the call yields nothing -- the caller then falls
// back to a rule-based split so goal nodes always exist.
//
// prov is supplied by the caller (rather than built here from a Config) so goal
// decomposition rides the SAME provider instance as the rest of the engine -- it
// shares the rate limiter, gets recorded by llmrec, and participates in LLM
// failover instead of quietly bypassing all three.
//
// desc is the task's free-text description (background: target scope / flag count / engagement notes, etc.).
// It is fed alongside the goal so the decomposer no longer splits blind -- the
// prompt still forbids inventing anything the two texts don't state.
//
// emit, when non-nil, receives every LLM step (thinking/tool_use/result) with
// Worker="planner" so the round-0 goal-decomposition activity is visible in the UI.
//
// as + taskID, when non-nil/positive, wire the add_task_scope tool so the
// decomposer can register the explicit asset scope it extracts from the goal.
//
// ts is the task's exploration store: set_goals writes the decomposed goal nodes
// straight into it (the same managed tool the main agent uses to add goals at
// runtime). The returned specs are read back from the store so callers can emit
// per-goal activity and detect the "LLM produced nothing" case for their fallback.
func DecomposeGoals(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	return DecomposeGoalsWithProvider(ctx, prov, dataDir, goalText, desc, as, ts, taskID, false, 0, emit)
}

// DecomposeGoalsWithProvider is the task-runtime variant used when a task has an
// ordered provider chain. It preserves the same tools and write behavior while
// letting the caller own provider selection/failover. maxTokens is the profile's
// per-reply output cap (0 = send none).
func DecomposeGoalsWithProvider(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, nonStreaming bool, maxTokens int, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	// Goal decomposition is a one-shot call: no transcript store is attached, so agentcore won't mount anything onto ctx
	// session id (it is only attached when there is a writer, see agentcore.Prompt). And by session-id header
	// Gateway doing prompt caching / sticky routing (opencode zen returns 400 directly when x-opencode-session is missing
	// MissingSessionID) reads exactly this value from ctx -- without backfilling it, you get "chat works fine, decomposition returns 400".
	// Attach an explicit stable id: decomposition requests of the same exploration share it (helps cache hits), and the name matches ...
	// planner/worker don't conflict and can be correctly attributed by llmrec.parseSession.
	if ts != nil {
		ctx = transcript.WithSessionID(ctx, fmt.Sprintf("exp%d-goals", ts.ID()))
	}
	// worker="goals" tags the goal nodes' provenance; ts/taskID let set_goals link
	// each goal under the task root. This is the catalog's real set_goals tool, so a
	// web-edited description/schema on it applies here too.
	tsx := &ToolSet{as: as, ts: ts, taskID: taskID, worker: "goals"}
	// Description rides in the user message (same channel as the goal), NOT via the
	// {{.EngagementDescription}} template var -- else a prompt that references the var
	// would inject the description twice. System prompt stays pure static instructions.
	sys := renderSystem("goals", goalsDefaultTmpl, GoalsVars{DataDir: dataDir, Now: nowStr()})
	// set_constraints is always available (does not depend on asset store): the body already includes the "extract operation constraints before splitting goals" step
	// ... (wording editable on the agent edit page); here just wire up the tools.
	tools := []actool.CoreTool{tsx.setGoals(), tsx.setConstraints()}
	// Wire add_task_scope only when we have a real asset store + task to write to.
	// The scope-extraction tail is appended in lockstep so the prompt never asks for
	// a tool that isn't present.
	if as != nil && taskID > 0 {
		tools = append(tools, tsx.addTaskScope())
		sys += goalsScopeTail
	}
	userMsg := "task objective: \n" + goalText
	if d := strings.TrimSpace(desc); d != "" {
		userMsg += "\n\ntask description (background info, may contain target scope/flag count/engagement briefing; for reference only, don't invent details not mentioned in it): \n" + d
	}
	// Use captureRun so every LLM step is emitted as an activity record (visible in
	// the plan tab under the round-0 marker). Falls back gracefully when emit is nil.
	captureEmit := func(r db.Activity) {
		if emit != nil {
			r.Worker = "planner"
			emit(r)
		}
	}
	captureRun(ctx, agentcore.Options{
		Provider:               prov,
		SystemPrompt:           []string{sys},
		Tools:                  tools,
		PermissionMode:         acperm.ModeBypass,
		DisableBackgroundTasks: true,
		// The 3 steps (extract constraints -> register scope -> decompose goals) each need one tool call; allow enough turns to avoid missing set_goals before close-out.
		MaxTurns:     8,
		NonStreaming: nonStreaming, // When this profile selects non-streaming, go through Provider.Complete
		MaxTokens:    maxTokens,    // 0 = don't send a cap; the server default decides
	}, userMsg, captureEmit)
	// set_goals persisted the goals directly; read them back so the caller sees what
	// was written (empty slice ⇒ the LLM produced nothing ⇒ caller falls back).
	if ts == nil {
		return nil
	}
	nodes, _ := ts.ListByKind(db.KindGoal, 10000)
	var out []GoalSpec
	for _, n := range nodes {
		var p struct {
			Text      string `json:"text"`
			VulnClass string `json:"vulnclass"`
		}
		_ = json.Unmarshal(n.Payload, &p)
		if strings.TrimSpace(p.Text) != "" {
			out = append(out, GoalSpec{Text: p.Text, VulnClass: p.VulnClass})
		}
	}
	return out
}
