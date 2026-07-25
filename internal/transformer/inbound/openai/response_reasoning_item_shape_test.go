package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/samber/lo"
)

// TestReasoningItemAlwaysCarriesSummaryArray verifies the reasoning
// output_item.added carries a "summary" array (and encrypted_content) even when
// empty, so a codex client allocates the summary container before the
// reasoning_summary_part.added / reasoning_summary_text.delta events reference it.
// Without "summary":[] the client errors "ReasoningSummaryPartAdded without active
// item". Non-reasoning items must not gain a spurious summary field.
func TestReasoningItemAlwaysCarriesSummaryArray(t *testing.T) {
	// in-progress reasoning item (empty summary) — the added event shape.
	added := ResponsesItem{
		ID:               "rs_1",
		Type:             "reasoning",
		Status:           lo.ToPtr("in_progress"),
		Summary:          []ResponsesReasoningSummary{},
		EncryptedContent: lo.ToPtr(""),
	}
	b, err := json.Marshal(added)
	if err != nil {
		t.Fatalf("marshal reasoning added: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"summary":[]`) {
		t.Errorf("reasoning added item must carry summary:[], got %s", s)
	}
	if !strings.Contains(s, `"encrypted_content":""`) {
		t.Errorf("reasoning added item must carry encrypted_content, got %s", s)
	}

	// finalized reasoning item (non-empty summary) is unchanged and still round-trips.
	done := ResponsesItem{
		ID:      "rs_1",
		Type:    "reasoning",
		Summary: []ResponsesReasoningSummary{{Type: "summary_text", Text: "thought"}},
	}
	db, _ := json.Marshal(done)
	if !strings.Contains(string(db), `"summary":[{`) {
		t.Errorf("finalized reasoning item must keep its summary content, got %s", string(db))
	}

	// a non-reasoning item must NOT gain a summary field.
	msg := ResponsesItem{ID: "m1", Type: "message", Role: "assistant"}
	mb, _ := json.Marshal(msg)
	if strings.Contains(string(mb), "summary") {
		t.Errorf("non-reasoning item must not carry a summary field, got %s", string(mb))
	}

	// round-trip: the marshaled reasoning item still unmarshals back to a reasoning item.
	var back ResponsesItem
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal round-trip: %v", err)
	}
	if back.Type != "reasoning" {
		t.Errorf("round-trip lost type: %+v", back)
	}
}
