package helper

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/bestruirui/octopus/internal/client"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/utils/log"
	"github.com/dlclark/regexp2"
)

func ChannelHttpClient(channel *model.Channel) (*http.Client, error) {
	if channel == nil {
		return nil, errors.New("channel is nil")
	}
	if !channel.Proxy {
		// Opt-in uTLS (Chrome JA3) applies only to direct upstream calls; proxied
		// channels keep the standard transport (uTLS-over-proxy is a follow-up). The
		// setting is default-off and must pass anyrouter shape re-verification first.
		if upstreamUTLSFingerprintEnabled() {
			return client.GetHTTPClientUTLSDirect()
		}
		return client.GetHTTPClientSystemProxy(false)
	} else if channel.ChannelProxy == nil || strings.TrimSpace(*channel.ChannelProxy) == "" {
		return client.GetHTTPClientSystemProxy(true)
	} else {
		return client.GetHTTPClientCustomProxy(strings.TrimSpace(*channel.ChannelProxy))
	}
}

func upstreamUTLSFingerprintEnabled() bool {
	enabled, err := op.SettingGetBool(model.SettingKeyUpstreamUTLSFingerprint)
	if err != nil {
		return false
	}
	return enabled
}

func ChannelBaseUrlDelayUpdate(channel *model.Channel, ctx context.Context) {
	if channel == nil {
		return
	}
	newBaseUrls := make([]model.BaseUrl, 0, len(channel.BaseUrls))
	for _, baseUrl := range channel.BaseUrls {
		if baseUrl.URL == "" {
			continue
		}
		httpClient, err := ChannelHttpClient(channel)
		if err != nil {
			log.Warnf("failed to get http client (channel=%d): %v", channel.ID, err)
			continue
		}
		delay, err := GetUrlDelay(httpClient, baseUrl.URL, ctx)
		if err != nil {
			log.Warnf("failed to get url delay (channel=%d): %v", channel.ID, err)
			continue
		}
		newBaseUrls = append(newBaseUrls, model.BaseUrl{
			URL:   baseUrl.URL,
			Delay: delay,
		})
	}
	if len(newBaseUrls) > 0 {
		op.ChannelBaseUrlUpdate(channel.ID, newBaseUrls)
	}
}

func ChannelAutoGroup(channel *model.Channel, ctx context.Context) {
	if channel == nil {
		return
	}
	if channel.AutoGroup == model.AutoGroupTypeNone {
		return
	}
	groups, err := op.GroupList(ctx)
	if err != nil {
		log.Warnf("get group list failed: %v", err)
		return
	}

	channelModelNames := model.ChannelSelectedModelNames(*channel)
	if len(channelModelNames) == 0 {
		return
	}

	for _, group := range groups {
		matchedModelNames := make([]string, 0, len(channelModelNames))

		switch channel.AutoGroup {
		case model.AutoGroupTypeExact:
			for _, modelName := range channelModelNames {
				if strings.EqualFold(modelName, group.Name) {
					matchedModelNames = append(matchedModelNames, modelName)
				}
			}

		case model.AutoGroupTypeFuzzy:
			groupNameLower := strings.ToLower(strings.TrimSpace(group.Name))
			if groupNameLower == "" {
				continue
			}
			for _, modelName := range channelModelNames {
				if strings.Contains(strings.ToLower(modelName), groupNameLower) {
					matchedModelNames = append(matchedModelNames, modelName)
				}
			}

		case model.AutoGroupTypeRegex:
			if group.MatchRegex == "" {
				for _, modelName := range channelModelNames {
					if strings.EqualFold(modelName, group.Name) {
						matchedModelNames = append(matchedModelNames, modelName)
					}
				}
				break
			}

			re, err := regexp2.Compile(group.MatchRegex, regexp2.ECMAScript)
			if err != nil {
				log.Warnf("compile regex failed (channel=%d group=%d regex=%q): %v", channel.ID, group.ID, group.MatchRegex, err)
				continue
			}
			for _, modelName := range channelModelNames {
				matched, err := re.MatchString(modelName)
				if err != nil {
					log.Warnf("match regex failed (channel=%d group=%d regex=%q model=%q): %v", channel.ID, group.ID, group.MatchRegex, modelName, err)
					continue
				}
				if matched {
					matchedModelNames = append(matchedModelNames, modelName)
				}
			}
		}

		if len(matchedModelNames) > 0 {
			items := make([]model.GroupIDAndLLMName, 0, len(matchedModelNames))
			for _, modelName := range matchedModelNames {
				items = append(items, model.GroupIDAndLLMName{
					ChannelID: channel.ID,
					ModelName: modelName,
				})
			}
			if err := op.GroupItemBatchAdd(group.ID, items, ctx); err != nil {
				log.Warnf("group item batch add failed (channel=%d group=%d): %v", channel.ID, group.ID, err)
			}
		}
	}
}

func ChannelEnsureModelGroups(channel *model.Channel, ctx context.Context) {
	if channel == nil {
		return
	}
	modelNames := model.ChannelSelectedModelNames(*channel)
	// Also register every model_mapping alias (the client-facing KEY) as a routable
	// pool. Previously a mapping was only a post-routing rename: "glm-5.2" →
	// "z-ai/glm-5.2" made the request forward as z-ai/glm-5.2 ONLY if the caller could
	// already route "glm-5.2" — but the alias itself was never a routable name, so
	// calling "glm-5.2" 404'd ("model not found") before the rename ever ran, and the
	// only workaround was to ALSO hand-add the alias to selected_models. Registering
	// the alias as its own pool (item ModelName = the alias) makes it directly callable;
	// at request time the channel's model_mapping still rewrites it to the upstream name
	// on the wire (applyModelMapping), identical to a mapped selected model. The admin
	// explicitly configured the mapping, so this exposes nothing beyond their intent.
	for clientName := range channel.ModelMapping {
		if clean := model.CleanOneMillionCapabilityModelName(clientName); clean != "" {
			modelNames = append(modelNames, clean)
		}
	}
	if len(modelNames) == 0 {
		return
	}
	if err := op.GroupEnsureChannelModels(channel.ID, modelNames, ctx); err != nil {
		log.Warnf("ensure channel model groups failed (channel=%d): %v", channel.ID, err)
	}
}
