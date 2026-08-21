package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResponseMessageItemAlwaysMarshalsContentArray(t *testing.T) {
	item := ResponsesItem{
		ID:   "msg_123",
		Type: "message",
		Role: "assistant",
	}

	data, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	str := string(data)
	if !strings.Contains(str, `"content":[]`) {
		t.Fatalf("expected message item to carry \"content\":[], got %s", str)
	}
}
