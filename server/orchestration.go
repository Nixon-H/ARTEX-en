package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// jsonResult marshals v to a JSON tool result.
func jsonResult(v any) (actool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(string(b)), nil
}

// This file implements the P2 "cross-task orchestration toolset" (docs/benchmark orchestration §2 P2). These are host tools -- requiring...
// It touches the Manager (any task's Store), the Engine (pause), and the task-creation flow, so it lives in the server layer.
// Read-type tools redirect "existing per-task tools" to run on the target task's store (building a temporary ToolSet...
// And Call its corresponding tool), thereby reusing exactly the same logic; control-type (spawn/pause) calls go to Manager/Engine directly.
// They are seeded into the tools table like traffic tools and bound per agent (bound only to the orchestration agent to be visible).

// hostTools is the runtime host-tool provider fed to ToolAugment: traffic tools
// (gated by capture) + cross-task orchestration tools + user-defined custom tools.
// The second return is the names of custom tools flagged `deferred` (schema
// withheld, routed via SearchExtraTools/ExecuteExtraTool). Per-agent binding still
// decides who actually sees any of them.
//
//nolint:unused // used as the hostTools provider in wireAgentAugment
func (s *Server) hostTools() ([]actool.CoreTool, map[string][]string) {
	tools := append(s.m.HostTools(), s.orchestrationTools()...)
	tools = append(tools, s.findingRetestTools()...)
	tools = append(tools, s.platformTools()...) // Platform operation tools (create/modify skill/tool/MCP, for Auto use)
	custom, err := s.customTools()
	if err != nil {
		log.Printf("[custom-tool] load failed: %v", err)
		return tools, nil
	}
	tools = append(tools, custom...)
	// deferred custom tools -> name -> its bound agent keys. ToolAugment turns a
	// name into a deferred entry only for agents it's actually bound to (so we don't
	// advertise a tool the per-agent binding will drop from the callable set).
	deferred := map[string][]string{}
	rows, _ := s.m.pg.ListCustomTools()
	for _, t := range rows {
		if t.Deferred && t.Enabled {
			deferred[t.Key] = t.Agents
		}
	}
	return tools, deferred
}

// orchestrationTools returns the cross-task tool set. Bound per-agent via the
// tools table (default: no binding -- opt-in for orchestration agents).
func (s *Server) orchestrationTools() []actool.CoreTool {
	return []actool.CoreTool{
		s.toolListTasks(),
		s.toolListLLMProfiles(),
		s.toolSpawnTask(),
		s.toolPauseTask(),
		s.toolGetTaskGraph(),
		s.toolListTaskFindings(),
		s.toolAddHint(),
		s.toolGetWorkerTrace(),
		s.toolListWorkerTraces(),
		s.toolSearchWorkerTraces(),
		s.toolGetTaskNodeDetail(),
		s.toolUpdateFindingReport(),
		s.toolGetFindingTraffic(),
		s.toolBindFindingTraffic(),
	}
}

// --- schema helpers ---

func strParam(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// parseProfileID reads an LLM profile id from a tool arg that may arrive as a JSON
// number (5) or a numeric string ("5"); returns 0 when absent/unparseable.
func parseProfileID(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		v, _ := strconv.ParseInt(strings.TrimSpace(str), 10, 64)
		return v
	}
	return 0
}

func objSchema(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		req := make([]any, len(required))
		for i, r := range required {
			req[i] = r
		}
		m["required"] = req
	}
	return m
}

func roTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		ReadOnly:   func(json.RawMessage) bool { return true },
		Concurrent: func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

func wrTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

// delegateToTask resolves the `task_id` in the input, builds a ToolSet bound to
// that task's store, strips task_id, and calls the chosen per-task tool -- so the
// cross-task read reuses the exact in-task logic against another task.
func (s *Server) delegateToTask(ctx context.Context, in json.RawMessage, pick func(*agent.ToolSet) actool.CoreTool) (actool.Result, error) {
	var head struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal(in, &head)
	if strings.TrimSpace(head.TaskID) == "" {
		return actool.Errorf("task_id is required"), nil
	}
	t, ok := s.m.Task(head.TaskID)
	if !ok {
		return actool.Errorf("task does not exist:" + head.TaskID), nil
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(in, &m)
	delete(m, "task_id")
	inner, _ := json.Marshal(m)
	tsx := agent.NewToolSet(t.Store, "orchestrator")
	if s.m.Assets() != nil {
		tsx.SetAssetStore(s.m.Assets(), s.m.Assets().Companies())
	}
	tsx.SetNotify(t.Notify)         // Generic wake-up (write ops without a dedicated callback go through it; read tools are a no-op)
	tsx.SetNotifyHint(t.NotifyHint) // add_hint -> record a "human added N strategic hints: ..." trigger and wake the planner
	return pick(tsx).Call(ctx, inner, nil)
}

// --- tools ---

func (s *Server) toolListTasks() actool.CoreTool {
	return roTool("list_tasks",
		"List all tasks (id/description/goal/status/runtime/parent task/LLM config); the orchestration agent uses it to grasp the whole picture -- which tasks have been stuck too long and which LLM each uses. Runtime: running = creation->now, terminal = creation->last activity (seconds). llm_profile: the config name used by the task's planner/worker; (active config) = follows global activation.",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			lastAct, _ := s.m.PG().LastActivityAll()
			// id -> name to resolve each task's pinned LLM profile.
			profName := map[int64]string{}
			if profs, err := s.m.pg.ListProfiles(); err == nil {
				for _, p := range profs {
					profName[p.ID] = p.Name
				}
			}
			out := make([]map[string]any, 0)
			for _, t := range s.m.List() {
				status := s.deriveTaskStatus(t)
				end := lastAct[t.ExpID]
				if live := s.engine.LastActivity(t.ID); live > end {
					end = live
				}
				dur := int64(0)
				if status == "running" {
					dur = time.Now().Unix() - t.CreatedAt
				} else if end > t.CreatedAt {
					dur = end - t.CreatedAt
				}
				row := map[string]any{"id": t.ID, "description": t.Description, "goal": t.Goal, "status": status, "run_seconds": dur}
				if t.ParentRef != "" {
					row["parent_ref"] = t.ParentRef
				}
				llmState := t.llmStateSnapshot()
				if llmState.ProfileID == nil {
					row["llm_profile"] = "(Active config)"
				} else if n, ok := profName[*llmState.ProfileID]; ok {
					row["llm_profile"] = n
				} else {
					row["llm_profile"] = fmt.Sprintf("#%d(deleted)", *llmState.ProfileID)
				}
				out = append(out, row)
			}
			return jsonResult(out)
		})
}

