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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	release "helm.sh/helm/v4/pkg/release/v1"
)

func TestPlanOrderIsDeterministic(t *testing.T) {
	hooks := []*release.Hook{
		testHook("cleanup", "Job", "", 10, nil, release.HookPreUpgrade),
		testHook("probe-b", "Job", "", 5, nil, release.HookPreUpgrade),
		testHook("precheck-1", "Job", "", -5, nil, release.HookPreUpgrade),
		testHook("migrate", "Job", "", 0, nil, release.HookPreUpgrade),
		testHook("probe-a", "Job", "", 5, nil, release.HookPreUpgrade),
		testHook("precheck-2", "Job", "", -5, nil, release.HookPreUpgrade),
	}

	want := []string{"precheck-1", "precheck-2", "migrate", "probe-a", "probe-b", "cleanup"}
	// The plan must be identical on every construction, regardless of
	// input ordering artifacts like map iteration.
	for i := 0; i < 20; i++ {
		plan := NewPlan(hooks, release.HookPreUpgrade, PlanOptions{})
		assert.Equal(t, want, stepNames(plan))
	}
}

func TestPlanDefaultOrderMatchesLegacy(t *testing.T) {
	// Without grouping, the plan must be a flat weight/name sort, the
	// same order the non-orchestrated path produces.
	hooks := []*release.Hook{
		testHook("zeta", "Job", "", 0, nil, release.HookPreInstall),
		testHook("alpha", "Job", "", 0, nil, release.HookPreInstall),
		testHook("mid", "Job", "", -1, nil, release.HookPreInstall),
	}
	plan := NewPlan(hooks, release.HookPreInstall, PlanOptions{})
	assert.Equal(t, []string{"mid", "alpha", "zeta"}, stepNames(plan))
	for _, s := range plan.Steps {
		assert.Empty(t, s.Group, "flat plan must not assign groups")
	}
}

func TestPlanGroupsByNamespaceAndKind(t *testing.T) {
	hooks := []*release.Hook{
		testHook("a", "Job", "ns-b", 0, nil, release.HookPreUpgrade),
		testHook("b", "Job", "ns-a", 5, nil, release.HookPreUpgrade),
		testHook("c", "Pod", "ns-a", -5, nil, release.HookPreUpgrade),
		testHook("d", "Job", "ns-a", -10, nil, release.HookPreUpgrade),
	}
	plan := NewPlan(hooks, release.HookPreUpgrade, PlanOptions{GroupByResource: true})

	// Groups sort lexicographically: ns-a/Job < ns-a/Pod < ns-b/Job.
	// Weights only order hooks within their own group, so the ns-a/Pod
	// hook "c" (weight -5) must not jump ahead of ns-a/Job hooks.
	assert.Equal(t, []string{"d", "b", "c", "a"}, stepNames(plan))
	assert.Equal(t, "ns-a/Job", plan.Steps[0].Group)
	assert.Equal(t, "ns-a/Pod", plan.Steps[2].Group)
	assert.Equal(t, "ns-b/Job", plan.Steps[3].Group)
}

func TestPlanExplicitGroupAnnotation(t *testing.T) {
	hooks := []*release.Hook{
		testHook("late", "Job", "zz", 0, map[string]string{GroupAnnotation: "01-schema"}, release.HookPreUpgrade),
		testHook("early", "Job", "aa", 0, map[string]string{GroupAnnotation: "02-data"}, release.HookPreUpgrade),
	}
	plan := NewPlan(hooks, release.HookPreUpgrade, PlanOptions{GroupByResource: true})
	assert.Equal(t, []string{"late", "early"}, stepNames(plan))
}

func TestPlanDeduplicatesIdenticalHooks(t *testing.T) {
	dup := testHook("migrate", "Job", "", 0, nil, release.HookPreUpgrade)
	hooks := []*release.Hook{dup, dup}
	plan := NewPlan(hooks, release.HookPreUpgrade, PlanOptions{})

	require.Len(t, plan.Steps, 1, "identical hook definitions must execute only once")
	require.Len(t, plan.Warnings, 1)
	assert.Contains(t, plan.Warnings[0], "duplicate hook definition")
}

func TestPlanKeepsSameNameDifferentContent(t *testing.T) {
	a := testHook("migrate", "Job", "ns-a", 0, nil, release.HookPreUpgrade)
	b := testHook("migrate", "Job", "ns-b", 0, nil, release.HookPreUpgrade)
	plan := NewPlan([]*release.Hook{a, b}, release.HookPreUpgrade, PlanOptions{})

	assert.Len(t, plan.Steps, 2, "same name in different namespaces are distinct hooks")
	assert.Empty(t, plan.Warnings)
}

func TestPlanTimeoutAnnotation(t *testing.T) {
	h := testHook("migrate", "Job", "", 0, map[string]string{TimeoutAnnotation: "90s"}, release.HookPreUpgrade)
	bad := testHook("broken", "Job", "", 0, map[string]string{TimeoutAnnotation: "not-a-duration"}, release.HookPreUpgrade)

	plan := NewPlan([]*release.Hook{h, bad}, release.HookPreUpgrade, PlanOptions{})
	require.Len(t, plan.Steps, 2)

	byName := map[string]time.Duration{}
	for _, s := range plan.Steps {
		byName[s.Hook.Name] = s.Timeout
	}
	assert.Equal(t, 90*time.Second, byName["migrate"])
	assert.Zero(t, byName["broken"], "invalid timeout falls back to executor default")
}
