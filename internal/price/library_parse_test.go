package price

import (
	"encoding/json"
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
)

// The same id under several providers: the model's own vendor wins, otherwise the
// fixed Provider order decides, and zero prices never win.
func TestParseLibraryVendorFirstThenFixedOrder(t *testing.T) {
	raw := `{
	  "openai":    {"models": {"claude-x": {"id": "claude-x", "cost": {"input": 1, "output": 1}},
	                           "shared-x": {"id": "shared-x", "cost": {"input": 2, "output": 2}},
	                           "zero-x":   {"id": "zero-x",   "cost": {"input": 0, "output": 0}}}},
	  "anthropic": {"models": {"claude-x": {"id": "Claude-X", "cost": {"input": 3, "output": 15, "cache_read": 0.3}},
	                           "shared-x": {"id": "shared-x", "cost": {"input": 9, "output": 9}}}},
	  "v0":        {"models": {"zero-x":   {"id": "zero-x",   "cost": {"input": 4, "output": 4}},
	                           "gpt-x":    {"id": "gpt-x",    "cost": {"input": 8, "output": 8}}}},
	  "deepseek":  {"models": {"gpt-x":    {"id": "gpt-x",    "cost": {"input": 7, "output": 7}}}},
	  "unknown":   {"models": {"only-x":   {"id": "only-x",   "cost": {"input": 5, "output": 5}}}}
	}`
	var cat modelsDevCatalog
	if err := json.Unmarshal([]byte(raw), &cat); err != nil {
		t.Fatal(err)
	}
	lib := parseLibrary(cat)
	want := map[string]model.LLMPrice{
		"claude-x": {Input: 3, Output: 15, CacheRead: 0.3}, // vendor (anthropic) beats openai
		"shared-x": {Input: 2, Output: 2},                  // no vendor: first in fixed order
		"zero-x":   {Input: 4, Output: 4},                  // a zero price never wins
		"gpt-x":    {Input: 7, Output: 7},                  // deepseek is before v0 in the order
	}
	if len(lib) != len(want) {
		t.Fatalf("got %v", lib)
	}
	for id, p := range want {
		if lib[id] != p {
			t.Fatalf("%s: got %+v want %+v", id, lib[id], p)
		}
	}
}

func TestIsBigChange(t *testing.T) {
	cases := []struct {
		from, to model.LLMPrice
		big      bool
	}{
		{model.LLMPrice{Input: 10}, model.LLMPrice{Input: 15}, false},
		{model.LLMPrice{Input: 10}, model.LLMPrice{Input: 15.1}, true},
		{model.LLMPrice{Input: 10}, model.LLMPrice{Input: 4.9}, true},
		{model.LLMPrice{Input: 10}, model.LLMPrice{Input: 5}, false},
		{model.LLMPrice{Input: 1}, model.LLMPrice{Input: 1, CacheWrite: 5}, false}, // 0 -> 5: was empty
	}
	for _, c := range cases {
		if got := IsBigChange(c.from, c.to); got != c.big {
			t.Fatalf("%+v -> %+v: got %v", c.from, c.to, got)
		}
	}
}
