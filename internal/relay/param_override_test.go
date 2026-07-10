package relay

import (
	"encoding/json"
	"testing"
)

// TestApplyParamOverrideMap 锁住 param_override 的合并语义：
// 非 null 值覆盖/新增该键，null 值删除该键（用于剥离上游不支持的参数）。
// 这是渠道级参数覆盖的对外契约，被人无意改回纯覆盖会破坏“删键”能力。
func TestApplyParamOverrideMap(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		override string
		want     map[string]any
	}{
		{
			name:     "override existing value",
			body:     `{"temperature":1,"model":"gpt-5.5"}`,
			override: `{"temperature":0.5}`,
			want:     map[string]any{"temperature": 0.5, "model": "gpt-5.5"},
		},
		{
			name:     "add new key",
			body:     `{"model":"gpt-5.5"}`,
			override: `{"top_p":0.9}`,
			want:     map[string]any{"model": "gpt-5.5", "top_p": 0.9},
		},
		{
			name:     "null deletes key (jianzhile max_output_tokens case)",
			body:     `{"model":"gpt-5.5","max_output_tokens":2048}`,
			override: `{"max_output_tokens":null}`,
			want:     map[string]any{"model": "gpt-5.5"},
		},
		{
			name:     "null on absent key is a no-op",
			body:     `{"model":"gpt-5.5"}`,
			override: `{"max_output_tokens":null}`,
			want:     map[string]any{"model": "gpt-5.5"},
		},
		{
			name:     "mixed set and delete in one override",
			body:     `{"model":"gpt-5.5","max_output_tokens":2048,"temperature":1}`,
			override: `{"max_output_tokens":null,"temperature":0.2,"stream":true}`,
			want:     map[string]any{"model": "gpt-5.5", "temperature": 0.2, "stream": true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			if err := json.Unmarshal([]byte(tc.body), &body); err != nil {
				t.Fatalf("unmarshal body: %v", err)
			}
			var override map[string]any
			if err := json.Unmarshal([]byte(tc.override), &override); err != nil {
				t.Fatalf("unmarshal override: %v", err)
			}

			applyParamOverrideMap(body, override)

			// 用 JSON 往返比较，规避 float64 与字面量的类型差异噪音。
			gotJSON, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("marshal got: %v", err)
			}
			wantJSON, err := json.Marshal(tc.want)
			if err != nil {
				t.Fatalf("marshal want: %v", err)
			}
			var gotNorm, wantNorm map[string]any
			_ = json.Unmarshal(gotJSON, &gotNorm)
			_ = json.Unmarshal(wantJSON, &wantNorm)
			if !jsonEqual(gotNorm, wantNorm) {
				t.Fatalf("param override mismatch\n got: %s\nwant: %s", gotJSON, wantJSON)
			}

			// 删键必须是真的删掉，而不是留一个显式 null。
			if _, hasNull := override["max_output_tokens"]; hasNull && override["max_output_tokens"] == nil {
				if _, stillThere := body["max_output_tokens"]; stillThere {
					t.Fatalf("expected max_output_tokens removed, but it survived: %s", gotJSON)
				}
			}
		})
	}
}

// jsonEqual 比较两个已经过 JSON 往返归一化的 map 是否深度相等。
func jsonEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	// map 的 Marshal 会按键排序，故字节相等即语义相等。
	return string(aj) == string(bj)
}
