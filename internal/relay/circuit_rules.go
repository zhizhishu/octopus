package relay

import (
	"errors"
	"strings"

	"github.com/zhizhishu/octopus/internal/relay/balancer"
)

const circuitRuleEvidenceLimit = 8 << 10

// circuitRuleEvidence is the text operator error rules match keywords against: the error
// message plus, for an upstream error, its error code and (bounded) response body, since
// the telling phrase ("insufficient_quota", "model not found") usually lives in the body.
func circuitRuleEvidence(err error) string {
	if err == nil {
		return ""
	}
	parts := []string{err.Error()}
	var upErr *upstreamError
	if errors.As(err, &upErr) && upErr != nil {
		if upErr.code != "" {
			parts = append(parts, upErr.code)
		}
		body := upErr.body
		if len(body) > circuitRuleEvidenceLimit {
			body = body[:circuitRuleEvidenceLimit]
		}
		if body != "" {
			parts = append(parts, body)
		}
	}
	return strings.Join(parts, "\n")
}

// applyCircuitErrorRules runs a counted failure through the operator error rules. Call it
// only where the failure is already charged to channel health, so caller cancels and
// request-shape rejections never trigger a hold.
//
// 老口径入口：hold 拿不到端点作用域元数据。媒体/未分作用域的路径（images、videos、
// midjourney、realtime、raw pass-through）继续用它，行为一个字都不变。
func applyCircuitErrorRules(channelID, keyID int, modelName string, statusCode int, err error) {
	applyCircuitErrorRulesScoped(channelID, keyID, modelName, statusCode, err, "", "")
}

// applyCircuitErrorRulesScoped 与 applyCircuitErrorRules 同义，额外把这道请求的作用域
// （endpoint / capability，与 balancer.RecordFailureWithStatusScoped 同一套口径）交给
// hold：只有这样，这条 hold 行在熔断探测调度那边才是 "已知的普通文本作用域"，会被
// 自动探测去验证是否恢复；不带作用域的老口径 hold 仍然不探。
//
// 只影响 hold 行的**元数据**：hold 的匹配、作用域选择、准入判定与真实失败记账全部不变。
func applyCircuitErrorRulesScoped(channelID, keyID int, modelName string, statusCode int, err error, endpoint, capability string) {
	balancer.ApplyErrorRulesScoped(channelID, keyID, modelName, statusCode, circuitRuleEvidence(err), endpoint, capability)
}
