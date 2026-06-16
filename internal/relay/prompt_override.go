package relay

import (
	"strings"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
)

type promptOverrideSnapshot struct {
	Mode    dbmodel.PromptOverrideMode
	Sources []string
}

type promptOverrideLayer struct {
	source string
	mode   dbmodel.PromptOverrideMode
	text   string
}

func applyPromptOverrides(req *transformerModel.InternalLLMRequest, plan *dbmodel.AccessPlan, rule *dbmodel.AccessRouteRule, channel *dbmodel.Channel) promptOverrideSnapshot {
	if req == nil || !req.IsChatRequest() {
		return promptOverrideSnapshot{Mode: dbmodel.PromptOverrideModeAppendSystem}
	}
	layers := promptOverrideLayers(plan, rule, channel)
	snapshot := promptOverrideSnapshot{Mode: dbmodel.PromptOverrideModeAppendSystem}
	for _, layer := range layers {
		if strings.TrimSpace(layer.text) == "" {
			continue
		}
		mode := normalizePromptOverrideMode(layer.mode)
		req.Messages = applyPromptOverrideLayer(req.Messages, layer.text, mode)
		snapshot.Sources = append(snapshot.Sources, layer.source)
		if mode == dbmodel.PromptOverrideModeReplaceSystem {
			snapshot.Mode = mode
		}
	}
	return snapshot
}

func clearResponsesRawPromptShape(req *transformerModel.InternalLLMRequest) {
	if req == nil {
		return
	}
	req.ResponsesInstructions = nil
	req.ResponsesInputRaw = nil
}

func promptOverrideLayers(plan *dbmodel.AccessPlan, rule *dbmodel.AccessRouteRule, channel *dbmodel.Channel) []promptOverrideLayer {
	layers := make([]promptOverrideLayer, 0, 4)
	if text, mode := globalPromptOverride(); text != "" {
		layers = append(layers, promptOverrideLayer{source: "global", mode: mode, text: text})
	}
	if plan != nil && strings.TrimSpace(plan.SystemPromptOverride) != "" {
		layers = append(layers, promptOverrideLayer{
			source: "access_plan:" + plan.Slug,
			mode:   plan.PromptOverrideMode,
			text:   plan.SystemPromptOverride,
		})
	}
	if rule != nil && strings.TrimSpace(rule.SystemPromptOverride) != "" {
		layers = append(layers, promptOverrideLayer{
			source: "route_rule:" + rule.RequestModel,
			mode:   rule.PromptOverrideMode,
			text:   rule.SystemPromptOverride,
		})
	}
	if channel != nil && strings.TrimSpace(channel.SystemPromptOverride) != "" {
		layers = append(layers, promptOverrideLayer{
			source: "channel:" + channel.Name,
			mode:   channel.PromptOverrideMode,
			text:   channel.SystemPromptOverride,
		})
	}
	return layers
}

func globalPromptOverride() (string, dbmodel.PromptOverrideMode) {
	text, err := op.SettingGetString(dbmodel.SettingKeyPromptOverrideSystem)
	if err != nil {
		return "", dbmodel.PromptOverrideModeAppendSystem
	}
	modeValue, err := op.SettingGetString(dbmodel.SettingKeyPromptOverrideMode)
	if err != nil {
		return strings.TrimSpace(text), dbmodel.PromptOverrideModeAppendSystem
	}
	return strings.TrimSpace(text), normalizePromptOverrideMode(dbmodel.PromptOverrideMode(modeValue))
}

func applyPromptOverrideLayer(messages []transformerModel.Message, text string, mode dbmodel.PromptOverrideMode) []transformerModel.Message {
	text = strings.TrimSpace(text)
	if text == "" {
		return messages
	}
	out := make([]transformerModel.Message, 0, len(messages)+1)
	if mode == dbmodel.PromptOverrideModeReplaceSystem {
		for _, msg := range messages {
			if msg.Role == "system" || msg.Role == "developer" {
				continue
			}
			out = append(out, msg)
		}
	} else {
		out = append(out, messages...)
	}
	system := text
	out = append([]transformerModel.Message{{
		Role: "system",
		Content: transformerModel.MessageContent{
			Content: &system,
		},
	}}, out...)
	return out
}

func normalizePromptOverrideMode(mode dbmodel.PromptOverrideMode) dbmodel.PromptOverrideMode {
	switch mode {
	case dbmodel.PromptOverrideModeReplaceSystem:
		return dbmodel.PromptOverrideModeReplaceSystem
	default:
		return dbmodel.PromptOverrideModeAppendSystem
	}
}
