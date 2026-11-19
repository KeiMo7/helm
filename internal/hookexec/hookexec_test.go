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

package hookexec

import (
	"fmt"

	release "helm.sh/helm/v4/pkg/release/v1"
)

// testHook builds a hook with a manifest carrying the given namespace and
// annotations, mimicking what SortManifests produces.
func testHook(name, kind, namespace string, weight int, annotations map[string]string, events ...release.HookEvent) *release.Hook {
	ann := ""
	for k, v := range annotations {
		ann += fmt.Sprintf("    %s: %q\n", k, v)
	}
	ns := ""
	if namespace != "" {
		ns = fmt.Sprintf("  namespace: %s\n", namespace)
	}
	manifest := fmt.Sprintf(`apiVersion: v1
kind: %s
metadata:
  name: %s
%s  annotations:
%s`, kind, name, ns, ann)
	return &release.Hook{
		Name:     name,
		Kind:     kind,
		Path:     "templates/hooks/" + name + ".yaml",
		Manifest: manifest,
		Weight:   weight,
		Events:   events,
	}
}

func stepNames(plan *Plan) []string {
	names := make([]string, 0, len(plan.Steps))
	for _, s := range plan.Steps {
		names = append(names, s.Hook.Name)
	}
	return names
}
