package action

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/resource"

	"helm.sh/helm/v4/pkg/chart/common"
	"helm.sh/helm/v4/pkg/kube"
	kubefake "helm.sh/helm/v4/pkg/kube/fake"
	rcommon "helm.sh/helm/v4/pkg/release/common"
)

const previewOldManifest = `
apiVersion: v1
kind: ConfigMap
metadata:
  name: demo
data:
  changed: old
  obsolete: old
  replicas: 3
---
apiVersion: v1
kind: Service
metadata:
  name: retired
spec:
  ports:
  - port: 80
`

const previewNewManifest = `
apiVersion: v1
kind: ConfigMap
metadata:
  name: demo
data:
  added: "yes"
  changed: new
  replicas: 3
---
apiVersion: v1
kind: Secret
metadata:
  name: generated
stringData:
  token: value
---
apiVersion: example.com/v1
kind: Widget
metadata:
  name: unknown
spec:
  enabled: true
`

func TestUpgradePreviewReadOnly(t *testing.T) {
	cfg := actionConfigFixture(t)
	oldRelease := releaseStub()
	oldRelease.Name = "preview"
	oldRelease.Namespace = "default"
	oldRelease.Info.Status = rcommon.StatusDeployed
	oldRelease.Manifest = previewOldManifest
	oldRelease.Chart = buildChart()
	require.NoError(t, cfg.Releases.Create(oldRelease))

	configMap := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":              "demo",
			"namespace":         "default",
			"resourceVersion":   "42",
			"generation":        int64(9),
			"creationTimestamp": nil,
		},
		"data": map[string]any{
			"changed":    "old",
			"obsolete":   "old",
			"replicas":   "3",
			"controller": "generated",
		},
	}}
	service := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata": map[string]any{
			"name":      "retired",
			"namespace": "default",
		},
		"spec": map[string]any{
			"clusterIP": "10.0.0.1",
			"ports":     []any{map[string]any{"port": int64(80)}},
		},
	}}

	client := &previewKubeClient{
		PrintingKubeClient: kubefake.PrintingKubeClient{Out: io.Discard},
		knownTypes: map[string]bool{
			"v1/ConfigMap": true,
			"v1/Service":   true,
			"v1/Secret":    true,
		},
		objects: map[string]runtime.Object{
			previewKubeKey(configMap.GroupVersionKind(), "default", "demo"):  &configMap,
			previewKubeKey(service.GroupVersionKind(), "default", "retired"): &service,
		},
	}
	cfg.KubeClient = client

	targetChart := buildChartWithTemplates([]*common.File{
		{Name: "templates/resources.yaml", Data: []byte(previewNewManifest)},
	})
	upgrade := NewUpgrade(cfg)
	upgrade.Namespace = "default"

	preview, err := upgrade.RunPreviewWithContext(t.Context(), oldRelease.Name, targetChart, nil)
	require.NoError(t, err)

	assert.Zero(t, client.writes, "preview must not invoke mutating Kubernetes operations")
	lastRelease, err := cfg.Releases.Last(oldRelease.Name)
	require.NoError(t, err)
	lastReleaseV1, err := releaserToV1Release(lastRelease)
	require.NoError(t, err)
	assert.Equal(t, 1, lastReleaseV1.Version, "preview must not store a release revision")

	assert.Equal(t, kube.DiffSummary{
		Total:          4,
		Added:          1,
		Updated:        1,
		Deleted:        1,
		Incomparable:   1,
		FieldChanges:   4,
		TypeMismatches: 1,
	}, preview.Summary)
	require.Len(t, preview.Resources, 4)

	configMapDiff := findPreviewResource(t, preview, "ConfigMap", "demo")
	assert.Equal(t, kube.DiffOperationUpdate, configMapDiff.Operation)
	require.Len(t, configMapDiff.Fields, 4)
	assert.Equal(t, kube.DiffOperationAdd, configMapDiff.Fields[0].Operation)
	assert.Equal(t, ".data.added", configMapDiff.Fields[0].Path)
	assert.Equal(t, kube.DiffOperationUpdate, configMapDiff.Fields[1].Operation)
	assert.Equal(t, ".data.changed", configMapDiff.Fields[1].Path)
	assert.Equal(t, kube.DiffOperationRemove, configMapDiff.Fields[2].Operation)
	assert.Equal(t, ".data.obsolete", configMapDiff.Fields[2].Path)
	assert.Equal(t, kube.DiffOperationType, configMapDiff.Fields[3].Operation)
	assert.Equal(t, ".data.replicas", configMapDiff.Fields[3].Path)
	assert.True(t, configMapDiff.Fields[3].TypeMismatch)

	assert.Equal(t, kube.DiffOperationAdd, findPreviewResource(t, preview, "Secret", "generated").Operation)
	assert.Equal(t, kube.DiffOperationDelete, findPreviewResource(t, preview, "Service", "retired").Operation)
	incomparable := findPreviewResource(t, preview, "Widget", "unknown")
	assert.Equal(t, kube.DiffOperationIncomparable, incomparable.Operation)
	assert.Contains(t, incomparable.Reason, "not available in the cluster")

	again, err := upgrade.RunPreviewWithContext(t.Context(), oldRelease.Name, targetChart, nil)
	require.NoError(t, err)
	assert.Equal(t, preview, again, "preview output must be stable across identical runs")
	assert.Zero(t, client.writes)
}

