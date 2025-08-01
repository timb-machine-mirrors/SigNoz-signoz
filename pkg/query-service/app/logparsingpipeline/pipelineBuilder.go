package logparsingpipeline

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	signozstanzahelper "github.com/SigNoz/signoz-otel-collector/processor/signozlogspipelineprocessor/stanza/operator/helper"
	"github.com/SigNoz/signoz/pkg/errors"
	"github.com/SigNoz/signoz/pkg/query-service/constants"
	"github.com/SigNoz/signoz/pkg/query-service/queryBuilderToExpr"
	"github.com/SigNoz/signoz/pkg/types/pipelinetypes"
	"github.com/antonmedv/expr"
	"github.com/antonmedv/expr/ast"
	"github.com/antonmedv/expr/parser"
	"github.com/google/uuid"
)

const (
	NOOP = "noop"
)

// To ensure names used in generated collector config are never judged invalid,
// only alphabets, digits and `-` are used when translating pipeline identifiers
var badCharsForCollectorConfName = regexp.MustCompile("[^a-zA-Z0-9-]")

func CollectorConfProcessorName(p pipelinetypes.GettablePipeline) string {
	normalizedAlias := badCharsForCollectorConfName.ReplaceAllString(p.Alias, "-")
	return constants.LogsPPLPfx + normalizedAlias
}

func PreparePipelineProcessor(gettablePipelines []pipelinetypes.GettablePipeline) (map[string]interface{}, []string, error) {
	processors := map[string]interface{}{}
	names := []string{}
	for pipelineIdx, v := range gettablePipelines {
		if !v.Enabled {
			continue
		}

		operators, err := getOperators(v.Config)
		if err != nil {
			return nil, nil, err
		}

		if len(operators) == 0 {
			continue
		}

		filterExpr, err := queryBuilderToExpr.Parse(v.Filter)
		if err != nil {
			return nil, nil, err
		}

		router := []pipelinetypes.PipelineOperator{
			{
				ID:   "router_signoz",
				Type: "router",
				Routes: &[]pipelinetypes.Route{
					{
						Output: operators[0].ID,
						Expr:   filterExpr,
					},
				},
				Default: NOOP,
			},
		}

		v.Config = append(router, operators...)

		// noop operator is needed as the default operator so that logs are not dropped
		noop := pipelinetypes.PipelineOperator{
			ID:   NOOP,
			Type: NOOP,
		}
		v.Config = append(v.Config, noop)

		processor := pipelinetypes.Processor{
			Operators: v.Config,
		}
		name := CollectorConfProcessorName(v)

		// Ensure name is unique
		if _, nameExists := processors[name]; nameExists {
			name = fmt.Sprintf("%s-%d", name, pipelineIdx)
		}

		processors[name] = processor
		names = append(names, name)
	}
	return processors, names, nil
}

