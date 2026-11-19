package action

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/cli-runtime/pkg/resource"

	"helm.sh/helm/v4/pkg/chart"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	chartutil "helm.sh/helm/v4/pkg/chart/v2/util"
	"helm.sh/helm/v4/pkg/kube"
)

const (
	DefaultPreviewLimit      = 200
	DefaultPreviewFieldLimit = kube.DefaultDiffMaxFields
)

// UpgradePreview is the read-only result of comparing an upgrade's rendered
// manifests with the objects currently in the cluster.
type UpgradePreview struct {
	Release     string              `json:"release"`
	Namespace   string              `json:"namespace"`
	Revision    int                 `json:"revision"`
	SummaryOnly bool                `json:"summaryOnly,omitempty"`
	Resources   []kube.ResourceDiff `json:"resources,omitempty"`
	Summary     kube.DiffSummary    `json:"summary"`
}

type previewManifestResource struct {
	apiVersion string
	kind       string
	namespace  string
	name       string
	info       *resource.Info
	object     map[string]any
	buildErr   error
}

// RunPreview executes a read-only upgrade preview with a background context.
func (u *Upgrade) RunPreview(name string, ch chart.Charter, vals map[string]any) (*UpgradePreview, error) {
	return u.RunPreviewWithContext(context.Background(), name, ch, vals)
}

// RunPreviewWithContext renders the requested upgrade and compares it with the
// live cluster without invoking any mutating Kubernetes operation.
func (u *Upgrade) RunPreviewWithContext(ctx context.Context, name string, ch chart.Charter, vals map[string]any) (*UpgradePreview, error) {
	if u.cfg == nil || u.cfg.KubeClient == nil {
		return nil, errors.New("kubernetes client is not configured")
	}
	if err := u.cfg.KubeClient.IsReachable(); err != nil {
		return nil, err
	}

	readOnlyClient, ok := u.cfg.KubeClient.(kube.ReadOnlyInterface)
	if !ok {
		return nil, errors.New("kubernetes client does not support read-only preview")
	}

	var chrt *chartv2.Chart
	switch typed := ch.(type) {
	case *chartv2.Chart:
		chrt = typed
	case chartv2.Chart:
		chrt = &typed
	default:
		return nil, errors.New("invalid chart apiVersion")
	}

	if err := chartutil.ValidateReleaseName(name); err != nil {
		return nil, fmt.Errorf("release name is invalid: %s", name)
	}

	currentRelease, upgradedRelease, _, err := u.prepareUpgradeWithOptions(ctx, name, chrt, vals, true)
	if err != nil {
		return nil, err
	}

	previousResources, err := buildPreviewResources(readOnlyClient, currentRelease.Manifest, currentRelease.Name, currentRelease.Namespace, false)
	if err != nil {
		return nil, fmt.Errorf("unable to build current release resources for preview: %w", err)
	}
	desiredResources, err := buildPreviewResources(readOnlyClient, upgradedRelease.Manifest, upgradedRelease.Name, upgradedRelease.Namespace, true)
	if err != nil {
		return nil, fmt.Errorf("unable to build upgrade resources for preview: %w", err)
	}

	limit := u.PreviewLimit
	if limit <= 0 {
		limit = DefaultPreviewLimit
	}
	fieldLimit := u.PreviewFieldLimit
	if fieldLimit <= 0 {
		fieldLimit = DefaultPreviewFieldLimit
	}

	preview := &UpgradePreview{
		Release:     name,
		Namespace:   upgradedRelease.Namespace,
		Revision:    upgradedRelease.Version,
		SummaryOnly: u.PreviewSummary,
	}
	comparer := previewComparer{
		ctx:    ctx,
		client: readOnlyClient,
		limit:  limit,
		diffOptions: kube.DiffOptions{
			MaxFields: fieldLimit,
		},
		previous: previousResources,
		desired:  desiredResources,
	}
	if err := comparer.compare(preview); err != nil {
		return nil, err
	}
	if u.HideSecret {
		redactPreviewSecrets(preview)
	}
	return preview, nil
}

type previewComparer struct {
	ctx         context.Context
	client      kube.ReadOnlyInterface
	limit       int
	diffOptions kube.DiffOptions
	previous    []previewManifestResource
	desired     []previewManifestResource

	resources    []kube.ResourceDiff
	operations   map[kube.DiffOperation]int
	fieldChanges int
	typeMismatch int
	omitted      int
	truncated    bool
}

