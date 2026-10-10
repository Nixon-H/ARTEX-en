package agent

import (
	"context"
	"errors"
	"fmt"
)

// AbortCause names why an agent run's context was cancelled. Every cancellation
// site should attach one so the activity trace can report the real initiator.
type AbortCause struct {
	Code  string
	Short string
	Text  string
}

func (c *AbortCause) Error() string { return c.Text }

func cause(code, short, text string) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: text}
}

// Causef builds a cause that includes runtime-specific detail.
func Causef(code, short, format string, args ...any) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: fmt.Sprintf(format, args...)}
}

var (
	// Task-level execution context.
	AbortPausedByUser = cause("paused_by_user", "User paused the task",
		"The user paused the task via the task control API (POST /api/tasks/{id}/control, action=pause). This planner/worker run was actively cancelled; running intents fall back to the frontier (open) and are re-claimed and re-executed from scratch after the task resumes")
	AbortPausedByOrchestrator = cause("paused_by_orchestrator", "The orchestration agent paused the task",
		"The orchestrator Agent called the pause_task tool to pause this task. This Planner/Worker run is proactively cancelled; running intents fall back to the frontier (open) and re-execute after resume")
	AbortTaskDeleted = cause("task_deleted", "Task was deleted",
		"The task is being deleted (DELETE /api/tasks/{id}); the deletion barrier has cancelled this task's running Planner, Worker and main Agent; this run's results will no longer be used")
	AbortPausedOnReload = cause("paused_on_reload", "Backend restored the task's paused state",
		"On backend startup, the task pause was restored from the persisted state in the database. This run was cancelled; normally there is no running Agent during the recovery phase")
	AbortGoalMet = cause("goal_met", "The planner judged the task goals achieved",
		"The planner determined the task goal is achieved and set the task to done, then cancelled the still-running workers; these intents are marked as stopped rather than failed")
	AbortSettleDrainTimeout = cause("settle_drain_timeout", "The task-timeout wrap-up grace period has been exhausted",
		"After the task reaches timeout, wait for the running Worker to finish gracefully, but the 90-second drain grace period was still insufficient, so a hard cancel is executed; the intent will be marked exhausted, while facts and assets already written during the wind-down are kept")

	// Per-work context.
	AbortKilledByPlanner = cause("killed_by_planner", "The planner terminated this intent",
		"The planner proactively terminated this intent by calling kill_work, usually meaning the direction drifted or there is no further value; the intent is marked as stopped and will not be automatically re-claimed")
	AbortWorkPausedByUser = cause("work_paused_by_user", "User paused this worker intent",
		"The user paused the running Worker. This call is cancelled and the intent becomes paused; all registered intents, facts, vulnerabilities, and activity records are kept, and execution restarts from the beginning after resuming")
	AbortWorkCancelledByUser = cause("work_cancelled_by_user", "User deleted this worker intent",
		"The user deleted a running Worker. This call is cancelled; after the Worker exits the write zone, the server handles the intent according to the user-selected deletion mode -- soft delete only marks it deleted and keeps all outputs, hard delete cascade-removes the intent and downstream nodes supported solely by it")
	AbortWorkFinished = cause("work_finished", "Worker has ended normally and released context",
		"Worker has ended normally; the engine releases its context resources in detachWork. This is not a run interruption; if it appears in an interruption message, it means cancellation raced with teardown")
	AbortPausedRaceGuard = cause("paused_race_guard", "Refused to start a new run while the task is paused",
		"While the task is paused, the engine refuses to issue new execution contexts, to prevent a race between claim and pause from continuing to start workers; already-claimed intents are returned to the frontier")

	// Main Agent and standalone conversation contexts.
	AbortChatStoppedByUser = cause("chat_stopped_by_user", "User stopped this round of dialogue",
		"The user clicked Stop, proactively aborting this round's main-agent or session-agent run. Activity records already produced are kept; you can continue sending the next message")
	AbortChatPausedWithTask = cause("chat_paused_with_task", "Task paused and aborted the main agent dialogue",
		"When the user pauses the task, the running main-agent conversation is cancelled in sync. Activity records already produced are kept; after resuming the task, this round's message is not automatically replayed")
	AbortChatTurnFinished = cause("chat_turn_finished", "Dialogue round ended; context released",
		"This round of conversation has ended normally; the server is releasing this round's context resources. This is not a run interruption; if it appears in an interruption message, it means the cancel and teardown events raced")

	// Process-level and per-run hard backstop.
	AbortShutdown = cause("shutdown", "Backend process is shutting down",
		"The backend process received SIGINT or SIGTERM and is restarting, updating, or shutting down. All running Agents will be cancelled; leftover running intents after restart reset to open and re-execute")
	AbortRunHardTimeout = cause("run_hard_timeout", "The single-run hard-timeout fallback has triggered",
		"A single run exceeded the soft wall-clock budget plus the extra grace period, meaning a model request or some tool went without returning for a long time, so the normal turn-boundary wrap-up could not execute. Focus on the last unreturned tool call before the interruption")
)

// AbortReason resolves the named cause attached to a cancelled run context.
func AbortReason(ctx context.Context) (code, short, text string, ok bool) {
	c := context.Cause(ctx)
	if c == nil {
		return "", "", "", false
	}
	var ac *AbortCause
	if errors.As(c, &ac) {
		return ac.Code, ac.Short, ac.Text, true
	}
	switch {
	case errors.Is(c, context.DeadlineExceeded):
		return "deadline_exceeded", "Upstream context reached deadline",
			"Upstream context reached deadline, but the setter did not attach a named cause via WithTimeoutCause: " + c.Error(), true
	case errors.Is(c, context.Canceled):
		return "canceled_no_cause", "The canceling side attached no named cause",
			"The upstream context was cancelled, but the cancelling side did not attach a named cause via context.WithCancelCause; please register the cause in agent/cancelcause.go and wire up that cancellation point", true
	default:
		return "other", firstLine(c.Error(), 80), c.Error(), true
	}
}
