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
	"slices"
	"strings"
)

// SourceStatus describes whether a registered source participated.
type SourceStatus struct {
	Source       Source `json:"source"`
	Participated bool   `json:"participated"`
}

// Origins returns every key path origin ordered by path, so two runs over
// the same input produce identical output.
func (t *Trace) Origins() []Origin {
	if t == nil {
		return nil
	}
	out := make([]Origin, 0, len(t.origins))
	for _, origin := range t.origins {
		out = append(out, origin)
	}
	slices.SortFunc(out, func(a, b Origin) int {
		return cmp.Compare(a.Path, b.Path)
	})
	return out
}

// OriginAt returns the origin of a dotted key path and whether it exists.
func (t *Trace) OriginAt(path string) (Origin, bool) {
	if t == nil {
		return Origin{}, false
	}
	origin, ok := t.origins[path]
	return origin, ok
}

// Conflicts returns all recorded conflicts in deterministic order.
func (t *Trace) Conflicts() []Conflict {
	if t == nil {
		return nil
	}
	return slices.Clone(t.conflicts)
}

// Warnings returns non fatal tracing problems. The merge result itself is
// always unaffected when a warning is present.
func (t *Trace) Warnings() []string {
	if t == nil {
		return nil
	}
	return slices.Clone(t.warnings)
}

// SourceStatuses returns every registered source with an explicit
// participated flag. Sources that never supplied a value are reported as not
// participating rather than silently omitted.
func (t *Trace) SourceStatuses() []SourceStatus {
	if t == nil {
		return nil
	}
	out := make([]SourceStatus, 0, len(t.sources))
	for _, src := range t.sources {
		out = append(out, SourceStatus{
			Source:       src,
			Participated: t.participating[src],
		})
	}
	return out
}

// Report renders a deterministic human readable summary: the path to source
// mapping, followed by conflicts and non participating sources.
func (t *Trace) Report() string {
	if t == nil {
		return ""
	}
	var b strings.Builder

	b.WriteString("VALUE PROVENANCE\n")
	for _, origin := range t.Origins() {
		nullMarker := ""
		if origin.ExplicitNull {
			nullMarker = " (explicit null)"
		}
		b.WriteString("  " + origin.Path + " <- " + origin.Source.String() + nullMarker + "\n")
	}

	if statuses := t.SourceStatuses(); len(statuses) > 0 {
		b.WriteString("\nSOURCES\n")
		for _, status := range statuses {
			state := "participated"
			if !status.Participated {
				state = "not participated"
			}
			b.WriteString("  " + status.Source.String() + ": " + state + "\n")
		}
	}

	if len(t.conflicts) > 0 {
		b.WriteString("\nCONFLICTS\n")
		for _, conflict := range t.conflicts {
			line := "  " + string(conflict.Kind) + " at " + conflict.Path +
				": winner=" + conflict.Winner.String() +
				" loser=" + conflict.Loser.String()
			if conflict.Kind == ConflictType {
				line += " (winner type " + conflict.WinnerType +
					", loser type " + conflict.LoserType + ")"
			}
			b.WriteString(line + "\n")
		}
	}

	if len(t.warnings) > 0 {
		b.WriteString("\nWARNINGS\n")
		for _, warning := range t.warnings {
			b.WriteString("  " + warning + "\n")
		}
	}

	return b.String()
}
