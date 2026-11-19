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
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	release "helm.sh/helm/v4/pkg/release/v1"
)

// memStore is an in-memory Store for tests.
type memStore struct {
	state *State
	err   error
}

func (m *memStore) Load(context.Context) (*State, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.state, nil
}

func (m *memStore) Save(_ context.Context, s *State) error {
	m.state = s
	return nil
}

// scriptRunner executes hooks according to a per-name function and
// records the execution order.
type scriptRunner struct {
	mu      sync.Mutex
	order   []string
	scripts map[string]func(ctx context.Context) (string, error)
}

func (r *scriptRunner) Run(ctx context.Context, h *release.Hook) (string, error) {
	r.mu.Lock()
	r.order = append(r.order, h.Name)
	r.mu.Unlock()
	if fn, ok := r.scripts[h.Name]; ok {
		return fn(ctx)
	}
	return "", nil
}

func newTestExecutor(store Store, runner Runner) *Executor {
	return &Executor{
		Release:   "rel",
		Namespace: "default",
		Revision:  1,
		Store:     store,
		Runner:    runner,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func threeHookPlan(t *testing.T) *Plan {
	t.Helper()
	hooks := []*release.Hook{
		testHook("first", "Job", "", -1, nil, release.HookPreUpgrade),
		testHook("second", "Job", "", 0, nil, release.HookPreUpgrade),
		testHook("third", "Job", "", 1, nil, release.HookPreUpgrade),
	}
	return NewPlan(hooks, release.HookPreUpgrade, PlanOptions{})
}

func TestExecuteSuccessRecordsState(t *testing.T) {
	store := &memStore{}
	runner := &scriptRunner{scripts: map[string]func(context.Context) (string, error){}}
	ex := newTestExecutor(store, runner)

	res, err := ex.Execute(context.Background(), threeHookPlan(t), nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"first", "second", "third"}, runner.order)
	assert.Len(t, res.Executed, 3)
	assert.Empty(t, res.Skipped)

	for _, rec := range store.state.Records {
		assert.Equal(t, PhaseSucceeded, rec.Phase)
		assert.False(t, rec.CompletedAt.IsZero())
	}
}

func TestExecuteFailFastSkipsRemaining(t *testing.T) {
	store := &memStore{}
	boom := errors.New("boom")
	runner := &scriptRunner{scripts: map[string]func(context.Context) (string, error){
		"second": func(context.Context) (string, error) { return "", boom },
	}}
	ex := newTestExecutor(store, runner)

	res, err := ex.Execute(context.Background(), threeHookPlan(t), nil)
	require.Error(t, err)
	assert.Equal(t, []string{"first", "second"}, runner.order, "no hook may run after the failure point")
	assert.ErrorIs(t, res.Err, boom)

	assert.Equal(t, PhaseSucceeded, store.state.Records[res.Executed[0]].Phase)
	assert.Equal(t, PhaseFailed, store.state.Records[res.Executed[1]].Phase)

	var third *Record
	for _, rec := range store.state.Records {
		if rec.Name == "third" {
			third = rec
		}
	}
	require.NotNil(t, third)
	assert.Equal(t, PhaseSkipped, third.Phase, "hooks after the failure point are marked skipped")
}

func TestExecuteTimeoutDistinctFromFailure(t *testing.T) {
	store := &memStore{}
	runner := &scriptRunner{scripts: map[string]func(context.Context) (string, error){
		"second": func(ctx context.Context) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		},
	}}
	ex := newTestExecutor(store, runner)
	ex.DefaultTimeout = 20 * time.Millisecond

	res, err := ex.Execute(context.Background(), threeHookPlan(t), nil)
	require.Error(t, err)
	assert.Contains(t, res.Err.Error(), "timed out")

	var second *Record
	for _, rec := range store.state.Records {
		if rec.Name == "second" {
			second = rec
		}
	}
	require.NotNil(t, second)
	assert.Equal(t, PhaseTimeout, second.Phase, "timeout must be recorded distinctly from business failure")
}

func TestExecuteIdempotentRerun(t *testing.T) {
	store := &memStore{}
	boom := errors.New("boom")
	failSecond := true
	runner := &scriptRunner{scripts: map[string]func(context.Context) (string, error){
		"second": func(context.Context) (string, error) {
			if failSecond {
				return "", boom
			}
			return "", nil
		},
	}}
	ex := newTestExecutor(store, runner)
	plan := threeHookPlan(t)

	_, err := ex.Execute(context.Background(), plan, nil)
	require.Error(t, err)

	failSecond = false
	runner.order = nil
	res, err := ex.Execute(context.Background(), plan, nil)
	require.NoError(t, err)

	assert.Equal(t, []string{"second", "third"}, runner.order,
		"already-succeeded hooks must not produce side effects again")
	assert.Len(t, res.Skipped, 1, "the previously succeeded hook is reported as skipped")
}

func TestExecuteDetectsInterruptedRun(t *testing.T) {
	store := &memStore{state: &State{
		Records: map[string]*Record{
			// Simulates a process killed while "second" was running.
		},
	}}
	plan := threeHookPlan(t)
	var secondKey string
	for _, s := range plan.Steps {
		switch s.Hook.Name {
		case "first":
			store.state.Records[s.Key] = &Record{Key: s.Key, Name: s.Hook.Name, Phase: PhaseSucceeded}
		case "second":
			// Simulates a process killed while "second" was running.
			store.state.Records[s.Key] = &Record{Key: s.Key, Name: s.Hook.Name, Phase: PhaseRunning}
			secondKey = s.Key
		}
	}

	runner := &scriptRunner{scripts: map[string]func(context.Context) (string, error){}}
	ex := newTestExecutor(store, runner)
	res, err := ex.Execute(context.Background(), plan, nil)
	require.NoError(t, err)

	assert.Equal(t, []string{secondKey}, res.Unknown,
		"hooks left in Running state must be reported as executed-but-unknown")
	assert.Equal(t, []string{"second", "third"}, runner.order,
		"unknown-result hooks are re-run, succeeded ones are skipped")
	assert.Len(t, res.Skipped, 1, "the already-succeeded hook is skipped")
}

