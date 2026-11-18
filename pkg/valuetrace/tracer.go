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
	"cmp"
	"fmt"
	"maps"
	"reflect"
	"slices"

	chartapi "helm.sh/helm/v4/pkg/chart"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
)

// leafValue is one value a source contributed at a leaf path.
type leafValue struct {
	value any
	null  bool
}

// Tracer observes the existing merge implementations and records which
// source supplied each key path.
//
// A Tracer only wraps loader.MergeMaps. It never merges values itself and
// it never mutates the values passed to it: the merged map returned is the
// exact map loader.MergeMaps would have returned without tracing.
//
// Internally every source's leaf contributions are collected independently.
// The final attribution is decided once, in Snapshot, by replaying the same
// precedence rules as loader.MergeMaps against the actual merged map, which
// keeps provenance consistent with the merge regardless of call ordering.
type Tracer struct {
	trace *Trace
	// contributions maps source -> path -> contributed leaf.
	contributions map[Source]map[string]leafValue
	// order records source registration order for deterministic output.
	order []Source
}

// NewTracer returns a tracer ready to observe merges.
func NewTracer() *Tracer {
	return &Tracer{
		trace:         newTrace(),
		contributions: map[Source]map[string]leafValue{},
	}
}

// RegisterSource records a source before it participates in a merge.
// Registered sources that never supply a value are reported as not
// participating instead of being silently omitted.
func (t *Tracer) RegisterSource(src Source) {
	if _, seen := t.contributions[src]; seen {
		return
	}
	t.contributions[src] = map[string]leafValue{}
	t.order = append(t.order, src)
	t.trace.sources = append(t.trace.sources, src)
}

// Merge merges current with incoming using the repository's loader.MergeMaps
// and records the provenance of every affected key path.
//
// The returned map is byte-for-byte equivalent to loader.MergeMaps(current,
// incoming).
func (t *Tracer) Merge(current, incoming map[string]any, src Source) map[string]any {
	t.RegisterSource(src)
	if len(incoming) > 0 {
		t.trace.participating[src] = true
		t.collectLeaves(incoming, src)
	}
	return loader.MergeMaps(current, incoming)
}

// ObserveStrval records a --set* assignment that strvals wrote into the
// destination map in place.
//
// written is the map produced by parsing the same raw assignment (for
// example via strvals.Parse); only leaf paths present in written are
// attributed to src, so unrelated keys keep their provenance.
func (t *Tracer) ObserveStrval(written map[string]any, src Source) {
	t.RegisterSource(src)
	if len(written) == 0 {
		return
	}
	t.trace.participating[src] = true
	t.collectLeaves(written, src)
}

// ObserveParsed records the leaves produced by parsing a --set* assignment.
// A parse error while building the provenance view must not affect the
// already completed merge: the source is still registered (so it can be
// reported) and a warning is recorded, then tracing degrades gracefully.
func (t *Tracer) ObserveParsed(written map[string]any, src Source, parseErr error) {
	t.RegisterSource(src)
	if parseErr != nil {
		t.warn("provenance unavailable for %s: %s", src.String(), parseErr)
		return
	}
	t.ObserveStrval(written, src)
}

// RecordParentDefaults observes the root chart's values.yaml as the parent
// values layer (lower precedence than user supplied files and CLI sets).
func (t *Tracer) RecordParentDefaults(chartDefaults map[string]any) {
	src := Source{Layer: LayerParent, Kind: KindChart, Name: ""}
	t.RegisterSource(src)
	if len(chartDefaults) > 0 {
		t.trace.participating[src] = true
		t.collectLeaves(chartDefaults, src)
	}
}

// collectLeaves stores every leaf carried by incoming against src.
func (t *Tracer) collectLeaves(incoming map[string]any, src Source) {
	bucket := t.contributions[src]
	for _, path := range deepLeaves("", incoming) {
		val, ok := lookup(incoming, path)
		if !ok {
			continue
		}
		bucket[path] = leafValue{value: val, null: val == nil}
	}
}