func (c *previewComparer) compare(preview *UpgradePreview) error {
	c.operations = make(map[kube.DiffOperation]int)

	previousByKey := make(map[string]previewManifestResource, len(c.previous))
	for _, entry := range c.previous {
		previousByKey[previewResourceKey(entry)] = entry
	}

	handledPrevious := make(map[string]struct{}, len(c.previous))
	seenDesired := make(map[string]struct{}, len(c.desired))
	for _, desired := range c.desired {
		if err := c.checkContext(); err != nil {
			return err
		}

		key := previewResourceKey(desired)
		if _, ok := seenDesired[key]; ok {
			continue
		}
		seenDesired[key] = struct{}{}

		previous, hasPrevious := previousByKey[key]
		if hasPrevious {
			handledPrevious[key] = struct{}{}
		}

		if desired.buildErr != nil {
			c.add(kube.ResourceDiff{
				APIVersion: desired.apiVersion,
				Kind:       desired.kind,
				Namespace:  desired.namespace,
				Name:       desired.name,
				Operation:  kube.DiffOperationIncomparable,
				Reason:     desired.buildErr.Error(),
			})
			continue
		}
		if hasPrevious && previous.buildErr != nil {
			c.add(kube.ResourceDiff{
				APIVersion: desired.apiVersion,
				Kind:       desired.kind,
				Namespace:  desired.namespace,
				Name:       desired.name,
				Operation:  kube.DiffOperationIncomparable,
				Reason:     previous.buildErr.Error(),
			})
			continue
		}

		currentObject, exists, err := c.getCurrentObject(desired.info)
		if err != nil {
			return fmt.Errorf("unable to read %s %s/%s for preview: %w", desired.kind, desired.namespace, desired.name, err)
		}

		if !exists {
			c.add(kube.ResourceDiff{
				APIVersion: desired.apiVersion,
				Kind:       desired.kind,
				Namespace:  desired.namespace,
				Name:       desired.name,
				Operation:  kube.DiffOperationAdd,
			})
			continue
		}

		currentMap, err := previewObjectMap(currentObject)
		if err != nil {
			return fmt.Errorf("unable to decode current %s %s/%s for preview: %w", desired.kind, desired.namespace, desired.name, err)
		}

		var previousMap map[string]any
		if hasPrevious {
			previousMap = previous.object
		}
		fields, truncated, reason := kube.DiffObjects(desired.object, currentMap, previousMap, c.diffOptions)
		if len(fields) == 0 && !truncated {
			continue
		}

		c.add(kube.ResourceDiff{
			APIVersion: desired.apiVersion,
			Kind:       desired.kind,
			Namespace:  desired.namespace,
			Name:       desired.name,
			Operation:  kube.DiffOperationUpdate,
			Fields:     fields,
			Reason:     reason,
			Truncated:  truncated,
		})
	}

	for _, previous := range c.previous {
		if err := c.checkContext(); err != nil {
			return err
		}

		key := previewResourceKey(previous)
		if _, ok := handledPrevious[key]; ok {
			continue
		}
		handledPrevious[key] = struct{}{}

		if previous.buildErr != nil {
			c.add(kube.ResourceDiff{
				APIVersion: previous.apiVersion,
				Kind:       previous.kind,
				Namespace:  previous.namespace,
				Name:       previous.name,
				Operation:  kube.DiffOperationIncomparable,
				Reason:     previous.buildErr.Error(),
			})
			continue
		}

		_, exists, err := c.getCurrentObject(previous.info)
		if err != nil {
			return fmt.Errorf("unable to read %s %s/%s for preview: %w", previous.kind, previous.namespace, previous.name, err)
		}
		if !exists {
			continue
		}

		c.add(kube.ResourceDiff{
			APIVersion: previous.apiVersion,
			Kind:       previous.kind,
			Namespace:  previous.namespace,
			Name:       previous.name,
			Operation:  kube.DiffOperationDelete,
		})
	}

	sort.SliceStable(c.resources, func(i, j int) bool {
		left, right := c.resources[i], c.resources[j]
		if left.Namespace != right.Namespace {
			return left.Namespace < right.Namespace
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		if left.APIVersion != right.APIVersion {
			return left.APIVersion < right.APIVersion
		}
		return left.Operation < right.Operation
	})

	preview.Resources = c.resources
	preview.Summary = kube.DiffSummary{
		Total:            len(c.resources) + c.omitted,
		Added:            c.operations[kube.DiffOperationAdd],
		Updated:          c.operations[kube.DiffOperationUpdate],
		Deleted:          c.operations[kube.DiffOperationDelete],
		Incomparable:     c.operations[kube.DiffOperationIncomparable],
		FieldChanges:     c.fieldChanges,
		TypeMismatches:   c.typeMismatch,
		OmittedResources: c.omitted,
		Truncated:        c.truncated,
	}
	return nil
}

func (c *previewComparer) add(resourceDiff kube.ResourceDiff) {
	c.operations[resourceDiff.Operation]++
	for _, field := range resourceDiff.Fields {
		c.fieldChanges++
		if field.TypeMismatch || field.Operation == kube.DiffOperationType {
			c.typeMismatch++
		}
	}
	if resourceDiff.Truncated {
		c.truncated = true
	}

	if len(c.resources) >= c.limit {
		c.omitted++
		c.truncated = true
		return
	}
	c.resources = append(c.resources, resourceDiff)
}

func (c *previewComparer) checkContext() error {
	select {
	case <-c.ctx.Done():
		return c.ctx.Err()
	default:
		return nil
	}
}

func (c *previewComparer) getCurrentObject(info *resource.Info) (runtime.Object, bool, error) {
	if info == nil {
		return nil, false, errors.New("missing resource mapping")
	}
	if getter, ok := c.client.(kube.ObjectGetter); ok {
		object, err := getter.GetResource(info)
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		return object, true, nil
	}

	objects, err := c.client.Get(kube.ResourceList{info}, false)
	if err != nil {
		return nil, false, err
	}
	for _, list := range objects {
		if len(list) > 0 {
			return list[0], true, nil
		}
	}
	return nil, false, nil
}

func buildPreviewResources(client kube.ReadOnlyInterface, manifest, releaseName, releaseNamespace string, applyMetadata bool) ([]previewManifestResource, error) {
	objects, err := decodePreviewManifest(manifest)
	if err != nil {
		return nil, err
	}

	resources := make([]previewManifestResource, 0, len(objects))
	for _, object := range objects {
		gvk := object.GroupVersionKind()
		entry := previewManifestResource{
			apiVersion: gvk.GroupVersion().String(),
			kind:       gvk.Kind,
			namespace:  object.GetNamespace(),
			name:       object.GetName(),
			object:     object.Object,
		}

		raw, err := runtime.Encode(unstructured.UnstructuredJSONScheme, object)
		if err != nil {
			return nil, fmt.Errorf("unable to encode %s %s: %w", gvk.Kind, object.GetName(), err)
		}
		infos, err := client.Build(bytes.NewReader(raw), false)
		if err != nil {
			if isUnknownPreviewType(err) {
				entry.buildErr = fmt.Errorf("resource type %s is not available in the cluster: %w", gvk.String(), err)
				resources = append(resources, entry)
				continue
			}
			return nil, err
		}
		if len(infos) == 0 {
			entry.buildErr = fmt.Errorf("resource type %s could not be mapped", gvk.String())
			resources = append(resources, entry)
			continue
		}

		for _, info := range infos {
			if applyMetadata {
				if err := setMetadataVisitor(releaseName, releaseNamespace, true)(info, nil); err != nil {
					return nil, err
				}
			}
			infoEntry := entry
			if info.Mapping != nil {
				infoGVK := info.Mapping.GroupVersionKind
				infoEntry.apiVersion = infoGVK.GroupVersion().String()
				infoEntry.kind = infoGVK.Kind
			}
			infoEntry.namespace = info.Namespace
			infoEntry.name = info.Name
			infoEntry.info = info
			if objectMap, err := previewObjectMap(info.Object); err == nil {
				infoEntry.object = objectMap
			}
			resources = append(resources, infoEntry)
		}
	}
	return resources, nil
}

func decodePreviewManifest(manifest string) ([]*unstructured.Unstructured, error) {
	decoder := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(manifest), 4096)
	var objects []*unstructured.Unstructured
	for {
		object := &unstructured.Unstructured{}
		err := decoder.Decode(object)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(object.Object) == 0 {
			continue
		}
		objects = append(objects, object)
	}
	return objects, nil
}

