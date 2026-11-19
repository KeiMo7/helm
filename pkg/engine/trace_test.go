package engine

import (
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"helm.sh/helm/v4/pkg/chart/common"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	releaseutil "helm.sh/helm/v4/pkg/release/v1/util"
)

func traceChart(templates map[string]string) *chart.Chart {
	c := &chart.Chart{
		Metadata: &chart.Metadata{Name: "trace"},
	}
	names := make([]string, 0, len(templates))
	for name := range templates {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		c.Templates = append(c.Templates, &common.File{
			Name:    name,
			ModTime: time.Now(),
			Data:    []byte(templates[name]),
		})
	}
	return c
}

func TestTemplateTraceSingleInclude(t *testing.T) {
	c := traceChart(map[string]string{
		"templates/deploy.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ include \"trace.name\" . }}\n",
		"templates/_helpers.tpl": `{{- define "trace.name" -}}
{{ .Values.missing.key }}
{{- end -}}`,
	})
	values := common.Values{
		"Values": map[string]any{"missing": map[string]any{}},
	}

	var e Engine
	e.Strict = true
	e.TemplateTrace = &TraceOptions{Templates: []string{"trace/templates/deploy.yaml"}}

	_, _, err := e.RenderWithContextTrace(t.Context(), c, values)
	require.Error(t, err)

	message := err.Error()
	assert.Contains(t, message, "Template trace:")
	assert.Contains(t, message, "resource: trace/templates/deploy.yaml")
	assert.Contains(t, message, `include "trace.name": trace/templates/deploy.yaml:4:`)
	assert.Contains(t, message, "error: trace/templates/_helpers.tpl:2:")
	assert.Contains(t, message, `missing key "key"`)
}

func TestTemplateTraceNestedIncludes(t *testing.T) {
	c := traceChart(map[string]string{
		"templates/deploy.yaml": "name: {{ include \"trace.one\" . }}\n",
		"templates/_helpers.tpl": `{{- define "trace.one" -}}
{{ include "trace.two" . }}
{{- end -}}
{{- define "trace.two" -}}
{{ include "trace.three" . }}
{{- end -}}
{{- define "trace.three" -}}
{{ .Values.missing.key }}
{{- end -}}`,
	})
	values := common.Values{
		"Values": map[string]any{"missing": map[string]any{}},
	}

	var e Engine
	e.Strict = true
	e.TemplateTrace = &TraceOptions{Templates: []string{"trace/templates/deploy.yaml"}}

	_, _, err := e.RenderWithContextTrace(t.Context(), c, values)
	require.Error(t, err)

	message := err.Error()
	assert.Contains(t, message, `include "trace.one"`)
	assert.Contains(t, message, `include "trace.two"`)
	assert.Contains(t, message, `include "trace.three"`)
	assert.Contains(t, message, "error: trace/templates/_helpers.tpl:8:")
}

func TestTemplateTraceTpl(t *testing.T) {
	c := traceChart(map[string]string{
		"templates/deploy.yaml": "value: {{ tpl \"password: {{ .Values.secret.missing }}\" . }}\n",
	})
	values := common.Values{
		"Values": map[string]any{
			"secret": map[string]any{"value": "do-not-print"},
		},
	}

	var e Engine
	e.Strict = true
	e.TemplateTrace = &TraceOptions{Templates: []string{"trace/templates/deploy.yaml"}}

	_, _, err := e.RenderWithContextTrace(t.Context(), c, values)
	require.Error(t, err)

	message := err.Error()
	assert.Contains(t, message, "Template trace:")
	assert.Contains(t, message, "tpl source: trace/templates/deploy.yaml:1:")
	assert.Contains(t, message, "tpl string line: 1:")
	assert.NotContains(t, message, "password:")
}

func TestTemplateTraceMissingListIndex(t *testing.T) {
	c := traceChart(map[string]string{
		"templates/deploy.yaml": "value: {{ index .Values.items 3 }}\n",
	})
	values := common.Values{
		"Values": map[string]any{"items": []any{"one", "two"}},
	}

	var e Engine
	e.TemplateTrace = &TraceOptions{Templates: []string{"trace/templates/deploy.yaml"}}

	_, _, err := e.RenderWithContextTrace(t.Context(), c, values)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "index 3 out of range")
}