// Snapshot finalizes the trace.
//
// mergedValues is the exact map returned by the observed CLI merges (before
// coalescing). renderedValues is the coalesced values actually used for
// rendering; it is used only to decide which chart default leaves survive.
// root is the chart tree whose values.yaml blocks supply the chart-default
// layer for leaves no higher precedence source claimed. renderedValues may
// equal mergedValues when coalescing output is not available.
func (t *Tracer) Snapshot(root chartapi.Charter, mergedValues, renderedValues map[string]any) *Trace {
	out := &Trace{
		origins:       map[string]Origin{},
		sources:       slices.Clone(t.trace.sources),
		participating: maps.Clone(t.trace.participating),
		warnings:      slices.Clone(t.trace.warnings),
	}

	// Replay precedence: higher layer, then higher index within a layer,
	// wins. Iterate sources in reverse precedence and keep the first source
	// whose contributed value equals the value now present in finalValues.
	sortedSources := t.sortedSources()

	// All leaf paths any source ever wrote.
	allPaths := map[string]bool{}
	for _, bucket := range t.contributions {
		for path := range bucket {
			allPaths[path] = true
		}
	}

	for path := range allPaths {
		finalVal, present := lookup(mergedValues, path)
		winner, winnerNull, found := t.attributePath(path, sortedSources, finalVal, present)
		if !found {
			// The path was overridden in a way no single leaf contribution
			// matches (for example removed by explicit null during
			// coalescing). Attribute to the highest precedence source that
			// wrote the path so provenance is never silently missing.
			winner = t.highestWriter(path, sortedSources)
			// Only mark explicit null when that winning source actually
			// wrote a null; a path merely absent from the partial map is an
			// inheritance case, not a null.
			if leaf, ok := t.contributions[winner][path]; ok {
				winnerNull = leaf.null
			}
		}
		out.origins[path] = Origin{
			Path:         path,
			Source:       winner,
			ExplicitNull: winnerNull,
		}
	}

	// Conflicts are computed from raw contributions before final value
	// matching, so overridden disagreements are still reported.
	out.conflicts = t.computeConflicts(allPaths)

	// Remaining leaves come from values.yaml blocks inside the chart tree.
	if root != nil {
		if err := t.attributeChartTree(root, "", renderedValues, out); err != nil {
			t.warn("chart default provenance incomplete: %s", err)
		}
	}

	return out
}

// sortedSources orders sources by precedence (lowest first).
func (t *Tracer) sortedSources() []Source {
	srcs := slices.Clone(t.order)
	slices.SortFunc(srcs, func(a, b Source) int {
		if c := cmp.Compare(int(a.Layer), int(b.Layer)); c != 0 {
			return c
		}
		return cmp.Compare(a.Index, b.Index)
	})
	return srcs
}

// attributePath finds the winning source for path by matching contributed
// values against the final merged value, scanning from highest to lowest
// precedence.
func (t *Tracer) attributePath(path string, lowToHigh []Source, finalVal any, present bool) (Source, bool, bool) {
	for i := len(lowToHigh) - 1; i >= 0; i-- {
		src := lowToHigh[i]
		leaf, ok := t.contributions[src][path]
		if !ok {
			continue
		}
		switch {
		case leaf.null && present && finalVal == nil:
			// Explicit null preserved in the merged map (MergeMaps keeps
			// null keys until coalescing removes them later).
			return src, true, true
		case leaf.null && !present:
			return src, true, true
		case present && reflect.DeepEqual(leaf.value, finalVal):
			return src, false, true
		}
	}
	return Source{}, false, false
}

// highestWriter returns the highest precedence source that wrote path.
func (t *Tracer) highestWriter(path string, lowToHigh []Source) Source {
	for i := len(lowToHigh) - 1; i >= 0; i-- {
		src := lowToHigh[i]
		if _, ok := t.contributions[src][path]; ok {
			return src
		}
	}
	return Source{}
}

// computeConflicts reports same layer disagreements and type mismatches.
func (t *Tracer) computeConflicts(allPaths map[string]bool) []Conflict {
	var conflicts []Conflict
	lowToHigh := t.sortedSources()

	for path := range allPaths {
		// Writers of this path from lowest to highest precedence.
		var writers []Source
		var leafs []leafValue
		for _, src := range lowToHigh {
			if leaf, ok := t.contributions[src][path]; ok {
				writers = append(writers, src)
				leafs = append(leafs, leaf)
			}
		}
		if len(writers) < 2 {
			continue
		}

		// Adjacent writer pairs reveal every disagreement, including ones
		// where an intermediate source restored an earlier value.
		for i := 1; i < len(writers); i++ {
			loser, winner := writers[i-1], writers[i]
			loserLeaf, winnerLeaf := leafs[i-1], leafs[i]
			if loser == winner {
				continue
			}
			// Equal contributions agree, so there is no disagreement even
			// though two sources wrote the same path.
			if reflect.DeepEqual(loserLeaf.value, winnerLeaf.value) &&
				loserLeaf.null == winnerLeaf.null {
				continue
			}

			conflict := Conflict{Path: path, Winner: winner, Loser: loser}
			switch {
			case winner.Layer == loser.Layer && isTypeConflict(loserLeaf.value, winnerLeaf.value):
				// Same layer disagreement with incompatible types: surface
				// the type mismatch (with path and both types) as well as
				// the same layer conflict so neither signal is lost.
				conflict.Kind = ConflictType
				conflict.WinnerType = valueTypeName(winnerLeaf.value)
				conflict.LoserType = valueTypeName(loserLeaf.value)
				conflicts = append(conflicts, conflict)

				sameLayer := Conflict{Path: path, Kind: ConflictSameLayer, Winner: winner, Loser: loser}
				conflicts = append(conflicts, sameLayer)
			case winner.Layer == loser.Layer:
				conflict.Kind = ConflictSameLayer
				conflicts = append(conflicts, conflict)
			case isTypeConflict(loserLeaf.value, winnerLeaf.value):
				conflict.Kind = ConflictType
				conflict.WinnerType = valueTypeName(winnerLeaf.value)
				conflict.LoserType = valueTypeName(loserLeaf.value)
				conflicts = append(conflicts, conflict)
			}
		}
	}

	// Structural type conflicts where a map subtree is replaced by a scalar
	// or array: the winning source writes the parent path while a lower
	// precedence source only wrote leaves beneath it (or vice versa).
	for _, structural := range t.computeStructuralConflicts(lowToHigh) {
		if !containsConflict(conflicts, structural) {
			conflicts = append(conflicts, structural)
		}
	}

	slices.SortFunc(conflicts, func(a, b Conflict) int {
		if c := cmp.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Kind, b.Kind); c != 0 {
			return c
		}
		return cmp.Compare(a.Winner.String(), b.Winner.String())
	})
	return conflicts
}

