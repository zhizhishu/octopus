package op

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

const (
	logAnomalyLookback           = 7 * 24 * time.Hour
	logAnomalyMinSamples         = 8
	logAnomalyRecentShare        = 0.30
	logAnomalyOutputDropRatio    = 0.40
	logAnomalyMinBaselineOutput  = 20
	logAnomalyLatencySpike       = 2.5
	logAnomalyMinBaselineLatency = 200
	logAnomalyMismatchRate       = 0.20
	logAnomalyMinMismatchCount   = 2
	logAnomalyDisclaimer         = "检查结果提供参考，不等同模型真实性证明。输出塌陷、延迟变形、回显不符都只是线索。"
)

type logAnomalyBucket struct {
	channelID   int
	channelName string
	model       string
	outputs     []int
	latencies   []int
	times       []int64
	mismatchN   int
	declaredN   int
}

// LogAnomalyScanGet 读近期日志，按渠道+模型扫统计异常。零上游请求。
func LogAnomalyScanGet(ctx context.Context) (model.LogAnomalyReport, error) {
	now := time.Now()
	logs, err := logAnomalyLogs(ctx, now.Add(-logAnomalyLookback).Unix())
	if err != nil {
		return model.LogAnomalyReport{}, err
	}
	return ScanLogAnomalies(logs, now), nil
}

// ScanLogAnomalies 纯函数，方便单测。样本太少的桶直接跳过，不记成异常。
func ScanLogAnomalies(logs []model.RelayLog, now time.Time) model.LogAnomalyReport {
	report := model.LogAnomalyReport{
		WindowHours: int(logAnomalyLookback / time.Hour),
		Findings:    []model.LogAnomalyFinding{},
		Disclaimer:  logAnomalyDisclaimer,
	}
	// SampleCount 口径保持不变: 只统计进入 token/latency 统计的有效样本
	// (error 行不计), 与既有告警基准同一口径。
	cutoff := now.Add(-logAnomalyLookback).Unix()
	buckets := map[string]*logAnomalyBucket{}
	passive := map[string]*model.LogAnomalyPassiveBucket{}

	for _, relayLog := range logs {
		if relayLog.Time < cutoff {
			continue
		}
		if relayLogExcludedFromModelTelemetry(relayLog) {
			continue
		}
		key := logAnomalyBucketKey(relayLog)
		acc := buckets[key]
		if acc == nil {
			acc = &logAnomalyBucket{
				channelID:   relayLog.ChannelId,
				channelName: strings.TrimSpace(relayLog.ChannelName),
				model:       logAnomalyModelName(relayLog),
			}
			buckets[key] = acc
		}
		if relayLog.UpstreamModelMismatch != nil {
			// alias_variant(同族别名疑似)不计入"名字经常不符"风险桶,
			// 但本行 UpstreamModelMismatch 原值保留不动, 原始事实不删。
			if !strings.EqualFold(strings.TrimSpace(relayLog.UpstreamMismatchCode), "alias_variant") {
				acc.declaredN++
				if *relayLog.UpstreamModelMismatch {
					acc.mismatchN++
				}
			}
		}
		if strings.TrimSpace(relayLog.Error) != "" {
			continue
		}
		acc.times = append(acc.times, relayLog.Time)
		acc.outputs = append(acc.outputs, relayLog.OutputTokens)
		acc.latencies = append(acc.latencies, relayLog.UseTime)

		// 被动观察摘要走独立 map(language+version 维度), 不把旧 token/latency
		// 统计按有无新摘要拆成两份。旧行/未观察(nil)不计; 仅汇总 Features 非空
		// 的 valid 观察为 no_profile_count, 不计算任何漂移阈值。
		if s := relayLog.PassiveSummary; s != nil && s.Features != nil &&
			s.Status == model.PassiveStatusInsufficientEvidence {
			pk := passiveBucketKey(relayLog, s)
			pb := passive[pk]
			if pb == nil {
				pb = &model.LogAnomalyPassiveBucket{
					ChannelID:      relayLog.ChannelId,
					Model:          logAnomalyModelName(relayLog),
					Endpoint:       strings.TrimSpace(relayLog.RequestEndpoint),
					IsStream:       relayLog.IsStream,
					Language:       s.Language,
					FeatureVersion: s.Version,
				}
				passive[pk] = pb
			}
			pb.Count++
			pb.NoProfileCount++
		}
	}

	report.BucketCount = len(buckets)
	for _, acc := range buckets {
		report.SampleCount += len(acc.times)
		report.Findings = append(report.Findings, scanLogAnomalyBucket(acc)...)
	}
	// passive 桶按 key 稳定排序输出, 保证报告可复现。
	pKeys := make([]string, 0, len(passive))
	for k := range passive {
		pKeys = append(pKeys, k)
	}
	sort.Strings(pKeys)
	for _, k := range pKeys {
		report.PassiveObservations = append(report.PassiveObservations, *passive[k])
	}
	sort.Slice(report.Findings, func(i, j int) bool {
		if report.Findings[i].Severity == report.Findings[j].Severity {
			if report.Findings[i].ChannelID == report.Findings[j].ChannelID {
				return report.Findings[i].Code < report.Findings[j].Code
			}
			return report.Findings[i].ChannelID < report.Findings[j].ChannelID
		}
		if report.Findings[i].Severity == "medium" {
			return true
		}
		return false
	})
	return report
}

