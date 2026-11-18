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

package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"helm.sh/helm/v4/pkg/chart/common"
	"helm.sh/helm/v4/pkg/chart/common/util"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/valuetrace"
)

// buildTraceAcceptanceChart builds a parent chart with one subchart and
// templates that read values from every provenance layer.
func buildTraceAcceptanceChart() *chart.Chart {
	child := &chart.Chart{
		Metadata: &chart.Metadata{Name: "web", Version: "0.1.0"},
		Values: map[string]any{
			"replicas": 1,
			"image":    "web:default",
		},
		Templates: []*common.File{
			{
				Name: "templates/config.yaml",
				Data: []byte(`replicas: {{ .Values.replicas }}
image: {{ .Values.image }}
`),
			},
		},
	}

	parent := &chart.Chart{
		Metadata: &chart.Metadata{Name: "app", Version: "0.1.0"},
		Values: map[string]any{
			"name": "from-parent-default",
			"web": map[string]any{
				"replicas": 2,
			},
		},
		Templates: []*common.File{
			{
				Name: "templates/root.yaml",
				Data: []byte(`name: {{ .Values.name }}
webReplicas: {{ .Values.web.replicas }}
`),
			},
		},
	}
	parent.AddDependency(child)
	return parent
}

// TestValuesTraceRenderParity is the acceptance check: with the same input
// values, rendering with provenance tracing enabled must produce output that
// is byte-for-byte identical to rendering without tracing. It also asserts
// the key path to source mapping table.
func TestValuesTraceRenderParity(t *testing.T) {
	root := buildTraceAcceptanceChart()

	// User supplied values stand in for the merged -f/--set result.
	userValues := map[string]any{
		"name": "from-cli",
		"web": map[string]any{
			"replicas": 5,
		},
	}

	options := common.ReleaseOptions{Name: "rel", Namespace: "ns"}

	// Baseline: existing, untraced pipeline.
	baselineValues, err := util.ToRenderValues(root, userValues, options, common.DefaultCapabilities)
	require.NoError(t, err)
	baselineRender, err := Render(root, baselineValues)
	require.NoError(t, err)

	// Traced pipeline: tracer observes the CLI inputs then coalescing
	// finalizes chart default provenance.
	tracer := valuetrace.NewTracer()
	tracer.Merge(map[string]any{}, userValues, valuetrace.Source{
		Layer: valuetrace.LayerCLI,
		Kind:  valuetrace.KindSet,
		Name:  "name=from-cli,web.replicas=5",
	})

	tracedValues, trace, err := util.ToRenderValuesTraced(root, userValues, options, common.DefaultCapabilities, tracer)
	require.NoError(t, err)
	require.NotNil(t, trace)

	// The coalesced values themselves must be identical.
	assert.Equal(t, baselineValues, tracedValues)

	tracedRender, err := Render(root, tracedValues)
	require.NoError(t, err)

	// Byte-for-byte render parity across every rendered template.
	require.Equal(t, len(baselineRender), len(tracedRender))
	for file, baselineContent := range baselineRender {
		tracedContent, ok := tracedRender[file]
		require.True(t, ok, "missing rendered file %s under tracing", file)
		assert.Equal(t, baselineContent, tracedContent,
			"rendered output for %s changed when tracing was enabled", file)
	}

	t.Logf("rendered output (identical with and without tracing):\n%v", tracedRender)
	t.Logf("\nprovenance mapping:\n%s", trace.Report())

	// The mapping table attributes each path to the correct layer.
	origins := map[string]valuetrace.Origin{}
	for _, origin := range trace.Origins() {
		origins[origin.Path] = origin
	}

	// CLI override wins for the paths it wrote.
	assert.Equal(t, valuetrace.LayerCLI, origins["name"].Source.Layer)
	assert.Equal(t, valuetrace.LayerCLI, origins["web.replicas"].Source.Layer)

	// Subchart default survives for the key the user never touched.
	webImage, ok := origins["web.image"]
	require.True(t, ok, "web.image must be attributed to the subchart default")
	assert.Equal(t, valuetrace.LayerChartDefault, webImage.Source.Layer)
	assert.Equal(t, "web", webImage.Source.Name)

	// Repeated snapshots are deterministic.
	tracer2 := valuetrace.NewTracer()
	tracer2.Merge(map[string]any{}, userValues, valuetrace.Source{
		Layer: valuetrace.LayerCLI,
		Kind:  valuetrace.KindSet,
		Name:  "name=from-cli,web.replicas=5",
	})
	_, trace2, err := util.CoalesceValuesTraced(root, userValues, tracer2)
	require.NoError(t, err)
	assert.Equal(t, trace.Origins(), trace2.Origins())
	assert.Equal(t, trace.Report(), trace2.Report())
}

// TestValuesTraceDoesNotPolluteRenderContext proves the trace structure is
// kept separate from the values handed to templates.
func TestValuesTraceDoesNotPolluteRenderContext(t *testing.T) {
	root := buildTraceAcceptanceChart()
	userValues := map[string]any{"name": "x"}

	tracer := valuetrace.NewTracer()
	tracer.Merge(map[string]any{}, userValues, valuetrace.Source{
		Layer: valuetrace.LayerCLI,
		Kind:  valuetrace.KindSet,
		Name:  "name=x",
	})
	rendered, trace, err := util.ToRenderValuesTraced(root, userValues,
		common.ReleaseOptions{Name: "rel"}, common.DefaultCapabilities, tracer)
	require.NoError(t, err)
	require.NotNil(t, trace)

	_, hasTraceKey := rendered["trace"]
	assert.False(t, hasTraceKey, "trace must not be injected into render values")
	_, hasProvenanceKey := rendered["provenance"]
	assert.False(t, hasProvenanceKey, "provenance must not be injected into render values")
}
