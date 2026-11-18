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

package values

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"helm.sh/helm/v4/pkg/getter"
	"helm.sh/helm/v4/pkg/valuetrace"
)

func writeValuesFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

// TestMergeValuesTracedParity proves the traced merge returns exactly the
// same values as the untraced MergeValues for an input exercising files,
// nested maps, arrays, explicit nulls and every --set* variant.
func TestMergeValuesTracedParity(t *testing.T) {
	dir := t.TempDir()
	first := writeValuesFile(t, dir, "first.yaml", `
replicas: 1
image:
  tag: v1
  pull: Always
ports:
  - 80
`)
	second := writeValuesFile(t, dir, "second.yaml", `
replicas: 2
image:
  tag: v2
cleared: null
ports:
  - 8080
`)

	opts := Options{
		ValueFiles:    []string{first, second},
		Values:        []string{"image.tag=v3"},
		StringValues:  []string{"replicas=5"},
		JSONValues:    []string{"json.key=42"},
		LiteralValues: []string{"lit.flag=true"},
	}

	plain, err := opts.MergeValues(getter.Providers{})
	require.NoError(t, err)

	traced, tracer, err := opts.MergeValuesTraced(getter.Providers{})
	require.NoError(t, err)
	require.NotNil(t, tracer)

	assert.Equal(t, plain, traced, "traced and untraced merge must be identical")

	trace := tracer.Snapshot(nil, traced, traced)
	origins := map[string]valuetrace.Origin{}
	for _, origin := range trace.Origins() {
		origins[origin.Path] = origin
	}

	// --set-string wins over both files; the second file still wins for the
	// keys it alone overrode (ports), proven below.
	assert.Equal(t, valuetrace.LayerCLI, origins["replicas"].Source.Layer)
	assert.Equal(t, "replicas=5", origins["replicas"].Source.Name)
	assert.Equal(t, valuetrace.LayerValueFile, origins["image.pull"].Source.Layer)

	// CLI set layers beat files.
	assert.Equal(t, valuetrace.LayerCLI, origins["image.tag"].Source.Layer)
	assert.Equal(t, valuetrace.LayerCLI, origins["json.key"].Source.Layer)
	assert.Equal(t, valuetrace.LayerCLI, origins["lit.flag"].Source.Layer)

	// Explicit null from the second file is preserved and marked.
	cleared, ok := origins["cleared"]
	require.True(t, ok)
	assert.True(t, cleared.ExplicitNull)

	// Array replacement keeps the second file as source.
	assert.Equal(t, "second.yaml", filepath.Base(origins["ports"].Source.Name))

	// Both files participated.
	statuses := map[string]bool{}
	for _, status := range trace.SourceStatuses() {
		statuses[filepath.Base(status.Source.Name)] = status.Participated
	}
	assert.True(t, statuses["first.yaml"])
	assert.True(t, statuses["second.yaml"])
}

// TestMergeValuesTracedSameLayerConflict verifies two value files writing the
// same key surface a conflict instead of a silent winner.
func TestMergeValuesTracedSameLayerConflict(t *testing.T) {
	dir := t.TempDir()
	first := writeValuesFile(t, dir, "a.yaml", "replicas: 1\n")
	second := writeValuesFile(t, dir, "b.yaml", "replicas: 2\n")

	opts := Options{ValueFiles: []string{first, second}}
	_, tracer, err := opts.MergeValuesTraced(getter.Providers{})
	require.NoError(t, err)

	trace := tracer.Snapshot(nil,
		mustMerge(t, opts), mustMerge(t, opts))

	var found bool
	for _, conflict := range trace.Conflicts() {
		if conflict.Path == "replicas" && conflict.Kind == valuetrace.ConflictSameLayer {
			found = true
			assert.Equal(t, "b.yaml", filepath.Base(conflict.Winner.Name))
			assert.Equal(t, "a.yaml", filepath.Base(conflict.Loser.Name))
		}
	}
	assert.True(t, found, "expected a same layer conflict for replicas")
}

// TestMergeValuesTracedNotParticipating marks a registered-but-empty source
// explicitly as not participating.
func TestMergeValuesTracedNotParticipating(t *testing.T) {
	dir := t.TempDir()
	empty := writeValuesFile(t, dir, "empty.yaml", "{}\n")
	real := writeValuesFile(t, dir, "real.yaml", "a: 1\n")

	opts := Options{ValueFiles: []string{empty, real}}
	_, tracer, err := opts.MergeValuesTraced(getter.Providers{})
	require.NoError(t, err)

	merged, err := opts.MergeValues(getter.Providers{})
	require.NoError(t, err)
	trace := tracer.Snapshot(nil, merged, merged)

	participation := map[string]bool{}
	for _, status := range trace.SourceStatuses() {
		participation[filepath.Base(status.Source.Name)] = status.Participated
	}
	assert.False(t, participation["empty.yaml"])
	assert.True(t, participation["real.yaml"])
}

func mustMerge(t *testing.T, opts Options) map[string]any {
	t.Helper()
	merged, err := opts.MergeValues(getter.Providers{})
	require.NoError(t, err)
	return merged
}
