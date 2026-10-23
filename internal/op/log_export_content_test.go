package op

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

// include_content=1 是管理员排障的主力功能：导出**完整**请求/响应正文。
//
// 回归背景：RelayLogList 无条件调 RelayLogOmitContent 清空 request_content /
// response_content，且 DB 查询时压根没取这两列。于是"完整导出"出来的 NDJSON 里
// 正文恒为 ""。排障人拿着空正文，会把一条真实的换后端/截断事故判成
// "客户端发的是空请求"或"上游没返回正文"——方向从一开始就是错的。
//
// 这条测试同时钉住两侧：要正文时真的有，要摘要时真的没有（摘要不能顺带泄露）。
func TestRelayLogExportIncludesContentWhenRequested(t *testing.T) {
	ctx := setupRelayLogTest(t)

	const reqBody = `{"model":"kimi-k3","messages":[{"role":"user","content":"SECRET_PROMPT_MARKER"}]}`
	const respBody = `{"choices":[{"message":{"content":"SECRET_REPLY_MARKER"}}]}`

	logs := []model.RelayLog{
		{
			ID: 9801, Time: 9801, RequestModelName: "kimi-k3", ChannelName: "primary", UserID: 1,
			RequestContent:  reqBody,
			ResponseContent: respBody,
		},
	}
	if err := db.GetDB().Create(&logs).Error; err != nil {
		t.Fatalf("create: %v", err)
	}

	// ---- 场景 A：完整导出（userSummary=false）→ 正文必须存在 ----
	var full bytes.Buffer
	if err := RelayLogStreamExportNDJSON(ctx, &full, nil, nil, 100, &model.RelayLogScope{}, false); err != nil {
		t.Fatalf("export full: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(full.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("导出只有 %d 行，期望 header + 至少一条记录: %q", len(lines), full.String())
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &row); err != nil {
		t.Fatalf("解析导出记录失败: %v (raw=%q)", err, lines[1])
	}
	gotReq, _ := row["request_content"].(string)
	gotResp, _ := row["response_content"].(string)
	if !strings.Contains(gotReq, "SECRET_PROMPT_MARKER") {
		t.Errorf("request_content 里没有原始正文（got=%q）：include_content=1 承诺的完整正文是空的，"+
			"排障人会把真实事故误判成客户端发了空请求", gotReq)
	}
	if !strings.Contains(gotResp, "SECRET_REPLY_MARKER") {
		t.Errorf("response_content 里没有原始正文（got=%q）", gotResp)
	}

	// ---- 场景 B：摘要导出（userSummary=true）→ 正文必须被清掉 ----
	var summary bytes.Buffer
	if err := RelayLogStreamExportNDJSON(ctx, &summary, nil, nil, 100, &model.RelayLogScope{}, true); err != nil {
		t.Fatalf("export summary: %v", err)
	}
	if strings.Contains(summary.String(), "SECRET_PROMPT_MARKER") ||
		strings.Contains(summary.String(), "SECRET_REPLY_MARKER") {
		t.Error("摘要导出里泄露了完整正文")
	}
}

// 列表页（默认 scope）仍然不能把正文取出来：一次几千行 × 几十 KB 会吃光内存，
// 而且列表根本不显示正文。这条防的是"修导出时顺手把列表也打开了"。
func TestRelayLogListStillOmitsContentByDefault(t *testing.T) {
	ctx := setupRelayLogTest(t)

	logs := []model.RelayLog{{
		ID: 9901, Time: 9901, RequestModelName: "kimi-k3", ChannelName: "primary", UserID: 1,
		RequestContent:  `{"marker":"LIST_SHOULD_NOT_CARRY_THIS"}`,
		ResponseContent: `{"marker":"LIST_SHOULD_NOT_CARRY_THIS"}`,
	}}
	if err := db.GetDB().Create(&logs).Error; err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := RelayLogList(ctx, nil, nil, 1, 10, &model.RelayLogScope{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d logs, want 1", len(got))
	}
	if got[0].RequestContent != "" || got[0].ResponseContent != "" {
		t.Fatalf("列表默认把正文带出来了（req=%q resp=%q）：几千行 × 几十 KB 会吃光内存",
			got[0].RequestContent, got[0].ResponseContent)
	}
}
