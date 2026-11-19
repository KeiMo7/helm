package engine

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

const defaultTemplateTraceDepth = 16

// TraceOptions controls opt-in template error tracing.
//
// Templates may be exact resource names, helper definition names, or glob
// patterns understood by path.Match. When a helper is selected, resources that
// can reach that helper through a literal include or template call are traced.
type TraceOptions struct {
	// Templates selects the templates to trace.
	Templates []string

	// MaxDepth limits the number of trace frames included in an error. A
	// non-positive value uses the default.
	MaxDepth int
}

// RenderTrace contains the source information needed to map rendered YAML
// errors back to chart templates.
type RenderTrace struct {
	templates map[string]renderable
	files     map[string]*traceFile
	graph     *templateGraph
	graphOnce sync.Once
}

type traceFile struct {
	root string
}

type tracePlan struct {
	roots    map[string]bool
	maxDepth int
}

type traceFrame struct {
	kind     string
	target   string
	location string
	template string
	detail   string
}

type sourcePosition struct {
	file   string
	line   int
	column int
}

type includeRef struct {
	from   string
	target string
	line   int
	column int
}

type templateGraph struct {
	templates map[string]renderable
	defines   map[string][]sourcePosition
	includes  map[string][]includeRef
}

var (
	includeLiteralRE  = regexp.MustCompile(`(?m)\{\{-?\s*include\s+(?:"([^"\n]+)"|` + "`([^`\n]+)`" + `)`)
	templateLiteralRE = regexp.MustCompile(`(?m)\{\{-?\s*template\s+(?:"([^"\n]+)"|` + "`([^`\n]+)`" + `)`)
	defineLiteralRE   = regexp.MustCompile(`(?m)\{\{-?\s*define\s+(?:"([^"\n]+)"|` + "`([^`\n]+)`" + `)`)
	actionLiteralRE   = regexp.MustCompile(`^(include|template)\s+(?:"([^"]+)"|` + "`([^`]+)`" + `)`)
	yamlLineRE        = regexp.MustCompile(`(?i)\bline\s*[:=]?\s*(\d+)`)
	yamlFileRE        = regexp.MustCompile(`YAML parse error on ([^:]+):`)
	tplFragmentRE     = regexp.MustCompile(`\{\{.*?\}\}`)
	missingKeyRE      = regexp.MustCompile(`map has no entry for key "([^"]+)"`)
	nilKeyRE          = regexp.MustCompile(`nil pointer evaluating .*\.([[:alnum:]_-]+)`)
	fieldKeyRE        = regexp.MustCompile(`can't evaluate field ([[:alnum:]_-]+)`)
	indexKeyRE        = regexp.MustCompile(`index out of range: \[?([0-9]+)\]?`)
	noTemplateRE      = regexp.MustCompile(`no template "([^"]+)"`)
	tplArgumentRE     = regexp.MustCompile(`(<tpl\s+)(?:"(?:\\.|[^"])*"|` + "`[^`]*`" + `)`)
	tplExecMessageRE  = regexp.MustCompile(`(error during tpl function execution for )"(?:\\.|[^"])*"`)
	tplParseMessageRE = regexp.MustCompile(`(cannot parse template )"(?:\\.|[^"])*"`)
)

func (opts *TraceOptions) maxDepth() int {
	if opts == nil || opts.MaxDepth <= 0 {
		return defaultTemplateTraceDepth
	}
	if opts.MaxDepth < 4 {
		return 4
	}
	return opts.MaxDepth
}