func TestUpgradePreviewFailsWhenClusterUnreachable(t *testing.T) {
	cfg := actionConfigFixture(t)
	client := &previewKubeClient{
		PrintingKubeClient: kubefake.PrintingKubeClient{Out: io.Discard},
		reachableErr:       errors.New("cluster unavailable"),
	}
	cfg.KubeClient = client

	upgrade := NewUpgrade(cfg)
	upgrade.Namespace = "default"
	_, err := upgrade.RunPreviewWithContext(t.Context(), "preview", buildChart(), nil)
	require.ErrorContains(t, err, "cluster unavailable")
	assert.Zero(t, client.writes)
}

func TestUpgradeRunRejectsPreviewMode(t *testing.T) {
	cfg := actionConfigFixture(t)
	client := &previewKubeClient{PrintingKubeClient: kubefake.PrintingKubeClient{Out: io.Discard}}
	cfg.KubeClient = client

	upgrade := NewUpgrade(cfg)
	upgrade.Preview = true
	_, err := upgrade.Run("preview", buildChart(), nil)
	require.ErrorContains(t, err, "RunPreviewWithContext")
	assert.Zero(t, client.writes)
}

func TestUpgradePreviewFailsOnReadError(t *testing.T) {
	cfg := actionConfigFixture(t)
	oldRelease := releaseStub()
	oldRelease.Name = "preview"
	oldRelease.Namespace = "default"
	oldRelease.Info.Status = rcommon.StatusDeployed
	oldRelease.Manifest = previewOldManifest
	oldRelease.Chart = buildChart()
	require.NoError(t, cfg.Releases.Create(oldRelease))

	client := &previewKubeClient{
		PrintingKubeClient: kubefake.PrintingKubeClient{Out: io.Discard},
		knownTypes: map[string]bool{
			"v1/ConfigMap": true,
			"v1/Secret":    true,
			"v1/Service":   true,
		},
		readErrors: map[string]error{
			"v1/ConfigMap/default/demo": apierrors.NewForbidden(
				schema.GroupResource{Resource: "configmaps"},
				"demo",
				errors.New("denied"),
			),
		},
		objects: map[string]runtime.Object{},
	}
	cfg.KubeClient = client

	targetChart := buildChartWithTemplates([]*common.File{
		{Name: "templates/configmap.yaml", Data: []byte(`
apiVersion: v1
kind: ConfigMap
metadata:
  name: demo
data:
  changed: new
`)},
	})
	upgrade := NewUpgrade(cfg)
	upgrade.Namespace = "default"
	_, err := upgrade.RunPreviewWithContext(t.Context(), oldRelease.Name, targetChart, nil)
	require.ErrorContains(t, err, "denied")
	assert.Zero(t, client.writes)
}

