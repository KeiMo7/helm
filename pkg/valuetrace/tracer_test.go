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

package valuetrace

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	chart "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
)

func fileSrc(index int, name string) Source {
	return Source{Layer: LayerValueFile, Kind: KindFile, Name: name, Index: index}
}

func setSrc(index int, raw string) Source {
	return Source{Layer: LayerCLI, Kind: KindSet, Name: raw, Index: index}
}

func originMap(t *Trace) map[string]Origin {
	out := map[string]Origin{}
	for _, origin := range t.Origins() {
		out[origin.Path] = origin
	}
	return out
}

// renderedFor stands in for the coalesced render values in unit tests by
// applying chart defaults onto the user supplied map with the repository's
// MergeMaps: parent defaults then surviving subchart defaults.
func renderedFor(root *chart.Chart, user map[string]any) map[string]any {
	rendered := loader.MergeMaps(root.Values, user)
	for _, dep := range root.Dependencies() {
		name := dep.Metadata.Name
		table, ok := rendered[name].(map[string]any)
		if !ok {
			table = map[string]any{}
		}
		rendered[name] = loader.MergeMaps(dep.Values, table)
	}
	return rendered
}

// TestMergeResultIdenticalToMergeMaps proves tracing never changes the merge
// result: every observed merge returns the exact map loader.MergeMaps would
// return without a tracer.
func TestMergeResultIdenticalToMergeMaps(t *testing.T) {
	tracer := NewTracer()

	current := map[string]any{}
	plain := map[string]any{}

	inputs := []map[string]any{
		{"replicas": 1, "image": map[string]any{"tag": "v1", "pull": "always"}},
		{"replicas": 2, "image": map[string]any{"tag": "v2"}},
		{"tags": []any{"a", "b"}},
		{"tags": []any{"c"}},
		{"cleared": nil},
	}
	for i, in := range inputs {
		current = tracer.Merge(current, in, fileSrc(i, "f.yaml"))
		plain = loader.MergeMaps(plain, in)
	}

	assert.Equal(t, plain, current)
}

// TestFourLayerPrecedence covers subchart default, parent values, value file
// and CLI set layers coexisting with a fixed, explainable precedence.
func TestFourLayerPrecedence(t *testing.T) {
	root := &chart.Chart{
		Metadata: &chart.Metadata{Name: "parent"},
		Values: map[string]any{
			"replicas":   2,
			"parentOnly": "p",
			"web": map[string]any{
				"replicas": 2,
			},
		},
	}
	web := &chart.Chart{
		Metadata: &chart.Metadata{Name: "web"},
		Values: map[string]any{
			"replicas": 1,
			"image":    "default-image",
		},
	}
	root.AddDependency(web)

	tracer := NewTracer()

	// CLI override file: replicas=3
	merged := tracer.Merge(
		map[string]any{},
		map[string]any{"replicas": 3, "web": map[string]any{"replicas": 3}},
		fileSrc(0, "overrides.yaml"),
	)
	// --set on the command line wins over the file
	merged = tracer.Merge(merged, map[string]any{"replicas": 5}, setSrc(0, "replicas=5"))

	tracer.RecordParentDefaults(root.Values)
	trace := tracer.Snapshot(root, merged, renderedFor(root, merged))

	origins := originMap(trace)

	// CLI set beats file beats parent beats subchart default.
	assert.Equal(t, LayerCLI, origins["replicas"].Source.Layer)
	assert.Equal(t, LayerValueFile, origins["web.replicas"].Source.Layer)
	assert.Equal(t, LayerParent, origins["parentOnly"].Source.Layer)

	// Subchart default is attributed to the subchart's values.yaml.
	webImage, ok := origins["web.image"]
	require.True(t, ok, "web.image should be attributed to subchart default")
	assert.Equal(t, LayerChartDefault, webImage.Source.Layer)
	assert.Equal(t, "web", webImage.Source.Name)

	// Determinism: two snapshots over identical input compare equal.
	tracer2 := NewTracer()
	merged2 := tracer2.Merge(map[string]any{},
		map[string]any{"replicas": 3, "web": map[string]any{"replicas": 3}},
		fileSrc(0, "overrides.yaml"))
	merged2 = tracer2.Merge(merged2, map[string]any{"replicas": 5}, setSrc(0, "replicas=5"))
	tracer2.RecordParentDefaults(root.Values)
	trace2 := tracer2.Snapshot(root, merged2, renderedFor(root, merged2))
	assert.Equal(t, trace.Report(), trace2.Report())
	assert.Equal(t, trace.Origins(), trace2.Origins())
}

