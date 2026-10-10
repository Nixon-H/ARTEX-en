package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Autumn-27/norma/harness"
)

// runTrace retains the latest tool call so an interrupted run can identify the
// operation that was still in flight.
type runTrace struct {
	startedAt time.Time
	id        string
	name      string
	input     string
	at        time.Time
	pending   bool
}

func (t *runTrace) start(id, name, input string) {
	t.id, t.name, t.input, t.at, t.pending = id, name, input, time.Now(), true
}

func (t *runTrace) done(id string) {
	if id == t.id {
		t.pending = false
	}
}

var reasonHint = map[harness.TerminalReason]string{
	harness.ReasonCompleted:         "The model ended this round normally but left no textual summary; facts and assets follow this round's tool-call records",
	harness.ReasonMaxTurns:          "Reached the step limit (MaxTurns): the SDK has already run wrap-up and written back facts and assets; the intent will be marked as exhausted for the planner to continue in a new direction, rather than being treated as a failure",
	harness.ReasonTimeout:           "Reached the single-run wall-clock budget (MaxDuration): when time is up, the running tool is interrupted and wrap-up runs in place, writing back the identified facts and assets; the intent will be marked as exhausted",
	harness.ReasonModelError:        "Model or API call failed (network, auth, rate limiting, provider 5xx, etc.); after retries are exhausted the intent is marked blocked -- a transport-layer failure means this intent was basically never really probed; check its execution trace (get_worker_trace) before deciding to re-dispatch or switch approaches",
	harness.ReasonBlockingLimit:     "Context length hit the hard cap; the request was intercepted before sending; narrow the intent granularity or compact tool returns",
	harness.ReasonPromptTooLong:     "Prompt too long and context-compaction retries exhausted; cannot continue",
	harness.ReasonImageError:        "The current model doesn't support this round's multimodal content; switch to a vision-capable model or keep tools from returning images",
	harness.ReasonStopHookPrevented: "A Stop hook blocked this round from ending, then failed to continue; check whether the task's Guard rules are too strict",
	harness.ReasonHookStopped:       "A tool or hook actively stopped further execution, e.g. an out-of-scope target or a disabled command; check the interception note in the last tool_result",
	harness.ReasonAbortedStreaming:  "Run canceled during the model's streaming-output generation phase",
	harness.ReasonAbortedTools:      "Run canceled during the tool-execution phase",
}

// terminalText renders a terminal event with no final text into a compact summary
// and a Markdown detail block.
func terminalText(ctx context.Context, term *harness.Terminal, tr *runTrace) (string, string) {
	reason := term.Reason
	aborted := reason == harness.ReasonAbortedStreaming || reason == harness.ReasonAbortedTools
	// Prompt may return ctx.Err directly without a terminal event. Preserve the
	// cancellation cause instead of falling back to an empty/unknown terminal reason.
	if reason == "" && ctx.Err() != nil {
		aborted = true
	}

	var sum string
	if aborted {
		_, short, _, ok := AbortReason(ctx)
		if !ok {
			short = "Failed to obtain the cancellation cause"
		}
		stage := "During execution"
		switch reason {
		case harness.ReasonAbortedStreaming:
			stage = "Model-output phase"
		case harness.ReasonAbortedTools:
			stage = "Tool-execution phase"
		}
		sum = "(Run interrupted: ..." + short + "; stopped at ..." + stage + progressSuffix(term, tr) + ", unfinished)"
	} else if reason == harness.ReasonMaxTurns || reason == harness.ReasonTimeout {
		sum = "(hit the run budget cap (" + string(reason) + "); wrapped up and wrote back facts" + progressSuffix(term, tr) + "; no textual summary this time)"
	} else {
		hint := terminalReasonHint(reason)
		sum = "(no textual summary; terminal state ..." + terminalReasonLabel(reason) + ": " + firstLine(hint, 80) + ")"
	}

	var b strings.Builder
	b.WriteString(sum)
	b.WriteString("\n\n")
	displayReason := terminalReasonLabel(reason)
	fmt.Fprintf(&b, "- **Terminal state**: `%s` - %s\n", displayReason, terminalReasonHint(reason))
	if aborted {
		code, _, why, ok := AbortReason(ctx)
		if ok {
			fmt.Fprintf(&b, "- **Interruption cause** (`%s`): %s\n", code, why)
		} else {
			b.WriteString("- **Interruption cause**: unavailable; the cancelling party may not have attached a named cause via context.WithCancelCause\n")
		}
	}
	if term.Err != nil {
		fmt.Fprintf(&b, "- **Underlying error**: `%v`\n", term.Err)
	}
	if aborted && strings.TrimSpace(term.Text) != "" {
		b.WriteString("- **Partial output produced before cancellation**:\n\n")
		b.WriteString(term.Text)
		b.WriteString("\n\n")
	}
	if term.Turns > 0 {
		fmt.Fprintf(&b, "- **Executed**: %d model rounds\n", term.Turns)
	}
	if !tr.startedAt.IsZero() {
		fmt.Fprintf(&b, "- **This run's elapsed time**: %s\n", roundDur(time.Since(tr.startedAt)))
	}
	if u := term.Usage; u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.CacheWriteTokens > 0 {
		fmt.Fprintf(&b, "- **Cumulative tokens**: input %d / output %d / cache reads %d / cache writes %d\n",
			u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens)
	}
	if tr.name == "" {
		b.WriteString("- **Tool calls**: this run ended before issuing any tool calls\n")
	} else if tr.pending {
		fmt.Fprintf(&b, "- **Tool executing at interruption**: `%s` (has been running for %s, **no result returned**)\n\n  ```json\n  %s\n  ```\n",
			tr.name, roundDur(time.Since(tr.at)), firstLine(tr.input, 300))
	} else {
		fmt.Fprintf(&b, "- **Last tool before interruption**: `%s` (returned normally)\n", tr.name)
	}
	return sum, b.String()
}

func terminalReasonLabel(reason harness.TerminalReason) string {
	if reason == "" {
		return "context_canceled"
	}
	return string(reason)
}

func terminalReasonHint(reason harness.TerminalReason) string {
	if hint := reasonHint[reason]; hint != "" {
		return hint
	}
	if reason == "" {
		return "The running context was canceled, but the underlying layer produced no Terminal event"
	}
	return "Unknown terminal state; the harness may have added a TerminalReason -- please add a reasonHint"
}

func progressSuffix(term *harness.Terminal, tr *runTrace) string {
	var parts []string
	if term.Turns > 0 {
		parts = append(parts, fmt.Sprintf("%d rounds", term.Turns))
	}
	if !tr.startedAt.IsZero() {
		parts = append(parts, roundDur(time.Since(tr.startedAt)))
	}
	if len(parts) == 0 {
		return ""
	}
	return ", already ran " + strings.Join(parts, " / ")
}

func roundDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	case d < time.Hour:
		return d.Round(time.Second).String()
	default:
		return d.Round(time.Minute).String()
	}
}