// toolListLLMProfiles lists the available LLM profiles (name/model/active) so an
// orchestration agent can pick one for spawn_task's llm_profile. Never leaks keys.
func (s *Server) toolListLLMProfiles() actool.CoreTool {
	return roTool("list_llm_profiles",
		"List the available LLM configs (profiles): id, name, model, format, whether currently active. Use the id in spawn_task's llm_profile_id parameter to assign a subtask-specific LLM (e.g. a cheap model for recon, a strong model for exploitation). Excludes API Keys.",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			profs, err := s.m.pg.ListProfiles()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			out := make([]map[string]any, 0, len(profs))
			for _, p := range profs {
				out = append(out, map[string]any{
					"id": p.ID, "name": p.Name, "model": p.Model, "format": p.Format, "is_active": p.IsDefault,
				})
			}
			return jsonResult(map[string]any{"profiles": out})
		})
}

func (s *Server) toolSpawnTask() actool.CoreTool {
	return wrTool("spawn_task",
		"Create a subtask and start the exploration engine, returning task_id. For dispatching one matter (e.g. one challenge / one target) as an independent task. parent_ref is optional: fill in the current orchestration-associated parent task id for parent-child linkage.",
		objSchema(map[string]any{
			"description":            strParam("Task description (short title)"),
			"goal":                   strParam("Task goal (what to achieve)"),
			"parent_ref":             strParam("Optional: parent task ID (for parent-child linkage)"),
			"source_task_ids":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": fmt.Sprintf("Optional: read-only inherited source task id list (max %d). Subtasks may read-only reference these tasks' established assets/conclusions as a starting point; unlike parent_ref's pure parent-child pointer, this is content inheritance.", db.MaxTaskSourceCount)},
			"llm_profile_id":         map[string]any{"type": "integer", "description": "Optional: specify the LLM config id used by this subtask's planner/worker (see list_llm_profiles); if left empty, inherit from the parent task, then fall back to the globally active config"},
			"timeout_seconds":        map[string]any{"type": "integer", "description": "Optional: task-level timeout (seconds). When it fires, graceful wind-down is triggered and the timeout terminal state is entered; blank or 0 = no limit"},
			"plan_heartbeat_seconds": map[string]any{"type": "integer", "description": "Optional: planner heartbeat trigger interval (sec). If the time since the last planning round ended / the task started reaches this value with no trigger in between -> trigger one planning round (deadlock fallback + wake up to supervise in-flight workers). Blank or 0 = default 600 (10min);"},
			"seed_first_intent":      map[string]any{"type": "boolean", "description": "Optional: for simple tasks this can be enabled to dispatch one seed intent at creation (content = description + target) so the worker starts testing without waiting for the first planner round; default false (standard plan-then-execute)."},
		}, "description", "goal"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Description          string          `json:"description"`
				Goal                 string          `json:"goal"`
				ParentRef            string          `json:"parent_ref"`
				SourceTaskIDs        []string        `json:"source_task_ids"`
				LLMProfileID         json.RawMessage `json:"llm_profile_id"`
				TimeoutSeconds       int             `json:"timeout_seconds"`
				PlanHeartbeatSeconds int             `json:"plan_heartbeat_seconds"`
				SeedFirstIntent      bool            `json:"seed_first_intent"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Description) == "" {
				a.Description = "Unnamed task"
			}
			if strings.TrimSpace(a.Goal) == "" {
				return actool.Errorf("goal is required"), nil
			}
			if a.TimeoutSeconds < 0 {
				a.TimeoutSeconds = 0
			}
			// Read-only inherited source tasks: count cap + each id valid/deduped/existing, validation rules consistent with HTTP task creation.
			if len(a.SourceTaskIDs) > db.MaxTaskSourceCount {
				return actool.Errorf(fmt.Sprintf("At most %d associated tasks may be selected", db.MaxTaskSourceCount)), nil
			}
			sourceIDs := make([]int64, 0, len(a.SourceTaskIDs))
			seenSources := map[int64]bool{}
			for _, raw := range a.SourceTaskIDs {
				id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
				if err != nil || id <= 0 || seenSources[id] {
					return actool.Errorf("Associated task ID invalid or duplicated"), nil
				}
				if _, ok := s.m.Task(strconv.FormatInt(id, 10)); !ok {
					return actool.Errorf(fmt.Sprintf("Associated task #%d does not exist", id)), nil
				}
				seenSources[id] = true
				sourceIDs = append(sourceIDs, id)
			}
			// LLM profile resolution: explicit id > inherit parent's pin > active(nil).
			var pin *int64
			if id := parseProfileID(a.LLMProfileID); id > 0 {
				if _, ok := s.loadProfileConfig(id); !ok {
					return actool.Errorf(fmt.Sprintf("LLM configuration #%d does not exist or has no API key set", id)), nil
				}
				pin = &id
			} else if a.ParentRef != "" {
				if pt, ok := s.m.Task(a.ParentRef); ok {
					pin = pt.LLMProfileID
				}
			}
			var llmIDs []int64
			if pin != nil {
				llmIDs = []int64{*pin}
			}
			t, err := s.m.CreateTaskWithOptions(a.Description, a.Goal, db.TaskCreateOptions{
				SourceTaskIDs:        sourceIDs,
				LLMProfileIDs:        llmIDs,
				TimeoutSeconds:       a.TimeoutSeconds,
				PlanHeartbeatSeconds: a.PlanHeartbeatSeconds,
			})
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if a.ParentRef != "" {
				t.ParentRef = a.ParentRef
				if id, e := strconv.ParseInt(t.ID, 10, 64); e == nil {
					_ = s.m.PG().SetParentRef(id, a.ParentRef)
				}
			}
			// Shared post-creation flow, reusing the same launchTask block as HTTP task creation (server.go createTask):
			// seed + visibly-in-background goal decomposition (round 0 / LLM steps / per-goal) + engine.Run.
			// seed_first_intent defaults to false (standard plan-then-execute); simple tasks may enable it to dispatch a work item directly as a test.
			s.launchTask(t, a.Description+" "+a.Goal, a.SeedFirstIntent)
			return actool.Text(fmt.Sprintf("task created: %s", t.ID)), nil
		})
}

func (s *Server) toolPauseTask() actool.CoreTool {
	return wrTool("pause_task", "Pause the specified task (stop its planner/worker loop).",
		objSchema(map[string]any{"task_id": strParam("Task ID to pause")}, "task_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID string `json:"task_id"`
			}
			_ = json.Unmarshal(in, &a)
			t, ok := s.m.Task(a.TaskID)
			if !ok {
				return actool.Errorf("task does not exist:" + a.TaskID), nil
			}
			if _, err := s.applyTaskControlWithCause(t, "pause", agent.AbortPausedByOrchestrator); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("task paused: " + a.TaskID), nil
		})
}

func (s *Server) toolGetTaskGraph() actool.CoreTool {
	return roTool("get_task_graph", "Read the exploration graph overview of a given task (same as graph_overview: asset counts/frontier/findings/coverage, etc.); specify the task with task_id.",
		objSchema(map[string]any{"task_id": strParam("Task ID")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GraphOverviewTool)
		})
}

func (s *Server) toolListTaskFindings() actool.CoreTool {
	return roTool("list_task_findings", "Read a given task's confirmed vulnerabilities (with flags/PoCs; each carrying id/task_id/intent_id/vulnclass/severity/summary/status); specify the task with task_id.",
		objSchema(map[string]any{"task_id": strParam("Task ID")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListFindingsTool)
		})
}

func (s *Server) toolAddHint() actool.CoreTool {
	return wrTool("add_task_hint", "Inject a strategic hint into the specified task (that task's planner will read it when generating intents next round).\n"+
		"★Prioritize batching: put multiple prompts into the hints array in one submission (returns an ids array, same length and order as hints, failed items get id=0); for a single prompt, omit hints and give top-level text directly.",
		objSchema(map[string]any{
			"task_id":      strParam("Task ID"),
			"hints":        map[string]any{"type": "array", "description": "【Use this first】hint array; each element's fields match the top level (text/asset_ids/traffic_refs).", "items": objSchema(map[string]any{"text": strParam("Hint content"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, "traffic_refs": agent.HintTrafficSchema()})},
			"text":         strParam("[Single] prompt content"),
			"traffic_refs": agent.HintTrafficSchema(),
			"asset_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "Anchored asset ids (optional, 0/1/multiple; asset ids within this task)"},
		}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).AddHintTool)
		})
}

func (s *Server) toolGetWorkerTrace() actool.CoreTool {
	return roTool("get_task_worker_trace",
		"View a work item (intent)'s execution process in a given task: get_task_worker_trace(task_id, intent_id) for step summaries; then pass step_ids=[...] to fetch full content of those steps (max 5 at a time; extras return only the first 5).",
		objSchema(map[string]any{
			"task_id":   strParam("Task ID"),
			"intent_id": map[string]any{"type": "integer", "description": "Intent ID (the work item within this task)"},
			"step_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "Optional: step ids to fetch full content for (max 5 at a time; extras return only the first 5, with the rest listed in omitted_step_ids)"},
		}, "task_id", "intent_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GetWorkerTraceTool)
		})
}

func (s *Server) toolListWorkerTraces() actool.CoreTool {
	return roTool("list_task_worker_traces", "List which work (intents) have run in the specified task + their step counts, to find which work is worth reviewing (then use get_task_worker_trace).",
		objSchema(map[string]any{"task_id": strParam("Task ID")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListWorkerTracesTool)
		})
}

func (s *Server) toolSearchWorkerTraces() actool.CoreTool {
	return roTool("search_task_worker_traces", "Search the execution history of all work in the specified task by keyword (returns matching step summaries + intent_id).",
		objSchema(map[string]any{"task_id": strParam("Task ID"), "q": strParam("Search keyword")}, "task_id", "q"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).SearchWorkerTracesTool)
		})
}

func (s *Server) toolGetTaskNodeDetail() actool.CoreTool {
	return roTool("get_task_node_detail",
		"Read the full content of an exploration-graph node in the specified task (finding/fact/intent/goal: summary + details/evidence/PoC). id is the exploration node id (e.g. as returned by report_finding, or an id in list_task_findings). Before writing a vulnerability report, use it to fetch that vulnerability's complete evidence.",
		objSchema(map[string]any{
			"task_id": strParam("Task ID"),
			"id":      map[string]any{"type": "integer", "description": "Exploration-graph node id (not an asset id)"},
		}, "task_id", "id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).NodeDetailTool)
		})
}

// toolUpdateFindingReport writes/overwrites a finding's detailed Markdown report.
// finding_id is the id report_finding returned ("finding recorded: <id>", the
// finding node id). The write (SetFindingReportByNodeID) is keyed by node_id and
// task-agnostic, so this host tool needs no task_id / exploration store.
func (s *Server) toolUpdateFindingReport() actool.CoreTool {
	return wrTool("update_finding_report",
		"Write/update the [detailed report] for a registered vulnerability (full Markdown text, replacing the old content wholesale). Pass report_finding's returned id as finding_id (the number in \"finding recorded: <id>\"). Suggested report contents: vulnerability overview, impact, reproduction steps, evidence/PoC, fix recommendations.",
		objSchema(map[string]any{
			"finding_id":       map[string]any{"type": "integer", "description": "Target vulnerability id (the id returned by report_finding)"},
			"report":           strParam("Full detailed report, Markdown format"),
			"evidence_version": map[string]any{"type": "integer", "description": "The evidence version returned by get_finding_traffic; used to prevent reports from overwriting newer evidence changes"},
		}, "finding_id", "report"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				EvidenceVersion *int64          `json:"evidence_version"`
				FindingID       json.RawMessage `json:"finding_id"`
				Report          string          `json:"report"`
			}
			_ = json.Unmarshal(in, &a)
			nodeID := parseProfileID(a.FindingID) // Reuse the "number or numeric string" parsing
			if nodeID <= 0 {
				return actool.Errorf("finding_id invalid"), nil
			}
			n, err := s.m.pg.SetFindingReportVersionByNodeID(ctx, nodeID, a.Report, a.EvidenceVersion)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if n == 0 {
				return actool.Errorf(fmt.Sprintf("No vulnerability record found for finding_id=%d (register it with report_finding first)", nodeID)), nil
			}
			return actool.Text(fmt.Sprintf("finding %d report updated (%d chars)", nodeID, len(a.Report))), nil
		})
}

// deriveTaskStatus mirrors listTasks' status derivation for the list_tasks tool.
func (s *Server) deriveTaskStatus(t *Task) string {
	lifecycle := t.lifecycleSnapshot()
	switch {
	case isTerminalStatus(lifecycle.Status):
		return lifecycle.Status
	case lifecycle.Paused || s.engine.IsPaused(t.ID):
		return "paused"
	case s.engine.ReadyFor(t) && s.engine.Started(t.ID):
		return "running"
	}
	return "created"
}

// orchestrationToolSeeds seeds the cross-task tools into the tools table so they
// are bindable per-agent (default: bound to nobody -- opt-in for orchestration
// agents). First-insert only, like the traffic seeds.
func (s *Server) seedOrchestrationTools() {
	// task-op + platform tools default-bind to the built-in Auto agent (it is inherently used to
	// operating platforms). SeedTool takes effect on first insert; rows already seeded in old databases are backfilled by seedAutoDefaultBindings.
	autoAgents, _ := json.Marshal([]string{"auto"})
	for _, t := range s.orchestrationTools() {
		schema, _ := json.Marshal(t.InputSchema())
		bindings := autoAgents
		if t.Name() == "bind_finding_traffic" {
			bindings = json.RawMessage(`["reporter"]`)
		}
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, bindings)
	}
	for _, t := range s.platformTools() {
		schema, _ := json.Marshal(t.InputSchema())
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, autoAgents)
	}
	s.refreshBuiltinToolSchemas()
	s.seedAutoDefaultBindings()
	s.seedPlannerDefaultBindings()
	s.seedPlannerListAssetsBinding()
	s.seedCompanyScopeRebind()
	s.seedWorkerReadToolsUnbind() // unbind list_facts/list_companies/list_worker_traces from worker defaults (one-off)
	s.seedWorkerReadbackRebind()  // fix accidental deletion by an old migration: rebind search_all_worker_traces/get_worker_trace/node_detail back to worker (one-off)
	s.seedAutoReportFindingBinding()
	s.unbindGoalMetDefault()
	s.reseedGoalsPrompt()             // goals prompt adds the "extract operation constraints" step -> append a new default version to old databases (one-off)
	s.reseedMainAgentPrompt()         // mainagent prompt adds "after goals are met, ask back via add_intent whether to create goals" (one-off)
	s.reseedPlannerPrompt()           // planner prompt: rewrite the legitimate reasons for "0 intents" + add quantified acceptance checklist (one-off)
	s.reseedWorkerPrompt()            // worker prompt: add evidence threshold for negative conclusions (one-off)
	s.seedReporterAgent()             // provision the "report writing" agent + tool bindings + finding trigger (one-off)
	s.upgradeReporterTriggerMessage() // backfill migration for old databases: have reporter return evidence_version (one-off)
	s.seedFindingTrafficTools()       // add optional evidence parameters and read-only evidence tools, preserving user config
	s.seedFindingWorkflowTools()
	// Note: the default tool bindings for pentest need no migration -- BuiltinToolSeeds already seeds them during fresh initialization
	// list_assets/insert_assets/report_finding/list_findings/list_companies together with
	// pentest (the project has no legacy databases, so no migration).
}

// refreshBuiltinToolSchemas propagates code schema/description changes on the
// orchestration + platform tools into already-seeded rows ONCE per version flag --
// SeedTool is first-insert-only, so a new param (e.g. spawn_task's llm_profile) never
// reaches an old DB otherwise. Preserves each tool's agent binding + enabled flag.
// Bump the flag whenever these tools' schemas/descriptions change in code.
func (s *Server) refreshBuiltinToolSchemas() {
	const flag = "tool_schema_refresh_v7_list_facts_paging"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	tools := append(s.orchestrationTools(), s.platformTools()...)
	for _, t := range tools {
		schema, _ := json.Marshal(t.InputSchema())
		if err := s.m.pg.RefreshToolDefaults(t.Name(), t.Description(), schema); err != nil {
			log.Printf("[tools] refresh %s schema failed: %v", t.Name(), err)
		}
	}
	// Also reset some built-in agent tools to code defaults:
	//   - goal_met: the description seeded in old databases carries the misleading "end this planning round" phrasing, which makes planner treat it as...
	//     ... a means of "ending idle rounds"; ... would misjudge the whole task as complete right after startup.
	//   - insert_assets: new related input parameter (marks whether an asset is relevant to the current task, deciding coverage inclusion),
	//     SeedTool first-insert-only; otherwise schemas already seeded in old databases would never receive this new parameter.
	//   - list_facts: changed to pagination, adding limit/before/q inputs; old databases with an already-seeded empty schema otherwise
	//     Shows 'no parameters' on the tool-management page, and the model cannot get descriptions of these parameters either.
	refreshBuiltin := map[string]bool{"goal_met": true, "insert_assets": true, "list_facts": true}
	for _, sd := range agent.BuiltinToolSeeds() {
		if !refreshBuiltin[sd.Key] {
			continue
		}
		schema, _ := json.Marshal(sd.Schema)
		if err := s.m.pg.RefreshToolDefaults(sd.Key, sd.Desc, schema); err != nil {
			log.Printf("[tools] refresh %s desc failed: %v", sd.Key, err)
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
	log.Printf("[tools] Refreshed orchestration/platform tool schema to code defaults (one-shot)")
}

// unbindGoalMetDefault removes goal_met's default "planner" binding ONCE (guarded by
// a settings flag), so existing DBs match the new default of NO agent. goal_met bypasses
// per-goal prove_goal to declare the whole task done -- powerful/risky and redundant with
// the prove_goal->auto-complete path -- so it ships unbound; users can re-bind it per agent
// in the UI. A user's own binding to another agent is untouched (we only strip planner).
func (s *Server) unbindGoalMetDefault() {
	const flag = "goal_met_unbind_default_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("planner", "goal_met"); err != nil {
		log.Printf("[tools] goal_met failed to unbind planner: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// reseedGoalsPrompt refreshes the goals-decomposer prompt to the [current code default] -- because the default body newly added
// The "extract operational constraints (set_constraints) before breaking down goals" step; but SeedPromptIfEmpty is insert-on-first-only, so on old databases
// Existing version 1 never receives this step. Here version management [appends a new version] and switches to it (ResetPromptToDefault),
// Old versions remain in history; if the user customized them they can be recovered from version records. settings flag guard -> run only once;
// If the default changes again later, bump this flag. Fresh databases need no handling (SeedPromptIfEmpty has already seeded the latest default).
func (s *Server) reseedGoalsPrompt() {
	const flag = "goals_prompt_constraint_step_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // Only one attempt regardless of outcome
	a, err := s.m.pg.GetAgentByKey("goals")
	if err != nil || a == nil {
		return // When a brand-new database has no agent rows yet, seedPrompts seeds the latest defaults directly; this migration is unneeded
	}
	tmpl := agent.BuiltinPromptSeeds()["goals"]
	if tmpl == "" {
		return
	}
	// Fresh databases: seedPrompts already seeded the latest defaults -> current version already equals code defaults; no need to append a duplicate version.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] goals prompt failed to refresh to the new default: %v", err)
		return
	}
	log.Printf("[prompts] goals prompt appended with a new default version (adding the extraction-operation constraint step, one-time)")
}

// reseedMainAgentPrompt refreshes the mainagent prompt to the [current code default] -- the default body newly adds "all goals
// When add_intent delivers an intent directly after achievement,the guidance "ask back whether to register it as a formal goal"; but SeedPromptIfEmpty is insert-on-first-only
// only,Old databases that already have versions won't receive it. Use version management to [append a new version] and switch to it (ResetPromptToDefault); old versions remain
// Kept in history; if the user customized them they can be recovered from version records. settings flag guard -> run only once. Fresh databases need no handling.
// (SeedPromptIfEmpty has already seeded the latest default). Fully isomorphic with reseedGoalsPrompt.
func (s *Server) reseedMainAgentPrompt() {
	const flag = "mainagent_prompt_goalless_intent_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // Only one attempt regardless of outcome
	a, err := s.m.pg.GetAgentByKey("mainagent")
	if err != nil || a == nil {
		return // When a brand-new database has no agent rows yet, seedPrompts seeds the latest defaults directly; this migration is unneeded
	}
	tmpl := agent.BuiltinPromptSeeds()["mainagent"]
	if tmpl == "" {
		return
	}
	// Fresh databases: seedPrompts already seeded the latest defaults -> current version already equals code defaults; no need to append a duplicate version.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] mainagent prompt failed to refresh to the new default: %v", err)
		return
	}
	log.Printf("[prompts] mainagent prompt has a new appended default version (after goals are met, ask back about creating goals; one-shot)")
}

// reseedPlannerPrompt refreshes the planner prompt to the [current code default] -- the default body was streamlined and refactored, and "restraint" was downgraded to
// Only dedupe, add "depth over coverage" and "hard baseline: if goals are unmet and no intent is running, output is mandatory", and cap re-review of negative conclusions.
// Each time the defaults change substantially, bump the flag below (currently v2) so existing old DBs refresh once more. SeedPromptIfEmpty is first-insert-only; old DBs that already have a version never receive it, hence version management.
// [Append a new version] and switch to it (ResetPromptToDefault); old versions stay in history, and users who customized can refer to the version records
// Recover. Guarded by the settings flag -> runs only once. Fresh databases need no handling (SeedPromptIfEmpty already seeded the latest default). And
// reseedGoalsPrompt is fully isomorphic.
func (s *Server) reseedPlannerPrompt() {
	const flag = "planner_prompt_compact_realistic_v2"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // Only one attempt regardless of outcome
	a, err := s.m.pg.GetAgentByKey("planner")
	if err != nil || a == nil {
		return // When a brand-new database has no agent rows yet, seedPrompts seeds the latest defaults directly; this migration is unneeded
	}
	tmpl := agent.BuiltinPromptSeeds()["planner"]
	if tmpl == "" {
		return
	}
	// Fresh databases: seedPrompts already seeded the latest defaults -> current version already equals code defaults; no need to append a duplicate version.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] planner prompt failed to refresh to the new default: %v", err)
		return
	}
	log.Printf("// [prompts] A new default version has been appended to the planner prompt (lean refactor + restrained-degradation dedup + depth-first + negative-review upper bound; one-shot)")
}

// reseedWorkerPrompt refreshes the worker prompt to the [current code default] ---- the default body removed "negative-type conclusions from the record_fact section
// the whole sentence "write observations + tentative readings", and decouples confidence(observed/inferred) from "whether the means for this intent have been exhausted" (these easily mislead the planner),
// Also tighten the facts array into per-item entries limited to the rare exception of "mutually fully independent, unmergeable" items.Bump flag to v3 so existing old databases are refreshed once more.
// SeedPromptIfEmpty is insert-on-first-only; databases with an older version will not receive it, so use version management to [append a new version] and switch to it; the old version stays in history and can be recovered.
// Guarded by a settings flag, runs only once. Fresh databases need no handling. Fully isomorphic with reseedGoalsPrompt.
func (s *Server) reseedWorkerPrompt() {
	const flag = "worker_prompt_compact_v4"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // Only one attempt regardless of outcome
	a, err := s.m.pg.GetAgentByKey("worker")
	if err != nil || a == nil {
		return // When a brand-new database has no agent rows yet, seedPrompts seeds the latest defaults directly; this migration is unneeded
	}
	tmpl := agent.BuiltinPromptSeeds()["worker"]
	if tmpl == "" {
		return
	}
	// Fresh databases: seedPrompts already seeded the latest defaults -> current version already equals code defaults; no need to append a duplicate version.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] worker prompt failed to refresh to the new default: %v", err)
		return
	}
	log.Printf("[prompts] worker prompt appended with new default version (lookup-context section converged to list_assets/list_findings, removed list_facts/node_detail/asset_neighbors, one-shot)")
}

// reporterToolCallMessage must unconditionally require reading get_finding_traffic once before writing the report.
// This tool is read-only and "does not depend on the capture switch"; manually bound evidence is readable whether or not auto-binding is off. If here
// Written as "read only when auto-binding is enabled"; with the default-off config the reporter won't pass evidence_version, ...
// SetFindingReportVersionByNodeID writes -1 under legacy semantics; vulnerability details and Markdown export
// It would then permanently display "evidence changed, report pending update", with no UI entry point to clear it.
const reporterToolCallMessage = "A vulnerability was just registered by report_finding. Read the returned JSON's finding_id (standalone vulnerability record ID) and finding_node_id (exploration node ID)," +
	"First use get_finding_traffic(finding_id) to read the current evidence list and its version (an empty list is normal; still write the report as usual);" +
	"If the run guide enables auto-binding, verify and associate this vulnerability's traffic before reading. Use finding_node_id for node details." +
	"Finally call update_finding_report(finding_id=finding_node_id, report, evidence_version=actually-read version) to save," +
	"evidence_version is required, otherwise the report will be permanently marked as pending update. Do not mix the two numbering schemes."

// Legacy trigger message (0.3.8 and earlier). Only records still verbatim identical to it will be overwritten by migration; user-modified ones stay as-is.
const reporterToolCallMessageV1 = "A vulnerability was just registered by report_finding above. Take the finding_id from the trigger context" +
	" (the number in the tool's \"finding recorded: <id>\"  return value) and the task id; write the detailed report for this vulnerability according to your role," +
	"Finally call update_finding_report(finding_id, report) to save."

// upgradeReporterTriggerMessage refreshes reporter trigger messages in old databases that still use the default copy to the new version.
// seedReporterAgent is guarded by reporter_agent_seed_v1 and writes triggers only when creating a new agent, so
// Databases upgraded from older versions miss the new copy -- the tool schema has been backfilled by seedFindingTrafficTools
// evidence_version, but nothing tells the reporter to use it. One-shot, and only overwrites unmodified copy.
func (s *Server) upgradeReporterTriggerMessage() {
	const flag = "reporter_trigger_evidence_version_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // Only one attempt
	triggers, err := s.m.pg.ListTriggersFor("reporter")
	if err != nil {
		log.Printf("[reporter] failed to read triggers: %v", err)
		return
	}
	for _, t := range triggers {
		if !t.OnToolCall || t.ToolCallMessage != reporterToolCallMessageV1 {
			continue // Leave untouched if modified by the user or not a finding trigger.
		}
		t.ToolCallMessage = reporterToolCallMessage
		if err := s.m.pg.UpdateTrigger(t); err != nil {
			log.Printf("[reporter] failed to upgrade trigger message: %v", err)
			return
		}
		log.Printf("[reporter] Trigger message upgraded to read and return evidence_version")
	}
}

// seedReporterAgent presets a "report-writing" custom agent (builtin=false, editable/deletable in the UI):
// Bind update_finding_report + task query tools, and attach a "triggered whenever report_finding is called"
// Trigger -- every time a vulnerability is registered, it is invoked to write a detailed report. One-shot (settings-flag guarded): once the user deletes it, it is never recreated.
// Dependency: the orchestration tool was already seeded into the store via SeedTool above this function, so it can be bound.
func (s *Server) seedReporterAgent() {
	const flag = "reporter_agent_seed_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // Only one attempt regardless of outcome

	if exist, _ := s.m.pg.GetAgentByKey("reporter"); exist != nil {
		return // key is already taken (manually created by user) -- do not overwrite
	}
	a, err := s.m.pg.CreateAgent("reporter", "Report writing",
		"Detailed vulnerability report writing: auto-triggered when a vulnerability is found; fetches evidence and the execution trace, then writes a Markdown report and writes it back.")
	if err != nil {
		log.Printf("[reporter] failed to create agent: %v", err)
		return
	}
	if err := s.m.pg.SeedPromptIfEmpty(a.ID, agent.ReporterDefaultPrompt); err != nil {
		log.Printf("[reporter] seed prompt failed: %v", err)
	}
	// Trigger run policy: parallel + none -- one report per vulnerability, multiple findings concurrently writing their own.
	// merge must be none: otherwise (default all) one wave of findings would be merged into a single run, defeating parallelism.
	// maxParallel=5: at most 5 reporting sessions at the same time, to avoid too many instantaneous LLM calls.
	if err := s.m.pg.SetAgentTriggerBehavior("reporter", "parallel", "none", 5); err != nil {
		log.Printf("[reporter] failed to set the trigger-run policy: %v", err)
	}
	// Bind the tools it needs: write reports + read evidence/execution progress/situational awareness.
	if err := s.m.pg.AddAgentToToolBinding("reporter", []string{
		"update_finding_report", "get_task_node_detail", "list_task_findings",
		"get_task_worker_trace", "list_task_worker_traces", "search_task_worker_traces",
		"get_task_graph",
	}); err != nil {
		log.Printf("[reporter] Failed to bind tool: %v", err)
	}
	// Trigger: fires when report_finding is called (the tool returns "finding recorded: <id>" carrying finding_id,
	// Task ID is also in the trigger message.)
	if _, err := s.m.pg.CreateTrigger(&db.AgentTrigger{
		AgentKey:        "reporter",
		Enabled:         true,
		OnToolCall:      true,
		ToolNames:       []string{"report_finding"},
		ToolCallMessage: reporterToolCallMessage,
	}); err != nil {
		log.Printf("[reporter] failed to create triggers: %v", err)
	}
	log.Printf("[reporter] pre-provisioned the 'report writing' agent + finding trigger")
}

// seedAutoReportFindingBinding adds "auto" to report_finding's binding ONCE so
// conversation-context agents can call it without requiring an intent_id.
func (s *Server) seedAutoReportFindingBinding() {
	const flag = "auto_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("auto", []string{"report_finding"}); err != nil {
		log.Printf("[auto] report_finding failed default binding: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerDefaultBindings adds "planner" to report_finding's binding ONCE
// (guarded by a settings flag), so existing DBs -- whose report_finding row was
// seeded as worker-only -- also let the planner record findings. Fresh DBs already
// get it via PlannerTools(); this only backfills without overriding a user unbind.
func (s *Server) seedPlannerDefaultBindings() {
	const flag = "planner_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"report_finding"}); err != nil {
		log.Printf("[planner] report_finding default binding failed: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerListAssetsBinding adds "planner" to list_assets's binding ONCE
// (guarded by a settings flag), so existing DBs -- whose list_assets row was seeded
// as auto/pentest-only -- also let the planner query the asset store by DSL. Fresh
// DBs already get it via PlannerTools(); this only backfills without overriding a
// user unbind.
func (s *Server) seedPlannerListAssetsBinding() {
	const flag = "planner_list_assets_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"list_assets"}); err != nil {
		log.Printf("[planner] list_assets failed default binding: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedCompanyScopeRebind changes add_company_scope's default binding ONCE on
// existing DBs (guarded by a settings flag): the tool moves off worker and onto
// planner -- defining a company's asset scope is a planning/main/auto concern, not
// something a worker does mid-exploration. Fresh DBs already get planner via
// PlannerTools() and lack worker via WorkerTools(); this only backfills old rows.
// One-shot + flag-guarded so a user who later re-binds worker isn't overridden.
func (s *Server) seedCompanyScopeRebind() {
	const flag = "company_scope_rebind_v1" // worker->planner default binding switch
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"add_company_scope"}); err != nil {
		log.Printf("[planner] add_company_scope default binding failed: %v", err)
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("worker", "add_company_scope"); err != nil {
		log.Printf("[worker] add_company_scope unbinding failed: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadToolsUnbind strips the read-context tools off worker's default
// binding ONCE on existing DBs (guarded by a settings flag): a worker executes one
// intent and writes back -- reading facts/companies and listing all workers' traces is
// a planning/main concern, not the executor's. Fresh DBs already lack these via
// WorkerTools(); this only backfills old rows without overriding a user who
// deliberately re-binds worker. Each RemoveAgentFromTool is per-tool +
// membership-guarded, so planner/mainagent bindings of the same tool are untouched.
//
// NOTE: search_all_worker_traces / get_worker_trace / node_detail are intentionally NOT
// unbound -- worker owns them for cross-work look-back + node drill-down (see WorkerTools).
// They used to be in this list back when worker lacked them; seedWorkerReadbackRebind
// repairs DBs whose old run stripped them.
func (s *Server) seedWorkerReadToolsUnbind() {
	const flag = "worker_readtools_unbind_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	for _, k := range []string{
		"list_facts", "list_companies", "list_worker_traces",
	} {
		if err := s.m.pg.RemoveAgentFromTool("worker", k); err != nil {
			log.Printf("[worker] %s failed to unbind from worker: %v", k, err)
			return // On error, don't persist the flag; retry on next startup
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadbackRebind re-binds the cross-work look-back / drill-down tools onto
// worker ONCE (guarded by a settings flag): an earlier seedWorkerReadToolsUnbind wrongly
// stripped search_all_worker_traces / get_worker_trace / node_detail from worker after
// they had been added to WorkerTools(), so any DB that ran that migration lost them.
// Fresh DBs already have them via WorkerTools() and this is a harmless no-op there.
// One-shot + flag-guarded so a user who later deliberately unbinds them isn't overridden.
func (s *Server) seedWorkerReadbackRebind() {
	const flag = "worker_readback_rebind_v2" // v2: append node_detail
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("worker", []string{
		"search_all_worker_traces", "get_worker_trace", "node_detail",
	}); err != nil {
		log.Printf("[worker] failed to rebind review/detail tools: %v", err)
		return // On error, don't persist the flag; retry on next startup
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedAutoDefaultBindings adds "auto" to the task-op + platform tools' bindings
// ONCE (guarded by a settings flag), so existing DBs whose tool rows were seeded
// before Auto existed still give Auto its default toolset -- without re-adding it
// after a user deliberately unbinds.
func (s *Server) seedAutoDefaultBindings() {
	const flag = "auto_default_bindings_v3" // v3: replace legacy asset tool names, add insert_assets/add_company_scope
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	keys := make([]string, 0, len(platformToolKeys)+12)
	for _, t := range s.orchestrationTools() {
		keys = append(keys, t.Name())
	}
	keys = append(keys, platformToolKeys...)
	// Asset tools: the Auto operator platform often needs to view/register assets and manage company scope.
	keys = append(keys, "insert_assets", "add_company_scope", "list_assets")
	if err := s.m.pg.AddAgentToToolBinding("auto", keys); err != nil {
		log.Printf("[auto] default binding failed: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}
