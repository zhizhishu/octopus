package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modelverify"
	"github.com/zhizhishu/octopus/internal/modelverify/numberdist"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/server/router"
)

// 本文件验收 hlwy 对照在**传输层**的边界：严格 DTO、离线重对照不写库、
// 计划接口一个上游请求都不发、执行循环该停就停。

// hlwyHTTPEnv 建一个带库带缓存的环境（hlwy 入队要读开关与渠道）。
func hlwyHTTPEnv(t *testing.T) (context.Context, *model.Channel) {
	t.Helper()
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "hlwy_http.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	if err := op.InitCache(); err != nil {
		t.Fatalf("init cache: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	ch := model.Channel{Name: "hlwy-http", Enabled: true, Model: "gpt-x"}
	if err := op.ChannelCreate(&ch, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	return ctx, &ch
}

// hlwyRoutes 把本文件要打的几条路由挂到测试用的 engine 上。
//
// 这里刻意重复一次路由表（而不是调 RegisterAll），是为了让「鉴权是否挂上」
// 与「handler 行为」分开验：本 engine 故意不带中间件，鉴权由
// TestHlwyRoutesRequireAuth 用真实路由表去验。
func hlwyRoutes() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/api/v1/model-audit")
	g.GET("/hlwy-baselines", listHlwyBaselines)
	g.POST("/hlwy-baselines/import", importHlwyBaseline)
	g.POST("/hlwy-baselines/from-result", createHlwyBaselineFromResult)
	g.POST("/hlwy-compare", compareHlwyResult)
	g.POST("/enqueue", enqueueModelAudit)
	g.GET("/plan", getModelAuditPlan)
	return r
}

// hlwyDo 发一个 JSON 请求（body 为 nil 时发空体）。
func hlwyDo(r *gin.Engine, method, path string, body []byte) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// hlwyDoJSON 把 map 编成 JSON 再发。
func hlwyDoJSON(t *testing.T, r *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return hlwyDo(r, method, path, raw)
}

// hlwyEnqueueBody 是一份**完全合法**的 hlwy 入队参数（判定点靠后面的用例去改坏）。
func hlwyEnqueueBody(channelID int, baselineID string) map[string]any {
	return map[string]any{
		"channel_id": channelID,
		"kind":       model.AuditKindHlwy,
		"models":     []string{"gpt-x"},
		"hlwy": map[string]any{
			"samples":     model.HlwyDefaultSamples,
			"baseline_id": baselineID,
			"protocol_id": numberdist.ProtocolID,
		},
	}
}

// TestHlwyRoutesRequireAuth 守住「新路由必须挂在既有的鉴权组里」。
//
// 用真实路由表（router.RegisterAll）发真实请求：未登录必须被拒，普通用户必须
// 403（挂进组不等于挂了 AdminOnly）。它同时确认路由确实注册上了——否则下面的
// 404 会假装成「被拒」。
func TestHlwyRoutesRequireAuth(t *testing.T) {
	// 用既有的真实鉴权夹具（临时库 + 真 JWT），不新造测试 provider：这样下面那句
	// 「非管理员必须 403」验的是生产上同一条 Auth + AdminOnly 链。
	ctx := setupSettingAuthTestDB(t)
	seedSettingAuthSiteSecrets(t, ctx)
	plain, err := op.UserCreate(model.UserCreateRequest{
		Username: "hlwy-plain", Password: "password", Role: model.UserRoleUser, Status: model.UserStatusActive,
	}, ctx)
	if err != nil {
		t.Fatalf("create non-admin user: %v", err)
	}
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	if err := router.RegisterAll(engine); err != nil {
		t.Fatalf("register routes: %v", err)
	}
	registered := map[string]bool{}
	for _, r := range engine.Routes() {
		registered[r.Method+" "+r.Path] = true
	}
	type route struct{ method, path string }
	wanted := []route{
		{http.MethodGet, "/api/v1/model-audit/hlwy-baselines"},
		{http.MethodPost, "/api/v1/model-audit/hlwy-baselines/import"},
		{http.MethodPost, "/api/v1/model-audit/hlwy-baselines/from-result"},
		{http.MethodPost, "/api/v1/model-audit/hlwy-compare"},
	}
	for _, w := range wanted {
		if !registered[w.method+" "+w.path] {
			t.Fatalf("路由没注册上: %s %s", w.method, w.path)
		}
		rec := hlwyDo(engine, w.method, w.path, []byte(`{}`))
		if rec.Code == http.StatusOK {
			t.Errorf("%s %s 未登录返回 200：该接口没有鉴权（body: %.200s）",
				w.method, w.path, rec.Body.String())
			continue
		}
		switch rec.Code {
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
		default:
			t.Errorf("%s %s 未登录返回 %d，期望被拒（400/401/403）",
				w.method, w.path, rec.Code)
		}
	}
	// 重对照必须只有一条入口：不允许悄悄多出 /recompare 之类的别名。
	for key := range registered {
		if strings.Contains(key, "/model-audit/") && strings.Contains(key, "recompare") {
			t.Errorf("不该存在额外的重对照路由: %s", key)
		}
	}
	// 挂进组不等于挂了 AdminOnly：普通用户的登录态也必须在这些接口上被拒。
	for _, w := range wanted {
		body := []byte(`{}`)
		if w.method == http.MethodGet {
			body = nil
		}
		if rec := doSettingAuthRequest(engine, w.method, w.path, jwtBearer(t, plain), body); rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s 非管理员 = %d, want 403 (body=%.200s)",
				w.method, w.path, rec.Code, rec.Body.String())
		}
	}
}

