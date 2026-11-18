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

import "fmt"

// Layer identifies one of the four value precedence layers.
//
// The numerical order matches merge precedence: larger numbers win.
type Layer int

const (
	// LayerChartDefault is the values.yaml shipped inside a (sub)chart.
	LayerChartDefault Layer = iota
	// LayerParent is the values block passed into a subchart by its parent
	// chart, i.e. the parent chart's values.yaml entry named after the
	// dependency.
	LayerParent
	// LayerValueFile is a -f/--values override file, in command line order.
	LayerValueFile
	// LayerCLI is a --set/--set-string/--set-file/--set-json/--set-literal
	// command line assignment.
	LayerCLI
)

// String returns a stable, human readable name for the layer.
func (l Layer) String() string {
	switch l {
	case LayerChartDefault:
		return "chart-default"
	case LayerParent:
		return "parent-values"
	case LayerValueFile:
		return "value-file"
	case LayerCLI:
		return "cli-set"
	default:
		return fmt.Sprintf("layer-%d", int(l))
	}
}

// Kind describes what kind of source supplied a value.
type Kind string

const (
	// KindChart is a values.yaml inside a chart.
	KindChart Kind = "chart"
	// KindFile is a -f/--values file.
	KindFile Kind = "file"
	// KindSet is one of the --set* command line assignments.
	KindSet Kind = "set"
)

// Source identifies one concrete value input.
type Source struct {
	// Layer is the precedence layer the source belongs to.
	Layer Layer `json:"layer"`
	// Kind is the input kind (chart, file or set).
	Kind Kind `json:"kind"`
	// Name identifies the source within its layer:
	//   - chart: dotted chart path, e.g. "myapp.web" or "" for the root chart
	//   - file:  the -f path as given on the command line
	//   - set:   the raw --set* argument
	Name string `json:"name"`
	// Index is the zero based position of the source inside its layer
	// (the order the files/assignments appeared on the command line).
	Index int `json:"index"`
}

// String renders the source as "layer:kind:name" for reports.
func (s Source) String() string {
	return fmt.Sprintf("%s:%s:%s", s.Layer, s.Kind, s.Name)
}

// ConflictKind classifies a conflict recorded during tracing.
type ConflictKind string

const (
	// ConflictSameLayer means two distinct sources in the same layer both
	// wrote the same key path with different values.
	ConflictSameLayer ConflictKind = "same-layer"
	// ConflictType means a higher precedence value could not be merged with
	// the lower precedence value because their types were incompatible
	// (for example a map replaced by a string, or a string replaced by an
	// array). The higher precedence value wins, matching existing merge
	// behaviour.
	ConflictType ConflictKind = "type-mismatch"
)

// Conflict describes a key path where two sources disagreed.
type Conflict struct {
	// Path is the dotted key path, for example "web.replicas".
	Path string `json:"path"`
	// Kind classifies the conflict.
	Kind ConflictKind `json:"kind"`
	// Winner is the source that supplied the final value.
	Winner Source `json:"winner"`
	// Loser is the source whose value was overridden.
	Loser Source `json:"loser"`
	// WinnerType and LoserType are the Go type names of the competing
	// values, populated for type conflicts.
	WinnerType string `json:"winnerType,omitempty"`
	LoserType  string `json:"loserType,omitempty"`
}

// Origin records where the final value of one key path came from.
type Origin struct {
	// Path is the dotted key path.
	Path string `json:"path"`
	// Source supplied the final value at this path.
	Source Source `json:"source"`
	// ExplicitNull is true when the source wrote an explicit null/empty
	// value. Explicit null means "clear this key"; an absent key (the
	// previous layer simply being inherited) never produces an origin.
	ExplicitNull bool `json:"explicitNull,omitempty"`
}

// Trace is the independent provenance structure returned alongside merged
// values. It is never embedded into the values map.
type Trace struct {
	// origins maps dotted key path to the winning origin.
	origins map[string]Origin
	// conflicts is kept sorted for deterministic output.
	conflicts []Conflict
	// sources is every source that participated, in registration order.
	sources []Source
	// participating tracks which registered sources actually supplied at
	// least one value.
	participating map[Source]bool
	// warnings are non fatal problems encountered while tracing.
	warnings []string
}

// newTrace builds an empty trace.
func newTrace() *Trace {
	return &Trace{
		origins:       map[string]Origin{},
		participating: map[Source]bool{},
	}
}
