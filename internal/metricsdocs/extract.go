// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package metricsdocs

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	prometheusPath = "github.com/prometheus/client_golang/prometheus"
	// Covers every subpackage, not just the metric-declaring ones; see
	// prometheusImportName.
	subpackagePrefix = prometheusPath + "/"
)

// constructor describes a Prometheus constructor that declares a metric family.
type constructor struct {
	metricType familyType
	// vector is true when the constructor takes a variable label slice as its
	// second argument.
	vector bool
}

// optsConstructors are the constructors that take a Prometheus Opts struct and
// therefore carry their own metric type.
var optsConstructors = map[string]constructor{
	"NewCounter":      {typeCounter, false},
	"NewCounterFunc":  {typeCounter, false},
	"NewCounterVec":   {typeCounter, true},
	"NewGauge":        {typeGauge, false},
	"NewGaugeFunc":    {typeGauge, false},
	"NewGaugeVec":     {typeGauge, true},
	"NewHistogram":    {typeHistogram, false},
	"NewHistogramVec": {typeHistogram, true},
	"NewSummary":      {typeSummary, false},
	"NewSummaryVec":   {typeSummary, true},
	"NewUntypedFunc":  {typeUntyped, false},
}

// typedConstMetrics are the const metric constructors whose name alone fixes the
// type of the descriptor they are given.
var typedConstMetrics = map[string]familyType{
	"NewConstHistogram":     typeHistogram,
	"MustNewConstHistogram": typeHistogram,
	"NewConstSummary":       typeSummary,
	"MustNewConstSummary":   typeSummary,
}

// constMetricValueTypes are the prometheus.ValueType constants accepted as the
// second argument of the NewConstMetric constructors.
var constMetricValueTypes = map[string]familyType{
	"CounterValue": typeCounter,
	"GaugeValue":   typeGauge,
	"UntypedValue": typeUntyped,
}

var (
	metricNamePattern = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelNamePattern  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// extract parses the non-test Go files directly under dir and returns the
// Prometheus metric families they declare, sorted by metric name.
func extract(dir string) ([]family, error) {
	files, err := parseDir(dir)
	if err != nil {
		return nil, err
	}

	descTypes, err := collectDescTypes(files)
	if err != nil {
		return nil, err
	}

	var declared []declaredFamily
	for _, file := range files {
		fileFamilies, err := file.declaredFamilies(descTypes)
		if err != nil {
			return nil, err
		}
		declared = append(declared, fileFamilies...)
	}

	seen := make(map[string]string, len(declared))
	families := make([]family, 0, len(declared))
	for _, d := range declared {
		if previous, ok := seen[d.Name]; ok {
			return nil, fmt.Errorf("%s: metric %s is already declared at %s", d.pos, d.Name, previous)
		}
		seen[d.Name] = d.pos
		families = append(families, d.family)
	}
	slices.SortFunc(families, func(a, b family) int { return strings.Compare(a.Name, b.Name) })
	return families, nil
}

// declaredFamily pairs a metric family with the position of its declaration so
// that conflicts can be reported against both call sites.
type declaredFamily struct {
	family
	pos string
}

// sourceFile is a parsed Go file together with the local name its Prometheus
// import is bound to.
type sourceFile struct {
	fset     *token.FileSet
	file     *ast.File
	promName string
}

// parseDir parses every non-test Go file directly under dir that imports the
// Prometheus client, in a stable order. Files in subdirectories belong to other
// packages and are not part of the reference.
func parseDir(dir string) ([]*sourceFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}
	fset := token.NewFileSet()
	var files []*sourceFile
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// The toolchain ignores these outright, so their metrics never reach a
		// build and must not reach the reference either.
		if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(dir, name)
		parsed, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution|parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
		promName, err := prometheusImportName(fset, parsed)
		if err != nil {
			return nil, err
		}
		if promName == "" {
			continue
		}
		if err := rejectBuildConstraints(fset, parsed); err != nil {
			return nil, err
		}
		files = append(files, &sourceFile{fset: fset, file: parsed, promName: promName})
	}
	return files, nil
}

