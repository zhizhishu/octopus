package relay

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

func TestCustomChannelAbsoluteMediaURLsKeepOperationSuffix(t *testing.T) {
	channel := &dbmodel.Channel{
		Type:             outbound.OutboundTypeCustomOpenAIChat,
		BaseUrls:         []dbmodel.BaseUrl{{URL: "https://chat.example/v1/chat/completions"}},
		OpenAIImagesPath: "https://cdn.example/openai/images",
		OpenAIVideosPath: "https://cdn.example/openai/videos",
	}
	images, err := openAIUpstreamURL(channel, channel.GetOpenAIImagesPath("/images/edits"))
	if err != nil {
		t.Fatal(err)
	}
	if images != "https://cdn.example/openai/images/edits" {
		t.Fatalf("images edits = %q", images)
	}
	variations, err := openAIUpstreamURL(channel, channel.GetOpenAIImagesPath("/images/variations"))
	if err != nil {
		t.Fatal(err)
	}
	if variations != "https://cdn.example/openai/images/variations" {
		t.Fatalf("images variations = %q", variations)
	}
	pollPath := videoUpstreamPathForChannel(channel, "", "task_1", false)
	poll, err := openAIUpstreamURL(channel, pollPath)
	if err != nil {
		t.Fatal(err)
	}
	if poll != "https://cdn.example/openai/videos/task_1" {
		t.Fatalf("video poll = %q (path %q)", poll, pollPath)
	}
	contentPath := videoUpstreamPathForChannel(channel, VideoMediaKindGenerations, "task_1", true)
	content, err := openAIUpstreamURL(channel, contentPath)
	if err != nil {
		t.Fatal(err)
	}
	if content != "https://cdn.example/openai/videos/generations/task_1/content" {
		t.Fatalf("video content = %q (path %q)", content, contentPath)
	}

	bare := &dbmodel.Channel{
		Type:     outbound.OutboundTypeCustomOpenAIChat,
		BaseUrls: []dbmodel.BaseUrl{{URL: "https://chat.example/v1/chat/completions"}},
	}
	for _, endpoint := range []string{
		bare.GetOpenAIImagesPath("/images/generations"),
		videoUpstreamPathForChannel(bare, "", "task_1", true),
	} {
		_, err := openAIUpstreamURL(bare, endpoint)
		if !errors.Is(err, errCustomChannelNoFullURL) {
			t.Fatalf("endpoint %q err = %v, want skip", endpoint, err)
		}
	}
	if _, ok := customChannelAbsoluteURL("/openai/images"); ok {
		t.Fatal("a relative path must not count as a full URL")
	}
}

func TestCustomChannelAbsoluteTasksPathKeepsPollAndContentSuffix(t *testing.T) {
	channel := &dbmodel.Channel{
		Type:             outbound.OutboundTypeCustomOpenAIChat,
		BaseUrls:         []dbmodel.BaseUrl{{URL: "https://chat.example/v1/chat/completions"}},
		OpenAIVideosPath: "https://cdn.example/openai/videos",
		OpenAITasksPath:  "https://cdn.example/v1/tasks",
	}
	pollPath := videoUpstreamPathForChannel(channel, VideoMediaKindGenerations, "task_1", false)
	poll, err := openAIUpstreamURL(channel, pollPath)
	if err != nil {
		t.Fatal(err)
	}
	if poll != "https://cdn.example/v1/tasks/task_1" {
		t.Fatalf("poll = %q (path %q), want the tasks prefix plus /{id}", poll, pollPath)
	}
	contentPath := videoUpstreamPathForChannel(channel, VideoMediaKindGenerations, "task_1", true)
	content, err := openAIUpstreamURL(channel, contentPath)
	if err != nil {
		t.Fatal(err)
	}
	if content != "https://cdn.example/v1/tasks/task_1/content" {
		t.Fatalf("content = %q (path %q), want the tasks prefix plus /{id}/content", content, contentPath)
	}
	if strings.Contains(poll, "/openai/videos") || strings.Contains(content, "/generations") {
		t.Fatalf("tasks poll must not use the videos prefix or the create kind: poll %q content %q", poll, content)
	}
}

func TestCustomChannelWithoutOverrideIsSkippedForNonChat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name string
		post func(t *testing.T, modelName string)
	}{
		{"embeddings", func(t *testing.T, modelName string) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"`+modelName+`","input":"hi"}`))
			req.Header.Set("Content-Type", "application/json")
			c.Request = req
			c.Set("api_key_id", 0)
			c.Set("user_id", 0)
			c.Set("request_ip", "127.0.0.1")
			Handler(inbound.InboundTypeOpenAIEmbedding, c)
			if rec.Code == http.StatusOK {
				t.Fatalf("embeddings succeeded: %s", rec.Body.String())
			}
		}},
		{"images", func(t *testing.T, modelName string) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"`+modelName+`","prompt":"cat"}`))
			req.Header.Set("Content-Type", "application/json")
			c.Request = req
			c.Set("api_key_id", 0)
			c.Set("user_id", 0)
			c.Set("request_ip", "127.0.0.1")
			ImagesHandler("/images/generations", c)
			if rec.Code == http.StatusOK {
				t.Fatalf("images succeeded: %s", rec.Body.String())
			}
		}},
		{"videos", func(t *testing.T, modelName string) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			req := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{"model":"`+modelName+`","prompt":"cat"}`))
			req.Header.Set("Content-Type", "application/json")
			c.Request = req
			c.Set("api_key_id", 0)
			c.Set("user_id", 0)
			c.Set("request_ip", "127.0.0.1")
			VideosHandler(c)
			if rec.Code == http.StatusOK {
				t.Fatalf("videos succeeded: %s", rec.Body.String())
			}
		}},
		{"systemone", func(t *testing.T, modelName string) {
			rec := postSystemone(t, modelName)
			if rec.Code == http.StatusOK {
				t.Fatalf("systemone succeeded: %s", rec.Body.String())
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := setupRelayErrorDB(t)
			var hits int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&hits, 1)
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(upstream.Close)
			modelName := "custom-skip-" + tc.name
			newSystemoneChannel(t, ctx, "custom-skip-"+tc.name, outbound.OutboundTypeCustomOpenAIChat, upstream.URL+"/v1/chat/completions", modelName)
			tc.post(t, modelName)
			if got := atomic.LoadInt32(&hits); got != 0 {
				t.Fatalf("upstream hits = %d, want 0", got)
			}
			assertSystemoneSkip(t, ctx, CustomChannelNoFullURLReason)
		})
	}
}
