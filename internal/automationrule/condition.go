// Package automationrule defines and evaluates the condition attached to a
// Marco 5 automation rule (docs/device-manifests.md): a supported subset of
// JSONLogic (https://jsonlogic.com/), not a bespoke DSL. It has no SQLite or
// MQTT imports - internal/registry validates a condition at write time,
// item 4's future execution engine evaluates one against a real event
// payload, neither of which this package needs to know about.
package automationrule

import (
	"encoding/json"
	"errors"
	"fmt"
)

// maxConditionBytes bounds a condition's serialized size, same spirit as
// devicemanifest's schema byte caps - a rule condition describes a simple
// field comparison, not an arbitrary program.
const maxConditionBytes = 2048

// maxConditionDepth bounds nesting (and/or/! descending into their
// operands), preventing pathological trees without limiting the shapes
// this subset actually needs.
const maxConditionDepth = 6

// comparisonOperators take exactly two operands: literal (string, number,
// bool, null) or {"var": "field.path"}.
var comparisonOperators = map[string]bool{
	"==": true, "!=": true, "<": true, "<=": true, ">": true, ">=": true,
}

// ValidateCondition checks that raw is well-formed JSON implementing this
// package's supported JSONLogic subset: and/or (2+ operands)/!(1 operand)/
// the comparison operators above/var, nested up to maxConditionDepth, with
// comparison operands limited to literals or var (no nested boolean
// subexpression as a comparison operand). An empty or absent condition is
// the caller's concern (it means "always true"), not this function's - a
// non-empty raw must always be well-formed.
func ValidateCondition(raw json.RawMessage) error {
	if len(raw) > maxConditionBytes {
		return fmt.Errorf("condition exceeds %d bytes", maxConditionBytes)
	}
	var node any
	if err := json.Unmarshal(raw, &node); err != nil {
		return fmt.Errorf("condition is not valid JSON: %w", err)
	}
	return validateConditionNode(node, 0)
}