func newTracePlan(opts *TraceOptions, templates map[string]renderable) *tracePlan {
	if opts == nil || len(opts.Templates) == 0 || len(templates) == 0 {
		return nil
	}

	var matchedRoots []string
	for name := range templates {
		if isPartialTemplate(name) {
			continue
		}
		if matchesAnyTemplate(opts.Templates, name) {
			matchedRoots = append(matchedRoots, name)
		}
	}

	if len(matchedRoots) == 0 && maySelectHelper(opts.Templates) {
		graph := buildTemplateGraph(templates)
		for name := range templates {
			if isPartialTemplate(name) {
				continue
			}
			if graph.reachesSelectedTemplate(name, opts.Templates) {
				matchedRoots = append(matchedRoots, name)
			}
		}
	}

	if len(matchedRoots) == 0 {
		return nil
	}
	roots := make(map[string]bool, len(matchedRoots))
	for _, name := range matchedRoots {
		roots[name] = true
	}
	return &tracePlan{roots: roots, maxDepth: opts.maxDepth()}
}

func maySelectHelper(patterns []string) bool {
	for _, pattern := range patterns {
		if strings.ContainsAny(pattern, "*?[") {
			return true
		}
		base := path.Base(pattern)
		if strings.HasPrefix(base, "_") || !strings.Contains(pattern, "/templates/") {
			return true
		}
	}
	return false
}

func isPartialTemplate(name string) bool {
	return strings.HasPrefix(path.Base(name), "_")
}

func matchesAnyTemplate(patterns []string, name string) bool {
	for _, pattern := range patterns {
		if matchTemplatePattern(pattern, name) {
			return true
		}
	}
	return false
}

func matchTemplatePattern(pattern, name string) bool {
	if pattern == "" {
		return false
	}
	pattern = strings.TrimPrefix(pattern, "./")
	if pattern == name {
		return true
	}
	if len(name) > len(pattern) && name[len(name)-len(pattern)-1] == '/' &&
		name[len(name)-len(pattern):] == pattern {
		return true
	}
	if !strings.ContainsAny(pattern, "*?[") {
		return false
	}
	if matched, err := path.Match(pattern, name); err == nil && matched {
		return true
	}
	matched, err := path.Match(pattern, path.Base(name))
	return err == nil && matched
}

func buildTemplateGraph(templates map[string]renderable) *templateGraph {
	g := &templateGraph{
		templates: templates,
		defines:   make(map[string][]sourcePosition),
		includes:  make(map[string][]includeRef),
	}

	for file, tpl := range templates {
		for _, match := range defineLiteralRE.FindAllStringSubmatchIndex(tpl.tpl, -1) {
			name := regexCapture(tpl.tpl, match, 1, 2)
			if name == "" {
				continue
			}
			line, column := lineColumn(tpl.tpl, match[0])
			g.defines[name] = append(g.defines[name], sourcePosition{file: file, line: line, column: column})
		}
		for _, re := range []*regexp.Regexp{includeLiteralRE, templateLiteralRE} {
			for _, match := range re.FindAllStringSubmatchIndex(tpl.tpl, -1) {
				name := regexCapture(tpl.tpl, match, 1, 2)
				if name == "" {
					continue
				}
				line, column := lineColumn(tpl.tpl, match[0])
				g.includes[file] = append(g.includes[file], includeRef{
					from:   file,
					target: name,
					line:   line,
					column: column,
				})
			}
		}
	}

	return g
}

func regexCapture(source string, match []int, groups ...int) string {
	for _, group := range groups {
		start := match[group*2]
		end := match[group*2+1]
		if start >= 0 && end >= 0 {
			return source[start:end]
		}
	}
	return ""
}

func lineColumn(source string, offset int) (int, int) {
	if offset < 0 {
		return 0, 0
	}
	if offset > len(source) {
		offset = len(source)
	}
	line := strings.Count(source[:offset], "\n") + 1
	lastNewline := strings.LastIndexByte(source[:offset], '\n')
	column := offset - lastNewline
	return line, column
}

func (g *templateGraph) ownerFiles(name string) []string {
	seen := make(map[string]bool)
	var files []string
	for _, position := range g.defines[name] {
		if !seen[position.file] {
			seen[position.file] = true
			files = append(files, position.file)
		}
	}
	if _, ok := g.templates[name]; ok && !seen[name] {
		files = append(files, name)
	}
	return files
}