// TestSameLayerConflict verifies two distinct sources in one layer writing
// the same key produce a conflict report instead of a silent winner.
func TestSameLayerConflict(t *testing.T) {
	tracer := NewTracer()
	merged := tracer.Merge(map[string]any{},
		map[string]any{"replicas": 1}, fileSrc(0, "a.yaml"))
	merged = tracer.Merge(merged,
		map[string]any{"replicas": 2}, fileSrc(1, "b.yaml"))
	trace := tracer.Snapshot(nil, merged, merged)

	conflicts := trace.Conflicts()
	require.Len(t, conflicts, 1)
	assert.Equal(t, ConflictSameLayer, conflicts[0].Kind)
	assert.Equal(t, "replicas", conflicts[0].Path)
	assert.Equal(t, "b.yaml", conflicts[0].Winner.Name)
	assert.Equal(t, "a.yaml", conflicts[0].Loser.Name)

	origins := originMap(trace)
	assert.Equal(t, "b.yaml", origins["replicas"].Source.Name)
}

// TestTypeConflict verifies string vs array and map vs scalar mismatches are
// reported with the full key path and both type names.
func TestTypeConflict(t *testing.T) {
	t.Run("string replaced by array", func(t *testing.T) {
		tracer := NewTracer()
		merged := tracer.Merge(map[string]any{},
			map[string]any{"ports": "http"}, fileSrc(0, "a.yaml"))
		merged = tracer.Merge(merged,
			map[string]any{"ports": []any{80, 443}}, fileSrc(1, "b.yaml"))
		trace := tracer.Snapshot(nil, merged, merged)

		var typeConflict *Conflict
		for i := range trace.Conflicts() {
			if trace.Conflicts()[i].Kind == ConflictType {
				typeConflict = &trace.Conflicts()[i]
			}
		}
		require.NotNil(t, typeConflict, "a type mismatch conflict must be reported")
		assert.Equal(t, "ports", typeConflict.Path)
		assert.Contains(t, typeConflict.WinnerType, "[]interface")
		assert.Equal(t, "string", typeConflict.LoserType)
		assert.Equal(t, []any{80, 443}, merged["ports"])
	})

	t.Run("map replaced by scalar", func(t *testing.T) {
		tracer := NewTracer()
		merged := tracer.Merge(map[string]any{},
			map[string]any{"meta": map[string]any{"a": 1}}, fileSrc(0, "a.yaml"))
		merged = tracer.Merge(merged,
			map[string]any{"meta": "off"}, fileSrc(1, "b.yaml"))
		trace := tracer.Snapshot(nil, merged, merged)

		var typeConflict *Conflict
		for i := range trace.Conflicts() {
			if trace.Conflicts()[i].Kind == ConflictType {
				typeConflict = &trace.Conflicts()[i]
			}
		}
		require.NotNil(t, typeConflict, "map replaced by scalar is a type conflict")
		assert.Equal(t, "meta", typeConflict.Path)
		assert.Equal(t, "string", typeConflict.WinnerType)
		assert.Equal(t, "off", merged["meta"])
	})
}

// TestExplicitNullVsAbsent distinguishes an explicit null ("clear this key")
// from an absent key ("inherit the previous layer").
func TestExplicitNullVsAbsent(t *testing.T) {
	tracer := NewTracer()
	// First source sets both keys.
	merged := tracer.Merge(map[string]any{},
		map[string]any{"keep": "v", "drop": "v"}, fileSrc(0, "a.yaml"))
	// Second source explicitly nulls drop but says nothing about keep.
	merged = tracer.Merge(merged,
		map[string]any{"drop": nil}, fileSrc(1, "b.yaml"))

	trace := tracer.Snapshot(nil, merged, merged)
	origins := originMap(trace)

	drop, ok := origins["drop"]
	require.True(t, ok)
	assert.True(t, drop.ExplicitNull, "explicit null must be marked")
	assert.Equal(t, "b.yaml", drop.Source.Name)

	keep, ok := origins["keep"]
	require.True(t, ok)
	assert.False(t, keep.ExplicitNull, "untouched key inherits and is not a null")
	assert.Equal(t, "a.yaml", keep.Source.Name)

	// MergeMaps keeps the null key; nothing is silently coerced to a zero
	// value.
	val, present := merged["drop"]
	assert.True(t, present)
	assert.Nil(t, val)
}

// TestArrayReplacementVsMapMerge documents the two combination rules: arrays
// are replaced wholesale while nested maps merge leaf by leaf.
func TestArrayReplacementVsMapMerge(t *testing.T) {
	tracer := NewTracer()
	merged := tracer.Merge(map[string]any{},
		map[string]any{
			"list":   []any{1, 2, 3},
			"nested": map[string]any{"a": 1, "b": 2},
		}, fileSrc(0, "a.yaml"))
	merged = tracer.Merge(merged,
		map[string]any{
			"list":   []any{9},
			"nested": map[string]any{"b": 22, "c": 33},
		}, fileSrc(1, "b.yaml"))
	trace := tracer.Snapshot(nil, merged, merged)

	// Array: whole value replaced (same type, no type conflict).
	assert.Equal(t, []any{9}, merged["list"])
	origins := originMap(trace)
	assert.Equal(t, "b.yaml", origins["list"].Source.Name)

	// Map: leaves merge and keep individual provenance.
	nested := merged["nested"].(map[string]any)
	assert.Equal(t, 1, nested["a"])
	assert.Equal(t, 22, nested["b"])
	assert.Equal(t, 33, nested["c"])
	assert.Equal(t, "a.yaml", origins["nested.a"].Source.Name)
	assert.Equal(t, "b.yaml", origins["nested.b"].Source.Name)
	assert.Equal(t, "b.yaml", origins["nested.c"].Source.Name)

	for _, conflict := range trace.Conflicts() {
		assert.NotEqual(t, ConflictType, conflict.Kind,
			"array->array and map merges are not type conflicts")
	}
}