func validateConditionNode(node any, depth int) error {
	if depth > maxConditionDepth {
		return errors.New("condition nesting is too deep")
	}
	switch value := node.(type) {
	case bool:
		return nil
	case map[string]any:
		if len(value) != 1 {
			return errors.New("condition node must have exactly one operator")
		}
		for operator, operand := range value {
			switch {
			case operator == "and" || operator == "or":
				items, ok := operand.([]any)
				if !ok || len(items) < 2 {
					return fmt.Errorf("%q requires an array of at least two conditions", operator)
				}
				for _, item := range items {
					if err := validateConditionNode(item, depth+1); err != nil {
						return err
					}
				}
				return nil
			case operator == "!":
				items, ok := operand.([]any)
				if !ok || len(items) != 1 {
					return errors.New(`"!" requires an array of exactly one condition`)
				}
				return validateConditionNode(items[0], depth+1)
			case operator == "var":
				return validateVarOperand(operand)
			case comparisonOperators[operator]:
				items, ok := operand.([]any)
				if !ok || len(items) != 2 {
					return fmt.Errorf("%q requires an array of exactly two operands", operator)
				}
				for _, item := range items {
					if err := validateComparisonOperand(item); err != nil {
						return err
					}
				}
				return nil
			default:
				return fmt.Errorf("unsupported condition operator %q", operator)
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported condition node type %T", node)
	}
}

// validateComparisonOperand allows only a literal or {"var": "..."} - not a
// nested boolean subexpression, keeping comparisons flat and evaluable
// without ambiguity about what "true == {"and": [...]}" would even mean.
func validateComparisonOperand(node any) error {
	switch value := node.(type) {
	case nil, bool, float64, string:
		return nil
	case map[string]any:
		if len(value) != 1 {
			return errors.New("comparison operand must be a literal or {\"var\": ...}")
		}
		operand, ok := value["var"]
		if !ok {
			return errors.New("comparison operand must be a literal or {\"var\": ...}")
		}
		return validateVarOperand(operand)
	default:
		return fmt.Errorf("unsupported comparison operand type %T", node)
	}
}

func validateVarOperand(operand any) error {
	path, ok := operand.(string)
	if !ok || path == "" {
		return errors.New(`"var" requires a non-empty field path string`)
	}
	return nil
}

// Evaluate parses raw as a validated condition (call ValidateCondition
// first at write time - Evaluate does not re-check size/depth/shape beyond
// what it needs to walk the tree) and evaluates it against payload, the
// event's own accepted JSON envelope. Field lookups missing from payload
// resolve to nil (JSONLogic's own "var" semantics), not an error.
func Evaluate(raw json.RawMessage, payload []byte) (bool, error) {
	var node any
	if err := json.Unmarshal(raw, &node); err != nil {
		return false, fmt.Errorf("condition is not valid JSON: %w", err)
	}
	var vars map[string]any
	if err := json.Unmarshal(payload, &vars); err != nil {
		return false, fmt.Errorf("event payload is not a JSON object: %w", err)
	}
	return evalCondition(node, vars)
}

func evalCondition(node any, vars map[string]any) (bool, error) {
	switch value := node.(type) {
	case bool:
		return value, nil
	case map[string]any:
		if len(value) != 1 {
			return false, errors.New("condition node must have exactly one operator")
		}
		for operator, operand := range value {
			switch {
			case operator == "and":
				items, err := conditionList(operand)
				if err != nil {
					return false, err
				}
				for _, item := range items {
					result, err := evalCondition(item, vars)
					if err != nil {
						return false, err
					}
					if !result {
						return false, nil
					}
				}
				return true, nil
			case operator == "or":
				items, err := conditionList(operand)
				if err != nil {
					return false, err
				}
				for _, item := range items {
					result, err := evalCondition(item, vars)
					if err != nil {
						return false, err
					}
					if result {
						return true, nil
					}
				}
				return false, nil
			case operator == "!":
				items, err := conditionList(operand)
				if err != nil || len(items) != 1 {
					return false, errors.New(`"!" requires an array of exactly one condition`)
				}
				result, err := evalCondition(items[0], vars)
				if err != nil {
					return false, err
				}
				return !result, nil
			case operator == "var":
				resolved, err := evalOperand(map[string]any{"var": operand}, vars)
				if err != nil {
					return false, err
				}
				return isTruthy(resolved), nil
			case comparisonOperators[operator]:
				items, ok := operand.([]any)
				if !ok || len(items) != 2 {
					return false, fmt.Errorf("%q requires an array of exactly two operands", operator)
				}
				left, err := evalOperand(items[0], vars)
				if err != nil {
					return false, err
				}
				right, err := evalOperand(items[1], vars)
				if err != nil {
					return false, err
				}
				return evalComparison(operator, left, right)
			default:
				return false, fmt.Errorf("unsupported condition operator %q", operator)
			}
		}
		return false, nil
	default:
		return false, fmt.Errorf("unsupported condition node type %T", node)
	}
}

func conditionList(operand any) ([]any, error) {
	items, ok := operand.([]any)
	if !ok || len(items) < 1 {
		return nil, errors.New("expected a non-empty array of conditions")
	}
	return items, nil
}

// evalOperand resolves a comparison operand: a literal value, or
// {"var": "a.b.c"} walked through vars by dot-path. A missing path segment
// (nil at any step, or the final key absent) resolves to nil, never an
// error.
func evalOperand(node any, vars map[string]any) (any, error) {
	object, ok := node.(map[string]any)
	if !ok {
		return node, nil
	}
	path, ok := object["var"].(string)
	if !ok {
		return nil, errors.New(`"var" requires a field path string`)
	}
	var current any = vars
	for _, segment := range splitPath(path) {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, nil
		}
		current = object[segment]
	}
	return current, nil
}

func splitPath(path string) []string {
	segments := []string{}
	start := 0
	for i := 0; i < len(path); i++ {
		if path[i] == '.' {
			segments = append(segments, path[start:i])
			start = i + 1
		}
	}
	return append(segments, path[start:])
}

func evalComparison(operator string, left, right any) (bool, error) {
	switch operator {
	case "==":
		return compareEqual(left, right), nil
	case "!=":
		return !compareEqual(left, right), nil
	}
	leftNumber, leftOK := left.(float64)
	rightNumber, rightOK := right.(float64)
	if !leftOK || !rightOK {
		return false, fmt.Errorf("%q requires numeric operands", operator)
	}
	switch operator {
	case "<":
		return leftNumber < rightNumber, nil
	case "<=":
		return leftNumber <= rightNumber, nil
	case ">":
		return leftNumber > rightNumber, nil
	case ">=":
		return leftNumber >= rightNumber, nil
	default:
		return false, fmt.Errorf("unsupported comparison operator %q", operator)
	}
}

func compareEqual(left, right any) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	switch leftValue := left.(type) {
	case float64:
		rightValue, ok := right.(float64)
		return ok && leftValue == rightValue
	case string:
		rightValue, ok := right.(string)
		return ok && leftValue == rightValue
	case bool:
		rightValue, ok := right.(bool)
		return ok && leftValue == rightValue
	default:
		return false
	}
}

func isTruthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case float64:
		return v != 0
	case string:
		return v != ""
	default:
		return true
	}
}