func getOperators(ops []pipelinetypes.PipelineOperator) ([]pipelinetypes.PipelineOperator, error) {
	filteredOp := []pipelinetypes.PipelineOperator{}
	for i, operator := range ops {
		if operator.Enabled {
			if operator.Type == "regex_parser" {
				parseFromNotNilCheck, err := fieldNotNilCheck(operator.ParseFrom)
				if err != nil {
					return nil, fmt.Errorf(
						"couldn't generate nil check for parseFrom of regex op %s: %w", operator.Name, err,
					)
				}
				operator.If = fmt.Sprintf(
					`%s && %s matches "%s"`,
					parseFromNotNilCheck,
					operator.ParseFrom,
					strings.ReplaceAll(
						strings.ReplaceAll(operator.Regex, `\`, `\\`),
						`"`, `\"`,
					),
				)

			} else if operator.Type == "grok_parser" {
				parseFromNotNilCheck, err := fieldNotNilCheck(operator.ParseFrom)
				if err != nil {
					return nil, fmt.Errorf(
						"couldn't generate nil check for parseFrom of grok op %s: %w", operator.Name, err,
					)
				}
				operator.If = parseFromNotNilCheck

			} else if operator.Type == "json_parser" {
				operators, err := processJSONParser(&operator)
				if err != nil {
					return nil, fmt.Errorf("couldn't process json_parser op %s: %s", operator.Name, err)
				}

				filteredOp = append(filteredOp, operators...)
				continue // Continue here to skip deduplication of json_parser operator
			} else if operator.Type == "add" {
				if strings.HasPrefix(operator.Value, "EXPR(") && strings.HasSuffix(operator.Value, ")") {
					expression := strings.TrimSuffix(strings.TrimPrefix(operator.Value, "EXPR("), ")")
					fieldsNotNilCheck, err := fieldsReferencedInExprNotNilCheck(expression)
					if err != nil {
						return nil, fmt.Errorf(
							"could'nt generate nil check for fields referenced in value expr of add operator %s: %w",
							operator.Name, err,
						)
					}
					if fieldsNotNilCheck != "" {
						operator.If = fieldsNotNilCheck
					}
				}
			} else if operator.Type == "move" || operator.Type == "copy" {
				fromNotNilCheck, err := fieldNotNilCheck(operator.From)
				if err != nil {
					return nil, fmt.Errorf(
						"couldn't generate nil check for From field of %s op %s: %w", operator.Type, operator.Name, err,
					)
				}
				operator.If = fromNotNilCheck
			} else if operator.Type == "remove" {
				fieldNotNilCheck, err := fieldNotNilCheck(operator.Field)
				if err != nil {
					return nil, fmt.Errorf(
						"couldn't generate nil check for field to be removed by op %s: %w", operator.Name, err,
					)
				}
				operator.If = fieldNotNilCheck
			} else if operator.Type == "trace_parser" {
				cleanTraceParser(&operator)
			} else if operator.Type == "time_parser" {
				parseFromNotNilCheck, err := fieldNotNilCheck(operator.ParseFrom)
				if err != nil {
					return nil, fmt.Errorf(
						"couldn't generate nil check for parseFrom of time parser op %s: %w", operator.Name, err,
					)
				}
				operator.If = parseFromNotNilCheck

				if operator.LayoutType == "strptime" {
					regex, err := pipelinetypes.RegexForStrptimeLayout(operator.Layout)
					if err != nil {
						return nil, fmt.Errorf(
							"couldn't generate layout regex for time_parser %s: %w", operator.Name, err,
						)
					}

					operator.If = fmt.Sprintf(
						`%s && %s matches "%s"`, operator.If, operator.ParseFrom, regex,
					)
				} else if operator.LayoutType == "epoch" {
					valueRegex := `^\\s*[0-9]+\\s*$`
					if strings.Contains(operator.Layout, ".") {
						valueRegex = `^\\s*[0-9]+\\.[0-9]+\\s*$`
					}

					operator.If = fmt.Sprintf(
						`%s && string(%s) matches "%s"`, operator.If, operator.ParseFrom, valueRegex,
					)

				}
				// TODO(Raj): Maybe add support for gotime too eventually
			} else if operator.Type == "severity_parser" {
				err := processSeverityParser(&operator)
				if err != nil {
					return nil, err
				}
			}

			filteredOp = append(filteredOp, operator)
		} else if i == len(ops)-1 && len(filteredOp) != 0 {
			filteredOp[len(filteredOp)-1].Output = ""
		}
	}

	for idx := range filteredOp {
		if idx > 0 {
			filteredOp[idx-1].Output = filteredOp[idx].ID
		}
	}
	return filteredOp, nil
}

func processSeverityParser(operator *pipelinetypes.PipelineOperator) error {
	if operator.Type != "severity_parser" {
		return errors.NewUnexpectedf(CodeInvalidOperatorType, "operator type received %s", operator.Type)
	}

	parseFromNotNilCheck, err := fieldNotNilCheck(operator.ParseFrom)
	if err != nil {
		return errors.WrapInvalidInputf(err, CodeFieldNilCheckType,
			"couldn't generate nil check for parseFrom of severity parser %s", operator.Name,
		)
	}
	operator.If = fmt.Sprintf(
		`%s && ( type(%s) == "string" || ( type(%s) in ["int", "float"] && %s == float(int(%s)) ) )`,
		parseFromNotNilCheck, operator.ParseFrom, operator.ParseFrom, operator.ParseFrom, operator.ParseFrom,
	)

	return nil
}

// processJSONParser converts simple JSON parser operator into multiple operators for JSONMapping of default variables
func processJSONParser(parent *pipelinetypes.PipelineOperator) ([]pipelinetypes.PipelineOperator, error) {
	if parent.Type != "json_parser" {
		return nil, errors.NewUnexpectedf(CodeInvalidOperatorType, "operator type received %s", parent.Type)
	}

	parseFromNotNilCheck, err := fieldNotNilCheck(parent.ParseFrom)
	if err != nil {
		return nil, errors.WrapInvalidInputf(err, CodeFieldNilCheckType,
			"couldn't generate nil check for parseFrom of json parser op %s: %s", parent.Name, err,
		)
	}
	parent.If = fmt.Sprintf(
		`%s && ((type(%s) == "string" && isJSON(%s) && type(fromJSON(unquote(%s))) == "map" ) || type(%s) == "map")`,
		parseFromNotNilCheck, parent.ParseFrom, parent.ParseFrom, parent.ParseFrom, parent.ParseFrom,
	)
	if parent.EnableFlattening {
		parent.MaxFlatteningDepth = constants.MaxJSONFlatteningDepth
	}

	// return if no mapping available
	if parent.Mapping == nil {
		return []pipelinetypes.PipelineOperator{*parent}, nil
	}

	mapping := parent.Mapping
	children := []pipelinetypes.PipelineOperator{}

	// cloning since the same function is used when saving pipelines (POST request) hence reversing
	// the same array inplace ends up with saving mapping in a reversed order in database
	cloneAndReverse := func(input []string) []string {
		cloned := slices.Clone(input)
		slices.Reverse(cloned)

		return cloned
	}

	generateCustomID := func() string {
		return fmt.Sprintf("%s-json-parser", uuid.NewString()) // json-parser helps in identifying processors part of JSON Parser
	}

	// reusable move operator function
	generateMoveOperators := func(keywords []string, to string) error {
		for _, keyword := range cloneAndReverse(keywords) {
			operator := pipelinetypes.PipelineOperator{
				Type:    "move",
				ID:      generateCustomID(),
				OnError: signozstanzahelper.SendOnErrorQuiet,
				From:    fmt.Sprintf(`%s["%s"]`, parent.ParseTo, keyword),
				To:      to,
			}

			fromNotNilCheck, err := fieldNotNilCheck(operator.From)
			if err != nil {
				return err
			}

			operator.If = fromNotNilCheck
			children = append(children, operator)
		}

		return nil
	}

	// JSONMapping: host
	err = generateMoveOperators(mapping[pipelinetypes.Host], `resource["host.name"]`)
	if err != nil {
		return nil, err
	}

	// JSONMapping: service
	err = generateMoveOperators(mapping[pipelinetypes.Service], `resource["service.name"]`)
	if err != nil {
		return nil, err
	}

	// JSONMapping: trace_id
	for _, keyword := range cloneAndReverse(mapping[pipelinetypes.TraceID]) {
		operator := pipelinetypes.PipelineOperator{
			Type:    "trace_parser",
			ID:      generateCustomID(),
			OnError: signozstanzahelper.SendOnErrorQuiet,
			TraceParser: &pipelinetypes.TraceParser{
				TraceId: &pipelinetypes.ParseFrom{
					ParseFrom: fmt.Sprintf(`%s["%s"]`, parent.ParseTo, keyword),
				},
			},
		}

		children = append(children, operator)
	}

	// JSONMapping: span_id
	for _, keyword := range cloneAndReverse(mapping[pipelinetypes.SpanID]) {
		operator := pipelinetypes.PipelineOperator{
			Type:    "trace_parser",
			ID:      generateCustomID(),
			OnError: signozstanzahelper.SendOnErrorQuiet,
			TraceParser: &pipelinetypes.TraceParser{
				SpanId: &pipelinetypes.ParseFrom{
					ParseFrom: fmt.Sprintf(`%s["%s"]`, parent.ParseTo, keyword),
				},
			},
		}

		children = append(children, operator)
	}

	// JSONMapping: trace_flags
	for _, keyword := range cloneAndReverse(mapping[pipelinetypes.TraceFlags]) {
		operator := pipelinetypes.PipelineOperator{
			Type:    "trace_parser",
			ID:      generateCustomID(),
			OnError: signozstanzahelper.SendOnErrorQuiet,
			TraceParser: &pipelinetypes.TraceParser{
				TraceFlags: &pipelinetypes.ParseFrom{
					ParseFrom: fmt.Sprintf(`%s["%s"]`, parent.ParseTo, keyword),
				},
			},
		}

		children = append(children, operator)
	}

	// JSONMapping: severity
	for _, keyword := range cloneAndReverse(mapping[pipelinetypes.Severity]) {
		operator := pipelinetypes.PipelineOperator{
			Type:      "severity_parser",
			ID:        generateCustomID(),
			OnError:   signozstanzahelper.SendOnErrorQuiet,
			ParseFrom: fmt.Sprintf(`%s["%s"]`, parent.ParseTo, keyword),
		}
		err := processSeverityParser(&operator)
		if err != nil {
			return nil, err
		}

		operator.Mapping = pipelinetypes.DefaultSeverityMapping
		children = append(children, operator)
	}

	// JSONMapping: environment
	err = generateMoveOperators(mapping[pipelinetypes.Environment], `resource["deployment.environment.name"]`)
	if err != nil {
		return nil, err
	}

	// JSONMapping: body
	err = generateMoveOperators(mapping[pipelinetypes.Message], `body`)
	if err != nil {
		return nil, err
	}

	// removed mapping reference so it doesn't appear in Collector's config
	parent.Mapping = nil
	return append(append([]pipelinetypes.PipelineOperator{}, *parent), children...), nil
}

// TODO: (Piyush) remove this in future
func cleanTraceParser(operator *pipelinetypes.PipelineOperator) {
	if operator.TraceId != nil && len(operator.TraceId.ParseFrom) < 1 {
		operator.TraceId = nil
	}
	if operator.SpanId != nil && len(operator.SpanId.ParseFrom) < 1 {
		operator.SpanId = nil
	}
	if operator.TraceFlags != nil && len(operator.TraceFlags.ParseFrom) < 1 {
		operator.TraceFlags = nil
	}
}

// Generates an expression checking that `fieldPath` has a non-nil value in a log record.
func fieldNotNilCheck(fieldPath string) (string, error) {
	_, err := expr.Compile(fieldPath)
	if err != nil {
		return "", errors.WrapInvalidInputf(err, CodeFieldNilCheckType, "invalid fieldPath %s", fieldPath)
	}

	// helper for turning `.` into `?.` in field paths.
	// Eg: a.b?.c.d -> a?.b?.c?.d
	optionalChainedPath := func(path string) string {
		return strings.ReplaceAll(
			strings.ReplaceAll(path, "?.", "."), ".", "?.",
		)
	}

	// Optional chaining before membership ops is not supported by expr.
	// Eg: The field `attributes.test["a.b"].value["c.d"].e` can't be checked using
	// the nil check `attributes.test?.["a.b"]?.value?.["c.d"]?.e != nil`
	// This needs to be worked around by checking that the target of membership op is not nil first.
	// Eg: attributes.test != nil && attributes.test["a.b"]?.value != nil && attributes.test["a.b"].value["c.d"]?.e != nil

	// Split once from the right to include the rightmost membership op and everything after it.
	// Eg: `attributes.test["a.b"].value["c.d"].e` would result in `attributes.test["a.b"].value` and `["c.d"].e`
	parts := rSplitAfterN(fieldPath, "[", 2)
	if len(parts) < 2 {
		// there is no [] access in fieldPath
		return fmt.Sprintf("%s != nil", optionalChainedPath(fieldPath)), nil
	}

	// recursively generate nil check for target of the rightmost membership op (attributes.test["a.b"].value)
	// should come out to be (attributes.test != nil && attributes.test["a.b"]?.value != nil)
	collectionNotNilCheck, err := fieldNotNilCheck(parts[0])
	if err != nil {
		return "", errors.WithAdditional(err, "couldn't generate nil check for %s", parts[0])
	}

	// generate nil check for entire path.
	suffixParts := strings.SplitAfter(parts[1], "]") // ["c.d"], ".e"
	fullPath := parts[0] + suffixParts[0]
	if len(suffixParts) > 1 {
		// attributes.test["a.b"].value["c.d"]?.e
		fullPath += optionalChainedPath(suffixParts[1])
	}
	fullPathCheck := fmt.Sprintf("%s != nil", fullPath)

	// If the membership op is for array/slice indexing, add check ensuring array is long enough
	// attributes.test[3] -> len(attributes.test) > 3 && attributes.test[3] != nil
	if !(strings.Contains(suffixParts[0], "'") || strings.Contains(suffixParts[0], `"`)) {
		fullPathCheck = fmt.Sprintf(
			"len(%s) > %s && %s",
			parts[0], suffixParts[0][1:len(suffixParts[0])-1], fullPathCheck,
		)
	}

	// If prefix is `attributes` or `resource` there is no need to add a nil check for
	// the prefix since all log records have non nil `attributes` and `resource` fields.
	if slices.Contains([]string{"attributes", "resource"}, parts[0]) {
		return fullPathCheck, nil
	}

	return fmt.Sprintf("%s && %s", collectionNotNilCheck, fullPathCheck), nil
}

// Split `str` after `sep` from the right to create up to `n` parts.
// rSplitAfterN("a.b.c.d", ".", 3) -> ["a.b", ".c", ".d"]
func rSplitAfterN(str string, sep string, n int) []string {
	reversedStr := reverseString(str)
	parts := strings.SplitAfterN(reversedStr, sep, n)
	slices.Reverse(parts)
	result := []string{}
	for _, p := range parts {
		result = append(result, reverseString(p))
	}
	return result
}

func reverseString(s string) string {
	r := []rune(s)
	for i := 0; i < len(r)/2; i++ {
		j := len(s) - 1 - i
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}

// Generate expression for checking that all fields referenced in `expr` have a non nil value in log record.
// Eg: `attributes.x + len(resource.y)` will return the expression `attributes.x != nil && resource.y != nil`
func fieldsReferencedInExprNotNilCheck(expr string) (string, error) {
	referencedFields, err := logFieldsReferencedInExpr(expr)
	if err != nil {
		return "", fmt.Errorf("couldn't extract log fields referenced in expr %s: %w", expr, err)
	}

	// Generating nil check for deepest fields takes care of their prefixes too.
	// Eg: `attributes.test.value + len(attributes.test)` needs a nil check only for `attributes.test.value`
	deepestFieldRefs := []string{}
	for _, field := range referencedFields {
		isPrefixOfAnotherReferencedField := slices.ContainsFunc(
			referencedFields, func(e string) bool {
				return len(e) > len(field) && strings.HasPrefix(e, field)
			},
		)
		if !isPrefixOfAnotherReferencedField {
			deepestFieldRefs = append(deepestFieldRefs, field)
		}
	}

	fieldExprChecks := []string{}
	for _, field := range deepestFieldRefs {
		checkExpr, err := fieldNotNilCheck(field)
		if err != nil {
			return "", fmt.Errorf("could not create nil check for %s: %w", field, err)
		}
		fieldExprChecks = append(fieldExprChecks, fmt.Sprintf("(%s)", checkExpr))
	}

	return strings.Join(fieldExprChecks, " && "), nil
}

// Expr AST visitor for extracting referenced log fields
// See more at https://github.com/expr-lang/expr/blob/master/ast/visitor.go
type logFieldsInExprExtractor struct {
	referencedFields []string
}

func (v *logFieldsInExprExtractor) Visit(node *ast.Node) {
	if n, ok := (*node).(*ast.MemberNode); ok {
		memberRef := n.String()

		// coalesce ops end up as MemberNode right now for some reason.
		// ignore such member nodes.
		if strings.Contains(memberRef, "??") {
			return
		}

		if strings.HasPrefix(memberRef, "attributes") || strings.HasPrefix(memberRef, "resource") {
			v.referencedFields = append(v.referencedFields, memberRef)
		}
	}
}

func logFieldsReferencedInExpr(expr string) ([]string, error) {
	// parse abstract syntax tree for expr
	exprAst, err := parser.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("could not parse expr: %w", err)
	}

	// walk ast for expr to collect all member references.
	v := &logFieldsInExprExtractor{}
	ast.Walk(&exprAst.Node, v)

	return v.referencedFields, nil
}
