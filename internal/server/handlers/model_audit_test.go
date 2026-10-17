package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/modelverify/behavior"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

func setupModelAuditRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/v1/model-audit/run", runModelAudit)
	return r
}

func postModelAudit(t *testing.T, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/model-audit/run", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	setupModelAuditRouter().ServeHTTP(rec, req)
	return rec
}

func TestRunModelAuditRequiresChannelAndModel(t *testing.T) {
	rec := postModelAudit(t, map[string]any{"model": "claude-sonnet-example"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing channel_id status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = postModelAudit(t, map[string]any{"channel_id": 1})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing model status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRunModelAuditRejectsUnknownProbe(t *testing.T) {
	rec := postModelAudit(t, map[string]any{
		"channel_id": 1,
		"model":      "claude-sonnet-example",
		"probes":     []string{"tls_ja3"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown probe status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("unknown probe")) {
		t.Fatalf("expected unknown probe message, got %s", rec.Body.String())
	}
}

func TestParseAuditProbesDefaultAndDedupe(t *testing.T) {
	got, err := parseAuditProbes(nil)
	if err != nil {
		t.Fatalf("default probes: %v", err)
	}
	want := behavior.DefaultProbes()
	if len(got) != len(want) {
		t.Fatalf("default len = %d, want %d", len(got), len(want))
	}
	got, err = parseAuditProbes([]string{"liveness", "liveness", " echo_rewrite "})
	if err != nil {
		t.Fatalf("dedupe: %v", err)
	}
	if len(got) != 2 || got[0] != behavior.ProbeLiveness || got[1] != behavior.ProbeEchoRewrite {
		t.Fatalf("dedupe result = %#v", got)
	}
}

// TestParseAuditProbesAcceptsOptionalProbes 守住"默认关闭 ≠ 不允许"这条线。
//
// 指纹探针因为开销高被排除在默认集之外；如果入参校验拿 DefaultProbes 当白名单，
// 用户在界面上勾了它却会被后端拒成 unknown probe。这个测试就是防这个退化。
func TestParseAuditProbesAcceptsOptionalProbes(t *testing.T) {
	optional := behavior.OptionalProbes()
	if len(optional) == 0 {
		t.Fatal("应当至少有一个默认关闭但可手动勾选的探针")
	}
	for _, id := range optional {
		// 默认关闭：不能出现在默认集里，否则等于偷偷加钱。
		for _, d := range behavior.DefaultProbes() {
			if d == id {
				t.Fatalf("%q 不应在默认探针集里", id)
			}
		}
		// 但必须被接受。
		got, err := parseAuditProbes([]string{"liveness", string(id)})
		if err != nil {
			t.Fatalf("手动勾选 %q 被拒: %v", id, err)
		}
		if len(got) != 2 || got[1] != id {
			t.Fatalf("手动勾选 %q 的结果 = %#v", id, got)
		}
	}
}

func TestAuditEndpointForChannelType(t *testing.T) {
	if got := auditEndpointFor(model.Channel{Type: outbound.OutboundTypeAnthropic}); got != "anthropic_messages" {
		t.Fatalf("anthropic endpoint = %q", got)
	}
	if got := auditEndpointFor(model.Channel{Type: outbound.OutboundTypeOpenAIResponse}); got != "openai_responses" {
		t.Fatalf("responses endpoint = %q", got)
	}
	if got := auditEndpointFor(model.Channel{Type: outbound.OutboundTypeOpenAIChat}); got != "openai_chat" {
		t.Fatalf("chat endpoint = %q", got)
	}
}
