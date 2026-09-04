package model

import "testing"

func TestIsClaudeCodeModel(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{name: "claude-opus-5", want: true},
		{name: "claude-opus-5[1m]", want: true},
		{name: "opus[1m]", want: true},
		{name: "sonnet-5", want: true},
		{name: "haiku-latest", want: true},
		{name: "claude-fable-5", want: true},
		{name: "mistral-large", want: false},
		{name: "deepseek-v4-pro", want: false},
		{name: "", want: false},
	}
	for _, tt := range tests {
		if got := IsClaudeCodeModel(tt.name); got != tt.want {
			t.Fatalf("IsClaudeCodeModel(%q)=%t, want %t", tt.name, got, tt.want)
		}
	}
}