// TestNotParticipatingSource ensures a registered source that supplied
// nothing is explicitly reported as not participating rather than blank.
func TestNotParticipatingSource(t *testing.T) {
	tracer := NewTracer()
	empty := fileSrc(0, "empty.yaml")
	tracer.RegisterSource(empty)
	merged := tracer.Merge(map[string]any{},
		map[string]any{"a": 1}, fileSrc(1, "real.yaml"))
	trace := tracer.Snapshot(nil, merged, merged)

	statuses := trace.SourceStatuses()
	statusByName := map[string]bool{}
	for _, status := range statuses {
		statusByName[status.Source.Name] = status.Participated
	}
	assert.False(t, statusByName["empty.yaml"])
	assert.True(t, statusByName["real.yaml"])
}

// TestDeterministicRepeatedRuns runs the same merge twice and asserts the
// full origin and conflict mapping is stable.
func TestDeterministicRepeatedRuns(t *testing.T) {
	build := func() *Trace {
		tracer := NewTracer()
		merged := tracer.Merge(map[string]any{},
			map[string]any{"z": 1, "a": map[string]any{"y": 1, "x": 1}},
			fileSrc(0, "a.yaml"))
		merged = tracer.Merge(merged,
			map[string]any{"a": map[string]any{"x": 2}, "z": 2},
			fileSrc(1, "b.yaml"))
		return tracer.Snapshot(nil, merged, merged)
	}

	first := build()
	second := build()
	assert.Equal(t, first.Origins(), second.Origins())
	assert.Equal(t, first.Conflicts(), second.Conflicts())
	assert.Equal(t, first.Report(), second.Report())
}

// TestEqualValuesNoSpuriousConflict ensures two sources agreeing on a value
// do not produce a conflict: a conflict requires a real disagreement.
func TestEqualValuesNoSpuriousConflict(t *testing.T) {
	tracer := NewTracer()
	merged := tracer.Merge(map[string]any{},
		map[string]any{"replicas": 3}, fileSrc(0, "a.yaml"))
	merged = tracer.Merge(merged,
		map[string]any{"replicas": 3, "other": 1}, fileSrc(1, "b.yaml"))
	trace := tracer.Snapshot(nil, merged, merged)

	for _, conflict := range trace.Conflicts() {
		assert.NotEqual(t, "replicas", conflict.Path,
			"equal values across sources must not be reported as a conflict")
	}

	// Even though the value is equal, the later source is the documented
	// winner because MergeMaps applies it last.
	origins := originMap(trace)
	assert.Equal(t, "b.yaml", origins["replicas"].Source.Name)
	assert.Equal(t, "b.yaml", origins["other"].Source.Name)
}

// TestNestedMapConflict locates a disagreement deep inside a nested map with
// its full dotted path rather than reporting only the table name.
func TestNestedMapConflict(t *testing.T) {
	tracer := NewTracer()
	merged := tracer.Merge(map[string]any{},
		map[string]any{"web": map[string]any{"meta": map[string]any{"retries": 1}}},
		fileSrc(0, "a.yaml"))
	merged = tracer.Merge(merged,
		map[string]any{"web": map[string]any{"meta": map[string]any{"retries": 5}}},
		fileSrc(1, "b.yaml"))
	trace := tracer.Snapshot(nil, merged, merged)

	var found bool
	for _, conflict := range trace.Conflicts() {
		if conflict.Path == "web.meta.retries" {
			found = true
			assert.Equal(t, "b.yaml", conflict.Winner.Name)
			assert.Equal(t, "a.yaml", conflict.Loser.Name)
		}
	}
	assert.True(t, found, "nested disagreement must be located by full path")
}

// TestObserveParsedDegrades verifies tracing errors are downgraded to
// warnings instead of breaking the merge.
func TestObserveParsedDegrades(t *testing.T) {
	tracer := NewTracer()
	src := setSrc(0, "broken")
	tracer.ObserveParsed(nil, src, assertError("boom"))

	trace := tracer.Snapshot(nil, map[string]any{}, map[string]any{})
	require.Len(t, trace.Warnings(), 1)
	assert.Contains(t, trace.Warnings()[0], "boom")

	// The source is still registered so it is reported as not participating.
	statuses := trace.SourceStatuses()
	require.Len(t, statuses, 1)
	assert.False(t, statuses[0].Participated)
}

type errString string

func (e errString) Error() string { return string(e) }

func assertError(msg string) error { return errString(msg) }
