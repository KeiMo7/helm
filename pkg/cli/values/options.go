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

package values

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"helm.sh/helm/v4/pkg/chart/v2/loader"
	"helm.sh/helm/v4/pkg/getter"
	"helm.sh/helm/v4/pkg/strvals"
	"helm.sh/helm/v4/pkg/valuetrace"
)

// Options captures the different ways to specify values
type Options struct {
	ValueFiles    []string // -f/--values
	StringValues  []string // --set-string
	Values        []string // --set
	FileValues    []string // --set-file
	JSONValues    []string // --set-json
	LiteralValues []string // --set-literal
}

// MergeValues merges values from files specified via -f/--values and directly
// via --set-json, --set, --set-string, or --set-file, marshaling them to YAML
func (opts *Options) MergeValues(p getter.Providers) (map[string]any, error) {
	return opts.mergeValuesWith(p, nil)
}

// MergeValuesTraced mirrors MergeValues but additionally records the
// provenance of every merged key path.
//
// The returned values map is produced by the exact same loader.MergeMaps and
// strvals calls as MergeValues, so given identical inputs the merged result
// is byte-for-byte identical. The trace is returned as an independent
// structure and never embedded into the values.
//
// Any failure inside the tracing machinery is non fatal: tracing degrades to
// a nil trace with a recorded warning while the plain merge result is still
// returned.
func (opts *Options) MergeValuesTraced(p getter.Providers) (map[string]any, *valuetrace.Tracer, error) {
	tracer := valuetrace.NewTracer()
	base, err := opts.mergeValuesWith(p, tracer)
	if err != nil {
		return base, nil, err
	}
	return base, tracer, nil
}

// mergeValuesWith is MergeValues with every merge step observed by tracer.
// When tracer is nil it behaves exactly like MergeValues.
func (opts *Options) mergeValuesWith(p getter.Providers, tracer *valuetrace.Tracer) (map[string]any, error) {
	base := map[string]any{}

	merge := func(incoming map[string]any, src valuetrace.Source) {
		if tracer == nil {
			base = loader.MergeMaps(base, incoming)
			return
		}
		base = tracer.Merge(base, incoming, src)
	}

	// User specified a values files via -f/--values
	for fileIndex, filePath := range opts.ValueFiles {
		raw, err := readFile(filePath, p)
		if err != nil {
			return nil, err
		}
		currentMap, err := loader.LoadValues(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("failed to parse %s: %w", filePath, err)
		}
		src := valuetrace.Source{
			Layer: valuetrace.LayerValueFile,
			Kind:  valuetrace.KindFile,
			Name:  filePath,
			Index: fileIndex,
		}
		if tracer != nil {
			tracer.RegisterSource(src)
		}
		// Merge with the previous map
		merge(currentMap, src)
	}

	// User specified a value via --set-json
	for setIndex, value := range opts.JSONValues {
		src := valuetrace.Source{
			Layer: valuetrace.LayerCLI,
			Kind:  valuetrace.KindSet,
			Name:  value,
			Index: setIndex,
		}
		trimmedValue := strings.TrimSpace(value)
		if trimmedValue != "" && trimmedValue[0] == '{' {
			// If value is JSON object format, parse it as map
			var jsonMap map[string]any
			if err := json.Unmarshal([]byte(trimmedValue), &jsonMap); err != nil {
				return nil, fmt.Errorf("failed parsing --set-json data JSON: %s", value)
			}
			if tracer != nil {
				tracer.RegisterSource(src)
			}
			merge(jsonMap, src)
		} else {
			// Otherwise, parse it as key=value format
			if err := strvals.ParseJSON(value, base); err != nil {
				return nil, fmt.Errorf("failed parsing --set-json data %s", value)
			}
			if tracer != nil {
				written, perr := strvals.ParseJSONIntoMap(value)
				tracer.ObserveParsed(written, src, perr)
			}
		}
	}

	// User specified a value via --set
	for setIndex, value := range opts.Values {
		if err := strvals.ParseInto(value, base); err != nil {
			return nil, fmt.Errorf("failed parsing --set data: %w", err)
		}
		if tracer != nil {
			written, perr := strvals.Parse(value)
			tracer.ObserveParsed(written, valuetrace.Source{
				Layer: valuetrace.LayerCLI,
				Kind:  valuetrace.KindSet,
				Name:  value,
				Index: setIndex,
			}, perr)
		}
	}

	// User specified a value via --set-string
	for setIndex, value := range opts.StringValues {
		if err := strvals.ParseIntoString(value, base); err != nil {
			return nil, fmt.Errorf("failed parsing --set-string data: %w", err)
		}
		if tracer != nil {
			written, perr := strvals.ParseString(value)
			tracer.ObserveParsed(written, valuetrace.Source{
				Layer: valuetrace.LayerCLI,
				Kind:  valuetrace.KindSet,
				Name:  value,
				Index: setIndex,
			}, perr)
		}
	}

	// User specified a value via --set-file
	for setIndex, value := range opts.FileValues {
		reader := func(rs []rune) (any, error) {
			bytes, err := readFile(string(rs), p)
			if err != nil {
				return nil, err
			}
			return string(bytes), err
		}
		if err := strvals.ParseIntoFile(value, base, reader); err != nil {
			return nil, fmt.Errorf("failed parsing --set-file data: %w", err)
		}
		if tracer != nil {
			written, perr := strvals.ParseFile(value, func(rs []rune) (any, error) {
				return reader(rs)
			})
			tracer.ObserveParsed(written, valuetrace.Source{
				Layer: valuetrace.LayerCLI,
				Kind:  valuetrace.KindSet,
				Name:  value,
				Index: setIndex,
			}, perr)
		}
	}

	// User specified a value via --set-literal
	for setIndex, value := range opts.LiteralValues {
		if err := strvals.ParseLiteralInto(value, base); err != nil {
			return nil, fmt.Errorf("failed parsing --set-literal data: %w", err)
		}
		if tracer != nil {
			written, perr := strvals.ParseLiteral(value)
			tracer.ObserveParsed(written, valuetrace.Source{
				Layer: valuetrace.LayerCLI,
				Kind:  valuetrace.KindSet,
				Name:  value,
				Index: setIndex,
			}, perr)
		}
	}

	return base, nil
}

// readFile load a file from stdin, the local directory, or a remote file with a url.
func readFile(filePath string, p getter.Providers) ([]byte, error) {
	if strings.TrimSpace(filePath) == "-" {
		return io.ReadAll(os.Stdin)
	}
	u, err := url.Parse(filePath)
	if err != nil {
		return nil, err
	}

	// FIXME: maybe someone handle other protocols like ftp.
	g, err := p.ByScheme(u.Scheme)
	if err != nil {
		return os.ReadFile(filePath)
	}
	data, err := g.Get(filePath, getter.WithURL(filePath))
	if err != nil {
		return nil, err
	}
	return data.Bytes(), nil
}
