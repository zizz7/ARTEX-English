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
		"The user paused the task via the task control API (POST /api/tasks/{id}/control, action=pause). The current Planner/Worker run was actively cancelled; in-progress intents are returned to the frontier (open) and will be reclaimed and re-executed from scratch after the task is resumed")
	AbortPausedByOrchestrator = cause("paused_by_orchestrator", "Orchestrator agent paused the task",
		"The orchestrator agent called the pause_task tool to pause this task. The current Planner/Worker run was actively cancelled; in-progress intents are returned to the frontier (open) and will be re-executed after resumption")
	AbortTaskDeleted = cause("task_deleted", "Task was deleted",
		"The task is being deleted (DELETE /api/tasks/{id}); the deletion barrier has cancelled the running Planner, Worker, and Main Agent for this task; results from this run will no longer be used")
	AbortPausedOnReload = cause("paused_on_reload", "Backend restored the task's paused state",
		"On startup the backend restored the task's paused state from the persisted database state. This run was cancelled; under normal circumstances there are no running agents during the restore phase")
	AbortGoalMet = cause("goal_met", "Planner: task goal achieved",
		"The planner determined that the task goal has been achieved and set the task to done, then cancelled any still-running Workers; these intents are marked as stopped, not failed")
	AbortSettleDrainTimeout = cause("settle_drain_timeout", "Timeout wrap-up drain exhausted",
		"After the task reached its timeout, it waited for running Workers to wrap up gracefully, but the 90-second drain grace period was still insufficient, so a hard cancel was performed; intents are marked as exhausted, and facts and assets already written during the wrap-up phase are preserved")

	// Per-work context.
	AbortKilledByPlanner = cause("killed_by_planner", "Planner terminated this intent",
		"The planner called kill_work to actively terminate this intent, typically because the exploration direction was off-track or no longer valuable; the intent is marked as stopped and will not be automatically reclaimed")
	AbortWorkPausedByUser = cause("work_paused_by_user", "User paused this Worker intent",
		"The user paused the running Worker. This call was cancelled and the intent transitions to paused; all registered intents, facts, findings, and activity records are preserved and will be re-executed from scratch after resumption")
	AbortWorkCancelledByUser = cause("work_cancelled_by_user", "User cancelled this Worker intent",
		"The user cancelled the running Worker. This call was cancelled; after the Worker exits its write section, the server will transactionally delete the intent and its directly produced facts, findings, and activity records")
	AbortWorkFinished = cause("work_finished", "Worker finished, context released",
		"The Worker finished normally; the engine released its context resources in detachWork. This is not a run interruption; if it appears in an interruption message, it indicates a race between cancellation and the completion event")
	AbortPausedRaceGuard = cause("paused_race_guard", "New run blocked: task is paused",
		"While the task is paused, the engine refuses to issue a new execution context, preventing a race between claim and pause from causing Workers to keep starting; already-claimed intents are returned to the frontier")

	// Main Agent and standalone conversation contexts.
	AbortChatStoppedByUser = cause("chat_stopped_by_user", "User stopped this conversation turn",
		"The user clicked stop to actively abort this turn of the Main Agent or conversation agent. Activity records already produced are preserved; the user can continue sending the next message")
	AbortChatPausedWithTask = cause("chat_paused_with_task", "Task pause aborted Main Agent chat",
		"When the user paused the task, the running Main Agent conversation was also synchronously cancelled. Activity records already produced are preserved; resuming the task will not automatically replay this turn's messages")
	AbortChatTurnFinished = cause("chat_turn_finished", "Chat turn finished, context released",
		"This conversation turn finished normally; the server is releasing that turn's context resources. This is not a run interruption; if it appears in an interruption message, it indicates a race between cancellation and the completion event")

	// Process-level and per-run hard backstop.
	AbortShutdown = cause("shutdown", "Backend process is shutting down",
		"The backend process received SIGINT or SIGTERM and is restarting, updating, or shutting down. All running agents will be cancelled; after restart, leftover running intents are reset to open and re-executed")
	AbortRunHardTimeout = cause("run_hard_timeout", "Hard timeout backstop triggered",
		"A single run exceeded its soft wall-clock budget plus the additional grace period, indicating that a model request or a tool call failed to return for an extended time, preventing normal turn-boundary wrap-up. Focus on the last unreturned tool call before the interruption")
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
			"Upstream context reached its deadline, but the setter did not attach a named cause via WithTimeoutCause: " + c.Error(), true
	case errors.Is(c, context.Canceled):
		return "canceled_no_cause", "Canceller did not attach a named cause",
			"Upstream context was cancelled, but the canceller did not attach a named cause via context.WithCancelCause; please register a cause in agent/cancelcause.go and wire it to that cancellation site", true
	default:
		return "other", firstLine(c.Error(), 80), c.Error(), true
	}
}
