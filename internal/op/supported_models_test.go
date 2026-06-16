package op

import "testing"

func TestIsModelSupported(t *testing.T) {
	tests := []struct {
		name            string
		supportedModels string
		modelName       string
		want            bool
	}{
		{name: "unrestricted allows empty", supportedModels: "", modelName: "", want: true},
		{name: "trims comma separated list", supportedModels: "gpt-4o, claude-sonnet", modelName: "claude-sonnet", want: true},
		{name: "claude full 1m allows opus shortcut", supportedModels: "claude-opus-4-8[1m]", modelName: "opus[1m]", want: true},
		{name: "opus shortcut allows full 1m spelling", supportedModels: "opus[1m]", modelName: "claude-opus-4-8[1m]", want: true},
		{name: "old claude 1m alias maps to current 1m spelling", supportedModels: "claude-opus-4-7[1m]", modelName: "claude-opus-4-8[1m]", want: true},
		{name: "one million suffix is treated as capability not model name", supportedModels: "claude-opus-4-8[1m]", modelName: "claude-opus-4-8", want: true},
		{name: "restricted rejects missing model", supportedModels: "gpt-4o", modelName: "", want: false},
		{name: "restricted rejects absent model", supportedModels: "gpt-4o", modelName: "gpt-5", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsModelSupported(tt.supportedModels, tt.modelName); got != tt.want {
				t.Fatalf("IsModelSupported(%q, %q) = %v, want %v", tt.supportedModels, tt.modelName, got, tt.want)
			}
		})
	}
}
