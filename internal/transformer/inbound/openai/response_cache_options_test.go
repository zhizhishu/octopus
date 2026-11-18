package openai

import (
	"encoding/json"
	"testing"
)

func TestResponsesInputPreservesCacheOptionsIntoInternal(t *testing.T) {
	req := &ResponsesRequest{Model: "gpt-6.2-sol", PromptCacheOptions: json.RawMessage(`{"ttl":"12h","mode":"explicit"}`)}
	internal, err := convertToInternalRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var options map[string]interface{}
	if err := json.Unmarshal(internal.PromptCacheOptions, &options); err != nil {
		t.Fatal(err)
	}
	if options["ttl"] != "12h" || options["mode"] != "explicit" {
		t.Fatalf("cache options were lost: %#v", options)
	}
}