func TestExecuteCorruptedStateFallsBackToFullRerun(t *testing.T) {
	store := &memStore{err: ErrStateCorrupted}
	runner := &scriptRunner{scripts: map[string]func(context.Context) (string, error){}}
	ex := newTestExecutor(store, runner)

	res, err := ex.Execute(context.Background(), threeHookPlan(t), nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"first", "second", "third"}, runner.order,
		"corrupted state must degrade to a full re-run, never a silent skip")
	assert.NotEmpty(t, res.Warnings, "the fallback must be surfaced as a warning")
}

func TestRollbackRunsOnFailure(t *testing.T) {
	store := &memStore{}
	boom := errors.New("boom")
	runner := &scriptRunner{scripts: map[string]func(context.Context) (string, error){
		"second": func(context.Context) (string, error) { return "", boom },
	}}
	ex := newTestExecutor(store, runner)

	rollback := NewPlan([]*release.Hook{
		testHook("restore", "Job", "", 0, nil, release.HookPreRollback),
	}, release.HookPreRollback, PlanOptions{})

	res, err := ex.Execute(context.Background(), threeHookPlan(t), rollback)
	require.Error(t, err)
	require.NotNil(t, res.Rollback)
	assert.NoError(t, res.Rollback.Err)
	assert.Equal(t, []string{"first", "second", "restore"}, runner.order)
}

func TestRollbackRetriesAreBounded(t *testing.T) {
	store := &memStore{}
	boom := errors.New("boom")
	rbAttempts := 0
	runner := &scriptRunner{scripts: map[string]func(context.Context) (string, error){
		"second": func(context.Context) (string, error) { return "", boom },
		"restore": func(context.Context) (string, error) {
			rbAttempts++
			return "", errors.New("restore failed")
		},
	}}
	ex := newTestExecutor(store, runner)
	ex.RollbackMaxAttempts = 3

	rollback := NewPlan([]*release.Hook{
		testHook("restore", "Job", "", 0, nil, release.HookPreRollback),
	}, release.HookPreRollback, PlanOptions{})

	res, err := ex.Execute(context.Background(), threeHookPlan(t), rollback)
	require.Error(t, err)
	assert.Equal(t, 3, rbAttempts, "rollback retries must be bounded")
	assert.Contains(t, res.Rollback.Err.Error(), "did not complete after 3 attempt")

	// Records of the failed rollback must be preserved, not swallowed.
	var restore *Record
	for _, rec := range store.state.Records {
		if rec.Name == "restore" {
			restore = rec
		}
	}
	require.NotNil(t, restore)
	assert.Equal(t, PhaseFailed, restore.Phase)
	assert.Equal(t, 3, restore.Attempts)
}

func TestRollbackKeepsSucceededStepsOnRetry(t *testing.T) {
	store := &memStore{}
	boom := errors.New("boom")
	stepAFails := true
	stepARuns := 0
	runner := &scriptRunner{scripts: map[string]func(context.Context) (string, error){
		"second": func(context.Context) (string, error) { return "", boom },
		"rb-a": func(context.Context) (string, error) {
			stepARuns++
			if stepAFails {
				stepAFails = false
				return "", errors.New("transient")
			}
			return "", nil
		},
	}}
	ex := newTestExecutor(store, runner)
	ex.RollbackMaxAttempts = 2

	rollback := NewPlan([]*release.Hook{
		testHook("rb-a", "Job", "", 0, nil, release.HookPreRollback),
		testHook("rb-b", "Job", "", 1, nil, release.HookPreRollback),
	}, release.HookPreRollback, PlanOptions{})

	res, err := ex.Execute(context.Background(), threeHookPlan(t), rollback)
	require.Error(t, err) // forward path failed
	require.NotNil(t, res.Rollback)
	assert.NoError(t, res.Rollback.Err, "rollback succeeds on the second attempt")
	assert.Equal(t, 2, stepARuns)
}

func TestHookOutputIsTruncated(t *testing.T) {
	store := &memStore{}
	long := make([]byte, 10000)
	for i := range long {
		long[i] = 'x'
	}
	runner := &scriptRunner{scripts: map[string]func(context.Context) (string, error){
		"first": func(context.Context) (string, error) { return string(long), nil },
	}}
	ex := newTestExecutor(store, runner)
	ex.MaxLogBytes = 1024

	_, err := ex.Execute(context.Background(), threeHookPlan(t), nil)
	require.NoError(t, err)

	for _, rec := range store.state.Records {
		assert.LessOrEqual(t, len(rec.Output), 1024, "persisted output must be capped")
		if rec.Name == "first" {
			assert.True(t, rec.OutputTruncated)
			assert.Len(t, rec.Output, 1024)
		}
	}
}

func TestTruncateMultibyteSafe(t *testing.T) {
	out, truncated := truncate("abc你好世界", 6)
	assert.True(t, truncated)
	assert.Equal(t, "abc你", out)

	// A cut landing inside a multi-byte rune backs off to the rune boundary.
	out, truncated = truncate("abc你好世界", 4)
	assert.True(t, truncated)
	assert.Equal(t, "abc", out)
}
