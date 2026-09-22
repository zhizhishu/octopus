package op

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
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
	cutoff := now.Add(-logAnomalyLookback).Unix()
	buckets := map[string]*logAnomalyBucket{}

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
			acc.declaredN++
			if *relayLog.UpstreamModelMismatch {
				acc.mismatchN++
			}
		}
		if strings.TrimSpace(relayLog.Error) != "" {
			continue
		}
		acc.times = append(acc.times, relayLog.Time)
		acc.outputs = append(acc.outputs, relayLog.OutputTokens)
		acc.latencies = append(acc.latencies, relayLog.UseTime)
	}

	report.BucketCount = len(buckets)
	for _, acc := range buckets {
		report.SampleCount += len(acc.times)
		report.Findings = append(report.Findings, scanLogAnomalyBucket(acc)...)
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
		Select("id", "time", "channel_id", "channel_name", "request_model_name", "actual_model_name", "output_tokens", "use_time", "error", "error_code", "error_strategy", "upstream_response_model", "upstream_model_mismatch").
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
		logs = append(logs, relayLog)
	}
	return logs, nil
}

func logAnomalyBucketKey(relayLog model.RelayLog) string {
	return strings.TrimSpace(relayLog.ChannelName) + "\x1f" + logAnomalyModelName(relayLog)
}

func logAnomalyModelName(relayLog model.RelayLog) string {
	if name := strings.TrimSpace(relayLog.RequestModelName); name != "" {
		return name
	}
	if name := strings.TrimSpace(relayLog.ActualModelName); name != "" {
		return name
	}
	return "unknown"
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
