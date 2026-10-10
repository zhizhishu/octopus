package op

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
)

// relayLogScopeParityCorpus 是一组"专门用来挑刺"的日志行：每一行都盯着两套筛选规则里
// 某一处的边界（空串 vs 空白串、可空列、大小写、前后空格、家族前缀、LIKE 元字符…）。
//
// 背景：日志列表是**两条路**——内存缓存逐条走 relayLogMatchScope（Go），数据库走
// relayLogApplyScope（手写 SQL WHERE）。两套手写规则只要有一处语义不同，就会出现
// "缓存里的行筛得住、DB 里的行筛不住"（或反之），表现为：数字对不上、翻页丢行、
// 边筛边进新数据时页面内容漂移。
func relayLogScopeParityCorpus() []model.RelayLog {
	yes, no := true, false
	rows := []model.RelayLog{
		// --- severity 三桶的正常形态 ---
		{ID: 1001, Time: 1001, RequestModelName: "gpt-4o", RequestEndpoint: "chat", TotalAttempts: 1},
		{ID: 1002, Time: 1002, RequestModelName: "gpt-4o", RequestEndpoint: "chat", TotalAttempts: 0},
		{ID: 1003, Time: 1003, RequestModelName: "gpt-4o", RequestEndpoint: "chat", TotalAttempts: 2},
		{ID: 1004, Time: 1004, RequestModelName: "gpt-4o", RequestEndpoint: "chat", TotalAttempts: 7},
		{ID: 1005, Time: 1005, RequestModelName: "gpt-4o", RequestEndpoint: "chat", Error: "boom", TotalAttempts: 1},
		{ID: 1006, Time: 1006, RequestModelName: "gpt-4o", RequestEndpoint: "chat", ErrorCode: "upstream_x"},
		{ID: 1007, Time: 1007, RequestModelName: "gpt-4o", RequestEndpoint: "chat", ErrorStatus: 502, TotalAttempts: 4},
		{ID: 1008, Time: 1008, RequestModelName: "gpt-4o", RequestEndpoint: "chat", ErrorStatus: 200, TotalAttempts: 1},
		{ID: 1009, Time: 1009, RequestModelName: "gpt-4o", RequestEndpoint: "chat", ErrorCode: "0", TotalAttempts: 1},

		// --- 边界①：空白串错误信息。Go/前端都 TrimSpace 后判空 = 不是错误；
		// 若 SQL 侧直接写 <> '' 就会把这几行算成 error ⇒ 两套规则漂移。---
		{ID: 1101, Time: 1101, RequestModelName: "ws-model", RequestEndpoint: "chat", Error: "   ", TotalAttempts: 1},
		{ID: 1102, Time: 1102, RequestModelName: "ws-model", RequestEndpoint: "chat", ErrorCode: "  ", TotalAttempts: 2},
		{ID: 1104, Time: 1104, RequestModelName: "ws-model", RequestEndpoint: "chat", Error: " boom "},

		// --- 边界②：模型名前后带空格。Go 侧两边都 TrimSpace 后比；SQL 侧若只对参数
		// 做 LOWER(?) 而不修列，DB 里带空格的行就永远筛不到。---
		{ID: 1201, Time: 1201, RequestModelName: " spaced-model ", ActualModelName: "spaced-model", RequestEndpoint: "chat"},
		{ID: 1202, Time: 1202, RequestModelName: "spaced-model", ActualModelName: " spaced-model ", RequestEndpoint: "chat"},
		{ID: 1203, Time: 1203, RequestModelName: "  GPT-4O  ", ActualModelName: "gpt-4o", RequestEndpoint: "chat"},
		{ID: 1204, Time: 1204, RequestModelName: "Spaced-Model", RequestEndpoint: "chat"},
		// 两边都带空格：Go 侧 TrimSpace 后能匹配，SQL 侧若只 LOWER 不 TRIM 就永远匹配不到。
		{ID: 1205, Time: 1205, RequestModelName: " padded-both ", ActualModelName: " padded-both ", RequestEndpoint: "chat"},
		// 带空格的 gpt 名：Go 侧 TrimSpace 后按 basename 前缀判成 openai 供应商；
		// SQL 侧 provider 的 LIKE 若比原列，就会把同一行判成非 openai。
		{ID: 1206, Time: 1206, RequestModelName: " gpt-4o ", RequestEndpoint: "chat"},

		// --- 边界③：厂商前缀家族（含无斜杠的 <provider>_ 形态）---
		{ID: 1301, Time: 1301, RequestModelName: "openai_gpt", ActualModelName: "openai_gpt", RequestEndpoint: "chat"},
		{ID: 1302, Time: 1302, RequestModelName: "openai/gpt-4o", ActualModelName: "openai/gpt-4o", RequestEndpoint: "chat"},
		{ID: 1303, Time: 1303, RequestModelName: "meta-llama/Llama-3-70B", ActualModelName: "meta-llama/Llama-3-70B", RequestEndpoint: "chat"},
		{ID: 1304, Time: 1304, RequestModelName: "not-a-google/my-model", ActualModelName: "not-a-google/my-model", RequestEndpoint: "chat"},
		{ID: 1305, Time: 1305, RequestModelName: "z-ai/glm-4", ActualModelName: "z-ai/glm-4", RequestEndpoint: "chat"},
		{ID: 1306, Time: 1306, RequestModelName: "chatgpt-4o-latest", ActualModelName: "chatgpt-4o-latest", RequestEndpoint: "chat"},
		{ID: 1307, Time: 1307, RequestModelName: "x/google/gemini-1.5", ActualModelName: "x/google/gemini-1.5", RequestEndpoint: "chat"},

		// --- 边界④：端点家族 / model_test 探针 ---
		{ID: 1401, Time: 1401, RequestModelName: "ep-a", RequestEndpoint: "chat", TotalAttempts: 1},
		{ID: 1402, Time: 1402, RequestModelName: "ep-a", RequestEndpoint: "chat_completions", TotalAttempts: 1},
		{ID: 1403, Time: 1403, RequestModelName: "ep-a", RequestEndpoint: "messages", TotalAttempts: 1},
		{ID: 1404, Time: 1404, RequestModelName: "ep-a", RequestEndpoint: "responses", TotalAttempts: 1},
		{ID: 1405, Time: 1405, RequestModelName: "ep-a", RequestEndpoint: "model_test", TotalAttempts: 1},
		{ID: 1406, Time: 1406, RequestModelName: "ep-a", RequestEndpoint: "model_test_responses", TotalAttempts: 1},
		{ID: 1407, Time: 1407, RequestModelName: "ep-a", RequestEndpoint: "gemini_generate_content", TotalAttempts: 1},

		// --- 边界⑤：三态 upstream_model_mismatch ---
		{ID: 1501, Time: 1501, RequestModelName: "mm", RequestEndpoint: "chat", UpstreamModelMismatch: &yes},
		{ID: 1502, Time: 1502, RequestModelName: "mm", RequestEndpoint: "chat", UpstreamModelMismatch: &no},
		{ID: 1503, Time: 1503, RequestModelName: "mm", RequestEndpoint: "chat"},

		// --- 边界⑥：search 的 LIKE 元字符必须按字面匹配，不能当通配符 ---
		{ID: 1601, Time: 1601, RequestModelName: "gpt-4o", RequestEndpoint: "chat", Error: `upload 100%_done\path`},
		{ID: 1602, Time: 1602, RequestModelName: "gpt-4o", RequestEndpoint: "chat", Error: "upload 100XadoneZpath"},
		{ID: 1603, Time: 1603, RequestModelName: "gpt-4o", RequestEndpoint: "chat", SessionKey: "sess-lf-abc-123"},
	}
	return rows
}