func scanLogAnomalyBucket(acc *logAnomalyBucket) []model.LogAnomalyFinding {
	findings := make([]model.LogAnomalyFinding, 0, 3)
	if acc.declaredN >= logAnomalyMinSamples {
		rate := float64(acc.mismatchN) / float64(acc.declaredN)
		if acc.mismatchN >= logAnomalyMinMismatchCount && rate >= logAnomalyMismatchRate {
			findings = append(findings, model.LogAnomalyFinding{
				Code:        "echo_mismatch_rate",
				Severity:    "medium",
				ChannelID:   acc.channelID,
				ChannelName: acc.channelName,
				Model:       acc.model,
				Title:       "近期上游回显经常对不上发出去的名字",
				Evidence: map[string]any{
					"declared":  acc.declaredN,
					"mismatch":  acc.mismatchN,
					"rate":      round4(rate),
					"threshold": logAnomalyMismatchRate,
				},
			})
		}
	}
	if len(acc.times) < logAnomalyMinSamples {
		return findings
	}

	order := make([]int, len(acc.times))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool {
		if acc.times[order[i]] == acc.times[order[j]] {
			return order[i] < order[j]
		}
		return acc.times[order[i]] < acc.times[order[j]]
	})
	recentN := int(math.Ceil(float64(len(order)) * logAnomalyRecentShare))
	if recentN < 2 {
		recentN = 2
	}
	if recentN >= len(order) {
		recentN = len(order) / 3
		if recentN < 2 {
			return findings
		}
	}
	split := len(order) - recentN
	baseOut := make([]int, 0, split)
	recentOut := make([]int, 0, recentN)
	baseLat := make([]int, 0, split)
	recentLat := make([]int, 0, recentN)
	for i, idx := range order {
		if i < split {
			baseOut = append(baseOut, acc.outputs[idx])
			baseLat = append(baseLat, acc.latencies[idx])
			continue
		}
		recentOut = append(recentOut, acc.outputs[idx])
		recentLat = append(recentLat, acc.latencies[idx])
	}

	baseOutMed := medianInt(baseOut)
	recentOutMed := medianInt(recentOut)
	if baseOutMed >= logAnomalyMinBaselineOutput && recentOutMed < int(float64(baseOutMed)*logAnomalyOutputDropRatio) {
		findings = append(findings, model.LogAnomalyFinding{
			Code:        "output_token_drop",
			Severity:    "medium",
			ChannelID:   acc.channelID,
			ChannelName: acc.channelName,
			Model:       acc.model,
			Title:       "近期成功调用的输出明显变短",
			Evidence: map[string]any{
				"baseline_median": baseOutMed,
				"recent_median":   recentOutMed,
				"baseline_n":      len(baseOut),
				"recent_n":        len(recentOut),
				"ratio":           round4(float64(recentOutMed) / float64(baseOutMed)),
			},
		})
	}

	baseLatMed := medianInt(baseLat)
	recentLatMed := medianInt(recentLat)
	if baseLatMed >= logAnomalyMinBaselineLatency && float64(recentLatMed) > float64(baseLatMed)*logAnomalyLatencySpike {
		findings = append(findings, model.LogAnomalyFinding{
			Code:        "latency_spike",
			Severity:    "low",
			ChannelID:   acc.channelID,
			ChannelName: acc.channelName,
			Model:       acc.model,
			Title:       "近期成功调用明显变慢",
			Evidence: map[string]any{
				"baseline_median_ms": baseLatMed,
				"recent_median_ms":   recentLatMed,
				"baseline_n":         len(baseLat),
				"recent_n":           len(recentLat),
				"ratio":              round4(float64(recentLatMed) / float64(baseLatMed)),
			},
		})
	}
	return findings
}

