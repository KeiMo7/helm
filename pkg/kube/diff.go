package kube

import (
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// DiffOperation identifies the kind of change represented by a resource or
// field diff.
type DiffOperation string

const (
	DiffOperationAdd          DiffOperation = "add"
	DiffOperationRemove       DiffOperation = "remove"
	DiffOperationUpdate       DiffOperation = "update"
	DiffOperationDelete       DiffOperation = "delete"
	DiffOperationType         DiffOperation = "type"
	DiffOperationIncomparable DiffOperation = "incomparable"
)

const (
	DefaultDiffMaxFields      = 100
	DefaultDiffMaxObjectBytes = 4 << 20
	DefaultDiffMaxNodes       = 20000
)

// DiffOptions limits the amount of work and output produced for a single
// resource comparison.
type DiffOptions struct {
	// MaxFields is the maximum number of field differences retained for one
	// resource. A value less than or equal to zero uses DefaultDiffMaxFields.
	MaxFields int
	// MaxObjectBytes refuses to recurse into objects larger than this size. A
	// value less than or equal to zero uses DefaultDiffMaxObjectBytes.
	MaxObjectBytes int
	// MaxNodes limits the number of values visited while comparing an object.
	// A value less than or equal to zero uses DefaultDiffMaxNodes.
	MaxNodes int
}

// FieldDiff describes one manifest-aligned field change.
type FieldDiff struct {
	Path         string        `json:"path"`
	Operation    DiffOperation `json:"operation"`
	Desired      any           `json:"desired"`
	Current      any           `json:"current"`
	DesiredType  string        `json:"desiredType"`
	CurrentType  string        `json:"currentType"`
	TypeMismatch bool          `json:"typeMismatch,omitempty"`
}

// ResourceDiff describes the planned change for one Kubernetes object.
type ResourceDiff struct {
	APIVersion string        `json:"apiVersion"`
	Kind       string        `json:"kind"`
	Namespace  string        `json:"namespace,omitempty"`
	Name       string        `json:"name"`
	Operation  DiffOperation `json:"operation"`
	Fields     []FieldDiff   `json:"fields,omitempty"`
	Reason     string        `json:"reason,omitempty"`
	Truncated  bool          `json:"truncated,omitempty"`
}

// DiffSummary is the machine-readable roll-up of a set of resource diffs.
type DiffSummary struct {
	Total            int  `json:"total"`
	Added            int  `json:"added"`
	Updated          int  `json:"updated"`
	Deleted          int  `json:"deleted"`
	Incomparable     int  `json:"incomparable"`
	FieldChanges     int  `json:"fieldChanges"`
	TypeMismatches   int  `json:"typeMismatches"`
	OmittedResources int  `json:"omittedResources,omitempty"`
	Truncated        bool `json:"truncated,omitempty"`
}

// SummarizeDiffs creates a deterministic summary for a resource diff list.
func SummarizeDiffs(resources []ResourceDiff, omitted int, truncated bool) DiffSummary {
	summary := DiffSummary{
		Total:            len(resources) + omitted,
		OmittedResources: omitted,
		Truncated:        truncated,
	}
	for _, resource := range resources {
		switch resource.Operation {
		case DiffOperationAdd:
			summary.Added++
		case DiffOperationUpdate:
			summary.Updated++
		case DiffOperationDelete:
			summary.Deleted++
		case DiffOperationIncomparable:
			summary.Incomparable++
		}
		for _, field := range resource.Fields {
			summary.FieldChanges++
			if field.TypeMismatch || field.Operation == DiffOperationType {
				summary.TypeMismatches++
			}
		}
		if resource.Truncated {
			summary.Truncated = true
		}
	}
	return summary
}

// DiffObjects compares desired manifest fields with the current cluster object.
//
// Fields that only exist in the cluster are ignored unless they were also
// present in the previous manifest. This keeps controller-populated fields out
// of the result while still reporting fields intentionally removed from the
// chart manifest.
func DiffObjects(desired, current, previous map[string]any, opts DiffOptions) ([]FieldDiff, bool, string) {
	opts = normalizeDiffOptions(opts)

	if objectSize(desired) > opts.MaxObjectBytes || objectSize(current) > opts.MaxObjectBytes {
		return nil, true, fmt.Sprintf("object exceeds the %d byte diff limit", opts.MaxObjectBytes)
	}

	state := diffState{
		options: opts,
		fields:  make([]FieldDiff, 0),
	}
	state.diffMap("", desired, current, previous, true)
	sortFieldDiffs(state.fields)
	return state.fields, state.truncated, state.reason
}

type diffState struct {
	options   DiffOptions
	fields    []FieldDiff
	nodes     int
	truncated bool
	reason    string
}

func (s *diffState) stop(reason string) bool {
	if s.truncated {
		return true
	}
	s.truncated = true
	s.reason = reason
	return true
}

func (s *diffState) canVisit() bool {
	if s.truncated {
		return false
	}
	s.nodes++
	if s.nodes > s.options.MaxNodes {
		return s.stop(fmt.Sprintf("object exceeds the %d node diff limit", s.options.MaxNodes))
	}
	return true
}

func (s *diffState) append(field FieldDiff) bool {
	if len(s.fields) >= s.options.MaxFields {
		return s.stop(fmt.Sprintf("field changes exceed the %d field diff limit", s.options.MaxFields))
	}
	s.fields = append(s.fields, field)
	return true
}

func (s *diffState) diffValue(path string, desired, current, previous any, previousKnown bool) bool {
	if !s.canVisit() {
		return false
	}

	desiredMap, desiredIsMap := asStringMap(desired)
	currentMap, currentIsMap := asStringMap(current)
	if desiredIsMap && currentIsMap {
		previousMap, _ := asStringMap(previous)
		return s.diffMap(path, desiredMap, currentMap, previousMap, previousKnown)
	}

	desiredList, desiredIsList := asSlice(desired)
	currentList, currentIsList := asSlice(current)
	if desiredIsList && currentIsList {
		previousList, _ := asSlice(previous)
		return s.diffList(path, desiredList, currentList, previousList, previousKnown)
	}

	equal, mismatch := semanticallyEqual(desired, current)
	desiredType := diffTypeName(desired)
	currentType := diffTypeName(current)
	if equal {
		if mismatch {
			return s.append(FieldDiff{
				Path:         path,
				Operation:    DiffOperationType,
				Desired:      desired,
				Current:      current,
				DesiredType:  desiredType,
				CurrentType:  currentType,
				TypeMismatch: true,
			})
		}
		return true
	}

	return s.append(FieldDiff{
		Path:         path,
		Operation:    DiffOperationUpdate,
		Desired:      desired,
		Current:      current,
		DesiredType:  desiredType,
		CurrentType:  currentType,
		TypeMismatch: desiredType != currentType,
	})
}

func (s *diffState) diffMap(path string, desired, current, previous map[string]any, previousKnown bool) bool {
	keys := make([]string, 0, len(desired)+len(current))
	seen := make(map[string]struct{}, len(desired)+len(current))
	for key := range desired {
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	for key := range current {
		if _, ok := seen[key]; !ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)

	for _, key := range keys {
		fieldPath := joinDiffPath(path, key)
		if ignoredDiffPath(fieldPath) {
			continue
		}
		desiredValue, inDesired := desired[key]
		currentValue, inCurrent := current[key]
		previousValue, inPrevious := previous[key]

		switch {
		case inDesired && inCurrent:
			previousValue, inPrevious = previous[key]
			if !s.diffValue(fieldPath, desiredValue, currentValue, previousValue, previousKnown && inPrevious) {
				return false
			}
		case inDesired:
			if desiredMap, ok := asStringMap(desiredValue); ok {
				previousMap, _ := asStringMap(previousValue)
				if !s.diffMap(fieldPath, desiredMap, map[string]any{}, previousMap, previousKnown && inPrevious) {
					return false
				}
				continue
			}
			if !s.append(FieldDiff{
				Path:        fieldPath,
				Operation:   DiffOperationAdd,
				Desired:     desiredValue,
				Current:     nil,
				DesiredType: diffTypeName(desiredValue),
				CurrentType: "null",
			}) {
				return false
			}
		case inCurrent:
			if !previousKnown || !inPrevious {
				continue
			}
			if !s.append(FieldDiff{
				Path:        fieldPath,
				Operation:   DiffOperationRemove,
				Desired:     nil,
				Current:     currentValue,
				DesiredType: "null",
				CurrentType: diffTypeName(currentValue),
			}) {
				return false
			}
		}
	}
	return true
}

func (s *diffState) diffList(path string, desired, current, previous []any, previousKnown bool) bool {
	limit := max(len(desired), len(current))
	for index := 0; index < limit; index++ {
		fieldPath := fmt.Sprintf("%s[%d]", path, index)
		hasDesired := index < len(desired)
		hasCurrent := index < len(current)

		switch {
		case hasDesired && hasCurrent:
			var previousValue any
			inPrevious := index < len(previous)
			if inPrevious {
				previousValue = previous[index]
			}
			if !s.diffValue(fieldPath, desired[index], current[index], previousValue, previousKnown && inPrevious) {
				return false
			}
		case hasDesired:
			if !s.append(FieldDiff{
				Path:        fieldPath,
				Operation:   DiffOperationAdd,
				Desired:     desired[index],
				Current:     nil,
				DesiredType: diffTypeName(desired[index]),
				CurrentType: "null",
			}) {
				return false
			}
		case hasCurrent:
			if !previousKnown || index >= len(previous) || ignoredDiffPath(fieldPath) {
				continue
			}
			if !s.append(FieldDiff{
				Path:        fieldPath,
				Operation:   DiffOperationRemove,
				Desired:     nil,
				Current:     current[index],
				DesiredType: "null",
				CurrentType: diffTypeName(current[index]),
			}) {
				return false
			}
		}
	}
	return true
}

func normalizeDiffOptions(opts DiffOptions) DiffOptions {
	if opts.MaxFields <= 0 {
		opts.MaxFields = DefaultDiffMaxFields
	}
	if opts.MaxObjectBytes <= 0 {
		opts.MaxObjectBytes = DefaultDiffMaxObjectBytes
	}
	if opts.MaxNodes <= 0 {
		opts.MaxNodes = DefaultDiffMaxNodes
	}
	return opts
}

func objectSize(value any) int {
	if value == nil {
		return 0
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return 0
	}
	return len(raw)
}

func asStringMap(value any) (map[string]any, bool) {
	typed, ok := value.(map[string]any)
	return typed, ok
}

func asSlice(value any) ([]any, bool) {
	typed, ok := value.([]any)
	return typed, ok
}

func joinDiffPath(path, key string) string {
	if path == "" {
		return "." + key
	}
	return path + "." + key
}

func sortFieldDiffs(fields []FieldDiff) {
	sort.SliceStable(fields, func(i, j int) bool {
		if fields[i].Path != fields[j].Path {
			return fields[i].Path < fields[j].Path
		}
		if fields[i].Operation != fields[j].Operation {
			return fields[i].Operation < fields[j].Operation
		}
		return diffSortValue(fields[i]) < diffSortValue(fields[j])
	})
}

func diffSortValue(field FieldDiff) string {
	raw, err := json.Marshal([]any{field.Desired, field.Current})
	if err != nil {
		return fmt.Sprintf("%v/%v", field.Desired, field.Current)
	}
	return string(raw)
}

func semanticallyEqual(desired, current any) (equal bool, typeMismatch bool) {
	desiredType := diffTypeName(desired)
	currentType := diffTypeName(current)
	typeMismatch = desiredType != currentType

	if reflect.DeepEqual(desired, current) {
		return true, false
	}

	desiredNumber, desiredIsNumber := numericValue(desired)
	currentNumber, currentIsNumber := numericValue(current)
	if desiredIsNumber && currentIsNumber {
		return desiredNumber.Cmp(currentNumber) == 0, desiredType != currentType
	}

	desiredBool, desiredIsBool := boolValue(desired)
	currentBool, currentIsBool := boolValue(current)
	if desiredIsBool && currentIsBool {
		return desiredBool == currentBool, desiredType != currentType
	}

	return false, typeMismatch
}

func numericValue(value any) (*big.Rat, bool) {
	var text string
	switch typed := value.(type) {
	case json.Number:
		text = typed.String()
	case int:
		text = strconv.FormatInt(int64(typed), 10)
	case int8:
		text = strconv.FormatInt(int64(typed), 10)
	case int16:
		text = strconv.FormatInt(int64(typed), 10)
	case int32:
		text = strconv.FormatInt(int64(typed), 10)
	case int64:
		text = strconv.FormatInt(typed, 10)
	case uint:
		text = strconv.FormatUint(uint64(typed), 10)
	case uint8:
		text = strconv.FormatUint(uint64(typed), 10)
	case uint16:
		text = strconv.FormatUint(uint64(typed), 10)
	case uint32:
		text = strconv.FormatUint(uint64(typed), 10)
	case uint64:
		text = strconv.FormatUint(typed, 10)
	case float32:
		text = strconv.FormatFloat(float64(typed), 'g', -1, 32)
	case float64:
		text = strconv.FormatFloat(typed, 'g', -1, 64)
	case string:
		text = strings.TrimSpace(typed)
	default:
		return nil, false
	}

	number, ok := new(big.Rat).SetString(text)
	if ok {
		return number, true
	}
	floatNumber, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return nil, false
	}
	number = new(big.Rat).SetFloat64(floatNumber)
	if number == nil {
		return nil, false
	}
	return number, true
}

func boolValue(value any) (bool, bool) {
	switch typed := value.(type) {
	case bool:
		return typed, true
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(typed))
		return parsed, err == nil
	default:
		return false, false
	}
}

func diffTypeName(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case bool, string:
		return fmt.Sprintf("%T", value)
	case json.Number,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return "number"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	default:
		return fmt.Sprintf("%T", value)
	}
}

func ignoredDiffPath(path string) bool {
	path = strings.TrimPrefix(path, ".")
	switch {
	case path == "status", strings.HasPrefix(path, "status."):
		return true
	case path == "metadata.creationTimestamp",
		path == "metadata.deletionGracePeriodSeconds",
		path == "metadata.deletionTimestamp",
		path == "metadata.generation",
		path == "metadata.resourceVersion",
		path == "metadata.selfLink",
		path == "metadata.uid",
		strings.HasPrefix(path, "metadata.managedFields"),
		strings.HasPrefix(path, "metadata.annotations.kubectl.kubernetes.io/last-applied-configuration"),
		path == "metadata.annotations.meta.helm.sh/release-name",
		path == "metadata.annotations.meta.helm.sh/release-namespace",
		path == "metadata.labels.app.kubernetes.io/managed-by":
		return true
	default:
		return false
	}
}
