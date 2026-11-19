/*
Copyright The Helm Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package hookexec

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"

	release "helm.sh/helm/v4/pkg/release/v1"
)

// DefaultMaxLogBytes caps how much hook output is persisted per record.
// Output beyond the cap is truncated and flagged, so long logs can never
// bloat the state store.
const DefaultMaxLogBytes = 4096

// DefaultRollbackMaxAttempts bounds rollback retries. Rollback never
// retries indefinitely.
const DefaultRollbackMaxAttempts = 3

// Runner executes a single hook. The executor is decoupled from the
// Kubernetes client through this interface, which keeps all ordering and
// lifecycle decisions unit-testable without a cluster. The returned error
// is treated as a business-level failure; a deadline-exceeded context is
// treated as a timeout.
type Runner interface {
	Run(ctx context.Context, h *release.Hook) (output string, err error)
}

// RunnerFunc adapts a function to Runner.
type RunnerFunc func(ctx context.Context, h *release.Hook) (string, error)

// Run implements Runner.
func (f RunnerFunc) Run(ctx context.Context, h *release.Hook) (string, error) {
	return f(ctx, h)
}

// Executor runs hook plans with deterministic, fail-fast, idempotent
// semantics. All hooks execute sequentially; hooks of equal weight are
// never run in parallel.
type Executor struct {
	// Release, Namespace and Revision identify the owning release in
	// persisted state records.
	Release   string
	Namespace string
	Revision  int
	// Store persists execution state. Required.
	Store Store
	// Runner executes individual hooks. Required.
	Runner Runner
	// DefaultTimeout applies to hooks without a timeout annotation.
	// Zero means no timeout.
	DefaultTimeout time.Duration
	// MaxLogBytes caps persisted hook output. Defaults to
	// DefaultMaxLogBytes.
	MaxLogBytes int
	// RollbackMaxAttempts bounds rollback retries. Defaults to
	// DefaultRollbackMaxAttempts.
	RollbackMaxAttempts int
	// Logger receives warnings and notices. Defaults to slog.Default().
	Logger *slog.Logger
	// now is overridable for tests.
	now func() time.Time
}

// RollbackResult reports the outcome of the rollback path.
type RollbackResult struct {
	// Executed lists rollback hook keys that ran, in order.
	Executed []string
	// Attempts counts rollback attempts made.
	Attempts int
	// FailedStep is the hook key that caused rollback to stop, if any.
	FailedStep string
	// Err is non-nil when rollback did not complete.
	Err error
}

// Result reports the outcome of an Execute call.
type Result struct {
	// Executed lists hook keys executed in this run, in order.
	Executed []string
	// Skipped lists hook keys skipped because a previous run already
	// completed them successfully (idempotent re-run).
	Skipped []string
	// Unknown lists hook keys found in "Running" state at load time:
	// executed by a previous process, result unknown. They are re-run.
	Unknown []string
	// FailedStep is the hook key that ended the forward path, if any.
	FailedStep string
	// Rollback describes the rollback path, when taken.
	Rollback *RollbackResult
	// Warnings carries non-fatal notices (corrupted state fallback,
	// duplicate definitions, ...).
	Warnings []string
	// Err is non-nil when the sequence did not complete.
	Err error
}

// Execute runs the plan sequentially. On the first failure (business
// error or timeout) no further steps are started; the failure is recorded
// and the rollback plan runs with bounded retries. Re-running Execute
// after a failure skips hooks already recorded as succeeded.
func (e *Executor) Execute(ctx context.Context, plan, rollbackPlan *Plan) (*Result, error) {
	res := &Result{Warnings: append([]string{}, plan.Warnings...)}

	state, err := e.loadState(plan)
	if err != nil {
		res.Warnings = append(res.Warnings, err.Error())
		e.logger().Warn("hook execution state unusable, falling back to full re-run", "error", err)
		state = e.freshState(plan)
	}

	// A record left in Running means the previous process died mid-hook:
	// the hook was executed but its result is unknown. Surface these and
	// re-run them, since their side effects cannot be confirmed.
	for _, step := range plan.Steps {
		if rec, ok := state.Records[step.Key]; ok && rec.Phase == PhaseRunning {
			rec.Phase = PhaseUnknown
			res.Unknown = append(res.Unknown, step.Key)
		}
	}
	if len(res.Unknown) > 0 {
		e.save(state)
	}

	for i, step := range plan.Steps {
		if rec, ok := state.Records[step.Key]; ok && rec.Phase == PhaseSucceeded {
			res.Skipped = append(res.Skipped, step.Key)
			continue
		}

		phase, runErr := e.runStep(ctx, state, step)
		res.Executed = append(res.Executed, step.Key)
		if runErr == nil {
			continue
		}

		// Fail-fast: every step after the failure point is marked
		// skipped and never started.
		res.FailedStep = step.Key
		res.Err = runErr
		for _, rest := range plan.Steps[i+1:] {
			state.Records[rest.Key] = &Record{
				Key: rest.Key, Name: rest.Hook.Name, Group: rest.Group, Phase: PhaseSkipped,
			}
		}
		e.save(state)
		e.logger().Warn("hook failed, aborting sequence and entering rollback",
			"hook", step.Hook.Name, "phase", phase, "error", runErr)

		res.Rollback = e.rollback(ctx, state, rollbackPlan)
		if res.Rollback.Err != nil {
			res.Err = fmt.Errorf("%w; rollback incomplete: %w", runErr, res.Rollback.Err)
		}
		return res, res.Err
	}

	return res, nil
}

// rollback executes the rollback plan with bounded retries. Completed
// rollback steps are recorded and never re-run across attempts. When
// attempts are exhausted the executor stops, keeps all records, and
// returns the error; it never swallows the failure and never loops
// forever.
func (e *Executor) rollback(ctx context.Context, state *State, plan *Plan) *RollbackResult {
	rb := &RollbackResult{}
	if plan == nil || len(plan.Steps) == 0 {
		return rb
	}

	maxAttempts := e.RollbackMaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultRollbackMaxAttempts
	}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		rb.Attempts = attempt
		failed := false
		for _, step := range plan.Steps {
			if rec, ok := state.Records[step.Key]; ok && rec.Phase == PhaseSucceeded {
				continue
			}
			_, runErr := e.runStep(ctx, state, step)
			rb.Executed = append(rb.Executed, step.Key)
			if runErr != nil {
				rb.FailedStep = step.Key
				rb.Err = runErr
				failed = true
				break
			}
		}
		if !failed {
			rb.Err = nil
			rb.FailedStep = ""
			return rb
		}
		e.logger().Warn("rollback attempt failed",
			"attempt", attempt, "maxAttempts", maxAttempts, "error", rb.Err)
	}
	rb.Err = fmt.Errorf("rollback did not complete after %d attempt(s), stopping at hook %q: %w",
		maxAttempts, rb.FailedStep, rb.Err)
	return rb
}

// runStep executes one hook, persisting Running before and the outcome
// after, so an interrupted process always leaves a recoverable record.
func (e *Executor) runStep(ctx context.Context, state *State, step Step) (Phase, error) {
	rec := state.Records[step.Key]
	if rec == nil {
		rec = &Record{Key: step.Key, Name: step.Hook.Name, Group: step.Group}
		state.Records[step.Key] = rec
	}
	rec.Phase = PhaseRunning
	rec.Attempts++
	rec.StartedAt = e.clock()()
	rec.CompletedAt = time.Time{}
	rec.Error = ""
	e.save(state)

	runCtx := ctx
	cancel := func() {}
	if timeout := step.TimeoutOrDefault(e.DefaultTimeout); timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	output, runErr := e.Runner.Run(runCtx, step.Hook)
	cancel()

	rec.CompletedAt = e.clock()()
	rec.Output, rec.OutputTruncated = truncate(output, e.maxLogBytes())

	switch {
	case runErr == nil:
		rec.Phase = PhaseSucceeded
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		// Timeout is recorded distinctly from a business-level failure.
		rec.Phase = PhaseTimeout
		rec.Error = fmt.Sprintf("hook timed out: %v", runErr)
		runErr = fmt.Errorf("hook %q timed out: %w", step.Hook.Name, runErr)
	default:
		rec.Phase = PhaseFailed
		rec.Error = runErr.Error()
		runErr = fmt.Errorf("hook %q failed: %w", step.Hook.Name, runErr)
	}
	e.save(state)
	return rec.Phase, runErr
}

// TimeoutOrDefault resolves the effective timeout for a step.
func (s Step) TimeoutOrDefault(def time.Duration) time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return def
}

func (e *Executor) loadState(plan *Plan) (*State, error) {
	state, err := e.Store.Load(context.Background())
	if err != nil {
		return nil, err
	}
	if state == nil {
		return e.freshState(plan), nil
	}
	return state, nil
}

func (e *Executor) freshState(plan *Plan) *State {
	return newState(e.Release, e.Namespace, e.Revision, plan.Event.String())
}

func (e *Executor) save(state *State) {
	state.UpdatedAt = e.clock()()
	if err := e.Store.Save(context.Background(), state); err != nil {
		e.logger().Error("failed to persist hook execution state", "error", err)
	}
}

func (e *Executor) clock() func() time.Time {
	if e.now != nil {
		return e.now
	}
	return time.Now
}

func (e *Executor) maxLogBytes() int {
	if e.MaxLogBytes > 0 {
		return e.MaxLogBytes
	}
	return DefaultMaxLogBytes
}

func (e *Executor) logger() *slog.Logger {
	if e.Logger != nil {
		return e.Logger
	}
	return slog.Default()
}

// truncate caps output at limit bytes (on a rune boundary) and reports
// whether truncation happened.
func truncate(output string, limit int) (string, bool) {
	if len(output) <= limit {
		return output, false
	}
	cut := output[:limit]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut, true
}