func logAnomalyLogs(ctx context.Context, since int64) ([]model.RelayLog, error) {
	var dbLogs []model.RelayLog
	err := db.GetDB().WithContext(ctx).
		Model(&model.RelayLog{}).
		Select("id", "time", "channel_id", "channel_name", "request_model_name", "actual_model_name", "request_endpoint", "is_stream", "output_tokens", "use_time", "error", "error_code", "error_strategy", "upstream_response_model", "upstream_model_mismatch", "upstream_mismatch_code", "passive_summary").
		Where("time >= ?", since).
		Find(&dbLogs).Error
	if err != nil {
		return nil, err
	}

	relayLogCacheLock.Lock()
	cachedLogs := make([]model.RelayLog, len(relayLogCache))
	copy(cachedLogs, relayLogCache)
	relayLogCacheLock.Unlock()

	seen := make(map[int64]struct{}, len(dbLogs))
	logs := make([]model.RelayLog, 0, len(dbLogs)+len(cachedLogs))
	for _, relayLog := range dbLogs {
		seen[relayLog.ID] = struct{}{}
		logs = append(logs, relayLog)
	}
	for _, relayLog := range cachedLogs {
		if relayLog.Time < since {
			continue
		}
		if _, ok := seen[relayLog.ID]; ok {
			continue
		}
		// 同 ID 的缓存行也登记 seen, 防止后续同 ID 缓存行被重复统计/重算。
		seen[relayLog.ID] = struct{}{}
		logs = append(logs, relayLog)
	}
	return logs, nil
}

func logAnomalyBucketKey(relayLog model.RelayLog) string {
	// 按 channelID + actual 模型优先 + request endpoint + is_stream 分桶,
	// 不再用 channelName + requestModel: 同名渠道/请求别名会让不同渠道或不同
	// 实际模型混进同一个桶。
	return strconv.Itoa(relayLog.ChannelId) + "\x1f" + logAnomalyModelName(relayLog) +
		"\x1f" + strings.TrimSpace(relayLog.RequestEndpoint) +
		"\x1f" + strconv.FormatBool(relayLog.IsStream)
}

func logAnomalyModelName(relayLog model.RelayLog) string {
	// actual 模型优先: 它才是上游实际服务的模型; 请求名可能只是别名/路由名。
	if name := strings.TrimSpace(relayLog.ActualModelName); name != "" {
		return name
	}
	if name := strings.TrimSpace(relayLog.RequestModelName); name != "" {
		return name
	}
	return "unknown"
}

// passiveBucketKey 与主桶无关的独立分桶 key: channelID + actual 优先 + endpoint +
// is_stream + language + feature version。
func passiveBucketKey(relayLog model.RelayLog, s *model.RelayLogPassiveSummary) string {
	return strconv.Itoa(relayLog.ChannelId) + "\x1f" + logAnomalyModelName(relayLog) +
		"\x1f" + strings.TrimSpace(relayLog.RequestEndpoint) +
		"\x1f" + strconv.FormatBool(relayLog.IsStream) +
		"\x1f" + s.Language + "\x1f" + s.Version
}

func medianInt(values []int) int {
	if len(values) == 0 {
		return 0
	}
	sorted := make([]int, len(values))
	copy(sorted, values)
	sort.Ints(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

func round4(value float64) float64 {
	return math.Round(value*10000) / 10000
}
