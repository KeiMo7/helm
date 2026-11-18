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

import "strings"

// lookup follows a dotted path through nested maps and returns the value.
// It returns false when any segment is missing or is not a map.
func lookup(m map[string]any, path string) (any, bool) {
	var current any = m
	for _, segment := range strings.Split(path, ".") {
		table, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = table[segment]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

// deepLeaves returns every dotted leaf path contained in m. Null values are
// leaves too.
func deepLeaves(prefix string, m map[string]any) []string {
	var paths []string
	for _, key := range sortedKeys(m) {
		path := joinPath(prefix, key)
		if sub, ok := m[key].(map[string]any); ok {
			paths = append(paths, deepLeaves(path, sub)...)
			continue
		}
		paths = append(paths, path)
	}
	return paths
}
