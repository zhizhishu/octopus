package relay

import (
	"reflect"
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// The branding layer may change only system text. Tools, images, tool results,
// metadata and outbound headers must remain the exact same as the baseline.
func TestBrandPromptOnlyChangesAllowedSystemText(t *testing.T) {
	const fixture = `{"model":"m","stream":false,"messages":[
        {"role":"system","content":"CLIENT_RULES"},
        {"role":"user","content":[{"type":"text","text":"describe this image"},{"type":"image_url","image_url":{"url":"https://upstream.example/test.png"}}]},
        {"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
        {"role":"tool","tool_call_id":"call_1","content":"result"}
    ],"tools":[{"type":"function","function":{"name":"lookup","description":"lookup a value","parameters":{"type":"object","properties":{"x":{"type":"integer"}}}}}]}`
	base := promptWire(t, inbound.InboundTypeOpenAIChat, fixture, outbound.OutboundTypeOpenAIChat, nil, nil, nil, nil)
	branded := promptWire(t, inbound.InboundTypeOpenAIChat, fixture, outbound.OutboundTypeOpenAIChat, nil, nil,
		ruleWith("BRAND_ONLY", after), nil)

	if !reflect.DeepEqual(base.header, branded.header) {
		t.Fatalf("brand changed outbound headers: base=%v branded=%v", base.header, branded.header)
	}
	beforeMessages, ok := base.body["messages"].([]any)
	if !ok || len(beforeMessages) < 4 {
		t.Fatalf("baseline fixture not exercised: %v", base.body["messages"])
	}
	afterMessages, ok := branded.body["messages"].([]any)
	if !ok || len(afterMessages) != len(beforeMessages) {
		t.Fatalf("brand changed message envelope: %v", branded.body["messages"])
	}
	sys := afterMessages[0].(map[string]any)
	if sys["content"] != "CLIENT_RULES\nBRAND_ONLY" {
		t.Fatalf("brand did not apply only to system content: %v", sys)
	}
	if !strings.Contains(string(branded.raw), "BRAND_ONLY") {
		t.Fatal("brand text not serialized into outbound request")
	}
	// Restore only the one approved field. Every other serialized field
	// must then match the real baseline transformation.
	sys["content"] = beforeMessages[0].(map[string]any)["content"]
	if !reflect.DeepEqual(base.body, branded.body) {
		t.Fatalf("brand mutated non-system fields: base=%v branded=%v", base.body, branded.body)
	}
}
