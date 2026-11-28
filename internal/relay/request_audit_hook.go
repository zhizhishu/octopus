package relay

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/relay/balancer"
	"github.com/zhizhishu/octopus/internal/relay/requestaudit"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

// request_audit_hook.go — 发上游前决策模型风控审查的 relay 接线（feat/request-audit）。
// 设计照 decisions-api-1128.md §4 + gpt-load internal/requestaudit：
//   - 全局默认全关（request_audit 设置为空）；按下游 api key 的 request_audit 开关启用；
//   - 取样前 24KB、sha256 指纹缓存 1h（包内实现，不存原文）；
//   - 2s 超时/失败 → fail-open 放行并记日志「未审查」；
//   - 审查调用走渠道闸门/RPM：与正常转发同一个 balancer.AdmitChannelGate 预占点，
//     发送前失败释放名额、字节已出门不退（feat/rpm-tpm 口径）。

var requestAuditCaller = requestaudit.NewChannelDecisionsCaller(resolveAuditChannel)

// resolveAuditChannel：取配置渠道、闸门预占、执行 HTTP 调用、按结果记账。
func resolveAuditChannel(ctx context.Context, channelID int, estInputTokens int64, fn func(baseURL, key string) (int, error)) error {
	if channelID <= 0 {
		return fmt.Errorf("audit channel not configured")
	}
	channel, err := op.ChannelGet(channelID, ctx)
	if err != nil || channel == nil {
		return fmt.Errorf("audit channel %d unavailable", channelID)
	}
	if !channel.Enabled {
		return fmt.Errorf("audit channel %d disabled", channelID)
	}
	ck := channel.GetChannelKey()
	key := strings.TrimSpace(ck.ChannelKey)
	if key == "" {
		return fmt.Errorf("audit channel %d has no usable key", channelID)
	}
	admission, reason := balancer.AdmitChannelGate(ctx, channel.ID, channel.MaxConcurrent, channel.RPMLimit, channel.TPMLimit, estInputTokens, 0, func() bool { return ctx.Err() != nil })
	if reason != "" {
		if admission != nil {
			admission.Release()
		}
		return fmt.Errorf("audit channel gate closed: %s", reason)
	}
	// 与 raw_protocol.go:290 同口径：准入与发送之间无实质失败分支，就地封口——
	// 成败/超时都计一次上游尝试；Release 只归还并发位（已封口不退 RPM/TPM）。
	if admission != nil {
		admission.Seal()
		defer admission.Release()
	}
	_, callErr := fn(channel.GetBaseUrl(), key)
	return callErr
}

// maybeAuditRequest 在选路前执行审查。返回 true=已拦截（响应已写）。
// 只在全局配置启用且该 api key 开了开关时生效；其它情况零开销直接放行。
func maybeAuditRequest(c *gin.Context, apiKeyID int, body []byte) bool {
	raw, err := op.SettingGetString(dbmodel.SettingKeyRequestAudit)
	if err != nil || strings.TrimSpace(raw) == "" {
		return false
	}
	cfg, err := requestaudit.ParseConfig(raw)
	if err != nil || cfg == nil || !cfg.Enabled {
		return false
	}
	apiKey, err := op.APIKeyGet(apiKeyID, c.Request.Context())
	if err != nil || !apiKey.RequestAudit {
		return false
	}
	threshold := apiKey.RequestAuditThreshold
	if threshold > 0 && threshold <= 1 {
		// per-key 阈值覆盖全部规则（工单：开关+阈值，默认 0.8 在规则层）。
		for i := range cfg.Rules {
			cfg.Rules[i].Threshold = threshold
		}
	}
	auditor := requestaudit.NewAuditor(requestAuditCaller)
	outcome := auditor.Audit(cfg, cfg.ChannelID, "gpt-6-luna", body)
	if !outcome.Audited {
		// fail-open：放行但记「未审查」，原因带在日志里（不记原文、不记 key）。
		log.Warnf("request audit unaudited (fail-open): key_id=%d reason=%s", apiKeyID, outcome.Reason)
		return false
	}
	if len(outcome.Findings) > 0 {
		parts := make([]string, 0, len(outcome.Findings))
		for _, f := range outcome.Findings {
			parts = append(parts, fmt.Sprintf("%s(%s %.2f)", f.RuleID, f.Action, f.Probability))
		}
		log.Infof("request audit findings: key_id=%d blocked=%v rules=%s", apiKeyID, outcome.Blocked, strings.Join(parts, ","))
	}
	if outcome.Blocked {
		resp_ErrorAuditBlocked(c)
		return true
	}
	return false
}

// resp_ErrorAuditBlocked 用 400 原样形状回客户端（风控命中属客户端内容问题，
// 与「风控不救原样退回」四条规则同族；不用 5xx 避免客户端盲目重试）。
func resp_ErrorAuditBlocked(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
		"error": gin.H{
			"type":    "invalid_request_error",
			"code":    "request_audit_blocked",
			"message": "request blocked by content audit policy",
		},
	})
}

// internalTestChannelNil 让 hook 跳过渠道测试/模型检测的进程内入口
// （检测题目必须原样到达上游，审查只针对真实下游流量）。
func internalTestChannelNil(testChannel *dbmodel.Channel) bool {
	return testChannel == nil
}