// TestRelayLogScopeRuleParity 把**同一批 DB 行**分别喂给两套规则：
//   - Go 侧：relayLogMatchScope（内存缓存走的那个函数，handlers 的 SSE 尾巴也用它）
//   - SQL 侧：relayLogApplyScope（数据库分页拼的 WHERE）
//
// 两边必须给出**完全相同的 id 集合**。不等就是规则漂移，测试直接把分歧行的字段打出来。
func TestRelayLogScopeRuleParity(t *testing.T) {
	ctx := setupRelayLogTest(t)

	corpus := relayLogScopeParityCorpus()
	if err := db.GetDB().WithContext(ctx).Create(&corpus).Error; err != nil {
		t.Fatalf("create corpus: %v", err)
	}
	// 可空列陷阱：直接插一行 NULL 的 error/error_code/total_attempts。
	// NOT (NULL OR ...) 是 NULL，会把行从三个桶里全部静默丢掉，所以 COALESCE 是必须的。
	if err := db.GetDB().WithContext(ctx).Exec(
		`INSERT INTO relay_logs (id, time, request_endpoint, request_model_name, error, error_code, total_attempts)
		 VALUES (?, ?, ?, ?, NULL, NULL, NULL)`, 1701, 1701, "chat", "null-model").Error; err != nil {
		t.Fatalf("insert null row: %v", err)
	}

	// Go 侧的输入就是 DB 里的行：只是换个规则函数过一遍。
	var all []model.RelayLog
	if err := db.GetDB().WithContext(ctx).Order("id ASC").Find(&all).Error; err != nil {
		t.Fatalf("load rows: %v", err)
	}

	yes, no := true, false
	scopes := []struct {
		name  string
		scope *model.RelayLogScope
	}{
		{"nil scope", nil},
		{"severity=error", &model.RelayLogScope{Severity: "error"}},
		{"severity=warn", &model.RelayLogScope{Severity: "warn"}},
		{"severity=success", &model.RelayLogScope{Severity: "success"}},
		{"retried", &model.RelayLogScope{RetriedOnly: true}},
		{"warn+retried", &model.RelayLogScope{Severity: "warn", RetriedOnly: true}},
		{"error+retried", &model.RelayLogScope{Severity: "error", RetriedOnly: true}},
		{"success+retried", &model.RelayLogScope{Severity: "success", RetriedOnly: true}},
		{"endpoint=chat", &model.RelayLogScope{Endpoint: "chat"}},
		{"endpoint=messages", &model.RelayLogScope{Endpoint: "messages"}},
		{"endpoint=model_test", &model.RelayLogScope{Endpoint: "model_test"}},
		{"endpoint=gemini", &model.RelayLogScope{Endpoint: "gemini"}},
		{"hide_model_test", &model.RelayLogScope{HideModelTest: true}},
		{"model=spaced-model", &model.RelayLogScope{Model: "spaced-model"}},
		{"model=padded-both", &model.RelayLogScope{Model: "padded-both"}},
		{"model=trimmmed-input", &model.RelayLogScope{Model: "  spaced-model  "}},
		{"model=gpt-4o", &model.RelayLogScope{Model: "gpt-4o"}},
		{"provider=openai", &model.RelayLogScope{Provider: "openai"}},
		{"provider=anthropic", &model.RelayLogScope{Provider: "anthropic"}},
		{"provider=google", &model.RelayLogScope{Provider: "google"}},
		{"provider=meta", &model.RelayLogScope{Provider: "meta"}},
		{"provider=zhipuai", &model.RelayLogScope{Provider: "zhipuai"}},
		{"provider=unknown", &model.RelayLogScope{Provider: "unknown-provider-xyz"}},
		{"search=boom", &model.RelayLogScope{Search: "boom"}},
		{"search=100%_done", &model.RelayLogScope{Search: `100%_done\path`}},
		{"search=spaced", &model.RelayLogScope{Search: "spaced"}},
		{"search=sess-lf", &model.RelayLogScope{Search: "sess-lf"}},
		{"search=1601", &model.RelayLogScope{Search: "1601"}},
		{"mismatch=true", &model.RelayLogScope{UpstreamModelMismatch: &yes}},
		{"mismatch=false", &model.RelayLogScope{UpstreamModelMismatch: &no}},
		{"error+endpoint=chat", &model.RelayLogScope{Severity: "error", Endpoint: "chat"}},
		{"warn+model=ws-model", &model.RelayLogScope{Severity: "warn", Model: "ws-model"}},
		{"error+search=boom", &model.RelayLogScope{Severity: "error", Search: "boom"}},
		{"success+provider=openai", &model.RelayLogScope{Severity: "success", Provider: "openai"}},
		{"hide_model_test+error", &model.RelayLogScope{HideModelTest: true, Severity: "error"}},
	}

	byID := make(map[int64]model.RelayLog, len(all))
	for _, row := range all {
		byID[row.ID] = row
	}

	drifts := 0
	for _, tc := range scopes {
		want := map[int64]bool{}
		for _, row := range all {
			if relayLogMatchScope(row, tc.scope) {
				want[row.ID] = true
			}
		}

		got := map[int64]bool{}
		var ids []int64
		q := relayLogApplyScope(db.GetDB().WithContext(ctx).Model(&model.RelayLog{}), tc.scope)
		if err := q.Pluck("id", &ids).Error; err != nil {
			t.Fatalf("%s: pluck ids: %v", tc.name, err)
		}
		for _, id := range ids {
			got[id] = true
		}

		var diffs []int64
		for id := range want {
			if !got[id] {
				diffs = append(diffs, id)
			}
		}
		for id := range got {
			if !want[id] {
				diffs = append(diffs, id)
			}
		}
		if len(diffs) == 0 {
			continue
		}
		drifts++
		sort.Slice(diffs, func(i, j int) bool { return diffs[i] < diffs[j] })
		t.Errorf("规则漂移 scope=%s: Go/SQL 结果不一致, 分歧 id=%v", tc.name, diffs)
		for _, id := range diffs {
			row, ok := byID[id]
			if !ok {
				t.Errorf("  id=%d SQL 侧命中但 Go 侧根本没有这一行", id)
				continue
			}
			t.Errorf("  id=%d inGo=%v inSQL=%v row=%s",
				id, want[id], got[id], describeParityRow(row))
		}
	}

	if drifts > 0 {
		t.Errorf("两套筛选规则共 %d 个 scope 出现漂移（Go=relayLogMatchScope vs SQL=relayLogApplyScope）", drifts)
	}
}