// TestHlwyEnqueueStrictDTO 守住入参的「不许猜」：未知字段、混用参数、
// 缺采样次数全部拒掉，且报错要能指到具体字段。
func TestHlwyEnqueueStrictDTO(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := hlwyRoutes()

	cases := []struct {
		name string
		body any
		want string
	}{
		{"未知字段", map[string]any{
			"channel_id": 1, "kind": model.AuditKindHlwy, "models": []string{"gpt-x"},
			"hlwy":       map[string]any{"samples": 100, "baseline_id": "b", "protocol_id": numberdist.ProtocolID},
			"candidates": []string{"b"},
		}, "candidates"},
		{"夹带探针", map[string]any{
			"channel_id": 1, "kind": model.AuditKindHlwy, "models": []string{"gpt-x"},
			"hlwy":   map[string]any{"samples": 100, "baseline_id": "b", "protocol_id": numberdist.ProtocolID},
			"probes": []string{"liveness"},
		}, "probes"},
		{"夹带日志", map[string]any{
			"channel_id": 1, "kind": model.AuditKindHlwy, "models": []string{"gpt-x"},
			"hlwy":   map[string]any{"samples": 100, "baseline_id": "b", "protocol_id": numberdist.ProtocolID},
			"log_id": 9,
		}, "log_id"},
		{"缺采样次数", map[string]any{
			"channel_id": 1, "kind": model.AuditKindHlwy, "models": []string{"gpt-x"},
			"hlwy": map[string]any{"baseline_id": "b", "protocol_id": numberdist.ProtocolID},
		}, "samples 必须显式指定"},
		{"采样次数越界", map[string]any{
			"channel_id": 1, "kind": model.AuditKindHlwy, "models": []string{"gpt-x"},
			"hlwy": map[string]any{"samples": 150, "baseline_id": "b", "protocol_id": numberdist.ProtocolID},
		}, "采样次数只能"},
		{"种类与参数不符", map[string]any{
			"channel_id": 1, "kind": model.AuditKindProbe, "models": []string{"gpt-x"},
			"hlwy": map[string]any{"samples": 100},
		}, "hlwy 参数需要 kind=hlwy_compare"},
		{"未知种类", map[string]any{
			"channel_id": 1, "kind": "hlwy_typo", "models": []string{"gpt-x"},
		}, "未知的检测任务种类"},
		{"缺协议", map[string]any{
			"channel_id": 1, "kind": model.AuditKindHlwy, "models": []string{"gpt-x"},
			"hlwy": map[string]any{"samples": 100, "baseline_id": "b"},
		}, "不支持的协议"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := hlwyDoJSON(t, r, http.MethodPost, "/api/v1/model-audit/enqueue", c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), c.want) {
				t.Fatalf("body = %s, want 含 %q", rec.Body.String(), c.want)
			}
		})
	}

	// 两个 JSON 文档串在一起必须被拒：否则第二个文档里的字段会被静默忽略。
	raw := `{"channel_id":1,"kind":"hlwy_compare","models":["gpt-x"],` +
		`"hlwy":{"samples":100,"baseline_id":"b","protocol_id":"` + numberdist.ProtocolID + `"}}` + `{}`
	rec := hlwyDo(r, http.MethodPost, "/api/v1/model-audit/enqueue", []byte(raw))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("尾随 JSON status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestHlwyEnqueueGateOffOverHTTP 守住开关键：关着的时候连渠道都不查、一行不建。
func TestHlwyEnqueueGateOffOverHTTP(t *testing.T) {
	ctx, ch := hlwyHTTPEnv(t)
	gin.SetMode(gin.TestMode)
	r := hlwyRoutes()

	rBefore := int64(0)
	db.GetDB().WithContext(ctx).Model(&model.ModelAuditResult{}).Count(&rBefore)
	rec := hlwyDoJSON(t, r, http.MethodPost, "/api/v1/model-audit/enqueue",
		hlwyEnqueueBody(ch.ID, "hlwy:whatever@000000000000"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), string(model.SettingKeyModelAuditHlwyEnabled)) {
		t.Fatalf("body = %s, want 提到开关 key", rec.Body.String())
	}
	rAfter := int64(0)
	db.GetDB().WithContext(ctx).Model(&model.ModelAuditResult{}).Count(&rAfter)
	if rBefore != rAfter {
		t.Fatalf("关着的时候不该建结果行: %d → %d", rBefore, rAfter)
	}
}

// TestHlwyPlanContract 守住计划接口的对外形状、口径与「零上游请求」。
//
// 页面按下「开始」的条件就是这份计划里的 enabled/max_calls/timeout_seconds：
// 它们必须出现在 data 顶层。曾经它们被塞进一个嵌套的 hlwy 字段，页面读到的
// enabled 永远是 undefined，提交键因此永久置灰——这不是样式问题，是接口说错话。
func TestHlwyPlanContract(t *testing.T) {
	ctx, ch := hlwyHTTPEnv(t)
	gin.SetMode(gin.TestMode)
	r := hlwyRoutes()

	saved, err := op.HlwyBaselineImport(ctx, hlwyHTTPLegacyBaseline(t, "plan-base", hlwyHTTPNumbers(100, 91)), time.Now().UTC())
	if err != nil {
		t.Fatalf("import baseline: %v", err)
	}

	// 把发包工厂换成一个「只要被调用就记一笔」的探针。
	var sends int32
	origFactory := hlwyNewSender
	hlwyNewSender = func(channel model.Channel, upstreamModel, endpoint string) (modelverify.Sender, error) {
		atomic.AddInt32(&sends, 1)
		return &hlwyFakeSender{}, nil
	}
	defer func() { hlwyNewSender = origFactory }()

	type plan struct {
		Kind                 string               `json:"kind"`
		Enabled              *bool                `json:"enabled"`
		Samples              int                  `json:"samples"`
		MaxCalls             int                  `json:"max_calls"`
		TimeoutSeconds       int                  `json:"timeout_seconds"`
		SampleTimeoutSeconds int                  `json:"sample_timeout_seconds"`
		ProtocolID           string               `json:"protocol_id"`
		Baseline             op.HlwyBaselineEntry `json:"baseline"`
		ConditionsNote       string               `json:"conditions_note"`
	}
	readPlan := func(t *testing.T, path string) plan {
		t.Helper()
		rec := hlwyDo(r, http.MethodGet, path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
		}
		var env struct {
			Data plan `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
		}
		if env.Data.Enabled == nil {
			t.Fatalf("data.enabled 缺失：页面会因此永久置灰提交键 (body=%s)", rec.Body.String())
		}
		return env.Data
	}
	base := "/api/v1/model-audit/plan?kind=" + model.AuditKindHlwy +
		"&channel_id=" + strconv.Itoa(ch.ID) + "&model=gpt-x&baseline_id=" + saved.Baseline.ID

	// 开关默认关着：计划照实给出，enabled=false（页面据此置灰，而不是报错）。
	p100 := readPlan(t, base+"&samples=100")
	if *p100.Enabled {
		t.Fatalf("默认关着，plan 里 enabled=true")
	}
	if p100.Kind != model.AuditKindHlwy {
		t.Fatalf("kind = %q, want %q", p100.Kind, model.AuditKindHlwy)
	}
	if p100.Samples != model.HlwyDefaultSamples || p100.MaxCalls != model.HlwyDefaultSamples {
		t.Fatalf("samples=%d max_calls=%d", p100.Samples, p100.MaxCalls)
	}
	if p100.TimeoutSeconds != 1300 {
		t.Fatalf("100 格整单超时 = %d, want 1300", p100.TimeoutSeconds)
	}
	if p100.SampleTimeoutSeconds != 60 {
		t.Fatalf("单格超时 = %d, want 60", p100.SampleTimeoutSeconds)
	}
	if p100.ProtocolID != numberdist.ProtocolID {
		t.Fatalf("protocol_id = %q, want %q", p100.ProtocolID, numberdist.ProtocolID)
	}
	if p100.Baseline.ID != saved.Baseline.ID || p100.Baseline.ValidCount == 0 {
		t.Fatalf("baseline = %+v", p100.Baseline)
	}
	if strings.TrimSpace(p100.ConditionsNote) == "" {
		t.Fatalf("conditions_note 不能为空")
	}

	p200 := readPlan(t, base+"&samples=200")
	if p200.Samples != model.HlwyMaxSamples || p200.MaxCalls != model.HlwyMaxSamples {
		t.Fatalf("samples=%d max_calls=%d", p200.Samples, p200.MaxCalls)
	}
	if p200.TimeoutSeconds != 1800 {
		t.Fatalf("200 格整单超时 = %d, want 1800", p200.TimeoutSeconds)
	}
	if n := atomic.LoadInt32(&sends); n != 0 {
		t.Fatalf("/plan 发了 %d 次上游请求，必须为 0", n)
	}

	// 打开开关之后同一接口仍然零请求，只是 enabled 变 true。
	if err := op.SettingSetString(model.SettingKeyModelAuditHlwyEnabled, "true"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if p := readPlan(t, base+"&samples=100"); !*p.Enabled {
		t.Fatalf("打开开关后 plan 应 enabled=true")
	}
	if n := atomic.LoadInt32(&sends); n != 0 {
		t.Fatalf("/plan 发了 %d 次上游请求，必须为 0", n)
	}

	// 计划与入队同一套口径：缺参数、未知种类、未知基准都不给出「可以开始」的假象，
	// 否则人点下去才发现被拒，白等一场。
	channelQuery := "&channel_id=" + strconv.Itoa(ch.ID)
	bad := []struct {
		name string
		path string
		want int
	}{
		{"缺 samples", base, http.StatusBadRequest},
		{"不支持的采样次数", base + "&samples=50", http.StatusBadRequest},
		{"缺基准", "/api/v1/model-audit/plan?kind=" + model.AuditKindHlwy + channelQuery + "&model=gpt-x&samples=100", http.StatusBadRequest},
		{"未知基准", "/api/v1/model-audit/plan?kind=" + model.AuditKindHlwy + channelQuery + "&model=gpt-x&samples=100&baseline_id=hlwy:nope@000000000000", http.StatusNotFound},
		{"缺模型", "/api/v1/model-audit/plan?kind=" + model.AuditKindHlwy + channelQuery + "&samples=100&baseline_id=" + saved.Baseline.ID, http.StatusBadRequest},
		{"未知种类", "/api/v1/model-audit/plan?kind=nope" + channelQuery + "&model=gpt-x", http.StatusBadRequest},
	}
	for _, c := range bad {
		rec := hlwyDo(r, http.MethodGet, c.path, nil)
		if rec.Code != c.want {
			t.Fatalf("%s: status = %d, want %d (body=%s)", c.name, rec.Code, c.want, rec.Body.String())
		}
	}

	// 旧的多探针计划一字不变：调用方拿到的还是原来那几个字段，且不再有嵌套 hlwy。
	rec := hlwyDo(r, http.MethodGet, "/api/v1/model-audit/plan?channel_id="+strconv.Itoa(ch.ID)+"&model=gpt-x", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy plan status = %d", rec.Code)
	}
	var legacy struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &legacy); err != nil {
		t.Fatalf("decode legacy: %v", err)
	}
	for _, key := range []string{"protocol", "model", "probes", "quick", "full"} {
		if _, ok := legacy.Data[key]; !ok {
			t.Fatalf("旧计划缺字段 %q: %s", key, rec.Body.String())
		}
	}
	if _, ok := legacy.Data["hlwy"]; ok {
		t.Fatalf("旧计划不该再有嵌套的 hlwy 字段: %s", rec.Body.String())
	}
}

// hlwyFakeSender 是只按键回答的假发送器（测试里不发任何真请求）。
type hlwyFakeSender struct {
	ask func(ctx context.Context, req modelverify.Ask) (modelverify.Response, error)
}

func (f *hlwyFakeSender) Ask(ctx context.Context, req modelverify.Ask) (modelverify.Response, error) {
	if f.ask == nil {
		return modelverify.Response{Content: "217", FinishReason: "stop"}, nil
	}
	return f.ask(ctx, req)
}

// hlwyHTTPLegacyBaseline 造一份旧格式基准文件（对象形态）。
func hlwyHTTPLegacyBaseline(t *testing.T, name string, numbers []int) []byte {
	t.Helper()
	bins := make([]float64, numberdist.DistLength)
	counts := map[int]int{}
	for _, n := range numbers {
		counts[n]++
	}
	for n, c := range counts {
		bins[n-1] = float64(c) / float64(len(numbers))
	}
	body := map[string]any{
		"name": name, "model": name, "iterations": len(numbers),
		"rawData": numbers, "distribution": bins,
	}
	out, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal baseline: %v", err)
	}
	return out
}

// hlwyHTTPSeedDone 落一条已完成的 hlwy 运行（带自洽报告），返回结果行与它用的基准。
func hlwyHTTPSeedDone(t *testing.T, ctx context.Context, ch *model.Channel, numbers []int) (model.ModelAuditResult, numberdist.Baseline) {
	t.Helper()
	saved, err := op.HlwyBaselineImport(ctx, hlwyHTTPLegacyBaseline(t, "cap-a", numbers), time.Now().UTC())
	if err != nil {
		t.Fatalf("import baseline: %v", err)
	}
	base, err := op.HlwyBaselineLoad(ctx, saved.Baseline.ID)
	if err != nil {
		t.Fatalf("load baseline: %v", err)
	}
	cmp, err := numberdist.Compare(numbers, []numberdist.Baseline{base}, base.ID)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	bc := model.HlwyBaselineComparison{
		SchemaVersion:     numberdist.SchemaVersion,
		AlgorithmVersion:  numberdist.AlgorithmVer,
		Execution:         model.HlwyExecutionComplete,
		Sufficiency:       model.HlwySufficiency(len(numbers)),
		Comparability:     model.HlwyComparabilityUnknown,
		ReasonCodes:       []string{},
		Counts:            model.HlwyCounts{PlannedCalls: model.HlwyDefaultSamples, StartedSlots: len(numbers), Valid: len(numbers)},
		RawNumbers:        numbers,
		Conditions:        op.HlwyUnknownConditions("openai-chat"),
		BaselineSnapshots: []numberdist.Baseline{model.HlwySnapshotOf(base)},
		Scorable:          model.HlwyScorable(len(numbers), model.HlwyComparabilityUnknown),
		Reason:            cmp.Reason,
		ValidCount:        cmp.ValidCount,
		Distribution:      cmp.Distribution,
		Stats:             cmp.Stats,
		ChosenBaselineID:  cmp.ChosenBaselineID,
		Matches:           cmp.Matches,
		Quality:           model.HlwyQuality{StrictInteger: len(numbers)},
	}
	rep := model.HlwyAuditReport{
		Kind:               model.AuditKindHlwy,
		RequestedModel:     "gpt-x",
		UpstreamModel:      "gpt-x",
		Verdict:            model.HlwyVerdictUnknown,
		Plain:              model.HlwyPlainOf(bc, false),
		BaselineComparison: bc,
	}.Filled()
	encoded, err := rep.Encode()
	if err != nil {
		t.Fatalf("encode report: %v", err)
	}
	opts := model.HlwyOptions{
		Samples:    model.HlwyDefaultSamples,
		BaselineID: base.ID,
		ProtocolID: numberdist.ProtocolID,
		Candidates: []numberdist.Baseline{model.HlwySnapshotOf(base)},
	}
	optsJSON, err := opts.Encode()
	if err != nil {
		t.Fatalf("encode options: %v", err)
	}
	now := time.Now().UTC()
	job := model.ModelAuditJob{
		ChannelID: ch.ID, ChannelName: ch.Name, Status: model.AuditJobDone,
		Total: 1, Finished: 1, Kind: model.AuditKindHlwy, Trigger: "manual",
		OptionsJSON: optsJSON, CreatedAt: now,
	}
	if err := db.GetDB().Create(&job).Error; err != nil {
		t.Fatalf("create job: %v", err)
	}
	res := model.ModelAuditResult{
		JobID: job.ID, ChannelID: ch.ID, ChannelName: ch.Name,
		Model: "gpt-x", UpstreamModel: "gpt-x", Status: model.AuditResultDone,
		Report: encoded, Attempt: 1, FinishedAt: &now, CreatedAt: now,
	}
	if err := db.GetDB().Create(&res).Error; err != nil {
		t.Fatalf("create result: %v", err)
	}
	return res, base
}

// TestHlwyCompareWritesNothing 守住重对照的「只读」语义。
//
// 它是这个功能里唯一能“随意换基准看分数”的入口。一旦它偷偷建任务/写报告，
// 就等于开了一个绕过预算与去重的后门。
func TestHlwyCompareWritesNothing(t *testing.T) {
	ctx, ch := hlwyHTTPEnv(t)
	res, base := hlwyHTTPSeedDone(t, ctx, ch, hlwyHTTPNumbers(100, 71))
	gin.SetMode(gin.TestMode)
	r := hlwyRoutes()

	var jobsBefore, resultsBefore int64
	db.GetDB().WithContext(ctx).Model(&model.ModelAuditJob{}).Count(&jobsBefore)
	db.GetDB().WithContext(ctx).Model(&model.ModelAuditResult{}).Count(&resultsBefore)
	var before model.ModelAuditResult
	if err := db.GetDB().WithContext(ctx).Where("id = ?", res.ID).First(&before).Error; err != nil {
		t.Fatalf("reload before: %v", err)
	}

	rec := hlwyDoJSON(t, r, http.MethodPost, "/api/v1/model-audit/hlwy-compare", map[string]any{
		"result_id":   res.ID,
		"baseline_id": base.ID,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	var env struct {
		Data struct {
			Report model.HlwyAuditReport `json:"report"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if env.Data.Report.BaselineComparison.ChosenBaselineID != base.ID {
		t.Fatalf("重算应使用请求指定的基准: %q", env.Data.Report.BaselineComparison.ChosenBaselineID)
	}
	if len(env.Data.Report.BaselineComparison.RawNumbers) != 100 {
		t.Fatalf("重算用的是那次运行已采到的数字: %d", len(env.Data.Report.BaselineComparison.RawNumbers))
	}

	var jobsAfter, resultsAfter int64
	db.GetDB().WithContext(ctx).Model(&model.ModelAuditJob{}).Count(&jobsAfter)
	db.GetDB().WithContext(ctx).Model(&model.ModelAuditResult{}).Count(&resultsAfter)
	if jobsBefore != jobsAfter || resultsBefore != resultsAfter {
		t.Fatalf("重对照建了行: jobs %d→%d results %d→%d",
			jobsBefore, jobsAfter, resultsBefore, resultsAfter)
	}
	var after model.ModelAuditResult
	if err := db.GetDB().WithContext(ctx).Where("id = ?", res.ID).First(&after).Error; err != nil {
		t.Fatalf("reload after: %v", err)
	}
	if after.Report != before.Report || after.Status != before.Status || after.Attempt != before.Attempt {
		t.Fatalf("重对照改动了结果行（status %q→%q attempt %d→%d）",
			before.Status, after.Status, before.Attempt, after.Attempt)
	}

	// 未知基准必须明确告知不存在，而不是静默退回原基准。
	rec = hlwyDoJSON(t, r, http.MethodPost, "/api/v1/model-audit/hlwy-compare", map[string]any{
		"result_id":   res.ID,
		"baseline_id": "hlwy:nope@000000000000",
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知基准 status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}

	// 还在跑的行没有可重算的快照。
	running := model.ModelAuditResult{
		JobID: res.JobID, ChannelID: ch.ID, ChannelName: ch.Name,
		Model: "gpt-x", UpstreamModel: "gpt-x", Status: model.AuditResultRunning,
		Report: before.Report, Attempt: 1, CreatedAt: time.Now().UTC(),
	}
	if err := db.GetDB().Create(&running).Error; err != nil {
		t.Fatalf("create running: %v", err)
	}
	rec = hlwyDoJSON(t, r, http.MethodPost, "/api/v1/model-audit/hlwy-compare", map[string]any{
		"result_id":   running.ID,
		"baseline_id": base.ID,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("运行中 status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

// hlwyHTTPNumbers 生成确定性数字序列。
func hlwyHTTPNumbers(n, seed int) []int {
	out := make([]int, n)
	x := uint32(seed)*2654435761 + 1
	for i := range out {
		x = x*1664525 + 1013904223
		out[i] = int(x%numberdist.DistLength) + 1
	}
	return out
}

// hlwyHTTPEnqueue 走真实入队路径建一条 hlwy 任务（开关已开）。
func hlwyHTTPEnqueue(t *testing.T, ctx context.Context, ch *model.Channel) (model.ModelAuditJob, model.ModelAuditResult) {
	t.Helper()
	return hlwyHTTPEnqueueSamples(t, ctx, ch, model.HlwyDefaultSamples)
}

// hlwyHTTPEnqueueSamples 同上，采样格数可指定（计划与运行都要覆盖 100 与 200）。
func hlwyHTTPEnqueueSamples(t *testing.T, ctx context.Context, ch *model.Channel, samples int) (model.ModelAuditJob, model.ModelAuditResult) {
	t.Helper()
	if err := op.SettingSetString(model.SettingKeyModelAuditHlwyEnabled, "true"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	builtins, err := numberdist.LoadBuiltins()
	if err != nil || len(builtins) == 0 {
		t.Fatalf("builtins: %v", err)
	}
	jobID, err := op.ModelAuditEnqueue(ctx, op.ModelAuditEnqueueInput{
		ChannelID: ch.ID,
		Models:    []string{"gpt-x"},
		Kind:      model.AuditKindHlwy,
		Hlwy: &op.HlwyEnqueueInput{
			Samples:    samples,
			BaselineID: builtins[0].ID,
			ProtocolID: numberdist.ProtocolID,
		},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	job, results, err := op.ModelAuditJobDetail(ctx, jobID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d", len(results))
	}
	return job, results[0]
}

// TestHlwyWorkerStopsOnFatalStatus 守住「上游明确拒了就别接着打 99 格」。
//
// 同时守住不泄密：上游错误正文里常常带着内部主机名/密钥提示，只允许落状态码。
func TestHlwyWorkerStopsOnFatalStatus(t *testing.T) {
	ctx, ch := hlwyHTTPEnv(t)
	job, res := hlwyHTTPEnqueue(t, ctx, ch)

	var calls int32
	origFactory := hlwyNewSender
	hlwyNewSender = func(channel model.Channel, upstreamModel, endpoint string) (modelverify.Sender, error) {
		return &hlwyFakeSender{ask: func(ctx context.Context, req modelverify.Ask) (modelverify.Response, error) {
			atomic.AddInt32(&calls, 1)
			return modelverify.Response{}, &modelverify.UpstreamStatusError{
				Status: http.StatusUnauthorized,
				Err:    errors.New("SECRET-UPSTREAM-BODY sk-live-xxx"),
			}
		}}, nil
	}
	defer func() { hlwyNewSender = origFactory }()

	runHlwyResult(ctx, &res, job)

	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("致命状态码之后还发了 %d 格，必须停在 1 格", n)
	}
	var row model.ModelAuditResult
	if err := db.GetDB().WithContext(ctx).Where("id = ?", res.ID).First(&row).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if row.Status != model.AuditResultFailed {
		t.Fatalf("status = %q, want failed", row.Status)
	}
	rep, err := model.ParseHlwyReport(row.Report)
	if err != nil {
		t.Fatalf("parse report: %v", err)
	}
	bc := rep.BaselineComparison
	if bc.StopReason != "http_401" {
		t.Fatalf("stop_reason = %q, want http_401", bc.StopReason)
	}
	if bc.Counts.StartedSlots != 1 || bc.Counts.Failed != 1 || bc.Counts.Valid != 0 || bc.Counts.Unresolved != 0 {
		t.Fatalf("账目 = %+v", bc.Counts)
	}
	if err := rep.Validate(); err != nil {
		t.Fatalf("终态报告必须自洽: %v", err)
	}
	blob := row.Report + row.ErrorMsg
	if strings.Contains(blob, "SECRET-UPSTREAM-BODY") || strings.Contains(blob, "sk-live") {
		t.Fatalf("上游错误正文进了库: %q", row.ErrorMsg)
	}
}

// TestHlwyWorkerStopsWhenCanceledMidRun 守住取消的生效点：已飞出去的那格记账，
// 下一格不再开。
func TestHlwyWorkerStopsWhenCanceledMidRun(t *testing.T) {
	ctx, ch := hlwyHTTPEnv(t)
	job, res := hlwyHTTPEnqueue(t, ctx, ch)
	jobID := job.ID

	var calls int32
	origFactory := hlwyNewSender
	hlwyNewSender = func(channel model.Channel, upstreamModel, endpoint string) (modelverify.Sender, error) {
		return &hlwyFakeSender{ask: func(ctx context.Context, req modelverify.Ask) (modelverify.Response, error) {
			if atomic.AddInt32(&calls, 1) == 1 {
				// 第一格就在飞的时候用户点了取消。
				if err := op.ModelAuditJobCancel(context.Background(), jobID); err != nil {
					t.Errorf("cancel: %v", err)
				}
			}
			return modelverify.Response{Content: "217", FinishReason: "stop"}, nil
		}}, nil
	}
	defer func() { hlwyNewSender = origFactory }()

	runHlwyResult(ctx, &res, job)

	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("取消之后又发了 %d 格（总调用），必须停在 1 格", n)
	}
	var row model.ModelAuditResult
	if err := db.GetDB().WithContext(ctx).Where("id = ?", res.ID).First(&row).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if row.Status != model.AuditResultSkipped {
		t.Fatalf("status = %q, want skipped", row.Status)
	}
	rep, err := model.ParseHlwyReport(row.Report)
	if err != nil {
		t.Fatalf("parse report: %v", err)
	}
	bc := rep.BaselineComparison
	if bc.Execution != model.HlwyExecutionCanceled {
		t.Fatalf("execution = %q", bc.Execution)
	}
	if bc.Counts.StartedSlots != 1 || bc.Counts.Valid != 1 || bc.Counts.Unresolved != 0 {
		t.Fatalf("已飞出去的那格必须记账: %+v", bc.Counts)
	}
	if len(bc.RawNumbers) != 1 || bc.RawNumbers[0] != 217 {
		t.Fatalf("已采到的数字 = %v", bc.RawNumbers)
	}
}

// TestHlwyWorkerNoCallWhenCanceledBeforeStart 守住「取消之后一格都不发」。
func TestHlwyWorkerNoCallWhenCanceledBeforeStart(t *testing.T) {
	ctx, ch := hlwyHTTPEnv(t)
	job, res := hlwyHTTPEnqueue(t, ctx, ch)

	var calls int32
	origFactory := hlwyNewSender
	hlwyNewSender = func(channel model.Channel, upstreamModel, endpoint string) (modelverify.Sender, error) {
		return &hlwyFakeSender{ask: func(ctx context.Context, req modelverify.Ask) (modelverify.Response, error) {
			atomic.AddInt32(&calls, 1)
			return modelverify.Response{Content: "217"}, nil
		}}, nil
	}
	defer func() { hlwyNewSender = origFactory }()

	if err := op.ModelAuditJobCancel(ctx, job.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	runHlwyResult(ctx, &res, job)

	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Fatalf("已取消的任务发了 %d 格，必须为 0", n)
	}
	var row model.ModelAuditResult
	if err := db.GetDB().WithContext(ctx).Where("id = ?", res.ID).First(&row).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if row.Status != model.AuditResultSkipped {
		t.Fatalf("status = %q, want skipped", row.Status)
	}
}

// TestHlwyWorkerFullRun 守住一次完整对照的账目：一百格与两百格各跑一遍。
//
// 每格都必须问同一道题（题目变了这轮就不可比）、调用次数必须正好等于计划格数
// （无效回答是证据，不补采——补采等于替模型掩盖它不会玩）、终态报告必须自洽
// 且与落库一致。
func TestHlwyWorkerFullRun(t *testing.T) {
	cases := []struct {
		samples     int
		wantInvalid int
		wantTimeout int
	}{
		{samples: model.HlwyDefaultSamples, wantInvalid: model.HlwyDefaultSamples / 10, wantTimeout: 1300},
		{samples: model.HlwyMaxSamples, wantInvalid: model.HlwyMaxSamples / 10, wantTimeout: 1800},
	}
	for _, c := range cases {
		t.Run(strconv.Itoa(c.samples), func(t *testing.T) {
			ctx, ch := hlwyHTTPEnv(t)
			job, res := hlwyHTTPEnqueueSamples(t, ctx, ch, c.samples)
			opts, err := model.ParseHlwyOptions(job.OptionsJSON)
			if err != nil {
				t.Fatalf("parse options: %v", err)
			}
			if got := op.HlwyRunTimeout(c.samples); got != c.wantTimeout {
				t.Fatalf("整单超时 = %d, want %d", got, c.wantTimeout)
			}

			var calls, badPrompt int32
			origFactory := hlwyNewSender
			hlwyNewSender = func(channel model.Channel, upstreamModel, endpoint string) (modelverify.Sender, error) {
				return &hlwyFakeSender{ask: func(aCtx context.Context, req modelverify.Ask) (modelverify.Response, error) {
					n := atomic.AddInt32(&calls, 1)
					if req.Prompt != numberdist.Prompt || len(req.Turns) != 0 || req.Thinking {
						atomic.AddInt32(&badPrompt, 1)
					}
					// 每十格故意回一句「不玩游戏」：解析不出数字 ⇒ 无效格。
					if n%10 == 0 {
						return modelverify.Response{Content: "我不玩这种游戏。", FinishReason: "stop"}, nil
					}
					// 再塞一格「2xx 但上游报错」：它同样是无效格，不是没跑成。
					if n == 7 {
						return modelverify.Response{}, &modelverify.UpstreamStatusError{
							Status: http.StatusOK, Err: errors.New("empty body"),
						}
					}
					return modelverify.Response{
						Content:      strconv.Itoa(int(n%numberdist.DistLength) + 1),
						FinishReason: "stop",
					}, nil
				}}, nil
			}
			defer func() { hlwyNewSender = origFactory }()

			runHlwyResult(ctx, &res, job)

			wantInvalid := c.wantInvalid + 1 // 第 7 格那一次也算无效
			wantValid := c.samples - wantInvalid
			if n := atomic.LoadInt32(&calls); int(n) != c.samples {
				t.Fatalf("总调用 = %d, want %d（无效回答不许补采）", n, c.samples)
			}
			if n := atomic.LoadInt32(&badPrompt); n != 0 {
				t.Fatalf("%d 格发的不是固定题目/带了额外轮次", n)
			}

			var row model.ModelAuditResult
			if err := db.GetDB().WithContext(ctx).Where("id = ?", res.ID).First(&row).Error; err != nil {
				t.Fatalf("reload result: %v", err)
			}
			if row.Status != model.AuditResultDone {
				t.Fatalf("status = %q, want done (err=%q)", row.Status, row.ErrorMsg)
			}
			if row.Attempt != 1 || row.FinishedAt == nil {
				t.Fatalf("attempt=%d finished_at=%v", row.Attempt, row.FinishedAt)
			}
			rep, err := model.ParseHlwyReport(row.Report)
			if err != nil {
				t.Fatalf("parse report: %v", err)
			}
			if err := rep.Validate(); err != nil {
				t.Fatalf("终态报告必须自洽: %v", err)
			}
			bc := rep.BaselineComparison
			if bc.Execution != model.HlwyExecutionComplete || bc.StopReason != "" {
				t.Fatalf("execution=%q stop_reason=%q", bc.Execution, bc.StopReason)
			}
			if bc.Counts.PlannedCalls != c.samples || bc.Counts.StartedSlots != c.samples {
				t.Fatalf("planned=%d started=%d, want %d", bc.Counts.PlannedCalls, bc.Counts.StartedSlots, c.samples)
			}
			if bc.Counts.Valid != wantValid || bc.Counts.Invalid != wantInvalid {
				t.Fatalf("valid=%d invalid=%d, want %d/%d", bc.Counts.Valid, bc.Counts.Invalid, wantValid, wantInvalid)
			}
			if bc.Counts.Failed != 0 || bc.Counts.Unresolved != 0 {
				t.Fatalf("failed=%d unresolved=%d", bc.Counts.Failed, bc.Counts.Unresolved)
			}
			if len(bc.RawNumbers) != wantValid {
				t.Fatalf("原始数字 = %d, want %d", len(bc.RawNumbers), wantValid)
			}
			if bc.Sufficiency != model.HlwySufficiency(wantValid) {
				t.Fatalf("sufficiency = %q, want %q", bc.Sufficiency, model.HlwySufficiency(wantValid))
			}

			// 内置基准全部冻结进报告：换基准不该需要重跑一轮。
			if bc.ChosenBaselineID != opts.BaselineID {
				t.Fatalf("chosen = %q, want %q", bc.ChosenBaselineID, opts.BaselineID)
			}
			if len(bc.BaselineSnapshots) != len(opts.Candidates) || len(bc.BaselineSnapshots) == 0 {
				t.Fatalf("快照 = %d, want %d", len(bc.BaselineSnapshots), len(opts.Candidates))
			}
			for i, snap := range bc.BaselineSnapshots {
				if snap.ID != opts.Candidates[i].ID {
					t.Fatalf("快照 %d = %q, want %q", i, snap.ID, opts.Candidates[i].ID)
				}
			}
			if !bc.Scorable || len(bc.Matches) != len(bc.BaselineSnapshots) {
				t.Fatalf("scorable=%v matches=%d snapshots=%d", bc.Scorable, len(bc.Matches), len(bc.BaselineSnapshots))
			}

			// 任务侧也要收尾：人从列表页看不出「跑完了没有」就等于没跑完。
			var jobRow model.ModelAuditJob
			if err := db.GetDB().WithContext(ctx).Where("id = ?", job.ID).First(&jobRow).Error; err != nil {
				t.Fatalf("reload job: %v", err)
			}
			if jobRow.Status != model.AuditJobDone || jobRow.Finished != 1 {
				t.Fatalf("job status=%q finished=%d", jobRow.Status, jobRow.Finished)
			}
		})
	}
}
