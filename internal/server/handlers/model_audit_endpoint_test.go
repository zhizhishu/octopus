package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 这条用例走的是**手动检测的真实入口**（runModelAudit 整条链路：取渠道、算端点、
// 建探针发送器），断言它给上游用的端点名就是 op.AuditEndpointForChannel 的返回值。
//
// 为什么不直接调 op.AuditEndpointForChannel 对比自己：那样只能证明函数返回什么，
// 证明不了入口有没有用它。判据与入口之间的接线断了（比如有人又写一份 switch），
// 只有这条会红。
func TestManualAuditEntryUsesSharedEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := op.InitCache(); err != nil {
		t.Fatalf("init cache: %v", err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_audit","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(upstream.Close)

	cases := []struct {
		name string
		typ  outbound.OutboundType
	}{
		{"OpenAIChat", outbound.OutboundTypeOpenAIChat},
		{"OpenAIResponse", outbound.OutboundTypeOpenAIResponse},
		{"Anthropic", outbound.OutboundTypeAnthropic},
		{"Gemini", outbound.OutboundTypeGemini},
		{"CustomOpenAIChat", outbound.OutboundTypeCustomOpenAIChat},
		// 未知类型（退役的 4 号位）也要走一遍：它不是"能不能测"的问题，
		// 而是端点判据不许 panic、必须是共享函数给出的默认值。
		{"退役的 4 号位", outbound.OutboundType(4)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			channel := model.Channel{
				Name:     "manual-audit-" + tc.name,
				Type:     tc.typ,
				Enabled:  true,
				Model:    "audit-model-" + tc.name,
				BaseUrls: []model.BaseUrl{{URL: upstream.URL}},
				Keys:     []model.ChannelKey{{Enabled: true, ChannelKey: "manual-audit-key"}},
			}
			if err := op.ChannelCreate(&channel, t.Context()); err != nil {
				t.Fatalf("create channel: %v", err)
			}

			body, err := json.Marshal(map[string]any{
				"channel_id": channel.ID,
				"model":      channel.Model,
				"probes":     []string{"liveness"},
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/model-audit/run", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router := gin.New()
			router.POST("/api/v1/model-audit/run", runModelAudit)
			router.ServeHTTP(rec, req)

			want := op.AuditEndpointForChannel(channel)

			// 未知类型/嵌入类型会被 modeltest 的准入挡在发探针之前（与端点判据无关，
			// 是"这个渠道能不能测"的判定）。那一支只要求：拒绝理由说的是渠道类型，
			// 而不是端点名错了。
			if rec.Code != http.StatusOK {
				if tc.typ == outbound.OutboundType(4) {
					if !bytes.Contains(rec.Body.Bytes(), []byte("不支持探针校验")) {
						t.Fatalf("未知类型的拒绝理由应当来自探针准入，实际：%d %s", rec.Code, rec.Body.String())
					}
					return
				}
				t.Fatalf("渠道类型 %d 的手动检测失败：%d %s", int(tc.typ), rec.Code, rec.Body.String())
			}

			var envelope struct {
				Data struct {
					Endpoint string `json:"endpoint"`
				} `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
			}
			if got := envelope.Data.Endpoint; got != want {
				t.Fatalf("手动检测端点 = %q, want %q（op.AuditEndpointForChannel 的返回值）", got, want)
			}
		})
	}
}
