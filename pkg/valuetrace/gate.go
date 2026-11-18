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

import "helm.sh/helm/v4/pkg/gates"

// Gate is the feature gate that enables value provenance tracing. It is
// disabled by default; when the environment variable is unset the merge
// pipeline behaves exactly as it did before tracing existed.
const Gate gates.Gate = "HELM_EXPERIMENTAL_VALUES_TRACE"

// Enabled reports whether provenance tracing is enabled for this process.
func Enabled() bool {
	return Gate.IsEnabled()
}
