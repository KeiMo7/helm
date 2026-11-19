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
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	release "helm.sh/helm/v4/pkg/release/v1"
)

// TestDrillSixHookRelease is the acceptance drill for the incident
// scenario: a release pipeline of six hooks (two pre-checks, one data
// migration, two liveness probes, one cleanup) where the sixth hook
// fails. It verifies and prints:
//
//  1. the deterministic execution sequence,
//  2. the rollback sequence taken after the failure,
//  3. the skip list when the release is re-run after fixing the cause,
//  4. that oversized hook output is truncated in the state record.
func TestDrillSixHookRelease(t *testing.T) {
	hooks := []*release.Hook{
		testHook("precheck-schema", "Job", "", -10, nil, release.HookPreUpgrade),
		testHook("precheck-quota", "Job", "", -10, nil, release.HookPreUpgrade),
		testHook("migrate-db", "Job", "", 0,
			map[string]string{TimeoutAnnotation: "5m"}, release.HookPreUpgrade),
		testHook("probe-api", "Job", "", 10, nil, release.HookPreUpgrade),
		testHook("probe-worker", "Job", "", 10, nil, release.HookPreUpgrade),
		testHook("cleanup-temp", "Job", "", 20, nil, release.HookPreUpgrade),
		// Rollback path: restore the migration backup, then verify.
		testHook("restore-backup", "Job", "", 0, nil, release.HookPreRollback),
		testHook("verify-restore", "Job", "", 10, nil, release.HookPreRollback),
	}

	plan := NewPlan(hooks, release.HookPreUpgrade, PlanOptions{})
	rollbackPlan := NewPlan(hooks, release.HookPreRollback, PlanOptions{})

	// The sixth hook (cleanup-temp) fails; the migration emits a log
	// larger than the state record limit.
	cleanupFails := true
	longLog := strings.Repeat("migration row processed\n", 500) // ~12KB
	runner := &scriptRunner{scripts: map[string]func(context.Context) (string, error){
		"migrate-db": func(context.Context) (string, error) { return longLog, nil },
		"cleanup-temp": func(context.Context) (string, error) {
			if cleanupFails {
				return "", errors.New("temp table already dropped")
			}
			return "", nil
		},
	}}

	store := NewFileStore(t.TempDir(), "shop", "pre-upgrade", 7)
	ex := &Executor{
		Release:     "shop",
		Namespace:   "default",
		Revision:    7,
		Store:       store,
		Runner:      runner,
		MaxLogBytes: 4096,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// Run 1: cleanup-temp fails.
	res, err := ex.Execute(context.Background(), plan, rollbackPlan)
	require.Error(t, err)

	execSeq := runner.order[:6]
	rollbackSeq := runner.order[6:]
	t.Logf("execution sequence: %v", execSeq)
	t.Logf("failure at: %s", res.FailedStep)
	t.Logf("rollback sequence: %v (attempts=%d)", rollbackSeq, res.Rollback.Attempts)

	assert.Equal(t, []string{
		"precheck-quota", "precheck-schema", "migrate-db",
		"probe-api", "probe-worker", "cleanup-temp",
	}, execSeq, "six hooks run in deterministic weight/name order")
	assert.Equal(t, []string{"restore-backup", "verify-restore"}, rollbackSeq,
		"rollback path runs to completion after the failure")
	assert.Equal(t, 1, res.Rollback.Attempts)

	// The migration log must be truncated in the persisted state.
	state, loadErr := store.Load(context.Background())
	require.NoError(t, loadErr)
	for _, rec := range state.Records {
		assert.LessOrEqual(t, len(rec.Output), 4096,
			"no record may hold more than the log cap, got %d bytes for %s", len(rec.Output), rec.Name)
		if rec.Name == "migrate-db" {
			assert.True(t, rec.OutputTruncated, "oversized migration log must be flagged truncated")
		}
	}

	// Run 2: cause fixed, idempotent re-run.
	cleanupFails = false
	runner.order = nil
	res2, err := ex.Execute(context.Background(), plan, rollbackPlan)
	require.NoError(t, err)

	t.Logf("re-run skip list (%d hooks):", len(res2.Skipped))
	for _, key := range res2.Skipped {
		t.Logf("  skip %s", key)
	}
	t.Logf("re-run executed: %v", runner.order)

	assert.Len(t, res2.Skipped, 5, "the five hooks that succeeded before are skipped")
	assert.Equal(t, []string{"cleanup-temp"}, runner.order,
		"only the previously failed hook re-runs")

	fmt.Fprintf(t.Output(), "\ndrill complete: %d executed, %d skipped, rollback attempts=%d\n",
		len(res2.Executed), len(res2.Skipped), res.Rollback.Attempts)
}

// TestDrillRollbackItselfFails covers the worst case: the forward path
// fails and the rollback path keeps failing too. Retries must be bounded
// and all completed-step records must survive.
func TestDrillRollbackItselfFails(t *testing.T) {
	hooks := []*release.Hook{
		testHook("migrate-db", "Job", "", 0, nil, release.HookPreUpgrade),
		testHook("restore-backup", "Job", "", 0, nil, release.HookPreRollback),
	}
	plan := NewPlan(hooks, release.HookPreUpgrade, PlanOptions{})
	rollbackPlan := NewPlan(hooks, release.HookPreRollback, PlanOptions{})

	runner := &scriptRunner{scripts: map[string]func(context.Context) (string, error){
		"migrate-db":     func(context.Context) (string, error) { return "", errors.New("migration failed") },
		"restore-backup": func(context.Context) (string, error) { return "", errors.New("backup corrupted") },
	}}
	store := NewFileStore(t.TempDir(), "shop", "pre-upgrade", 8)
	ex := &Executor{
		Release: "shop", Namespace: "default", Revision: 8,
		Store: store, Runner: runner,
		RollbackMaxAttempts: 3,
		Logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	res, err := ex.Execute(context.Background(), plan, rollbackPlan)
	require.Error(t, err)
	t.Logf("forward error: %v", err)
	t.Logf("rollback stopped after %d attempts at %s", res.Rollback.Attempts, res.Rollback.FailedStep)

	assert.Equal(t, 3, res.Rollback.Attempts, "rollback retries are bounded, never infinite")
	assert.Contains(t, err.Error(), "rollback incomplete")

	state, loadErr := store.Load(context.Background())
	require.NoError(t, loadErr)
	var restore *Record
	for _, rec := range state.Records {
		if rec.Name == "restore-backup" {
			restore = rec
		}
	}
	require.NotNil(t, restore, "failed rollback steps must remain recorded")
	assert.Equal(t, PhaseFailed, restore.Phase)
	assert.Equal(t, 3, restore.Attempts)
}
