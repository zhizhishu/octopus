package op

import (
	"testing"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

func TestFilterModelNamesForEndpointFamilyUsesSelectedChannelModels(t *testing.T) {
	ctx := setupAccessPlanTest(t)

	anthropicChannel := model.Channel{
		Name:    "family-anthropic",
		Type:    outbound.OutboundTypeAnthropic,
		Enabled: true,
		Model:   "claude-fable-5",
	}
	if err := ChannelCreate(&anthropicChannel, ctx); err != nil {
		t.Fatalf("create anthropic channel: %v", err)
	}
	openAIChannel := model.Channel{
		Name:    "family-responses",
		Type:    outbound.OutboundTypeOpenAIResponse,
		Enabled: true,
		Model:   "gpt-5.5",
	}
	if err := ChannelCreate(&openAIChannel, ctx); err != nil {
		t.Fatalf("create openai channel: %v", err)
	}
	discoveredOnlyChannel := model.Channel{
		Name:             "family-discovered-only",
		Type:             outbound.OutboundTypeAnthropic,
		Enabled:          true,
		DiscoveredModels: []string{"claude-discovered-only"},
	}
	if err := ChannelCreate(&discoveredOnlyChannel, ctx); err != nil {
		t.Fatalf("create discovered-only channel: %v", err)
	}

	names := []string{"claude-fable-5", "gpt-5.5", "claude-discovered-only"}
	assertChannelModels(t,
		FilterModelNamesForEndpointFamily(ctx, names, model.APIKeyEndpointFamilyAnthropic),
		[]string{"claude-fable-5"},
	)
	assertChannelModels(t,
		FilterModelNamesForEndpointFamily(ctx, names, model.APIKeyEndpointFamilyOpenAICompatible),
		[]string{"gpt-5.5"},
	)
}

func TestFilterModelNamesForEndpointFamilyIncludesAvailableRouteTargets(t *testing.T) {
	ctx := setupAccessPlanTest(t)

	channel := model.Channel{
		Name:    "family-route-responses",
		Type:    outbound.OutboundTypeOpenAIResponse,
		Enabled: true,
		Model:   "gpt-5.5",
	}
	if err := ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create route channel: %v", err)
	}

	plans, err := AccessPlanList(ctx)
	if err != nil {
		t.Fatalf("list plans: %v", err)
	}
	var vip model.AccessPlan
	for _, plan := range plans {
		if plan.Slug == "vip" {
			vip = plan
			break
		}
	}
	if vip.ID == 0 {
		t.Fatalf("vip plan not found")
	}
	if _, err := AccessPlanUpdateRouteTargets(vip.ID, []model.AccessRouteTarget{{
		RequestModel:  "codex-route",
		ChannelID:     channel.ID,
		UpstreamModel: "gpt-5.5",
		Enabled:       true,
		Priority:      1,
		Weight:        1,
	}}, ctx); err != nil {
		t.Fatalf("create route target: %v", err)
	}

	names := []string{"codex-route", "claude-fable-5"}
	assertChannelModels(t,
		FilterModelNamesForEndpointFamily(ctx, names, model.APIKeyEndpointFamilyOpenAICompatible),
		[]string{"codex-route"},
	)
	assertChannelModels(t,
		FilterModelNamesForEndpointFamily(ctx, names, model.APIKeyEndpointFamilyAnthropic),
		nil,
	)
}