func (g *templateGraph) reachable(root string) map[string][]includeRef {
	paths := map[string][]includeRef{root: nil}
	queue := []string{root}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, ref := range g.includes[current] {
			for _, owner := range g.ownerFiles(ref.target) {
				if _, ok := paths[owner]; ok {
					continue
				}
				nextPath := append(append([]includeRef(nil), paths[current]...), ref)
				paths[owner] = nextPath
				queue = append(queue, owner)
			}
		}
	}
	return paths
}

func (g *templateGraph) reachesSelectedTemplate(root string, patterns []string) bool {
	if matchesAnyTemplate(patterns, root) {
		return true
	}
	for file := range g.reachable(root) {
		if matchesAnyTemplate(patterns, file) {
			return true
		}
		for name, positions := range g.defines {
			if !matchesAnyTemplate(patterns, name) {
				continue
			}
			for _, position := range positions {
				if position.file == file {
					return true
				}
			}
		}
	}
	return false
}

func newRenderTrace(plan *tracePlan, templates map[string]renderable) *RenderTrace {
	if plan == nil {
		return nil
	}
	files := make(map[string]*traceFile, len(plan.roots))
	for root := range plan.roots {
		files[root] = &traceFile{root: root}
	}
	return &RenderTrace{
		templates: templates,
		files:     files,
	}
}

func (rt *RenderTrace) templateGraph() *templateGraph {
	rt.graphOnce.Do(func() {
		rt.graph = buildTemplateGraph(rt.templates)
	})
	return rt.graph
}

// AnnotateYAMLParseError appends template source information to an invalid
// YAML error without changing its existing text.
func (rt *RenderTrace) AnnotateYAMLParseError(err error, files map[string]string) error {
	if rt == nil || err == nil {
		return err
	}

	file := findYAMLFile(err, files)
	if file == "" {
		return err
	}
	tf, ok := rt.files[file]
	if !ok {
		return err
	}

	line := findYAMLLine(err.Error())
	content, ok := files[file]
	if !ok {
		content = ""
	}

	traceText := rt.yamlTrace(tf.root, file, content, line)
	if traceText == "" {
		return err
	}
	return errors.New(err.Error() + "\n\n" + traceText)
}

// AnnotateUnknownYAML appends an explicit unknown-location trace to an error
// when YAML was transformed and can no longer be mapped to a template.
func (rt *RenderTrace) AnnotateUnknownYAML(err error) error {
	if rt == nil || err == nil {
		return err
	}
	return errors.New(err.Error() + "\n\nTemplate trace:\n  rendered location: unknown\n  template position: unknown")
}

func findYAMLFile(err error, files map[string]string) string {
	message := err.Error()
	matches := yamlFileRE.FindStringSubmatch(message)
	if len(matches) == 2 {
		if _, ok := files[matches[1]]; ok {
			return matches[1]
		}
	}
	longest := ""
	for file := range files {
		if strings.Contains(message, file) && len(file) > len(longest) {
			longest = file
		}
	}
	if longest != "" {
		return longest
	}
	if len(files) == 1 && strings.Contains(err.Error(), "YAML") {
		for file := range files {
			return file
		}
	}
	return ""
}

func findYAMLLine(message string) int {
	matches := yamlLineRE.FindStringSubmatch(message)
	if len(matches) != 2 {
		return 0
	}
	line, err := strconv.Atoi(matches[1])
	if err != nil {
		return 0
	}
	return line
}

type yamlCandidate struct {
	file     string
	line     int
	score    int
	distance int
}

