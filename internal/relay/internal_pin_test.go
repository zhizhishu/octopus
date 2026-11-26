package relay

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	transformermodel "github.com/zhizhishu/octopus/internal/transformer/model"
)

// 内部钉扎机制的纯内存口径：标记只能由进程内代码设置/读取，取不到时行为正确。
// 静态编写，未在 Windows 本机运行（环境约束禁止 go test）。

func newPinTestContext(t *testing.T) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	c.Request = req
	return c
}

func TestInternalPinRoundtrip(t *testing.T) {
	c := newPinTestContext(t)
	if internalPinnedChannel(c) != nil {
		t.Fatal("unmarked context must report no pin")
	}
	channel := dbmodel.Channel{ID: 42, Name: "测试渠道", Enabled: false}
	MarkInternalPinnedChannel(c, channel)
	got := internalPinnedChannel(c)
	if got == nil || got.ID != 42 {
		t.Fatalf("pinned channel id = %v, want 42", got)
	}
}

func TestInternalPinInvalidValues(t *testing.T) {
	c := newPinTestContext(t)
	MarkInternalPinnedChannel(c, dbmodel.Channel{ID: 0})
	if internalPinnedChannel(c) != nil {
		t.Fatal("channel without ID must not pin")
	}
	c.Set(internalPinnedChannelContextKey, "not a channel")
	if internalPinnedChannel(c) != nil {
		t.Fatal("foreign value under the pin key must be ignored")
	}
}

func TestStashAndReadInternalResponse(t *testing.T) {
	c := newPinTestContext(t)
	if InternalPinnedResponse(c) != nil {
		t.Fatal("no stashed response should read as nil")
	}
	resp := &transformermodel.InternalLLMResponse{ID: "resp_1"}
	metrics := NewRelayMetrics(1, 1, "127.0.0.1", "m", nil)
	metrics.InternalResponse = resp
	stashInternalResponse(c, metrics)
	if got := InternalPinnedResponse(c); got == nil || got.ID != "resp_1" {
		t.Fatalf("stashed response = %v, want resp_1", got)
	}
}
