package op

import (
	"testing"

	"github.com/bestruirui/octopus/internal/model"
)

// 上游回显筛选必须是三态: 上游没声明模型名的行既不属于"一致"也不属于"不一致"。
// 若把它算进任一边, 筛选结果就会撒谎——"不一致"那一栏会混进一堆"我们根本没观测到"的行。
func TestRelayLogMatchScopeUpstreamModelMismatchIsTriState(t *testing.T) {
	mismatch := true
	match := false

	mismatchRow := model.RelayLog{
		RequestModelName:      "claude-opus-4-8",
		ActualModelName:       "claude-opus-4-8",
		UpstreamResponseModel: "claude-haiku-4-5",
		UpstreamModelMismatch: &mismatch,
	}
	matchRow := model.RelayLog{
		RequestModelName:      "claude-opus-4-8",
		ActualModelName:       "claude-opus-4-8",
		UpstreamResponseModel: "Claude-Opus-4-8",
		UpstreamModelMismatch: &match,
	}
	// 第三态: 上游压根没回 model 字段(很多中转如此), 或加列之前的老记录。
	undeclaredRow := model.RelayLog{
		RequestModelName: "claude-opus-4-8",
		ActualModelName:  "claude-opus-4-8",
	}

	all := []model.RelayLog{mismatchRow, matchRow, undeclaredRow}

	for _, tc := range []struct {
		name  string
		scope *model.RelayLogScope
		want  int
	}{
		{name: "no filter keeps every row", scope: &model.RelayLogScope{}, want: 3},
		{name: "only mismatches", scope: &model.RelayLogScope{UpstreamModelMismatch: &mismatch}, want: 1},
		{name: "only matches", scope: &model.RelayLogScope{UpstreamModelMismatch: &match}, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kept := 0
			for _, row := range all {
				if relayLogMatchScope(row, tc.scope) {
					kept++
				}
			}
			if kept != tc.want {
				t.Fatalf("kept %d rows, want %d", kept, tc.want)
			}
		})
	}

	// 第三态在"不一致"筛选下必须被排除——这是这条筛选最容易写错的地方。
	if relayLogMatchScope(undeclaredRow, &model.RelayLogScope{UpstreamModelMismatch: &mismatch}) {
		t.Fatal("a row where the upstream declared nothing must not appear in the mismatch bucket")
	}
	if relayLogMatchScope(undeclaredRow, &model.RelayLogScope{UpstreamModelMismatch: &match}) {
		t.Fatal("a row where the upstream declared nothing must not appear in the match bucket")
	}
}