func (rt *RenderTrace) yamlTrace(root, file, content string, yamlLine int) string {
	if yamlLine <= 0 {
		return fmt.Sprintf("Template trace:\n  resource: %s\n  rendered location: %s\n  template position: unknown", root, file)
	}

	graph := rt.templateGraph()
	paths := graph.reachable(root)
	candidates := rt.findYAMLCandidates(content, yamlLine, paths)
	if len(candidates) == 0 {
		return fmt.Sprintf("Template trace:\n  resource: %s\n  rendered location: %s:%d\n  template position: unknown", root, file, yamlLine)
	}

	best, ok := selectYAMLCandidate(candidates)
	if !ok {
		return fmt.Sprintf("Template trace:\n  resource: %s\n  rendered location: %s:%d\n  template position: unknown", root, file, yamlLine)
	}

	var out strings.Builder
	out.WriteString("Template trace:\n")
	fmt.Fprintf(&out, "  resource: %s\n", root)
	for _, ref := range paths[best.file] {
		fmt.Fprintf(&out, "  include %q: %s:%d:%d\n", ref.target, ref.from, ref.line, ref.column)
	}
	fmt.Fprintf(&out, "  YAML error: %s:%d (rendered %s:%d)\n", best.file, best.line, file, yamlLine)
	return strings.TrimRight(out.String(), "\n")
}

func (rt *RenderTrace) findYAMLCandidates(content string, yamlLine int, paths map[string][]includeRef) []yamlCandidate {
	var candidates []yamlCandidate
	seen := make(map[string]bool)
	for _, renderedLine := range renderedLineCandidates(content, yamlLine) {
		renderedText := strings.TrimSpace(renderedLine)
		if renderedText == "" {
			continue
		}
		for file := range paths {
			tpl, ok := rt.templates[file]
			if !ok {
				continue
			}
			for index, sourceLine := range strings.Split(tpl.tpl, "\n") {
				score, matched := matchRenderedSourceLine(sourceLine, renderedText)
				if !matched {
					continue
				}
				key := fmt.Sprintf("%s:%d", file, index+1)
				if seen[key] {
					continue
				}
				seen[key] = true
				candidates = append(candidates, yamlCandidate{
					file:     file,
					line:     index + 1,
					score:    score,
					distance: len(paths[file]),
				})
			}
		}
		if len(candidates) > 0 {
			break
		}
	}
	return candidates
}

func renderedLineCandidates(content string, yamlLine int) []string {
	lines := strings.Split(content, "\n")
	var indexes []int
	add := func(index int) {
		if index < 0 || index >= len(lines) {
			return
		}
		for _, existing := range indexes {
			if existing == index {
				return
			}
		}
		indexes = append(indexes, index)
	}

	add(yamlLine - 1)
	for index, line := range lines {
		if strings.TrimSpace(line) != "---" {
			continue
		}
		start := index + 1
		for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
			start++
		}
		add(start + yamlLine - 1)
	}

	result := make([]string, 0, len(indexes))
	for _, index := range indexes {
		result = append(result, lines[index])
	}
	return result
}

func matchRenderedSourceLine(sourceLine, renderedLine string) (int, bool) {
	trimmedSource := strings.TrimSpace(sourceLine)
	if trimmedSource == "" || strings.Contains(trimmedSource, "{{define") || strings.TrimSpace(trimmedSource) == "{{end}}" {
		return 0, false
	}
	if trimmedSource == renderedLine {
		return 1000 + len(renderedLine), true
	}

	hasExpression := false
	var pattern strings.Builder
	remaining := trimmedSource
	for {
		start := strings.Index(remaining, "{{")
		if start < 0 {
			pattern.WriteString(flexibleQuotedRegexp(remaining))
			break
		}
		end := strings.Index(remaining[start+2:], "}}")
		if end < 0 {
			pattern.WriteString(flexibleQuotedRegexp(remaining))
			break
		}
		pattern.WriteString(flexibleQuotedRegexp(remaining[:start]))
		pattern.WriteString(`.*?`)
		hasExpression = true
		remaining = remaining[start+end+4:]
	}
	if !hasExpression {
		return 0, false
	}

	staticLength := len(strings.TrimSpace(tplFragmentRE.ReplaceAllString(trimmedSource, "")))
	if staticLength < 2 {
		return 0, false
	}

	matched, err := regexp.MatchString("^"+pattern.String()+"$", renderedLine)
	if err != nil || !matched {
		return 0, false
	}
	return staticLength, true
}