func TestTemplateTraceDuplicateDefinitionUsesActualSource(t *testing.T) {
	c := traceChart(map[string]string{
		"templates/deploy.yaml":    "value: {{ include \"trace.duplicate\" . }}\n",
		"templates/_first.tpl":     `{{- define "trace.duplicate" -}}{{ .Values.first.missing }}{{- end -}}`,
		"templates/_second.tpl":    `{{- define "trace.duplicate" -}}{{ .Values.second.missing }}{{- end -}}`,
		"templates/_unused.go.tpl": `{{- define "trace.unused" -}}unused{{- end -}}`,
	})
	values := common.Values{
		"Values": map[string]any{
			"first":  map[string]any{},
			"second": map[string]any{},
		},
	}

	var e Engine
	e.Strict = true
	e.TemplateTrace = &TraceOptions{Templates: []string{"trace/templates/deploy.yaml"}}

	_, _, err := e.RenderWithContextTrace(t.Context(), c, values)
	require.Error(t, err)

	message := err.Error()
	assert.Contains(t, message, "Template trace:")
	assert.Contains(t, message, "error: trace/templates/_first.tpl:1:")
}

func TestTemplateTraceUndefinedTemplate(t *testing.T) {
	c := traceChart(map[string]string{
		"templates/deploy.yaml": "value: {{ include \"trace.missing\" . }}\n",
	})

	var e Engine
	e.TemplateTrace = &TraceOptions{Templates: []string{"trace/templates/deploy.yaml"}}

	_, _, err := e.RenderWithContextTrace(t.Context(), c, common.Values{})
	require.Error(t, err)

	message := err.Error()
	assert.Contains(t, message, `include "trace.missing"`)
	assert.Contains(t, message, `template "trace.missing" is not defined`)
}

func TestTemplateTraceSelectsHelper(t *testing.T) {
	c := traceChart(map[string]string{
		"templates/deploy.yaml":  "value: {{ include \"trace.helper\" . }}\n",
		"templates/_helpers.tpl": `{{- define "trace.helper" -}}{{ .Values.missing.key }}{{- end -}}`,
	})
	values := common.Values{"Values": map[string]any{"missing": map[string]any{}}}

	var e Engine
	e.Strict = true
	e.TemplateTrace = &TraceOptions{Templates: []string{"trace.helper"}}

	_, _, err := e.RenderWithContextTrace(t.Context(), c, values)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Template trace:")
	assert.Contains(t, err.Error(), `include "trace.helper"`)
}

func TestTemplateTraceTruncatesLongChains(t *testing.T) {
	templates := map[string]string{
		"templates/deploy.yaml": "value: {{ include \"trace.0\" . }}\n",
	}
	var helpers strings.Builder
	for i := range 10 {
		if i == 9 {
			_, _ = helpers.WriteString(`{{- define "trace.9" -}}{{ .Values.missing.key }}{{- end -}}`)
			break
		}
		_, _ = helpers.WriteString(`{{- define "trace.` + strconv.Itoa(i) + `" -}}{{ include "trace.` + strconv.Itoa(i+1) + `" . }}{{- end -}}`)
	}
	templates["templates/_helpers.tpl"] = helpers.String()

	values := common.Values{"Values": map[string]any{"missing": map[string]any{}}}
	var e Engine
	e.Strict = true
	e.TemplateTrace = &TraceOptions{
		Templates: []string{"trace/templates/deploy.yaml"},
		MaxDepth:  4,
	}

	_, _, err := e.RenderWithContextTrace(t.Context(), traceChart(templates), values)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "frames omitted")
}

func TestTemplateTraceInvalidYAML(t *testing.T) {
	c := traceChart(map[string]string{
		"templates/deploy.yaml": "{{ include \"trace.manifest\" . }}\n",
		"templates/_helpers.tpl": `{{- define "trace.manifest" -}}
apiVersion: v1
kind: ConfigMap
metadata:
  name: trace
data:
  broken: [unclosed
{{- end -}}`,
	})

	var e Engine
	e.TemplateTrace = &TraceOptions{Templates: []string{"trace/templates/deploy.yaml"}}

	files, renderTrace, err := e.RenderWithContextTrace(t.Context(), c, common.Values{})
	require.NoError(t, err)
	require.NotNil(t, renderTrace)

	_, _, err = releaseutil.SortManifests(files, nil, releaseutil.InstallOrder)
	require.Error(t, err)
	original := err.Error()
	annotated := renderTrace.AnnotateYAMLParseError(err, files)
	require.Error(t, annotated)

	assert.True(t, strings.HasPrefix(annotated.Error(), original), "the original YAML error must remain the prefix")
	assert.Contains(t, annotated.Error(), "Template trace:")
	assert.Contains(t, annotated.Error(), `include "trace.manifest": trace/templates/deploy.yaml:1:`)
	assert.Contains(t, annotated.Error(), "YAML error: trace/templates/_helpers.tpl:")
}

