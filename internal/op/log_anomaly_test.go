package op

import (
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/model"
)

func TestScanLogAnomaliesFlagsOutputDropAndIgnoresTinySamples(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	yes := true
	logs := make([]model.RelayLog, 0, 12)
	for i := 0; i < 8; i++ {
		logs = append(logs, model.RelayLog{
			ID:               int64(i + 1),
			Time:             now.Add(-time.Duration(11-i) * time.Hour).Unix(),
			ChannelId:        3,
			ChannelName:      "demo",
			RequestModelName: "claude-opus-4-8",
			OutputTokens:     200,
			UseTime:          800,
		})
	}
	for i := 0; i < 4; i++ {
		logs = append(logs, model.RelayLog{
			ID:               int64(20 + i),
			Time:             now.Add(-time.Duration(3-i) * time.Hour).Unix(),
			ChannelId:        3,
			ChannelName:      "demo",
			RequestModelName: "claude-opus-4-8",
			OutputTokens:     20,
			UseTime:          900,
		})
	}
	logs = append(logs, model.RelayLog{
		ID:                    99,
		Time:                  now.Unix(),
		ChannelId:             9,
		ChannelName:           "tiny",
		RequestModelName:      "gpt-5.6-sol",
		OutputTokens:          1,
		UseTime:               50,
		UpstreamModelMismatch: &yes,
	})

	report := ScanLogAnomalies(logs, now)
	if report.BucketCount != 2 {
		t.Fatalf("bucket count = %d, want 2", report.BucketCount)
	}

	foundDrop := false
	for _, finding := range report.Findings {
		if finding.Code == "output_token_drop" && finding.ChannelID == 3 {
			foundDrop = true
		}
		if finding.ChannelID == 9 {
			t.Fatalf("tiny sample must not produce a finding, got %#v", finding)
		}
	}
	if !foundDrop {
		t.Fatalf("expected output_token_drop, got %#v", report.Findings)
	}
}

func TestScanLogAnomaliesFlagsEchoMismatchRate(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	yes := true
	no := false
	logs := make([]model.RelayLog, 0, 10)
	for i := 0; i < 8; i++ {
		mismatch := &no
		if i < 3 {
			mismatch = &yes
		}
		logs = append(logs, model.RelayLog{
			ID:                    int64(i + 1),
			Time:                  now.Add(-time.Duration(10-i) * time.Hour).Unix(),
			ChannelId:             4,
			ChannelName:           "echo",
			RequestModelName:      "claude-opus-4-8",
			OutputTokens:          80,
			UseTime:               400,
			UpstreamResponseModel: "claude-haiku-4-5",
			UpstreamModelMismatch: mismatch,
		})
	}

	report := ScanLogAnomalies(logs, now)
	found := false
	for _, finding := range report.Findings {
		if finding.Code == "echo_mismatch_rate" && finding.ChannelID == 4 {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected echo_mismatch_rate, got %#v", report.Findings)
	}
}

func TestScanLogAnomaliesSkipsLocalValidationRows(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	logs := []model.RelayLog{{
		ID:               1,
		Time:             now.Unix(),
		ChannelId:        1,
		ChannelName:      "local",
		RequestModelName: "gpt-5.6-sol",
		ErrorCode:        model.RelayLogErrorCodeClientEmptyRequest,
		ErrorStrategy:    model.RelayLogErrorStrategyLocalValidationPart,
		OutputTokens:     0,
	}}
	report := ScanLogAnomalies(logs, now)
	if report.BucketCount != 0 || len(report.Findings) != 0 {
		t.Fatalf("local validation rows must be ignored, got %#v", report)
	}
}
