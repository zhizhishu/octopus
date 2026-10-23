package relay

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// silentHeaderUpstream 发完状态行+响应头，然后**再也不写正文**，并 hold 住连接。
//
// 这是错误体读取最恶劣的形态：`io.LimitReader` 只限字节数、不限时间，裸 ReadAll
// 会一直等下去。实测在 TestEpisodeNormalSuccessRestores 里卡满 5 分钟才被包超时
// 打断，goroutine 栈停在 forward → io.ReadAll。
func silentHeaderUpstream(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if f, ok := w.(http.Flusher); ok {
			f.Flush() // 让头部真的上线
		}
		<-r.Context().Done() // 不写正文，也不返回
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fetchErrorResponse(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// 核心：上游发完头就沉默时，读取必须在预算内返回，不能永远挂住。
//
// 走**真实 HTTP**（真 Listen、真上游、真 http.Client），不是喂一个假 io.Reader ——
// 阻塞行为是传输层给的，假 reader 模拟不出来。
//
// 变异：把 readErrorBodyFromResponse 换回裸 io.ReadAll(io.LimitReader(...))，
// 本测试不会在 5s 内返回，被测试自身的看门狗判红。
func TestReadErrorBodyBoundedOnSilentUpstream(t *testing.T) {
	previous := errorBodyReadBudget
	errorBodyReadBudget = 150 * time.Millisecond
	t.Cleanup(func() { errorBodyReadBudget = previous })

	srv := silentHeaderUpstream(t, http.StatusBadGateway)
	resp := fetchErrorResponse(t, srv.URL)

	type outcome struct {
		body []byte
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		body, err := readErrorBodyFromResponse(resp)
		done <- outcome{body, err}
	}()

	select {
	case got := <-done:
		if got.err == nil {
			t.Fatal("上游发完头就沉默，读取却报告成功：说明这次读根本没有等到 EOF，数据不可信")
		}
		// 必须能区分"预算到点"与"上游真的断了"，否则运维无法判断是谁的问题。
		if got.err.Error() == "" {
			t.Fatal("读取失败必须带上原因")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("错误体读取没有在预算内返回（5s 看门狗）：" +
			"裸 io.ReadAll 会一直挂住，relay 协程卡死、" +
			"defer 的 UserReleaseQuota 永不执行，用户在途额度被永久占用")
	}
}

// 对照组：正常返回错误体的上游必须被完整读到（防误伤）。
func TestReadErrorBodyReadsNormalErrorBody(t *testing.T) {
	const payload = `{"error":{"message":"upstream boom"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)

	resp := fetchErrorResponse(t, srv.URL)
	body, err := readErrorBodyFromResponse(resp)
	if err != nil {
		t.Fatalf("完整的错误体必须读成功，got %v", err)
	}
	if string(body) != payload {
		t.Fatalf("error body = %q, want %q", string(body), payload)
	}
}

// 预算必须是有限的正值：<=0 会被读成"禁用"，错误体读取又会无限挂住。
func TestErrorBodyReadBudgetIsFinite(t *testing.T) {
	if errorBodyReadBudget <= 0 {
		t.Fatal("errorBodyReadBudget 必须 > 0：<=0 等于关掉预算")
	}
	if errorBodyReadBudget > time.Minute {
		t.Fatalf("errorBodyReadBudget = %s 太长：上游沉默一分钟才放弃，用户早已超时", errorBodyReadBudget)
	}
}

// 生产默认值必须是 10s 这一档（不是被测试改小的值）。
func TestErrorBodyReadBudgetDefaultIsTenSeconds(t *testing.T) {
	// 重新读常量默认值：用一个独立变量钉住，防止有人把默认值悄悄调成 0 或半小时。
	const wantProductionDefault = 10 * time.Second
	if errorBodyReadBudget == 150*time.Millisecond {
		t.Skip("上一条测试改了包级预算（已在 Cleanup 还原），跳过默认值断言")
	}
	if errorBodyReadBudget != wantProductionDefault {
		t.Fatalf("生产默认预算 = %s, want %s", errorBodyReadBudget, wantProductionDefault)
	}
}