func previewObjectMap(object runtime.Object) (map[string]any, error) {
	if object == nil {
		return nil, nil
	}
	if unstructuredObject, ok := object.(*unstructured.Unstructured); ok {
		return unstructuredObject.Object, nil
	}
	return runtime.DefaultUnstructuredConverter.ToUnstructured(object)
}

func previewResourceKey(resource previewManifestResource) string {
	gvk := schema.FromAPIVersionAndKind(resource.apiVersion, resource.kind)
	groupKind := schema.GroupKind{Group: gvk.Group, Kind: gvk.Kind}
	return fmt.Sprintf("%s/%s/%s", groupKind.String(), resource.namespace, resource.name)
}

func isUnknownPreviewType(err error) bool {
	if meta.IsNoMatchError(err) {
		return true
	}
	errText := err.Error()
	return strings.Contains(errText, "no matches for kind") ||
		strings.Contains(errText, "unable to recognize") ||
		strings.Contains(errText, "could not find the requested resource")
}

func redactPreviewSecrets(preview *UpgradePreview) {
	for resourceIndex := range preview.Resources {
		if preview.Resources[resourceIndex].Kind != "Secret" {
			continue
		}
		for fieldIndex := range preview.Resources[resourceIndex].Fields {
			preview.Resources[resourceIndex].Fields[fieldIndex].Desired = "<redacted>"
			preview.Resources[resourceIndex].Fields[fieldIndex].Current = "<redacted>"
		}
	}
}
