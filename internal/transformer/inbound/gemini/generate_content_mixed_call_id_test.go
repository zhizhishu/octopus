package gemini

import (
	"testing"

	"github.com/zhizhishu/octopus/internal/transformer/model"
)

// Parallel Gemini functions may have the same name. A mixed old/new client
// can explicitly answer one call and omit the ID on the next result; the
// second result must match the *remaining* call, not reuse the first ID.
func TestGeminiMixedExplicitAndLegacyFunctionResponseIDs(t *testing.T) {
	cases := []struct {
		name       string
		firstID    string
		secondID   string
		wantFirst  string
		wantSecond string
	}{
		{"first explicit", "call-first", "", "call-first", "call-second"},
		{"second explicit", "call-second", "", "call-second", "call-first"},
		{"legacy first", "", "call-second", "call-first", "call-second"},
		{"unknown explicit", "unseen", "", "unseen", "call-first"},
		{"both legacy", "", "", "call-first", "call-second"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &model.GeminiGenerateContentRequest{Contents: []*model.GeminiContent{
				{Role: "model", Parts: []*model.GeminiPart{
					{FunctionCall: &model.GeminiFunctionCall{ID: "call-first", Name: "lookup"}},
					{FunctionCall: &model.GeminiFunctionCall{ID: "call-second", Name: "lookup"}},
				}},
				{Role: "user", Parts: []*model.GeminiPart{
					{FunctionResponse: &model.GeminiFunctionResponse{ID: tc.firstID, Name: "lookup", Response: map[string]interface{}{"ok": 1}}},
					{FunctionResponse: &model.GeminiFunctionResponse{ID: tc.secondID, Name: "lookup", Response: map[string]interface{}{"ok": 2}}},
				}},
			}}
			got := convertGeminiMessages(req)
			if len(got) != 3 || got[1].ToolCallID == nil || got[2].ToolCallID == nil {
				t.Fatalf("tool response messages missing: %#v", got)
			}
			first, second := *got[1].ToolCallID, *got[2].ToolCallID
			if first != tc.wantFirst || second != tc.wantSecond {
				t.Fatalf("IDs = %q, %q; want %q, %q", first, second, tc.wantFirst, tc.wantSecond)
			}
		})
	}
}
