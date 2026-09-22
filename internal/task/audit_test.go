package task

import (
	"testing"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

func TestScheduledAuditEndpoint(t *testing.T) {
	if got := scheduledAuditEndpoint(model.Channel{Type: outbound.OutboundTypeAnthropic}); got != "anthropic_messages" {
		t.Fatalf("anthropic endpoint = %s", got)
	}
	if got := scheduledAuditEndpoint(model.Channel{Type: outbound.OutboundTypeOpenAIResponse}); got != "openai_responses" {
		t.Fatalf("responses endpoint = %s", got)
	}
	if got := scheduledAuditEndpoint(model.Channel{}); got != "openai_chat" {
		t.Fatalf("default endpoint = %s", got)
	}
}

func TestScheduledTargetKeyKeepsChannelAndModelApart(t *testing.T) {
	left := scheduledTargetKey(1, "claude-opus-4-8")
	right := scheduledTargetKey(2, "claude-opus-4-8")
	if left == right {
		t.Fatal("different channels must not share a target key")
	}
}
