package server

import (
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// Server-side parsing of the retry policy, see docs/LLMretry design.md. Of the five layers:
//   - Connection setup / empty responses / same-provider safety windows are "endpoint-scoped": each LLM config can override them
//     Global defaults (if an item in a profile is left blank, it inherits the global value; if the global value is also unconfigured, the built-in default is used);
//   - Circuit breaking / intent rerun are process-level; there is only one global instance.
//
// Global policy: one DB read of one settings row; call sites are all on low-frequency paths (building provider, work teardown, ...
// saving the config), not worth adding another cache layer; circuit-breaker parameters are the exception -- they must be read on every failure path, so
// Pushed to the Registry for persistence by applyRetryPolicy.

// retryPolicy reads the global policy; a nil DB yields the zero policy (all
// layers on their built-in defaults).
func (s *Server) retryPolicy() db.LLMRetryPolicy {
	if s.m == nil || s.m.pg == nil {
		return db.LLMRetryPolicy{}
	}
	return s.m.pg.LLMRetryPolicy()
}

// resolveRetry layers one profile's override on top of the global policy and
// converts the result into the form agent.Config carries. Rules combine field by
// field, so a profile that only pins an interval still inherits the global count.
func resolveRetry(o db.RetryOverride, pol db.LLMRetryPolicy) agent.RetryConfig {
	connect := o.Connect.Or(pol.Connect)
	empty := o.Empty.Or(pol.Empty)
	stream := o.Stream.Or(pol.Stream)
	return agent.RetryConfig{
		// Counts keep their original "0 = default / negative = off" semantics here: the SDK's MaxRetries /
		// EmptyResponseRetries is fully isomorphic to it; just let it parse itself.
		ConnectAttempts: connect.Attempts, ConnectInterval: connect.Interval(),
		EmptyAttempts: empty.Attempts, EmptyInterval: empty.Interval(),
		StreamAttempts: stream.Attempts, StreamInterval: stream.Interval(),
	}
}

// applyProfileRetry fills cfg.Retry for a profile read from the DB.
func (s *Server) applyProfileRetry(cfg *agent.Config, p *db.LLMProfile) {
	if p == nil {
		return
	}
	cfg.Retry = resolveRetry(p.Retry, s.retryPolicy())
}

// Default for circuit breaking (polling cooldown), consistent with llmpool's built-in -- only overridden here when "the user configured a value".
// For intent-rerun defaults see engine.go's modelErrorRetries / modelErrorRetryBackoff.

// applyRetryPolicy pushes the process-wide layers of the policy into the objects
// that consume them on a hot path: the circuit-breaker registry. Called at
// startup and whenever the policy is saved.
func (s *Server) applyRetryPolicy() {
	pol := s.retryPolicy()
	if s.llmHealth != nil {
		s.llmHealth.SetPolicy(pol.Breaker.Attempts, pol.Breaker.Interval())
	}
}

// modelErrorRetryPolicy resolves the intent-level replay knobs (layer ⑤): how
// many times a model_error work is re-run and how long to back off between runs.
func (e *Engine) modelErrorRetryPolicy() (retries int, backoff time.Duration) {
	retries, backoff = modelErrorRetries, modelErrorRetryBackoff
	if e == nil || e.m == nil || e.m.pg == nil {
		return retries, backoff
	}
	rule := e.m.pg.LLMRetryPolicy().Intent
	if rule.Attempts != 0 {
		retries = max(rule.Attempts, 0)
	}
	if d := rule.Interval(); d > 0 {
		backoff = d
	}
	return retries, backoff
}

// emptyTurnNudgeLimit resolves how many empty-turn continuations one work may
// inject (see steerHooks.Stop). It deliberately reuses layer ②'s knob ---- empty-response
// retry count": the two are two means to the same end. The SDK layer handles "not a single content block", by means of
// Re-send the same request as-is; here handles "thinking only, with neither body text nor tools", by appending an instruction to let
// Let the model continue with its existing thinking (re-sending as-is is meaningless for this kind of context-shape-determined idling). The empty-determination criteria
// The difference is because the SDK judges by "whether any event was yielded", while thinking deltas are themselves events -- but user-configured semantics treat them differently.
// "How many times to retry an empty response" means "if the model produced no substantive content, try again"; both layers share a single count
// Only then does it match this mental model.
//
// Read the global policy rather than some profile's override: a run may switch profiles mid-way due to failover, and this...
// It is the total gate for the whole intent and should not change when switching endpoints. Semantically isomorphic to the SDK's emptyRetries():
// 0 = default defaultEmptyTurnNudges; -1 (negative) = disable idle continuation; >0 = use that value.
func (e *Engine) emptyTurnNudgeLimit() int {
	if e == nil || e.m == nil || e.m.pg == nil {
		return defaultEmptyTurnNudges
	}
	switch n := e.m.pg.LLMRetryPolicy().Empty.Attempts; {
	case n == 0:
		return defaultEmptyTurnNudges
	case n < 0:
		return 0
	default:
		return n
	}
}
