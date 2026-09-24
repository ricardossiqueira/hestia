package automationrule

import (
	"strings"
	"testing"
)

func TestValidateConditionAcceptsSupportedShapes(t *testing.T) {
	tests := []string{
		`{"==": [{"var": "pressed"}, true]}`,
		`{"!=": [{"var": "state"}, "off"]}`,
		`{"<": [{"var": "brightness"}, 80]}`,
		`{"<=": [{"var": "brightness"}, 80]}`,
		`{">": [{"var": "brightness"}, 80]}`,
		`{">=": [{"var": "brightness"}, 80]}`,
		`{"and": [{"==": [{"var": "a"}, 1]}, {"==": [{"var": "b"}, 2]}]}`,
		`{"or": [{"==": [{"var": "a"}, 1]}, {"==": [{"var": "b"}, 2]}]}`,
		`{"!": [{"==": [{"var": "a"}, 1]}]}`,
		`{"var": "pressed"}`,
		`{"and": [{"or": [{"==": [{"var": "a"}, 1]}, {"==": [{"var": "b"}, 2]}]}, {"!": [{"==": [{"var": "c"}, 3]}]}]}`,
	}
	for _, tt := range tests {
		t.Run(tt, func(t *testing.T) {
			if err := ValidateCondition([]byte(tt)); err != nil {
				t.Errorf("ValidateCondition(%s) = %v, want nil", tt, err)
			}
		})
	}
}

func TestValidateConditionRejectsUnsupportedShapes(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"not json", "nope"},
		{"unknown operator", `{"eval": "1+1"}`},
		{"and with one operand", `{"and": [{"==": [1, 1]}]}`},
		{"and with non-array", `{"and": true}`},
		{"not with two operands", `{"!": [{"==": [1, 1]}, {"==": [2, 2]}]}`},
		{"comparison with one operand", `{"==": [1]}`},
		{"comparison with nested boolean operand", `{"==": [{"and": [true, true]}, true]}`},
		{"var with non-string path", `{"var": 1}`},
		{"var with empty path", `{"var": ""}`},
		{"two operators in one node", `{"==": [1, 1], "!=": [2, 2]}`},
		{"array at top level", `[{"==": [1, 1]}]`},
		{"number at top level", `42`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateCondition([]byte(tt.body)); err == nil {
				t.Errorf("ValidateCondition(%s) = nil, want error", tt.body)
			}
		})
	}
}

func TestValidateConditionRejectsExcessiveDepth(t *testing.T) {
	condition := `{"==": [1, 1]}`
	for i := 0; i < maxConditionDepth+2; i++ {
		condition = `{"!": [` + condition + `]}`
	}
	if err := ValidateCondition([]byte(condition)); err == nil {
		t.Error("ValidateCondition() = nil for excessively nested condition, want error")
	}
}

func TestValidateConditionRejectsOversizedPayload(t *testing.T) {
	huge := `{"==": [{"var": "` + strings.Repeat("a", maxConditionBytes) + `"}, 1]}`
	if err := ValidateCondition([]byte(huge)); err == nil {
		t.Error("ValidateCondition() = nil for oversized condition, want error")
	}
}

func TestEvaluateComparisonOperators(t *testing.T) {
	payload := []byte(`{"pressed": true, "state": "off", "brightness": 80, "label": "x"}`)
	tests := []struct {
		name      string
		condition string
		want      bool
	}{
		{"equal true", `{"==": [{"var": "pressed"}, true]}`, true},
		{"equal false", `{"==": [{"var": "state"}, "on"]}`, false},
		{"not equal true", `{"!=": [{"var": "state"}, "on"]}`, true},
		{"less than true", `{"<": [{"var": "brightness"}, 100]}`, true},
		{"less than false", `{"<": [{"var": "brightness"}, 10]}`, false},
		{"less or equal boundary", `{"<=": [{"var": "brightness"}, 80]}`, true},
		{"greater than true", `{">": [{"var": "brightness"}, 10]}`, true},
		{"greater or equal boundary", `{">=": [{"var": "brightness"}, 80]}`, true},
		{"missing var is nil, not equal to a value", `{"==": [{"var": "missing"}, "x"]}`, false},
		{"missing var equals null literal", `{"==": [{"var": "missing"}, null]}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Evaluate([]byte(tt.condition), payload)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("Evaluate(%s) = %v, want %v", tt.condition, got, tt.want)
			}
		})
	}
}

func TestEvaluateAndOrNot(t *testing.T) {
	payload := []byte(`{"a": 1, "b": 2, "c": 3}`)
	tests := []struct {
		name      string
		condition string
		want      bool
	}{
		{"and both true", `{"and": [{"==": [{"var": "a"}, 1]}, {"==": [{"var": "b"}, 2]}]}`, true},
		{"and one false", `{"and": [{"==": [{"var": "a"}, 1]}, {"==": [{"var": "b"}, 99]}]}`, false},
		{"or one true", `{"or": [{"==": [{"var": "a"}, 99]}, {"==": [{"var": "b"}, 2]}]}`, true},
		{"or both false", `{"or": [{"==": [{"var": "a"}, 99]}, {"==": [{"var": "b"}, 99]}]}`, false},
		{"not negates true", `{"!": [{"==": [{"var": "a"}, 1]}]}`, false},
		{"not negates false", `{"!": [{"==": [{"var": "a"}, 99]}]}`, true},
		{"nested and/or", `{"and": [{"or": [{"==": [{"var": "a"}, 99]}, {"==": [{"var": "b"}, 2]}]}, {"==": [{"var": "c"}, 3]}]}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Evaluate([]byte(tt.condition), payload)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("Evaluate(%s) = %v, want %v", tt.condition, got, tt.want)
			}
		})
	}
}

func TestEvaluateVarByDotPathAndTruthiness(t *testing.T) {
	payload := []byte(`{"sensor": {"value": 42}, "flag": true, "empty": "", "zero": 0}`)
	tests := []struct {
		name      string
		condition string
		want      bool
	}{
		{"nested path resolves", `{"==": [{"var": "sensor.value"}, 42]}`, true},
		{"nested path missing parent resolves nil", `{"==": [{"var": "missing.value"}, null]}`, true},
		{"bare var truthy bool", `{"var": "flag"}`, true},
		{"bare var falsy empty string", `{"var": "empty"}`, false},
		{"bare var falsy zero", `{"var": "zero"}`, false},
		{"bare var falsy missing", `{"var": "missing"}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Evaluate([]byte(tt.condition), payload)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("Evaluate(%s) = %v, want %v", tt.condition, got, tt.want)
			}
		})
	}
}

func TestEvaluateNumericComparisonRejectsNonNumericOperands(t *testing.T) {
	payload := []byte(`{"label": "x"}`)
	if _, err := Evaluate([]byte(`{"<": [{"var": "label"}, 10]}`), payload); err == nil {
		t.Error("Evaluate() = nil error for non-numeric operand in \"<\", want error")
	}
}