// containsConflict reports whether an equivalent conflict already exists.
func containsConflict(conflicts []Conflict, want Conflict) bool {
	for _, conflict := range conflicts {
		if conflict.Path == want.Path && conflict.Kind == want.Kind &&
			conflict.Winner == want.Winner && conflict.Loser == want.Loser {
			return true
		}
	}
	return false
}

// computeStructuralConflicts detects map-vs-nonmap replacements that the
// per-leaf comparison cannot see because the competing sources write
// different path sets (for example "meta.a" versus "meta").
func (t *Tracer) computeStructuralConflicts(lowToHigh []Source) []Conflict {
	var conflicts []Conflict

	// Build, per source, the set of leaf paths it wrote.
	pathsBySource := map[Source][]string{}
	for src, bucket := range t.contributions {
		paths := make([]string, 0, len(bucket))
		for path := range bucket {
			paths = append(paths, path)
		}
		slices.Sort(paths)
		pathsBySource[src] = paths
	}

	for i := 1; i < len(lowToHigh); i++ {
		loser, winner := lowToHigh[i-1], lowToHigh[i]
		if loser == winner {
			continue
		}
		for winnerPath, winnerLeaf := range t.contributions[winner] {
			if winnerLeaf.null {
				continue
			}
			for loserPath, loserLeaf := range t.contributions[loser] {
				if !isStructuralOverlap(winnerPath, loserPath) {
					continue
				}
				if !isTypeConflict(loserLeaf.value, winnerLeaf.value) {
					continue
				}
				conflictPath := winnerPath
				if len(loserPath) < len(conflictPath) {
					conflictPath = loserPath
				}
				conflicts = append(conflicts, Conflict{
					Path:       conflictPath,
					Kind:       ConflictType,
					Winner:     winner,
					Loser:      loser,
					WinnerType: valueTypeName(winnerLeaf.value),
					LoserType:  valueTypeName(loserLeaf.value),
				})
			}
		}
	}
	return conflicts
}

// isStructuralOverlap reports whether two dotted paths are the same or one
// is an ancestor of the other, which is the shape a map replacement takes.
func isStructuralOverlap(a, b string) bool {
	if a == b {
		return true
	}
	short, long := a, b
	if len(short) > len(long) {
		short, long = long, short
	}
	return len(long) > len(short) && long[:len(short)+1] == short+"."
}

// attributeChartTree walks the chart tree through the same accessor the
// coalescer uses, attributing unclaimed leaves to each chart's values.yaml.
func (t *Tracer) attributeChartTree(chrt chartapi.Charter, prefix string, finalValues map[string]any, out *Trace) error {
	accessor, err := chartapi.NewAccessor(chrt)
	if err != nil {
		return err
	}
	chartName := accessor.Name()
	src := Source{Layer: LayerChartDefault, Kind: KindChart, Name: chartName}

	for _, path := range deepLeaves(prefix, accessor.Values()) {
		if _, claimed := out.origins[path]; claimed {
			continue
		}
		if _, exists := lookup(finalValues, path); !exists {
			continue
		}
		out.origins[path] = Origin{Path: path, Source: src}
		out.participating[src] = true
	}

	for _, dep := range accessor.Dependencies() {
		depAccessor, err := chartapi.NewAccessor(dep)
		if err != nil {
			return err
		}
		depPrefix := joinPath(prefix, depAccessor.Name())
		if err := t.attributeChartTree(dep, depPrefix, finalValues, out); err != nil {
			return err
		}
	}
	return nil
}

// warn records a non fatal tracing problem. Warnings never affect merging.
func (t *Tracer) warn(format string, args ...any) {
	t.trace.warnings = append(t.trace.warnings, fmt.Sprintf(format, args...))
}

func joinPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func valueTypeName(v any) string {
	if v == nil {
		return "null"
	}
	return fmt.Sprintf("%T", v)
}

// isTypeConflict reports whether replacing old with val is a type conflict.
// Two maps never conflict (they merge); a map on either side, or values of
// different concrete types, do. Equal concrete types (including one array
// replacing another) are ordinary precedence based replacement.
func isTypeConflict(old, val any) bool {
	if old == nil || val == nil {
		return false
	}
	_, oldIsMap := old.(map[string]any)
	_, valIsMap := val.(map[string]any)
	if oldIsMap || valIsMap {
		return true
	}
	return valueTypeName(old) != valueTypeName(val)
}