func TestTemplateTraceInvalidYAMLUnknown(t *testing.T) {
	rt := &RenderTrace{files: map[string]*traceFile{"trace/templates/deploy.yaml": {root: "trace/templates/deploy.yaml"}}}
	err := rt.AnnotateUnknownYAML(assert.AnError)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "template position: unknown")
}

func TestTemplateTraceDisabledIsUnchanged(t *testing.T) {
	c := traceChart(map[string]string{
		"templates/deploy.yaml":  "value: {{ include \"trace.helper\" . }}\n",
		"templates/_helpers.tpl": `{{- define "trace.helper" -}}{{ .Values.missing.key }}{{- end -}}`,
	})
	values := common.Values{"Values": map[string]any{"missing": map[string]any{}}}

	var e Engine
	e.Strict = true
	_, err := e.Render(c, values)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "Template trace:")
}

func TestTemplateTraceUnselectedErrorIsUnchanged(t *testing.T) {
	c := traceChart(map[string]string{
		"templates/deploy.yaml":  "value: {{ include \"trace.helper\" . }}\n",
		"templates/other.yaml":   "value: other\n",
		"templates/_helpers.tpl": `{{- define "trace.helper" -}}{{ .Values.missing.key }}{{- end -}}`,
	})
	values := common.Values{"Values": map[string]any{"missing": map[string]any{}}}

	var withoutTrace Engine
	withoutTrace.Strict = true
	_, err := withoutTrace.Render(c, values)
	require.Error(t, err)
	withoutErr := err.Error()

	withTrace := Engine{
		Strict:        true,
		TemplateTrace: &TraceOptions{Templates: []string{"trace/templates/other.yaml"}},
	}
	_, _, err = withTrace.RenderWithContextTrace(t.Context(), c, values)
	require.Error(t, err)

	assert.Equal(t, withoutErr, err.Error())
}

func TestTemplateTraceSuccessOutputUnchanged(t *testing.T) {
	c := traceChart(map[string]string{
		"templates/deploy.yaml":  "name: {{ include \"trace.helper\" . }}\nvalue: {{ tpl .Values.dynamic . }}\n",
		"templates/_helpers.tpl": `{{- define "trace.helper" -}}{{ .Release.Name }}{{- end -}}`,
	})
	values := common.Values{
		"Release": common.Values{"Name": "trace-release"},
		"Values":  map[string]any{"dynamic": "{{ .Release.Name }}"},
	}

	var withoutTrace Engine
	want, err := withoutTrace.Render(c, values)
	require.NoError(t, err)

	e := &Engine{TemplateTrace: &TraceOptions{Templates: []string{"trace/templates/deploy.yaml"}}}
	got, trace, err := e.RenderWithContextTrace(t.Context(), c, values)
	require.NoError(t, err)
	require.NotNil(t, trace)
	assert.Equal(t, want, got)
}

func BenchmarkRenderTemplateTraceDisabled(b *testing.B) {
	c := traceChart(map[string]string{
		"templates/deploy.yaml":  "name: {{ include \"trace.helper\" . }}\n",
		"templates/_helpers.tpl": `{{- define "trace.helper" -}}{{ .Release.Name }}{{- end -}}`,
	})
	values := common.Values{"Release": common.Values{"Name": "bench"}}
	e := new(Engine)

	b.ReportAllocs()
	for b.Loop() {
		if _, err := e.Render(c, values); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRenderTemplateTraceUnselected(b *testing.B) {
	c := traceChart(map[string]string{
		"templates/deploy.yaml":  "name: {{ include \"trace.helper\" . }}\n",
		"templates/_helpers.tpl": `{{- define "trace.helper" -}}{{ .Release.Name }}{{- end -}}`,
	})
	values := common.Values{"Release": common.Values{"Name": "bench"}}
	e := &Engine{TemplateTrace: &TraceOptions{Templates: []string{"trace/templates/not-rendered.yaml"}}}

	b.ReportAllocs()
	for b.Loop() {
		if _, err := e.Render(c, values); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRenderTemplateTraceEnabled(b *testing.B) {
	c := traceChart(map[string]string{
		"templates/deploy.yaml":  "name: {{ include \"trace.helper\" . }}\n",
		"templates/_helpers.tpl": `{{- define "trace.helper" -}}{{ .Release.Name }}{{- end -}}`,
	})
	values := common.Values{"Release": common.Values{"Name": "bench"}}
	e := &Engine{TemplateTrace: &TraceOptions{Templates: []string{"trace/templates/deploy.yaml"}}}

	b.ReportAllocs()
	for b.Loop() {
		if _, err := e.Render(c, values); err != nil {
			b.Fatal(err)
		}
	}
}
