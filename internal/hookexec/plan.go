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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"go.yaml.in/yaml/v3"

	release "helm.sh/helm/v4/pkg/release/v1"
	releaseutil "helm.sh/helm/v4/pkg/release/v1/util"
)

// Step is one unit of an execution plan.
type Step struct {
	// Hook is the hook to execute.
	Hook *release.Hook
	// Key is the idempotency key. Identical hook definitions (same
	// namespace, kind, name and manifest content) share a key.
	Key string
	// Group is the execution group this step belongs to.
	Group string
	// Timeout is the resolved per-hook timeout. Zero means the executor
	// default applies.
	Timeout time.Duration
}

// Plan is a deterministic, ordered execution sequence. Building a plan is
// a pure function of the hook definitions and requires no cluster access,
// so ordering decisions are fully unit-testable.
type Plan struct {
	Event release.HookEvent
	Steps []Step
	// Warnings carries non-fatal notices, e.g. duplicate hook
	// definitions that were collapsed into a single execution.
	Warnings []string
}

// PlanOptions controls plan construction. The zero value reproduces the
// legacy execution order exactly: one flat sequence sorted by weight and
// name.
type PlanOptions struct {
	// GroupByResource isolates hooks into per-resource execution groups
	// (explicit helm.sh/hook-group, or "<namespace>/<kind>" derived from
	// the manifest). Groups execute sequentially in lexicographic order;
	// ordering across groups is never interleaved, which prevents
	// dependency inversion between namespaces and resource types.
	// Disabled by default to preserve the legacy flat ordering.
	GroupByResource bool
	// DefaultNamespace is used when a manifest does not declare a
	// namespace.
	DefaultNamespace string
}

// NewPlan builds the execution plan for the given event from a release's
// hooks. Hooks are filtered by event, deduplicated by idempotency key, and
// ordered deterministically.
func NewPlan(hooks []*release.Hook, event release.HookEvent, opts PlanOptions) *Plan {
	plan := &Plan{Event: event}

	seen := map[string]bool{}
	var selected []*release.Hook
	for _, h := range hooks {
		if !hasEvent(h, event) {
			continue
		}
		key := hookKey(h, opts.DefaultNamespace)
		if seen[key] {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf(
				"duplicate hook definition %q (kind %s) collapsed into a single execution",
				h.Name, h.Kind))
			continue
		}
		seen[key] = true
		selected = append(selected, h)
	}

	if !opts.GroupByResource {
		// Legacy semantics: a single flat sequence ordered by the shared
		// weight/name comparator. This order is identical to the
		// non-orchestrated execution path.
		releaseutil.SortHooksByWeight(selected)
		for _, h := range selected {
			plan.Steps = append(plan.Steps, Step{
				Hook:    h,
				Key:     hookKey(h, opts.DefaultNamespace),
				Timeout: hookTimeout(h),
			})
		}
		return plan
	}

	groups := map[string][]*release.Hook{}
	var groupKeys []string
	for _, h := range selected {
		g := hookGroup(h, opts.DefaultNamespace)
		if _, ok := groups[g]; !ok {
			groupKeys = append(groupKeys, g)
		}
		groups[g] = append(groups[g], h)
	}
	sort.Strings(groupKeys)

	for _, g := range groupKeys {
		members := groups[g]
		releaseutil.SortHooksByWeight(members)
		for _, h := range members {
			plan.Steps = append(plan.Steps, Step{
				Hook:    h,
				Key:     hookKey(h, opts.DefaultNamespace),
				Group:   g,
				Timeout: hookTimeout(h),
			})
		}
	}
	return plan
}

func hasEvent(h *release.Hook, event release.HookEvent) bool {
	for _, e := range h.Events {
		if e == event {
			return true
		}
	}
	return false
}

// hookKey derives the idempotency key from the hook identity and content.
// The manifest hash makes the key sensitive to the hook "version": two
// definitions with the same name but different content are distinct hooks,
// while byte-identical duplicates collapse into one execution.
func hookKey(h *release.Hook, defaultNamespace string) string {
	sum := sha256.Sum256([]byte(h.Manifest))
	return fmt.Sprintf("%s/%s/%s@%s",
		hookNamespace(h, defaultNamespace), h.Kind, h.Name, hex.EncodeToString(sum[:8]))
}

// hookGroup resolves the execution group for a hook: the explicit
// annotation wins, otherwise "<namespace>/<kind>" from the manifest.
func hookGroup(h *release.Hook, defaultNamespace string) string {
	if g := hookAnnotations(h)[GroupAnnotation]; g != "" {
		return g
	}
	return hookNamespace(h, defaultNamespace) + "/" + h.Kind
}

// hookTimeout parses the per-hook timeout annotation. Invalid values are
// ignored and fall back to the executor default.
func hookTimeout(h *release.Hook) time.Duration {
	v := hookAnnotations(h)[TimeoutAnnotation]
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// hookNamespace extracts metadata.namespace from the hook manifest,
// falling back to the release namespace.
func hookNamespace(h *release.Hook, defaultNamespace string) string {
	tmp := struct {
		Metadata struct {
			Namespace string `yaml:"namespace"`
		} `yaml:"metadata"`
	}{}
	if err := yaml.Unmarshal([]byte(h.Manifest), &tmp); err != nil {
		return defaultNamespace
	}
	if tmp.Metadata.Namespace == "" {
		return defaultNamespace
	}
	return tmp.Metadata.Namespace
}

// hookAnnotations extracts metadata.annotations from the hook manifest.
// Parse failures yield no annotations, matching the lenient behavior of
// the existing hook parsing.
func hookAnnotations(h *release.Hook) map[string]string {
	tmp := struct {
		Metadata struct {
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"metadata"`
	}{}
	if err := yaml.Unmarshal([]byte(h.Manifest), &tmp); err != nil {
		return nil
	}
	return tmp.Metadata.Annotations
}
