package agent

import (
	"context"
	"encoding/json"

	actool "github.com/Autumn-27/norma/tool"
)

// This file turns "built-in tools" from pure code into an enumerable catalog that the DB can override:
//   - BuiltinToolSeeds(): expands the built-in toolsets of the three worker agents into seed records (key +
//     description + parameter schema + default-bound agent), for the server to idempotently seed into the tools table at startup.
//   - ToolResolve hook: at runtime, applies the tools rows in the DB to the assembled tools via "filter by agent +
//     override description/schema + inject parameter defaults". key/handler stay in the code layer; the DB only changes "prose and defaults".
// handler (Call behavior) always comes from code -- the DB cannot change it, only the descriptions the model sees and the default inputs.

// ToolSeed is a seedable snapshot of a built-in tool: key is CoreTool.Name() (hard-bound to the handler,
// read-only in the UI), Desc/Schema taken from the tool definition in code, Agents are which agents the code gives it to by default.
type ToolSeed struct {
	Key    string         // = CoreTool.Name(), primary key, immutable
	Desc   string         // Top-level description (overridable in the UI)
	Schema map[string]any // Parameter JSON-Schema (structure read-only; description/default editable in the UI)
	Agents []string       // Default-bound agent keys (worker/planner/mainagent)
}

// builtinToolsByAgent builds each worker agent's domain toolset with a "read-only shell" ToolSet (nil stores).
// Tool constructors only stuff closures into Spec and never dereference the store at construction time, so nil is safe --
// these tools are only used here to read Name()/Description()/InputSchema(), never to Call.
//
// Deliberately [excludes] the SDK generic tools actool.DefaultTools() (Read/Write/Edit/MultiEdit/LS/Glob/
// Grep/Bash): every agent always has them with no "bound to whom" tradeoff, and their docs live mostly in Prompt()
// (this table only covers Description(), which would be a misleading half-override). No seed -> no DB row -> ToolResolve
// passes them through untouched with behavior unchanged. Only artex's own domain tools enter the table for management.
func builtinToolsByAgent() map[string][]actool.CoreTool {
	ts := NewToolSet(nil, "")
	return map[string][]actool.CoreTool{
		"mainagent": ts.MainAgentTools(),
		"planner":   ts.PlannerTools(),
		"worker":    ts.WorkerTools(),
		// goals (goal decomposer) binds set_goals + set_constraints by default: relying on them to handle the decomposed goals,
		// Extracted operational constraints are written into the store. Shares the same managed tool with mainagent; on the web side the description/schema can be edited and checked per agent.
		"goals": {ts.setGoals(), ts.setConstraints()},
		// auto binds vuln-reporting + asset-management tools by default; other domain tools can be ticked on demand in the UI.
		// New DBs are seeded by this; old DBs migrate via seedAutoDefaultBindings.
		"auto": {ts.addFinding(), ts.insertAssets(), ts.addCompanyScope(), ts.listAssets(), ts.listCompanies()},
		// pentest (standalone pentest agent) default bindings: query assets / insert assets / report findings / query findings / query companies.
		// New databases are seeded from this; old databases are migrated by seedPentestDefaultBindings.
		"pentest": {ts.listAssets(), ts.insertAssets(), ts.addFinding(), ts.listFindings(), ts.listCompanies()},
	}
}

// defaultUnbound: these system tools enter the catalog as usual (visible on the web side, manually checkable per agent), but
// By default [not bound to any agent] -- ToolResolve discards tools with empty bindings for all agents; explicit opt-in is required.
// the reason it still remains in some agent's base toolset (e.g. goal_met in PlannerTools): first, so that the seed can
// Construct it to get the desc/schema; second, after the user manually re-binds it, it exists in the runtime base so ToolResolve keeps it.
//
// goal_met: bypasses per-goal prove_goal and declares [the whole task complete] globally; heavyweight with misjudgment risk, and also
// "prove_goal marks the last goal -> auto close-out", so it is not given to any agent by default; bind it manually when needed.
var defaultUnbound = map[string]bool{"goal_met": true}