func TestUpgradePreviewSummaryLimit(t *testing.T) {
	cfg := actionConfigFixture(t)
	oldRelease := releaseStub()
	oldRelease.Name = "preview"
	oldRelease.Namespace = "default"
	oldRelease.Info.Status = rcommon.StatusDeployed
	oldRelease.Manifest = ""
	oldRelease.Chart = buildChart()
	require.NoError(t, cfg.Releases.Create(oldRelease))

	client := &previewKubeClient{
		PrintingKubeClient: kubefake.PrintingKubeClient{Out: io.Discard},
		knownTypes:         map[string]bool{"v1/ConfigMap": true},
		objects:            map[string]runtime.Object{},
	}
	cfg.KubeClient = client

	targetChart := buildChartWithTemplates([]*common.File{
		{Name: "templates/resources.yaml", Data: []byte(`
apiVersion: v1
kind: ConfigMap
metadata:
  name: one
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: two
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: three
`)},
	})
	upgrade := NewUpgrade(cfg)
	upgrade.Namespace = "default"
	upgrade.PreviewLimit = 2
	upgrade.PreviewSummary = true

	preview, err := upgrade.RunPreviewWithContext(t.Context(), oldRelease.Name, targetChart, nil)
	require.NoError(t, err)
	assert.Len(t, preview.Resources, 2)
	assert.Equal(t, 1, preview.Summary.OmittedResources)
	assert.Equal(t, 3, preview.Summary.Added)
	assert.True(t, preview.Summary.Truncated)
	assert.True(t, preview.SummaryOnly)
}

func TestUpgradePreviewFiveResourceTypesWithoutWrites(t *testing.T) {
	cfg := actionConfigFixture(t)
	oldRelease := releaseStub()
	oldRelease.Name = "preview"
	oldRelease.Namespace = "default"
	oldRelease.Info.Status = rcommon.StatusDeployed
	oldRelease.Manifest = ""
	oldRelease.Chart = buildChart()
	require.NoError(t, cfg.Releases.Create(oldRelease))

	knownTypes := map[string]bool{
		"v1/ConfigMap":       true,
		"v1/Secret":          true,
		"v1/Service":         true,
		"v1/ServiceAccount":  true,
		"apps/v1/Deployment": true,
	}
	client := &previewKubeClient{
		PrintingKubeClient: kubefake.PrintingKubeClient{Out: io.Discard},
		knownTypes:         knownTypes,
		objects:            map[string]runtime.Object{},
	}
	cfg.KubeClient = client

	targetChart := buildChartWithTemplates([]*common.File{
		{Name: "templates/resources.yaml", Data: []byte(`
apiVersion: v1
kind: ConfigMap
metadata:
  name: config
---
apiVersion: v1
kind: Secret
metadata:
  name: secret
---
apiVersion: v1
kind: Service
metadata:
  name: service
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: account
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: deployment
`)},
	})
	upgrade := NewUpgrade(cfg)
	upgrade.Namespace = "default"

	preview, err := upgrade.RunPreviewWithContext(t.Context(), oldRelease.Name, targetChart, nil)
	require.NoError(t, err)
	assert.Equal(t, 5, preview.Summary.Added)
	assert.Equal(t, 5, preview.Summary.Total)
	assert.Len(t, preview.Resources, 5)

	kinds := make(map[string]struct{}, len(preview.Resources))
	for _, resourceDiff := range preview.Resources {
		kinds[resourceDiff.Kind] = struct{}{}
	}
	assert.Len(t, kinds, 5)
	assert.Zero(t, client.writes)
}

