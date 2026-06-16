package model

import (
	"encoding/json"
	"testing"
)

func TestStatsAPIKeyUsageJSONOmitsCacheMetrics(t *testing.T) {
	usage := NewStatsAPIKeyUsage(StatsAPIKey{
		APIKeyID: 42,
		StatsMetrics: StatsMetrics{
			InputToken:      100,
			OutputToken:     20,
			InputCost:       0.1,
			OutputCost:      0.2,
			WaitTime:        300,
			RequestSuccess:  4,
			RequestFailed:   1,
			CacheHitToken:   50,
			CacheWriteToken: 10,
			CacheInputToken: 100,
		},
	})

	payload, err := json.Marshal(usage)
	if err != nil {
		t.Fatalf("marshal api key usage stats: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal api key usage stats: %v", err)
	}

	for _, key := range []string{"cache_hit_token", "cache_write_token", "cache_input_token"} {
		if _, ok := got[key]; ok {
			t.Fatalf("api key usage stats should not expose %q: %s", key, payload)
		}
	}
	for _, key := range []string{"api_key_id", "input_token", "output_token", "input_cost", "output_cost", "wait_time", "request_success", "request_failed"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("api key usage stats should expose %q: %s", key, payload)
		}
	}
}