// rejectBuildConstraints refuses a file that some builds exclude, since its
// metrics would be conditional and a flat table cannot say so. Only files that
// import the Prometheus client are checked, so an unrelated platform-specific
// file cannot fail the whole run. The legacy "+build" form counts at any spacing
// the toolchain accepts, rather than only the spelling gofmt settles on.
func rejectBuildConstraints(fset *token.FileSet, file *ast.File) error {
	for _, group := range file.Comments {
		if group.Pos() > file.Package {
			return nil
		}
		for _, comment := range group.List {
			legacy := strings.TrimSpace(strings.TrimPrefix(comment.Text, "//"))
			if strings.HasPrefix(comment.Text, "//go:build") || strings.HasPrefix(legacy, "+build") {
				return fmt.Errorf("%s: build constraints are not supported", fset.Position(comment.Pos()))
			}
		}
	}
	return nil
}

// prometheusImportName returns the local name the Prometheus client is imported
// under, or the empty string when the file does not use it.
func prometheusImportName(fset *token.FileSet, file *ast.File) (string, error) {
	name := ""
	imported := false
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		if strings.HasPrefix(path, subpackagePrefix) {
			// promauto declares and registers a metric in one call, so accepting
			// it would let a metric ship undocumented.
			return "", fmt.Errorf("%s: importing %s is not supported; teach internal/metricsdocs about it first", fset.Position(imp.Pos()), path)
		}
		if path != prometheusPath {
			continue
		}
		if imported {
			// Only one qualifier is tracked, so calls made through a second name
			// would go unread, and a trailing blank import drops the file.
			return "", fmt.Errorf("%s: %s is imported more than once", fset.Position(imp.Pos()), prometheusPath)
		}
		imported = true
		switch {
		case imp.Name == nil:
			name = "prometheus"
		case imp.Name.Name == "_":
			name = ""
		case imp.Name.Name == ".":
			// A dot import makes constructor calls indistinguishable from any
			// other unqualified call, so refuse it rather than miss metrics.
			return "", fmt.Errorf("%s: dot import of %s is not supported", fset.Position(imp.Pos()), prometheusPath)
		default:
			name = imp.Name.Name
		}
	}
	return name, nil
}

// collectDescTypes maps descriptor variable names to the metric type implied by
// the const metric constructors that use them. A prometheus.Desc carries no type
// of its own, so its type can only come from those call sites.
func collectDescTypes(files []*sourceFile) (map[string]familyType, error) {
	types := make(map[string]familyType)
	for _, file := range files {
		var walkErr error
		ast.Inspect(file.file, func(node ast.Node) bool {
			if walkErr != nil {
				return false
			}
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			fn, via, ok := file.prometheusCall(call)
			if !ok || via != "" {
				return true
			}
			metricType, ok, err := file.constMetricType(fn, call)
			if err != nil {
				walkErr = err
				return false
			}
			if !ok {
				return true
			}
			desc, ok := call.Args[0].(*ast.Ident)
			if !ok {
				// The descriptor is not named here, so this call cannot inform
				// its type. Any descriptor left without a type fails at its own
				// declaration instead.
				return true
			}
			if existing, ok := types[desc.Name]; ok && existing != metricType {
				walkErr = file.errorf(call, "descriptor %s is used as both a %s and a %s", desc.Name, existing, metricType)
				return false
			}
			types[desc.Name] = metricType
			return true
		})
		if walkErr != nil {
			return nil, walkErr
		}
	}
	return types, nil
}

// prometheusCall reports which Prometheus function a call invokes, if any. The
// via result names the intermediate value for a call routed through one, as in
// prometheus.V2.NewHistogramVec, and is empty for a direct call.
func (s *sourceFile) prometheusCall(call *ast.CallExpr) (name, via string, ok bool) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", "", false
	}
	switch qualifier := selector.X.(type) {
	case *ast.Ident:
		if qualifier.Name != s.promName {
			return "", "", false
		}
		return selector.Sel.Name, "", true
	case *ast.SelectorExpr:
		pkg, ok := qualifier.X.(*ast.Ident)
		if !ok || pkg.Name != s.promName {
			return "", "", false
		}
		return selector.Sel.Name, qualifier.Sel.Name, true
	}
	return "", "", false
}

