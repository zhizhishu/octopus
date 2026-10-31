package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/price"
	"github.com/zhizhishu/octopus/internal/relay/bodycache"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 同步与异步生图共用一份计费口径（images_billing.go）。

func TestImageRequestCountDefaultsAndClamps(t *testing.T) {
	cases := []struct {
		payload map[string]any
		want    int
	}{
		{nil, 1},
		{map[string]any{}, 1},
		{map[string]any{"n": float64(3)}, 3},
		{map[string]any{"n": "4"}, 4},
		{map[string]any{"n": float64(0)}, 1},
		{map[string]any{"n": float64(-2)}, 1},
		{map[string]any{"n": "abc"}, 1},
		{map[string]any{"n": float64(100000)}, imageRequestCountMax},
	}
	for _, c := range cases {
		if got := imageRequestCount(c.payload); got != c.want {
			t.Fatalf("imageRequestCount(%v) = %d, want %d", c.payload, got, c.want)
		}
	}
}

func TestMultipartImageCountReadsN(t *testing.T) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("model", "m")
	fw, _ := w.CreateFormFile("image", "a.png")
	_, _ = fw.Write(bytes.Repeat([]byte{0x89}, 4096))
	_ = w.WriteField("n", "2")
	_ = w.Close()
	bc, err := bodycache.New(io.NopCloser(bytes.NewReader(buf.Bytes())))
	if err != nil {
		t.Fatalf("bodycache: %v", err)
	}
	defer bc.Close()
	if got := multipartImageCount(bc, w.Boundary()); got != 2 {
		t.Fatalf("multipart n = %d, want 2", got)
	}
}

// 配了按张价：同步生图按「单价 × n × 倍率」收，token 费用不再计入（token 数保留展示）。
func TestApplyImagePricingPerImageOverridesTokenCost(t *testing.T) {
	const modelName = "img-billing-perimage-model"
	price.SetLLMPrice(modelName, dbmodel.LLMPrice{Input: 5, Output: 40, PerCall: 0.25})
	t.Cleanup(func() { price.SetLLMPrice(modelName, dbmodel.LLMPrice{}) })

	m := newImagesRelayMetrics(1, 1, "", modelName, "images_generations", "/v1/images/generations")
	m.SetUsageFromImages(modelName, imagesUsage{InputTokens: 1000, OutputTokens: 4000})
	m.ApplyImagePricing(modelName, func() int { return 3 })

	if m.Stats.InputCost != 0 || m.Stats.OutputCost != 0.75 {
		t.Fatalf("cost = in %v out %v, want 0 / 0.75", m.Stats.InputCost, m.Stats.OutputCost)
	}
	if m.Stats.OutputToken != 4000 || m.BillingSnapshot.FinalOutputCost != 0.75 || m.BillingSnapshot.FinalInputCost != 0 {
		t.Fatalf("snapshot/tokens = %+v tokens %d", m.BillingSnapshot, m.Stats.OutputToken)
	}
}

// 没配按张价：保持原来的 token 计费，n 读都不读。
func TestApplyImagePricingKeepsTokenCostWithoutPerCall(t *testing.T) {
	const modelName = "img-billing-token-model"
	price.SetLLMPrice(modelName, dbmodel.LLMPrice{Input: 5, Output: 40})
	t.Cleanup(func() { price.SetLLMPrice(modelName, dbmodel.LLMPrice{}) })

	m := newImagesRelayMetrics(1, 1, "", modelName, "images_generations", "/v1/images/generations")
	m.SetUsageFromImages(modelName, imagesUsage{InputTokens: 1000, OutputTokens: 4000})
	m.ApplyImagePricing(modelName, func() int { t.Fatal("n must not be read for token pricing"); return 0 })

	want := 1000*5e-6 + 4000*40e-6
	if got := m.Stats.InputCost + m.Stats.OutputCost; math.Abs(got-want) > 1e-12 {
		t.Fatalf("cost = %v, want %v", got, want)
	}
}

// 端到端：同步 /v1/images/generations 配了按张价时，真的从余额里扣「单价 × n」，只扣一次。
// 删掉 ImagesHandler 里的 ApplyImagePricing 调用，这条会红（会按 token 价扣）。
func TestImagesHandlerChargesPerImagePrice(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"data":[{"url":"https://img.example/a.png"},{"url":"https://img.example/b.png"}],` +
			`"usage":{"input_tokens":1000,"output_tokens":4000}}`))
	}))
	t.Cleanup(upstream.Close)

	const modelName = "img-billing-sync-model"
	ch := dbmodel.Channel{
		Name:     "img-billing-channel",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		Model:    modelName,
		Priority: 1,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "img-billing-secret"}},
	}
	if err := op.ChannelCreate(&ch, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	price.SetLLMPrice(modelName, dbmodel.LLMPrice{Input: 5, Output: 40, PerCall: 0.25})
	t.Cleanup(func() { price.SetLLMPrice(modelName, dbmodel.LLMPrice{}) })

	user, err := op.UserCreate(dbmodel.UserCreateRequest{
		Username: "img-billing-user", Password: "test-password-123",
		Role: dbmodel.UserRoleUser, Status: dbmodel.UserStatusActive, Balance: 10,
	}, ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"`+modelName+`","prompt":"two cats","n":2}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", user.ID)
	c.Set("request_ip", "127.0.0.1")

	ImagesHandler("/images/generations", c)

	if rec.Code != http.StatusOK {
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		t.Fatalf("images failed: %d %v", rec.Code, body)
	}
	after, err := op.UserGet(user.ID)
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	if charged := 10 - after.Balance; math.Abs(charged-0.5) > 1e-9 {
		t.Fatalf("charged %v, want 0.5 (0.25 × 2 images, not the token price)", charged)
	}
}
