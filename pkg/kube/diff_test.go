package kube

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiffObjectsReportsManifestAlignedChanges(t *testing.T) {
	desired := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":   "demo",
			"labels": map[string]any{"alpha": "1", "beta": "2"},
		},
		"data": map[string]any{
			"added":    "yes",
			"changed":  "new",
			"replicas": 3,
			"same":     "same",
		},
	}
	current := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":              "demo",
			"generation":        int64(12),
			"creationTimestamp": nil,
			"labels":            map[string]any{"beta": "2", "alpha": "1"},
		},
		"data": map[string]any{
			"changed":    "old",
			"controller": "generated",
			"obsolete":   "old",
			"replicas":   "3",
			"same":       "same",
		},
	}
	previous := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":   "demo",
			"labels": map[string]any{"alpha": "1", "beta": "2"},
		},
		"data": map[string]any{
			"changed":  "old",
			"obsolete": "old",
			"replicas": 3,
			"same":     "same",
		},
	}

	fields, truncated, reason := DiffObjects(desired, current, previous, DiffOptions{})
	require.False(t, truncated)
	assert.Empty(t, reason)
	require.Len(t, fields, 4)

	assert.Equal(t, FieldDiff{
		Path:         ".data.added",
		Operation:    DiffOperationAdd,
		Desired:      "yes",
		Current:      nil,
		DesiredType:  "string",
		CurrentType:  "null",
		TypeMismatch: false,
	}, fields[0])
	assert.Equal(t, FieldDiff{
		Path:         ".data.changed",
		Operation:    DiffOperationUpdate,
		Desired:      "new",
		Current:      "old",
		DesiredType:  "string",
		CurrentType:  "string",
		TypeMismatch: false,
	}, fields[1])
	assert.Equal(t, FieldDiff{
		Path:         ".data.obsolete",
		Operation:    DiffOperationRemove,
		Desired:      nil,
		Current:      "old",
		DesiredType:  "null",
		CurrentType:  "string",
		TypeMismatch: false,
	}, fields[2])
	assert.Equal(t, FieldDiff{
		Path:         ".data.replicas",
		Operation:    DiffOperationType,
		Desired:      3,
		Current:      "3",
		DesiredType:  "number",
		CurrentType:  "string",
		TypeMismatch: true,
	}, fields[3])
}

func TestDiffObjectsSemanticComparison(t *testing.T) {
	tests := []struct {
		name          string
		desired       any
		current       any
		operation     DiffOperation
		typeMismatch  bool
		expectChanges bool
	}{
		{
			name:          "numeric type mismatch is equal but annotated",
			desired:       3,
			current:       "3",
			operation:     DiffOperationType,
			typeMismatch:  true,
			expectChanges: true,
		},
		{
			name:          "different numeric types are semantically equal",
			desired:       int64(3),
			current:       float64(3),
			expectChanges: false,
		},
		{
			name:          "different numeric values are an update",
			desired:       int64(3),
			current:       "4",
			operation:     DiffOperationUpdate,
			typeMismatch:  true,
			expectChanges: true,
		},
		{
			name:          "boolean string is semantically equal but annotated",
			desired:       true,
			current:       "true",
			operation:     DiffOperationType,
			typeMismatch:  true,
			expectChanges: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			desired := map[string]any{"spec": map[string]any{"value": test.desired}}
			current := map[string]any{"spec": map[string]any{"value": test.current}}
			fields, truncated, reason := DiffObjects(desired, current, desired, DiffOptions{})
			require.False(t, truncated)
			assert.Empty(t, reason)
			if !test.expectChanges {
				assert.Empty(t, fields)
				return
			}
			require.Len(t, fields, 1)
			assert.Equal(t, ".spec.value", fields[0].Path)
			assert.Equal(t, test.operation, fields[0].Operation)
			assert.Equal(t, test.typeMismatch, fields[0].TypeMismatch)
		})
	}
}

func TestDiffObjectsListChanges(t *testing.T) {
	desired := map[string]any{
		"spec": map[string]any{
			"containers": []any{
				map[string]any{"name": "app", "image": "new"},
			},
		},
	}
	current := map[string]any{
		"spec": map[string]any{
			"containers": []any{
				map[string]any{
					"name":  "app",
					"image": "old",
					"env":   []any{map[string]any{"name": "CURRENT"}},
				},
				map[string]any{"name": "controller-sidecar"},
			},
		},
	}
	previous := map[string]any{
		"spec": map[string]any{
			"containers": []any{
				map[string]any{
					"name":  "app",
					"image": "old",
					"env":   []any{map[string]any{"name": "OLD"}},
				},
			},
		},
	}

	fields, truncated, reason := DiffObjects(desired, current, previous, DiffOptions{})
	require.False(t, truncated)
	assert.Empty(t, reason)
	require.Len(t, fields, 2)
	assert.Equal(t, ".spec.containers[0].env", fields[0].Path)
	assert.Equal(t, DiffOperationRemove, fields[0].Operation)
	assert.Equal(t, ".spec.containers[0].image", fields[1].Path)
	assert.Equal(t, DiffOperationUpdate, fields[1].Operation)
}

func TestDiffObjectsIgnoresFieldOrder(t *testing.T) {
	desired := map[string]any{
		"metadata": map[string]any{
			"name":   "demo",
			"labels": map[string]any{"alpha": "1", "beta": "2", "gamma": "3"},
		},
	}
	current := map[string]any{
		"metadata": map[string]any{
			"name":   "demo",
			"labels": map[string]any{"gamma": "3", "beta": "2", "alpha": "1"},
		},
	}

	fields, truncated, reason := DiffObjects(desired, current, desired, DiffOptions{})
	require.False(t, truncated)
	assert.Empty(t, reason)
	assert.Empty(t, fields)
}

func TestDiffObjectsTruncatesAtLimit(t *testing.T) {
	desired := map[string]any{"data": map[string]any{"a": "new", "b": "new", "c": "new"}}
	current := map[string]any{"data": map[string]any{"a": "old", "b": "old", "c": "old"}}

	fields, truncated, reason := DiffObjects(desired, current, desired, DiffOptions{MaxFields: 2})
	require.True(t, truncated)
	assert.Contains(t, reason, "2 field diff limit")
	assert.Len(t, fields, 2)
}

func TestDiffObjectsRejectsOversizedObject(t *testing.T) {
	desired := map[string]any{"data": map[string]any{"value": "desired"}}
	current := map[string]any{"data": map[string]any{"value": "current"}}

	fields, truncated, reason := DiffObjects(desired, current, desired, DiffOptions{MaxObjectBytes: 1})
	require.True(t, truncated)
	assert.Contains(t, reason, "byte diff limit")
	assert.Empty(t, fields)
}

func TestDiffObjectsStableOrdering(t *testing.T) {
	desired := map[string]any{
		"metadata": map[string]any{"name": "demo"},
		"spec": map[string]any{
			"z": "new",
			"a": []any{"new"},
			"m": map[string]any{"b": "new", "a": "new"},
		},
	}
	current := map[string]any{
		"metadata": map[string]any{"name": "demo"},
		"spec": map[string]any{
			"z": "old",
			"a": []any{"old"},
			"m": map[string]any{"b": "old", "a": "old"},
		},
	}

	fields, _, _ := DiffObjects(desired, current, desired, DiffOptions{})
	require.NotEmpty(t, fields)
	for range 25 {
		next, _, _ := DiffObjects(desired, current, desired, DiffOptions{})
		assert.Equal(t, fields, next)
	}
}
