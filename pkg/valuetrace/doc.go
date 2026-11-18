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

/*
Package valuetrace records the provenance of coalesced values: which layer
(subchart defaults, parent values, value files or command line overrides)
supplied the final value of every key path.

All tracing functionality is opt in. When disabled, values are merged
exactly as before and no trace structures are allocated. The trace is kept
in a structure separate from the values map so that the rendering context is
never polluted.

The merge semantics themselves live in pkg/chart/v2/loader and
pkg/chart/common/util. This package reuses those implementations and only
observes the inputs and outputs of each merge step; it never performs a
merge of its own.
*/
package valuetrace