func flexibleQuotedRegexp(value string) string {
	return strings.ReplaceAll(regexp.QuoteMeta(value), " ", `\s*`)
}

func selectYAMLCandidate(candidates []yamlCandidate) (yamlCandidate, bool) {
	best := candidates[0]
	ambiguous := false
	for _, candidate := range candidates[1:] {
		switch {
		case candidate.score > best.score:
			best = candidate
			ambiguous = false
		case candidate.score == best.score && candidate.distance < best.distance:
			best = candidate
			ambiguous = false
		case candidate.score == best.score && candidate.distance == best.distance &&
			(candidate.file != best.file || candidate.line != best.line):
			ambiguous = true
		}
	}
	return best, !ambiguous
}

func appendExecutionTrace(base error, root string, err error, maxDepth int) error {
	frames := collectTraceFrames(root, err)
	if len(frames) <= 1 {
		return base
	}
	trace := formatTraceFrames(frames, maxDepth)
	if trace == "" {
		return base
	}
	message := base.Error() + "\n\n" + trace
	return errors.New(redactTemplateArguments(message))
}

func redactTemplateArguments(message string) string {
	message = tplArgumentRE.ReplaceAllString(message, `${1}"<redacted>"`)
	message = tplExecMessageRE.ReplaceAllString(message, `${1}"<redacted>"`)
	return tplParseMessageRE.ReplaceAllString(message, `${1}"<redacted>"`)
}

func collectTraceFrames(root string, err error) []traceFrame {
	frames := []traceFrame{{kind: "resource", target: root}}
	current := err
	inTpl := false
	tplLine, tplColumn := 0, 0
	var lastMessage string

	for current != nil {
		traceable, ok := parseTemplateExecErrorString(current.Error())
		if ok {
			if traceable.message != "" {
				lastMessage = traceable.message
			}
			if traceable.location != "" {
				frame := traceFrame{
					location: traceable.location,
					template: traceable.executingTemplate,
				}
				action, target := parseTraceAction(traceable.expression)
				frame.kind = action
				frame.target = target
				if frame.kind == "" {
					frame.kind = "error"
				}
				skipFrame := false
				if inTpl {
					if line, column, relative := relativeTemplateLocation(traceable.location, traceable.executingTemplate, root); relative {
						if line == tplLine && column == tplColumn {
							skipFrame = true
						} else {
							frame.kind = "tpl-string"
							frame.location = fmt.Sprintf("%d:%d", line, column)
						}
					}
				}
				if frame.kind == "tpl" {
					inTpl = true
					_, tplLine, tplColumn, _ = splitTemplateLocation(traceable.location)
				}
				if !skipFrame && !hasTraceFrame(frames, frame) {
					frames = append(frames, frame)
				}
			}
		}
		current = errors.Unwrap(current)
	}

	if len(frames) == 1 {
		return frames
	}

	frames[len(frames)-1].detail = traceDetail(lastMessage, err.Error())
	return frames
}

func hasTraceFrame(frames []traceFrame, frame traceFrame) bool {
	for _, existing := range frames {
		if existing.location == frame.location {
			return true
		}
	}
	return false
}

