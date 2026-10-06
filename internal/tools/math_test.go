package tools

import (
	"context"
	"strings"
	"testing"
)

func TestMathEvaluates(t *testing.T) {
	tests := []struct{ expr, want string }{
		{"1 + 2 * 3", "7"},
		{"(1 + 2) * 3", "9"},
		{"10 / 4", "2.5"},
		{"-2^2", "-4"},
		{"2^3^2", "512"},
		{"2 ** 10", "1024"},
		{"2^-1", "0.5"},
		{"7 % 3", "1"},
		{"sqrt(16) + abs(-3)", "7"},
		{"max(1, 5, 3) - min(4, 2)", "3"},
		{"round(pi, 2)", "3.14"},
		{"round(2.5)", "3"},
		{"pow(2, 8)", "256"},
		{"1e3 + 2.5E-1", "1000.25"},
		{"0.1 + 0.2", "0.30000000000000004"},
		{"log(1000)", "3"},
		{"2 * pi", "6.283185307179586"},
		{"  3*(2+ 1 )  ", "9"},
	}
	tool := &MathTool{}
	for _, tt := range tests {
		got, err := tool.Call(context.Background(), `{"expression":`+jsonString(tt.expr)+`}`)
		if err != nil {
			t.Errorf("%q: erreur %v", tt.expr, err)
			continue
		}
		if got != tt.want {
			t.Errorf("%q = %q, attendu %q", tt.expr, got, tt.want)
		}
	}
}

func TestMathErrors(t *testing.T) {
	for _, expr := range []string{
		"", "1 / 0", "1 % 0", "sqrt(-1)", "ln(0)", "1 +", "(1 + 2", "1 + 2)", "foo(1)", "foo",
		"sqrt(1, 2)", "pow(1)", "2 $ 3", "1 2", "max()",
	} {
		if _, err := (&MathTool{}).Call(context.Background(), `{"expression":`+jsonString(expr)+`}`); err == nil {
			t.Errorf("%q: erreur attendue", expr)
		}
	}
}

func TestMathRejectsUnknownArgument(t *testing.T) {
	_, err := (&MathTool{}).Call(context.Background(), `{"expr":"1+1"}`)
	if err == nil || !strings.Contains(err.Error(), "arguments invalides") {
		t.Fatalf("err = %v", err)
	}
}

func jsonString(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}