// constMetricType reports the metric type a const metric constructor gives to
// the descriptor in its first argument.
func (s *sourceFile) constMetricType(fn string, call *ast.CallExpr) (familyType, bool, error) {
	if metricType, ok := typedConstMetrics[fn]; ok {
		if len(call.Args) == 0 {
			return "", false, s.errorf(call, "%s requires a descriptor argument", fn)
		}
		return metricType, true, nil
	}
	if fn != "NewConstMetric" && fn != "MustNewConstMetric" {
		return "", false, nil
	}
	if len(call.Args) < 2 {
		return "", false, s.errorf(call, "%s requires a descriptor and a value type argument", fn)
	}
	// Only the selected name is looked up below, so the qualifier has to be
	// checked here: an untyped constant of the same name from another package
	// is assignable to prometheus.ValueType and would otherwise set the type.
	selector, ok := call.Args[1].(*ast.SelectorExpr)
	if !ok || !s.qualifiedByPrometheus(selector) {
		return "", false, s.errorf(call.Args[1], "%s value type must be a %s constant", fn, s.promName)
	}
	metricType, ok := constMetricValueTypes[selector.Sel.Name]
	if !ok {
		return "", false, s.errorf(call.Args[1], "unsupported %s value type %s", fn, selector.Sel.Name)
	}
	return metricType, true, nil
}

// declaredFamilies returns the metric families declared by the package-level
// vars of the file.
func (s *sourceFile) declaredFamilies(descTypes map[string]familyType) ([]declaredFamily, error) {
	extracted := make(map[*ast.CallExpr]bool)
	var families []declaredFamily
	for _, decl := range s.file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.VAR {
			continue
		}
		for _, spec := range genDecl.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, value := range valueSpec.Values {
				call, ok := value.(*ast.CallExpr)
				if !ok {
					continue
				}
				fn, via, ok := s.prometheusCall(call)
				if !ok || via != "" || !isMetricConstructor(fn) {
					continue
				}
				if i >= len(valueSpec.Names) {
					return nil, s.errorf(call, "cannot attribute %s to a variable name", fn)
				}
				f, err := s.family(fn, call, valueSpec.Names[i].Name, descTypes)
				if err != nil {
					return nil, err
				}
				extracted[call] = true
				families = append(families, declaredFamily{family: f, pos: s.position(call)})
			}
		}
	}
	if err := s.rejectUnextracted(extracted); err != nil {
		return nil, err
	}
	return families, nil
}

func isMetricConstructor(fn string) bool {
	_, ok := optsConstructors[fn]
	return ok || fn == "NewDesc"
}

// rejectUnextracted fails on metric constructors this extractor did not read.
// Each of them declares a real metric, so erroring is the only way to keep the
// generated reference complete.
func (s *sourceFile) rejectUnextracted(extracted map[*ast.CallExpr]bool) error {
	called := s.callees()
	var err error
	ast.Inspect(s.file, func(node ast.Node) bool {
		if err != nil {
			return false
		}
		if selector, ok := node.(*ast.SelectorExpr); ok {
			// A constructor taken as a value hides its call site behind a name
			// this generator cannot follow, and the options it is handed need
			// not be a literal either, so the reference itself is refused.
			if !called[selector] && s.qualifiedByPrometheus(selector) && strings.HasPrefix(selector.Sel.Name, "New") {
				err = s.errorf(selector, "%s.%s is referenced as a value; declare metrics with a direct constructor call", s.promName, selector.Sel.Name)
				return false
			}
			return true
		}
		call, ok := node.(*ast.CallExpr)
		if !ok || extracted[call] {
			return true
		}
		fn, via, prom := s.prometheusCall(call)
		switch {
		case !prom && s.takesMetricOpts(call):
			// The options literal is the only remaining sign that a metric
			// starts here.
			err = s.errorf(call, "metric options are passed to a call this generator cannot resolve; declare metrics with a direct %s constructor call", s.promName)
		case !prom:
			return true
		case via != "" && (isMetricConstructor(fn) || s.takesMetricOpts(call)):
			// Registrars such as prometheus.DefaultRegisterer are reached the
			// same way, so only the calls that declare a metric are refused.
			err = s.errorf(call, "%s.%s is not a metric constructor this generator reads", via, fn)
		case via != "":
			return true
		case isMetricConstructor(fn):
			err = s.errorf(call, "%s must be the value of a package-level var to be documented", fn)
		case s.takesMetricOpts(call):
			err = s.errorf(call, "%s is not a metric constructor this generator knows", fn)
		default:
			return true
		}
		return false
	})
	return err
}

