package modeltest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

func TestSystemOneChannelProbeSendsDecisionBody(t *testing.T) {
	ctx := setupModelTestDB(t)
	var gotPath, gotUA, gotOriginator string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotUA = r.Header.Get("User-Agent")
		gotOriginator = r.Header.Get("Originator")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"q":{"type":"noul","noul":1}}}`))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:    "decision",
		Type:    outbound.OutboundTypeSystemOne,
		Enabled: true,
		Cloak:   dbmodel.ChannelCloak{Mode: "always", Shape: "codex"},
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL + "/v1",
		}},
		Keys:  []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "decision-key"}},
		Model: "jev-1",
	}
	result, err := RunChannel(ctx, channel, dbmodel.ModelTestRequest{
		Model:    "jev-1",
		Endpoint: "openai_chat",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(result.Results) != 1 {
		t.Fatalf("results = %#v", result.Results)
	}
	got := result.Results[0]
	if !got.Success {
		t.Fatalf("probe failed: %s", got.Error)
	}
	if gotPath != "/v1/systemone" {
		t.Fatalf("path = %q, want /v1/systemone (base ended in /v1)", gotPath)
	}
	if gotBody["state"] != "ping" {
		t.Fatalf("state = %#v, want ping", gotBody["state"])
	}
	questions, _ := gotBody["questions"].(map[string]any)
	q, _ := questions["q"].(map[string]any)
	if q["type"] != "noul" {
		t.Fatalf("question = %#v, want one noul question", gotBody["questions"])
	}
	if gotOriginator != "" || strings.Contains(strings.ToLower(gotUA), "codex") || strings.Contains(strings.ToLower(gotUA), "claude") {
		t.Fatalf("probe wore a CLI identity: ua=%q originator=%q", gotUA, gotOriginator)
	}
	if got.UpstreamPath != "/v1/systemone" {
		t.Fatalf("reported path = %q", got.UpstreamPath)
	}
}
