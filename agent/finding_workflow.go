package agent

import (
	"encoding/json"
	"fmt"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

const findingIDGuidance = "\n\n**Finding ID convention**: finding_id is the standalone vulnerability record ID; finding_node_id is the exploration node ID. The id of list_findings / list_task_findings / node_detail / get_task_node_detail remains the exploration node ID; read the standalone ID from the finding_id in the same response. get_finding_traffic / bind_finding_traffic use the standalone finding_id. The legacy update_finding_report's finding_id parameter still takes finding_node_id. Do not use the number on the first line of report_finding for evidence tools, and do not guess other numbers when an ID error occurs."

// The server supplies the persisted setting. A missing setting/host is off.
// Consulted at assembly and again on writes so an already-running session
// cannot keep binding after the user switches the feature off.
var FindingTrafficBindingEnabled func() bool

func findingTrafficBindingEnabled() bool {
	return FindingTrafficBindingEnabled != nil && FindingTrafficBindingEnabled()
}

// Applied after ToolResolve: user descriptions and prompts remain intact, while
// all actual reporters (including Planner and custom chat agents) see the same
// API contract. Disabled/unbound tools are never reintroduced here.
func findingWorkflowTools(agentKey string, tools []actool.CoreTool) ([]actool.CoreTool, string) {
	if !findingTrafficBindingEnabled() {
		out := make([]actool.CoreTool, 0, len(tools))
		for _, tool := range tools {
			if tool.Name() == "bind_finding_traffic" {
				continue
			}
			if agentKey == "reporter" && (tool.Name() == "traffic_search" || tool.Name() == "traffic_get" || tool.Name() == "traffic_blob") {
				continue
			}
			switch tool.Name() {
			case "report_finding", "add_hint", "add_task_hint":
				// Work on a copy: toggling back on must restore the original schema.
				raw, _ := json.Marshal(tool.InputSchema())
				var schema map[string]any
				if json.Unmarshal(raw, &schema) == nil {
					stripTrafficParameters(schema)
					tool = DecorateTool(tool, tool.Description(), schema)
				}
			}
			out = append(out, tool)
		}
		return out, ""
	}
	out := append([]actool.CoreTool(nil), tools...)
	has := map[string]bool{}
	for i, tool := range out {
		has[tool.Name()] = true
		note := ""
		switch tool.Name() {
		case "report_finding":
			note = "\nChecked and bound by the report agent before writing the report by default. The reporter keeps the verification commands, key outputs, and existing real traffic IDs with their purposes in evidence for the report agent to verify against execution records; no extra packet lookup just for binding. Explicit immediate binding is supported: traffic_refs or evidence_hint_id may submit verified references, the latter reading the structured reference of the specified hint in this task; if either is invalid the whole report fails. TCP/packet-less cases don't need these optional params. Returns finding_id and finding_node_id denoting the standalone record and the exploration node respectively."
		case "add_hint", "add_task_hint":
			note = "\nWhen handing over confirmed findings, keep the verified traffic's IDs, purposes, descriptions, and order in the corresponding hint's traffic_refs (top level for single, the matching hints element for batch), and state in text the specific finding each proves. The caller must not hand over text alone while dropping existing traffic references. Unverified candidates must not be passed as evidence.\""
		case "get_finding_traffic", "bind_finding_traffic", "list_findings", "list_task_findings", "node_detail", "get_task_node_detail", "update_finding_report":
			note = findingIDGuidance
		}
		if note != "" {
			out[i] = DecorateTool(tool, tool.Description()+note, tool.InputSchema())
		}
	}
	guidance := ""
	if has["report_finding"] || has["add_task_hint"] || has["add_hint"] {
		guidance = "\n\n**Traffic evidence handoff (optional)**: automatic binding is done by the report agent after the finding is recorded and before the report is written. The reporter should keep the verification commands, key outputs, and existing real traffic IDs with their purposes in evidence, with intent_id in the task, so the report agent can trace back; no extra packet lookup just for binding. When Auto / Planner reports on behalf, do not drop the worker's existing references. add_hint / add_task_hint can hand off via traffic_refs; explicit immediate binding still supports report_finding's traffic_refs / evidence_hint_id. For TCP or packet-less cases, register normally; never guess IDs, and never re-probe just to fill in packets."
		if has["add_task_hint"] && !has["add_hint"] {
			guidance += "\nWhen a platform conversation has no task context, do not call report_finding directly; hand off to the existing corresponding task via add_task_hint, have that task's Agent register it, and verify the result with list_task_findings.\""
		}
		if has["prove_goal"] || has["goal_met"] {
			guidance += "\nBefore judging a goal complete, first finish reporting / handing over existing evidence from this run. Do not end the task or cancel workers merely because a text-level vulnerability is already recorded while evidence handover is still incomplete; if there is no packet capture, waiting or forced capture is not required."
		}
	}
	if has["update_finding_report"] && has["bind_finding_traffic"] && has["get_finding_traffic"] {
		guidance += "\n\n**auto-link traffic before reporting (Enabled)**: reconcile and link traffic for this vulnerabilityLink traffic, then write the report.start from report_finding return JSON or get_task_node_detail / list_task_findings obtain the exact finding_id and finding_node_id.read vulnerability details, execution trace of the corresponding intent and the existing evidence list, preferring the real IDs handed off by the reporter.If this verification is HTTP and the traffic tools are available, use traffic_search to filter candidates, then use traffic_get to verify one by one that the requests/responses actually support this vulnerability; Domains and timestamps are only used for filtering, not for proving attribution.Link the confirmed evidence in reproduction order with bind_finding_traffic(finding_id, traffic_refs), choosing baseline / proof / verification / supporting and stating the purpose.Only operate on this vulnerability; do not re-create the vulnerability or re-probe the target.After a successful bind, call get_finding_traffic again to get the latest version, read the required body text, then pass the actually read version as evidence_version to update_finding_report (its finding_id parameter still uses finding_node_id).Existing bindings need not be appended again.For TCP, uncaptured traffic, unavailable tools, or no exact match, skip auto-binding and write the report normally based on textual/command evidence with the reason stated; never guess just to pad traffic.A failed bind must not be claimed as success; keep the existing evidence and state the unbound reason in the report."
	}
	if guidance != "" || has["get_finding_traffic"] || has["update_finding_report"] {
		guidance += findingIDGuidance
	}
	return out, guidance
}

func stripTrafficParameters(schema map[string]any) {
	props, _ := schema["properties"].(map[string]any)
	delete(props, "traffic_refs")
	delete(props, "evidence_hint_id")
	if required, ok := schema["required"].([]any); ok {
		kept := required[:0]
		for _, key := range required {
			if key != "traffic_refs" && key != "evidence_hint_id" {
				kept = append(kept, key)
			}
		}
		schema["required"] = kept
	}
	if hints, ok := props["hints"].(map[string]any); ok {
		if items, ok := hints["items"].(map[string]any); ok {
			stripTrafficParameters(items)
		}
	}
}

// HintTrafficSchema is shared by the task-local and cross-task hint tools.
func HintTrafficSchema() map[string]any {
	return map[string]any{"type": "array", "description": "Optional: verified traffic references corresponding to the specific vulnerability in this prompt, in original order; after handoff, report_finding may pass evidence_hint_id to carry these references.", "items": obj(map[string]any{"traffic_id": str("Real traffic ID"), "role": str("baseline / proof / verification / supporting"), "note": str("What conclusion this traffic supports")}, "traffic_id")}
}

func (t *ToolSet) findingRefsFromHint(hintID int64, explicit []db.TrafficRef) ([]db.TrafficRef, error) {
	if hintID <= 0 {
		return db.NormalizeTrafficRefs(explicit)
	}
	n, err := t.ts.GetNode(hintID) // local store only: inherited hints cannot supply evidence
	if err != nil {
		return nil, err
	}
	if n == nil || n.Kind != db.KindHint {
		return nil, fmt.Errorf("evidence_hint_id=%d must be a hint node of this task (inherited hints can't be bound directly)", hintID)
	}
	var payload struct {
		Refs []db.TrafficRef `json:"traffic_refs"`
	}
	if err := json.Unmarshal(n.Payload, &payload); err != nil {
		return nil, err
	}
	return db.NormalizeTrafficRefs(append(append([]db.TrafficRef{}, explicit...), payload.Refs...))
}