// callees collects the callee expression of every call in the file, so that a
// constructor reference can be told apart from a constructor call. A callee
// wrapped in parentheses is not recorded, which only makes the check above
// stricter.
func (s *sourceFile) callees() map[ast.Expr]bool {
	called := make(map[ast.Expr]bool)
	ast.Inspect(s.file, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok {
			called[call.Fun] = true
		}
		return true
	})
	return called
}

// takesMetricOpts reports whether a call is handed a Prometheus Opts struct in
// any argument position. Every Opts-taking call is suspect rather than matched
// against optsConstructors, so a constructor added upstream fails loudly instead
// of passing unnoticed.
func (s *sourceFile) takesMetricOpts(call *ast.CallExpr) bool {
	return slices.ContainsFunc(call.Args, s.isMetricOpts)
}

func (s *sourceFile) isMetricOpts(arg ast.Expr) bool {
	lit, ok := arg.(*ast.CompositeLit)
	if !ok {
		return false
	}
	selector, ok := lit.Type.(*ast.SelectorExpr)
	return ok && s.qualifiedByPrometheus(selector) && strings.HasSuffix(selector.Sel.Name, "Opts")
}

// qualifiedByPrometheus reports whether a selector reads a symbol from the
// Prometheus import rather than from a package that merely shares its name.
func (s *sourceFile) qualifiedByPrometheus(selector *ast.SelectorExpr) bool {
	pkg, ok := selector.X.(*ast.Ident)
	return ok && pkg.Name == s.promName
}

func (s *sourceFile) family(fn string, call *ast.CallExpr, varName string, descTypes map[string]familyType) (family, error) {
	if fn == "NewDesc" {
		return s.descFamily(call, varName, descTypes)
	}
	return s.optsFamily(fn, optsConstructors[fn], call)
}

// optsFamily extracts a metric family from a constructor taking a Prometheus
// Opts struct.
func (s *sourceFile) optsFamily(fn string, c constructor, call *ast.CallExpr) (family, error) {
	if len(call.Args) == 0 {
		return family{}, s.errorf(call, "%s requires an options argument", fn)
	}
	opts, ok := call.Args[0].(*ast.CompositeLit)
	if !ok {
		return family{}, s.errorf(call.Args[0], "%s options must be a composite literal", fn)
	}

	f := family{Type: c.metricType}
	var namespace, subsystem, name string
	for _, element := range opts.Elts {
		field, ok := element.(*ast.KeyValueExpr)
		if !ok {
			return family{}, s.errorf(element, "%s options must be written with field names", fn)
		}
		key, ok := field.Key.(*ast.Ident)
		if !ok {
			return family{}, s.errorf(field.Key, "%s option field names must be identifiers", fn)
		}
		var err error
		// Fields that do not contribute to the metric identity, such as bucket
		// boundaries and summary objectives, are deliberately ignored.
		switch key.Name {
		case "Namespace":
			namespace, err = s.stringValue(field.Value)
		case "Subsystem":
			subsystem, err = s.stringValue(field.Value)
		case "Name":
			name, err = s.stringValue(field.Value)
		case "Help":
			f.Help, err = s.stringValue(field.Value)
		case "ConstLabels":
			f.ConstLabels, err = s.labelKeys(field.Value)
		}
		if err != nil {
			return family{}, err
		}
	}
	if name == "" {
		return family{}, s.errorf(call, "%s options must set Name to a string literal", fn)
	}
	f.Name = buildFQName(namespace, subsystem, name)

	if c.vector {
		if len(call.Args) < 2 {
			return family{}, s.errorf(call, "%s requires a variable label argument", fn)
		}
		labels, err := s.stringSlice(call.Args[1])
		if err != nil {
			return family{}, err
		}
		f.Labels = labels
	}
	if err := s.validate(call, f); err != nil {
		return family{}, err
	}
	return f, nil
}