func describeParityRow(row model.RelayLog) string {
	mismatch := "nil"
	if row.UpstreamModelMismatch != nil {
		mismatch = fmt.Sprintf("%v", *row.UpstreamModelMismatch)
	}
	return fmt.Sprintf(
		"severity=%s error=%q error_code=%q error_status=%d total_attempts=%d endpoint=%q req_model=%q actual_model=%q mismatch=%s",
		relayLogSeverityValue(row), row.Error, row.ErrorCode, row.ErrorStatus, row.TotalAttempts,
		row.RequestEndpoint, row.RequestModelName, row.ActualModelName, mismatch)
}

// TestRelayLogSeverityCountsParity 锁死"徽章数字"与"列表内容"必须是同一个宇宙：
// RelayLogSeverityCounts 走 SQL 计数，RelayLogList 走 SQL+缓存合并，两边对同一份数据
// 必须给出同样的三桶划分（每个 scope 下 success+warn+error == total == 匹配行数）。
func TestRelayLogSeverityCountsParity(t *testing.T) {
	ctx := setupRelayLogTest(t)

	corpus := relayLogScopeParityCorpus()
	if err := db.GetDB().WithContext(ctx).Create(&corpus).Error; err != nil {
		t.Fatalf("create corpus: %v", err)
	}

	counts, err := RelayLogSeverityCounts(ctx, nil, nil, nil)
	if err != nil {
		t.Fatalf("severity counts: %v", err)
	}

	goBuckets := map[string]int64{}
	var rows []model.RelayLog
	if err := db.GetDB().WithContext(ctx).Find(&rows).Error; err != nil {
		t.Fatalf("load rows: %v", err)
	}
	for _, row := range rows {
		goBuckets[relayLogSeverityValue(row)]++
	}

	if counts.Success != goBuckets["success"] || counts.Warn != goBuckets["warn"] || counts.Error != goBuckets["error"] {
		t.Errorf("徽章计数与 Go 规则分桶不一致: SQL(success=%d warn=%d error=%d) Go(success=%d warn=%d error=%d)",
			counts.Success, counts.Warn, counts.Error,
			goBuckets["success"], goBuckets["warn"], goBuckets["error"])
	}
	if counts.Total != counts.Success+counts.Warn+counts.Error {
		t.Errorf("三桶之和 %d != total %d（分类不互斥/不完备）",
			counts.Success+counts.Warn+counts.Error, counts.Total)
	}
	if counts.Total != int64(len(rows)) {
		t.Errorf("徽章 total=%d 与全表行数 %d 不一致", counts.Total, len(rows))
	}
}