func parseTraceAction(expression string) (string, string) {
	expression = strings.TrimSpace(expression)
	if expression == "" {
		return "", ""
	}
	if matches := actionLiteralRE.FindStringSubmatch(expression); len(matches) > 0 {
		return matches[1], firstNonEmpty(matches[2:]...)
	}
	if strings.HasPrefix(expression, "tpl ") || expression == "tpl" {
		return "tpl", ""
	}
	return "", ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func relativeTemplateLocation(location, dynamicTemplate, root string) (int, int, bool) {
	file, line, column, ok := splitTemplateLocation(location)
	if !ok {
		return 0, 0, false
	}
	if dynamicTemplate != root && file != root && dynamicTemplate != "gotpl" && file != "gotpl" {
		return 0, 0, false
	}
	return line, column, true
}

func splitTemplateLocation(location string) (string, int, int, bool) {
	last := strings.LastIndexByte(location, ':')
	if last < 0 {
		return "", 0, 0, false
	}
	column, err := strconv.Atoi(location[last+1:])
	if err != nil {
		return "", 0, 0, false
	}
	rest := location[:last]
	previous := strings.LastIndexByte(rest, ':')
	if previous < 0 {
		return "", 0, 0, false
	}
	line, err := strconv.Atoi(rest[previous+1:])
	if err != nil {
		return "", 0, 0, false
	}
	return rest[:previous], line, column, true
}

func traceDetail(lastMessage, fullError string) string {
	message := lastMessage + "\n" + fullError
	if matches := missingKeyRE.FindStringSubmatch(message); len(matches) == 2 {
		return fmt.Sprintf("missing key %q", matches[1])
	}
	if matches := nilKeyRE.FindStringSubmatch(message); len(matches) == 2 {
		return fmt.Sprintf("missing key %q", matches[1])
	}
	if matches := fieldKeyRE.FindStringSubmatch(message); len(matches) == 2 {
		return fmt.Sprintf("missing field %q", matches[1])
	}
	if matches := indexKeyRE.FindStringSubmatch(message); len(matches) == 2 {
		return fmt.Sprintf("index %s out of range", matches[1])
	}
	if matches := noTemplateRE.FindStringSubmatch(message); len(matches) == 2 {
		return fmt.Sprintf("template %q is not defined", matches[1])
	}
	return ""
}

func formatTraceFrames(frames []traceFrame, maxDepth int) string {
	if len(frames) == 0 {
		return ""
	}
	if maxDepth <= 0 {
		maxDepth = defaultTemplateTraceDepth
	}
	if maxDepth < 4 {
		maxDepth = 4
	}

	omitted := 0
	if len(frames) > maxDepth {
		head := maxDepth / 2
		tail := maxDepth - head - 1
		omitted = len(frames) - head - tail
		truncated := make([]traceFrame, 0, maxDepth)
		truncated = append(truncated, frames[:head]...)
		truncated = append(truncated, traceFrame{kind: "omitted", detail: strconv.Itoa(omitted)})
		truncated = append(truncated, frames[len(frames)-tail:]...)
		frames = truncated
	}

	var out strings.Builder
	out.WriteString("Template trace:")
	for _, frame := range frames {
		out.WriteByte('\n')
		switch frame.kind {
		case "resource":
			fmt.Fprintf(&out, "  resource: %s", frame.target)
		case "omitted":
			fmt.Fprintf(&out, "  ... (%s frames omitted)", frame.detail)
		case "include":
			if frame.target != "" {
				fmt.Fprintf(&out, "  include %q: %s", frame.target, frame.location)
			} else {
				fmt.Fprintf(&out, "  include: %s", frame.location)
			}
		case "template":
			if frame.target != "" {
				fmt.Fprintf(&out, "  template %q: %s", frame.target, frame.location)
			} else {
				fmt.Fprintf(&out, "  template: %s", frame.location)
			}
		case "tpl":
			fmt.Fprintf(&out, "  tpl source: %s", frame.location)
		case "tpl-string":
			fmt.Fprintf(&out, "  tpl string line: %s", frame.location)
		default:
			fmt.Fprintf(&out, "  error: %s", frame.location)
		}
		if frame.detail != "" {
			fmt.Fprintf(&out, " (%s)", frame.detail)
		}
	}
	return out.String()
}