// descFamily extracts a metric family from a prometheus.NewDesc call, taking its
// type from the const metric call sites collected across the package.
func (s *sourceFile) descFamily(call *ast.CallExpr, varName string, descTypes map[string]familyType) (family, error) {
	if len(call.Args) != 4 {
		return family{}, s.errorf(call, "NewDesc takes four arguments, got %d", len(call.Args))
	}
	name, err := s.stringValue(call.Args[0])
	if err != nil {
		return family{}, err
	}
	help, err := s.stringValue(call.Args[1])
	if err != nil {
		return family{}, err
	}
	labels, err := s.stringSlice(call.Args[2])
	if err != nil {
		return family{}, err
	}
	constLabels, err := s.labelKeys(call.Args[3])
	if err != nil {
		return family{}, err
	}
	metricType, ok := descTypes[varName]
	if !ok {
		return family{}, s.errorf(call, "cannot determine the type of descriptor %s: no %s const metric constructor in the package references it by name", varName, s.promName)
	}
	f := family{Name: name, Type: metricType, Help: help, Labels: labels, ConstLabels: constLabels}
	if err := s.validate(call, f); err != nil {
		return family{}, err
	}
	return f, nil
}

// stringValue evaluates a string literal, including the concatenations used to
// wrap long help strings.
func (s *sourceFile) stringValue(expr ast.Expr) (string, error) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			break
		}
		value, err := strconv.Unquote(e.Value)
		if err != nil {
			return "", s.errorf(e, "cannot unquote string literal %s: %w", e.Value, err)
		}
		return value, nil
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			break
		}
		left, err := s.stringValue(e.X)
		if err != nil {
			return "", err
		}
		right, err := s.stringValue(e.Y)
		if err != nil {
			return "", err
		}
		return left + right, nil
	case *ast.ParenExpr:
		return s.stringValue(e.X)
	}
	return "", s.errorf(expr, "expected a string literal")
}

// stringSlice evaluates a []string literal. An explicit nil is accepted because
// NewDesc spells "no variable labels" that way.
func (s *sourceFile) stringSlice(expr ast.Expr) ([]string, error) {
	if isNil(expr) {
		return nil, nil
	}
	lit, ok := expr.(*ast.CompositeLit)
	if !ok {
		return nil, s.errorf(expr, "expected a []string literal")
	}
	values := make([]string, 0, len(lit.Elts))
	for _, element := range lit.Elts {
		value, err := s.stringValue(element)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

// labelKeys evaluates the keys of a prometheus.Labels literal, or an explicit
// nil. Values are ignored: they routinely come from runtime state such as build
// metadata, so only the keys are documentable. The keys come from a map literal,
// so their declaration order carries no meaning and they are sorted instead.
func (s *sourceFile) labelKeys(expr ast.Expr) ([]string, error) {
	if isNil(expr) {
		return nil, nil
	}
	lit, ok := expr.(*ast.CompositeLit)
	if !ok {
		return nil, s.errorf(expr, "expected a %s.Labels literal", s.promName)
	}
	keys := make([]string, 0, len(lit.Elts))
	for _, element := range lit.Elts {
		entry, ok := element.(*ast.KeyValueExpr)
		if !ok {
			return nil, s.errorf(element, "expected a key/value pair in a %s.Labels literal", s.promName)
		}
		key, err := s.stringValue(entry.Key)
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys, nil
}

// validate rejects anything outside the legacy Prometheus name grammar. Help is
// required even though Prometheus tolerates an empty one, because a
// reference row with a blank description is not worth publishing.
func (s *sourceFile) validate(node ast.Node, f family) error {
	if !metricNamePattern.MatchString(f.Name) {
		return s.errorf(node, "invalid metric name %q", f.Name)
	}
	if f.Help == "" {
		return s.errorf(node, "metric %s has no help text", f.Name)
	}
	for _, label := range slices.Concat(f.Labels, f.ConstLabels) {
		if !labelNamePattern.MatchString(label) {
			return s.errorf(node, "metric %s has an invalid label name %q", f.Name, label)
		}
	}
	return nil
}

func isNil(expr ast.Expr) bool {
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == "nil"
}

// buildFQName joins the name parts the way prometheus.BuildFQName does, minus
// upstream's empty result for an empty name: optsFamily rejects that first.
func buildFQName(namespace, subsystem, name string) string {
	parts := make([]string, 0, 3)
	for _, part := range []string{namespace, subsystem, name} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "_")
}

func (s *sourceFile) position(node ast.Node) string {
	return s.fset.Position(node.Pos()).String()
}

func (s *sourceFile) errorf(node ast.Node, format string, args ...any) error {
	return fmt.Errorf("%s: %w", s.position(node), fmt.Errorf(format, args...))
}
