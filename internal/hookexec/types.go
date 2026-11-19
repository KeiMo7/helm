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

// Package hookexec provides orchestrated hook execution on top of the
// existing Helm hook model: deterministic ordering, fail-fast semantics,
// bounded rollback, idempotent re-runs, and cluster-independent state
// recording.
//
// The package is opt-in. It reuses the existing hook parsing
// (pkg/release/v1/util.SortManifests) and ordering
// (pkg/release/v1/util.SortHooksByWeight) and does not change the default
// execution behavior of Helm.
package hookexec

import (
	"time"
)

// Orchestration annotations. These follow the existing helm.sh/hook-*
// convention and are parsed from the hook manifest; no new annotation
// system is introduced.
const (
	// TimeoutAnnotation sets a per-hook timeout as a Go duration string
	// (e.g. "30s", "5m"). When absent, the executor default applies.
	TimeoutAnnotation = "helm.sh/hook-timeout"

	// GroupAnnotation assigns the hook to an explicit execution group.
	// Hooks are only ordered within their group; groups run one after
	// another in lexicographic order of their key. When group isolation
	// is enabled and this annotation is absent, the group defaults to
	// "<namespace>/<kind>" derived from the manifest, so hooks in
	// different namespaces or of different resource types are never
	// ordered against each other.
	GroupAnnotation = "helm.sh/hook-group"
)

// Phase is the lifecycle state of a single hook execution, as recorded in
// the execution state store.
type Phase string

const (
	// PhasePending means the hook has not been executed yet.
	PhasePending Phase = "Pending"
	// PhaseRunning means the hook was started. If a record is found in
	// this phase at load time, the process was interrupted and the hook
	// result is unknown.
	PhaseRunning Phase = "Running"
	// PhaseSucceeded means the hook completed successfully.
	PhaseSucceeded Phase = "Succeeded"
	// PhaseFailed means the hook returned a business-level error.
	PhaseFailed Phase = "Failed"
	// PhaseTimeout means the hook exceeded its deadline and was
	// interrupted. It is recorded distinctly from PhaseFailed.
	PhaseTimeout Phase = "Timeout"
	// PhaseSkipped means the hook was not executed because an earlier
	// hook in the same sequence failed.
	PhaseSkipped Phase = "Skipped"
	// PhaseUnknown means the hook was started but its result is unknown
	// (the executing process was killed or restarted mid-run).
	PhaseUnknown Phase = "Unknown"
)

// Record is the persisted execution state of a single hook.
type Record struct {
	// Key is the idempotency key identifying the hook definition.
	Key string `json:"key"`
	// Name is the hook resource name, for display purposes.
	Name string `json:"name"`
	// Group is the execution group the hook belongs to.
	Group string `json:"group"`
	// Phase is the current lifecycle phase.
	Phase Phase `json:"phase"`
	// Attempts counts how many times execution of this hook was started.
	Attempts int `json:"attempts"`
	// StartedAt is the time the last attempt started.
	StartedAt time.Time `json:"started_at,omitzero"`
	// CompletedAt is the time the last attempt finished.
	CompletedAt time.Time `json:"completed_at,omitzero"`
	// Output is the captured hook output, truncated to the configured
	// maximum number of bytes.
	Output string `json:"output,omitempty"`
	// OutputTruncated indicates Output was cut off to fit the limit.
	OutputTruncated bool `json:"output_truncated,omitempty"`
	// Error carries the failure message for failed/timed-out hooks.
	Error string `json:"error,omitempty"`
}

// State is the persisted execution state of one hook sequence for one
// release revision and event. It is stored independently of cluster
// resources; cluster state is never consulted to reconstruct progress.
type State struct {
	Release   string             `json:"release"`
	Namespace string             `json:"namespace"`
	Revision  int                `json:"revision"`
	Event     string             `json:"event"`
	Records   map[string]*Record `json:"records"`
	UpdatedAt time.Time          `json:"updated_at"`
}

// newState returns an empty state for the given coordinates.
func newState(release, namespace string, revision int, event string) *State {
	return &State{
		Release:   release,
		Namespace: namespace,
		Revision:  revision,
		Event:     event,
		Records:   map[string]*Record{},
	}
}