func TestRedactPreviewSecrets(t *testing.T) {
	preview := &UpgradePreview{
		Resources: []kube.ResourceDiff{
			{
				Kind: "Secret",
				Fields: []kube.FieldDiff{
					{
						Path:        ".stringData.token",
						Desired:     "new-secret",
						Current:     "old-secret",
						DesiredType: "string",
						CurrentType: "string",
					},
				},
			},
			{
				Kind: "ConfigMap",
				Fields: []kube.FieldDiff{
					{Path: ".data.value", Desired: "new", Current: "old"},
				},
			},
		},
	}

	redactPreviewSecrets(preview)
	assert.Equal(t, "<redacted>", preview.Resources[0].Fields[0].Desired)
	assert.Equal(t, "<redacted>", preview.Resources[0].Fields[0].Current)
	assert.Equal(t, "string", preview.Resources[0].Fields[0].DesiredType)
	assert.Equal(t, "new", preview.Resources[1].Fields[0].Desired)
}

func findPreviewResource(t *testing.T, preview *UpgradePreview, kind, name string) kube.ResourceDiff {
	t.Helper()
	for _, resourceDiff := range preview.Resources {
		if resourceDiff.Kind == kind && resourceDiff.Name == name {
			return resourceDiff
		}
	}
	require.FailNow(t, fmt.Sprintf("resource %s/%s not found in preview", kind, name))
	return kube.ResourceDiff{}
}

type previewKubeClient struct {
	kubefake.PrintingKubeClient
	knownTypes   map[string]bool
	objects      map[string]runtime.Object
	readErrors   map[string]error
	reachableErr error
	writes       int
}

func (c *previewKubeClient) IsReachable() error {
	return c.reachableErr
}

func (c *previewKubeClient) Build(reader io.Reader, _ bool) (kube.ResourceList, error) {
	var object unstructured.Unstructured
	if err := json.NewDecoder(reader).Decode(&object.Object); err != nil {
		return nil, err
	}
	if len(object.Object) == 0 {
		return nil, nil
	}

	gvk := object.GroupVersionKind()
	if !c.knownTypes[gvk.GroupVersion().String()+"/"+gvk.Kind] {
		return nil, fmt.Errorf("unable to recognize %q: no matches for kind %q", gvk.String(), gvk.Kind)
	}
	namespace := object.GetNamespace()
	if namespace == "" {
		namespace = "default"
	}
	info := &resource.Info{
		Name:      object.GetName(),
		Namespace: namespace,
		Object:    &object,
		Mapping: &meta.RESTMapping{
			GroupVersionKind: gvk,
			Resource: schema.GroupVersionResource{
				Group:    gvk.Group,
				Version:  gvk.Version,
				Resource: gvk.Kind,
			},
		},
	}
	return kube.ResourceList{info}, nil
}

func (c *previewKubeClient) GetResource(info *resource.Info) (runtime.Object, error) {
	key := previewKubeKey(info.Mapping.GroupVersionKind, info.Namespace, info.Name)
	if err := c.readErrors[key]; err != nil {
		return nil, err
	}
	if object, ok := c.objects[key]; ok {
		return object, nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: info.Mapping.GroupVersionKind.Kind}, info.Name)
}

func (c *previewKubeClient) Create(kube.ResourceList, ...kube.ClientCreateOption) (*kube.Result, error) {
	c.writes++
	return &kube.Result{}, nil
}

func (c *previewKubeClient) Delete(kube.ResourceList, metav1.DeletionPropagation) (*kube.Result, []error) {
	c.writes++
	return &kube.Result{}, nil
}

func (c *previewKubeClient) Update(kube.ResourceList, kube.ResourceList, ...kube.ClientUpdateOption) (*kube.Result, error) {
	c.writes++
	return &kube.Result{}, nil
}

func previewKubeKey(gvk schema.GroupVersionKind, namespace, name string) string {
	return fmt.Sprintf("%s/%s/%s/%s", gvk.GroupVersion().String(), gvk.Kind, namespace, name)
}