// BuiltinToolSeeds dedupes and merges each agent's built-in tool set into a seed list: same-named tools (e.g. list_assets
// owned by multiple agents) are merged into one entry with the union of Agents; tools in defaultUnbound are force-bound to empty.
func BuiltinToolSeeds() []ToolSeed {
	byAgent := builtinToolsByAgent()
	order := []string{"mainagent", "goals", "planner", "worker", "auto", "pentest"}

	type acc struct {
		tool   actool.CoreTool
		agents []string
	}
	m := map[string]*acc{}
	var keys []string
	for _, ak := range order {
		for _, t := range byAgent[ak] {
			a, ok := m[t.Name()]
			if !ok {
				a = &acc{tool: t}
				m[t.Name()] = a
				keys = append(keys, t.Name())
			}
			a.agents = append(a.agents, ak)
		}
	}

	out := make([]ToolSeed, 0, len(keys))
	for _, k := range keys {
		a := m[k]
		agents := a.agents
		if defaultUnbound[k] {
			agents = []string{} // ... enters the catalog and can be bound manually, but is given to no agent by default (stored as [] not null, same as other tools)
		}
		out = append(out, ToolSeed{
			Key:    k,
			Desc:   a.tool.Description(),
			Schema: a.tool.InputSchema(),
			Agents: agents,
		})
	}
	return out
}

// ToolResolve, if set, post-processes an agent's fully-assembled tool list against
// the DB tools table: it drops tools not bound to this agent (or globally disabled)
// and wraps the rest so the model sees the DB-overridden description/schema and
// Default input params get injected. Tools with no matching DB row (MCP/skill/host tools like
// traffic) pass through untouched. nil = tools unchanged. Wired in server/assembly.go.
var ToolResolve func(ctx context.Context, agentKey string, tools []actool.CoreTool) []actool.CoreTool

// DecorateTool wraps t so Description()/InputSchema() report the DB overrides and
// Call() injects scalar parameter defaults (from schema's "default" props) whenever
// the model omitted them. Name/Prompt/permission/scheduler flags delegate to t, so
// the tool's identity and handler are unchanged. Empty desc/schema fall back to t's.
func DecorateTool(t actool.CoreTool, desc string, schema map[string]any) actool.CoreTool {
	if desc == "" {
		desc = t.Description()
	}
	if len(schema) == 0 {
		schema = t.InputSchema()
	}
	return &overriddenTool{CoreTool: t, desc: desc, schema: schema}
}

// overriddenTool is a CoreTool decorator: it embeds the original (so all behavioral
// methods -- Prompt/IsReadOnly/IsConcurrencySafe/CheckPermissions/Name -- delegate)
// and overrides only the model-facing description/schema plus default injection.
type overriddenTool struct {
	actool.CoreTool
	desc   string
	schema map[string]any
}

func (o *overriddenTool) Description() string         { return o.desc }
func (o *overriddenTool) InputSchema() map[string]any { return o.schema }

func (o *overriddenTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	return o.CoreTool.Call(ctx, injectDefaults(in, o.schema), tc)
}

// injectDefaults fills scalar parameter defaults declared in the (possibly edited)
// schema into the input JSON whenever the model omitted the field or left it empty/
// null. Structure (names/types/required) is untouched -- only default values are merged in.
func injectDefaults(in json.RawMessage, schema map[string]any) json.RawMessage {
	defs := scalarDefaults(schema)
	if len(defs) == 0 {
		return in
	}
	m := map[string]json.RawMessage{}
	if len(in) > 0 {
		if err := json.Unmarshal(in, &m); err != nil {
			return in // non-object input: don't touch it
		}
	}
	changed := false
	for k, dv := range defs {
		if cur, ok := m[k]; !ok || isEmptyJSON(cur) {
			m[k] = dv
			changed = true
		}
	}
	if !changed {
		return in
	}
	b, err := json.Marshal(m)
	if err != nil {
		return in
	}
	return b
}

// scalarDefaults extracts properties[k]["default"] for scalar params (string/
// integer/number/boolean). Array/object defaults are skipped: merging them is
// ambiguous and not worth the surprise.
func scalarDefaults(schema map[string]any) map[string]json.RawMessage {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	out := map[string]json.RawMessage{}
	for name, raw := range props {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		dv, ok := p["default"]
		if !ok || dv == nil {
			continue
		}
		switch p["type"] {
		case "string", "integer", "number", "boolean":
			if b, err := json.Marshal(dv); err == nil {
				out[name] = b
			}
		}
	}
	return out
}

func isEmptyJSON(raw json.RawMessage) bool {
	s := string(raw)
	return s == "null" || s == `""`
}
