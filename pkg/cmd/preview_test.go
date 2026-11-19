package cmd

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/cli/output"
	"helm.sh/helm/v4/pkg/kube"
)

func TestUpgradePreviewWriterUsesSameResult(t *testing.T) {
	preview := &action.UpgradePreview{
		Release:   "demo",
		Namespace: "default",
		Revision:  2,
		Resources: []kube.ResourceDiff{
			{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Namespace:  "default",
				Name:       "demo",
				Operation:  kube.DiffOperationUpdate,
				Fields: []kube.FieldDiff{
					{
						Path:        ".spec.replicas",
						Operation:   kube.DiffOperationUpdate,
						Desired:     float64(4),
						Current:     float64(3),
						DesiredType: "number",
						CurrentType: "number",
					},
				},
			},
		},
		Summary: kube.DiffSummary{
			Total:        1,
			Updated:      1,
			FieldChanges: 1,
		},
	}
	writer := &upgradePreviewWriter{preview: preview}

	var jsonOutput bytes.Buffer
	require.NoError(t, output.JSON.Write(&jsonOutput, writer))
	var decoded action.UpgradePreview
	require.NoError(t, json.Unmarshal(jsonOutput.Bytes(), &decoded))
	assert.Equal(t, preview.Release, decoded.Release)
	assert.Equal(t, preview.Summary, decoded.Summary)
	assert.Equal(t, preview.Resources, decoded.Resources)

	var yamlOutput bytes.Buffer
	require.NoError(t, output.YAML.Write(&yamlOutput, writer))
	var decodedYAML action.UpgradePreview
	require.NoError(t, yaml.Unmarshal(yamlOutput.Bytes(), &decodedYAML))
	assert.Equal(t, decoded.Resources, decodedYAML.Resources)
	assert.Equal(t, decoded.Summary, decodedYAML.Summary)

	var tableOutput bytes.Buffer
	require.NoError(t, output.Table.Write(&tableOutput, writer))
	assert.Contains(t, tableOutput.String(), "UPDATE Deployment default/demo (apps/v1)")
	assert.Contains(t, tableOutput.String(), "~ .spec.replicas: 3 -> 4")
	assert.Contains(t, tableOutput.String(), "fieldChanges=1")
}

func TestUpgradePreviewWriterSummaryOnly(t *testing.T) {
	preview := &action.UpgradePreview{
		Release:     "demo",
		Namespace:   "default",
		Revision:    2,
		SummaryOnly: true,
		Resources: []kube.ResourceDiff{
			{Kind: "ConfigMap", Name: "demo", Operation: kube.DiffOperationAdd},
		},
		Summary: kube.DiffSummary{Total: 1, Added: 1},
	}

	var jsonOutput bytes.Buffer
	require.NoError(t, output.JSON.Write(&jsonOutput, &upgradePreviewWriter{preview: preview}))
	var decoded action.UpgradePreview
	require.NoError(t, json.Unmarshal(jsonOutput.Bytes(), &decoded))
	assert.Empty(t, decoded.Resources)
	assert.Equal(t, preview.Summary, decoded.Summary)

	var tableOutput bytes.Buffer
	require.NoError(t, output.Table.Write(&tableOutput, &upgradePreviewWriter{preview: preview}))
	assert.Contains(t, tableOutput.String(), "added=1")
	assert.NotContains(t, tableOutput.String(), "ADD ConfigMap")
}
