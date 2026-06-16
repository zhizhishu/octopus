package relay

import (
	"net/http"
	"strings"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	transformermodel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/gin-gonic/gin"
)

type routeGroupResult struct {
	Group           dbmodel.Group
	AccessPlan      *dbmodel.AccessPlan
	AccessRouteRule *dbmodel.AccessRouteRule
	AccessRouteUsed bool
}

func selectRouteGroup(c *gin.Context, apiKeyID int, requestModel string, aliases ...string) (routeGroupResult, int, string, error) {
	ctx := c.Request.Context()
	plan, err := op.AccessPlanSelect(apiKeyID, accessPlanHeader(c), ctx)
	if err != nil {
		return routeGroupResult{}, http.StatusForbidden, err.Error(), err
	}

	var rule *dbmodel.AccessRouteRule
	for _, candidate := range routeModelCandidates(requestModel, aliases...) {
		if plan != nil {
			group, matchedRule, ok, err := op.AccessPlanGroupForModel(plan, candidate, ctx)
			if err != nil {
				return routeGroupResult{}, http.StatusInternalServerError, err.Error(), err
			}
			if matchedRule != nil && strings.EqualFold(strings.TrimSpace(candidate), strings.TrimSpace(requestModel)) {
				rule = matchedRule
			}
			if ok {
				return routeGroupResult{
					Group:           group,
					AccessPlan:      plan,
					AccessRouteRule: matchedRule,
					AccessRouteUsed: true,
				}, 0, "", nil
			}
			if strings.EqualFold(strings.TrimSpace(candidate), strings.TrimSpace(requestModel)) && matchedRule != nil && matchedRule.FallbackMode == dbmodel.AccessRouteFallbackNone {
				return routeGroupResult{}, http.StatusServiceUnavailable, "no available channel", nil
			}
		}

		group, err := op.GroupGetEnabledMap(candidate, ctx)
		if err == nil {
			return routeGroupResult{
				Group:           group,
				AccessPlan:      plan,
				AccessRouteRule: rule,
				AccessRouteUsed: false,
			}, 0, "", nil
		}
	}
	return routeGroupResult{}, http.StatusNotFound, "model not found", nil
}

func prepareAnthropicModelCompatibility(inboundType inbound.InboundType, req *transformermodel.InternalLLMRequest) []string {
	if inboundType != inbound.InboundTypeAnthropic || req == nil {
		return nil
	}
	if transformermodel.AnthropicModelWantsOneMillionBeta(req.Model) {
		req.TransformOptions.AnthropicOneMillionBeta = true
	}
	return transformermodel.AnthropicModelAliasCandidates(req.Model)
}

func isSupportedRequestModel(supportedModels, requestModel string, aliases []string) bool {
	if op.IsModelSupported(supportedModels, requestModel) {
		return true
	}
	for _, alias := range aliases {
		if op.IsModelSupported(supportedModels, alias) {
			return true
		}
	}
	return false
}

func routeModelCandidates(requestModel string, aliases ...string) []string {
	seen := map[string]bool{}
	candidates := make([]string, 0, 1+len(aliases))
	for _, value := range append([]string{requestModel}, aliases...) {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		key := strings.ToLower(trimmed)
		if seen[key] {
			continue
		}
		seen[key] = true
		candidates = append(candidates, trimmed)
	}
	return candidates
}

func accessPlanHeader(c *gin.Context) string {
	if value := strings.TrimSpace(c.GetHeader("X-Octopus-Plan")); value != "" {
		return value
	}
	return strings.TrimSpace(c.GetHeader("X-Octopus-Group"))
}

// shouldReturnToOriginalGroup decides whether, after every targeted route candidate
// has failed at runtime, the request should fall back to the model pool (the original
// group) and try its channels. Every fallback mode except an explicit "none" spills to
// the pool — including the default AccessRouteFallbackGroup ("failover"), whose very
// name means "fall back to the group". Previously only "return_group" spilled, so a
// route left on the default mode would dead-end on its primary channel even when the
// pool held a healthy redundant channel (a single flaky upstream then took the model
// fully down). This mirrors the selection-time behaviour in selectRouteGroup, which
// already falls through to the pool for any non-"none" mode. Only "none" opts out.
func shouldReturnToOriginalGroup(routeResult routeGroupResult, alreadyTried bool) bool {
	return !alreadyTried &&
		routeResult.AccessRouteUsed &&
		routeResult.AccessRouteRule != nil &&
		routeResult.AccessRouteRule.FallbackMode != dbmodel.AccessRouteFallbackNone
}
