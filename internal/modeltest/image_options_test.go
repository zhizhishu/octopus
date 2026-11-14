package modeltest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// These are real RunChannel -> ImagesHandler -> Gemini bridge requests; the
// only replacement is an httptest upstream returning a tiny fixture image.
func TestRunChannelImageOptionsReachGeminiUpstream(t *testing.T) {
	ctx := setupModelTestDB(t)
	for _, tier := range []string{"2K", "4K"} {
		t.Run(tier, func(t *testing.T) {
			var mu sync.Mutex
			var received map[string]any
			var path string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode upstream: %v", err)
				}
				mu.Lock()
				received, path = body, r.URL.Path
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jRZkAAAAASUVORK5CYII="}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`))
			}))
			defer server.Close()
			channel := dbmodel.Channel{
				Name:     "image-options-fixture",
				Type:     outbound.OutboundTypeGemini,
				BaseUrls: []dbmodel.BaseUrl{{URL: server.URL}},
				Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "fixture-key"}},
			}
			response, err := RunChannel(ctx, channel, dbmodel.ModelTestRequest{
				Model: "gemini-3-pro-image", Endpoint: imageGenerationTestEndpointName,
				Size: "1792x1024", Quality: tier,
			})
			if err != nil {
				t.Fatal(err)
			}
			if response.Summary.Success != 1 {
				t.Fatalf("real image test did not succeed: %+v", response)
			}
			mu.Lock()
			defer mu.Unlock()
			if !strings.HasSuffix(path, "/gemini-3-pro-image:generateContent") {
				t.Fatalf("wrong Gemini upstream path: %s", path)
			}
			config, ok := received["generationConfig"].(map[string]any)
			if !ok {
				t.Fatalf("missing generationConfig: %+v", received)
			}
			imageConfig, ok := config["imageConfig"].(map[string]any)
			if !ok || imageConfig["imageSize"] != tier || imageConfig["aspectRatio"] != "16:9" {
				t.Fatalf("explicit image options did not reach the real bridge: %+v", config)
			}
		})
	}
}