// TestRelayLogListMatchesCountPerSeverity 锁"分页列表"与"计数"在同一 scope 下同一结果：
// 列表按页取完的 id 集合，必须与该 severity 的 SQL 计数完全相等——这是"数字对不上、
// 最后一页点不到"这类投诉的根因测试。
func TestRelayLogListMatchesCountPerSeverity(t *testing.T) {
	ctx := setupRelayLogTest(t)

	corpus := relayLogScopeParityCorpus()
	if err := db.GetDB().WithContext(ctx).Create(&corpus).Error; err != nil {
		t.Fatalf("create corpus: %v", err)
	}

	for _, severity := range []string{"success", "warn", "error"} {
		scope := &model.RelayLogScope{Severity: severity}
		want, err := RelayLogCount(ctx, nil, nil, scope)
		if err != nil {
			t.Fatalf("count %s: %v", severity, err)
		}

		seen := map[int64]bool{}
		const pageSize = 3 // 故意小于桶大小，逼出跨页合并/去重路径
		for page := 1; page <= 200; page++ {
			logs, err := RelayLogList(ctx, nil, nil, page, pageSize, scope)
			if err != nil {
				t.Fatalf("list %s page %d: %v", severity, page, err)
			}
			if len(logs) == 0 {
				break
			}
			for _, l := range logs {
				if seen[l.ID] {
					t.Errorf("severity=%s 翻页出现重复行 id=%d (page=%d)", severity, l.ID, page)
				}
				seen[l.ID] = true
				if got := relayLogSeverityValue(l); got != severity {
					t.Errorf("severity=%s 列表里混进了 %s 的行 id=%d (page=%d)", severity, got, l.ID, page)
				}
			}
		}
		if int64(len(seen)) != want {
			t.Errorf("severity=%s: 列表翻完 %d 行, 但计数说 %d 行 → 数字与内容不一致", severity, len(seen), want)
		}
	}
}

var _ = context.Background

// TestRelayLogSeverityTabOnlyResidualDifference 记录两套规则**仅剩**的一处差异，钉住它
// 而不是假装不存在——有人哪天顺手修了它会看到这个测试红，从而知道要同步改这边。
//
// 差异：Go 的 strings.TrimSpace / 前端的 String.trim() 都吃掉 \t \n \r \f \v，而
// SQL 的 TRIM()（SQLite/MySQL/Postgres 一致）只吃空格。所以一个字面值为 "\t\n" 的
// error 会被 Go/前端判成"成功"、被 SQL 判成"错误"。
//
// 为什么现在不修：没有任何生产路径能写出这种值——上游 body 抽出来的 code/msg 全部过
// stringValue()→TrimSpace，抽不出东西时退回的是 octopus 自己的模板文案（
// "Upstream request failed (status %d, code %s)."），本地错误也都是有字的模板。
// 三个数据库方言也没有可移植的"去全部空白"表达式（Postgres 要 btrim(x,E' \t\n')、
// MySQL 要 TRIM(BOTH '\t' FROM x)、SQLite 要 char(9)，写法互不兼容）。
//
// ponytail: 天花板 = 手写 SQL 谓词天生只能逼近 Go 的字符串语义。
// 升级路径 = 落一列由 relayLogSeverityValue 唯一计算出来的 severity 持久列，
// 过滤/计数都改成 severity = ?：这类差异整类消失，顺带让 severity 过滤走索引
// （relayLogApplyScope 上方那段注释里抱怨的 ~64s severity=success 慢页也正是它）。
func TestRelayLogSeverityTabOnlyResidualDifference(t *testing.T) {
	ctx := setupRelayLogTest(t)

	row := model.RelayLog{ID: 2001, Time: 2001, RequestModelName: "tab-only", Error: "\t\n", TotalAttempts: 1}
	if err := db.GetDB().WithContext(ctx).Create(&row).Error; err != nil {
		t.Fatalf("create row: %v", err)
	}

	goSev := relayLogSeverityValue(row)
	if goSev != "success" {
		t.Errorf("Go 侧应把 tab-only 错误当成功: got %q", goSev)
	}

	var errorIDs []int64
	q := relayLogApplyScope(db.GetDB().WithContext(ctx).Model(&model.RelayLog{}),
		&model.RelayLogScope{Severity: "error"})
	if err := q.Pluck("id", &errorIDs).Error; err != nil {
		t.Fatalf("pluck: %v", err)
	}
	sqlSaysError := len(errorIDs) == 1 && errorIDs[0] == 2001
	if !sqlSaysError {
		t.Fatalf("SQL 侧现在把 tab-only 判成什么变了 (error ids=%v)。"+
			"如果你刚修好了 whitespace 语义，请删除本测试并同步 relayLogErrorSQLCond 的注释", errorIDs)
	}
	t.Logf("已知残留差异（不可达，见函数注释）: Go=%s vs SQL=error, id=%d", goSev, row.ID)
}
