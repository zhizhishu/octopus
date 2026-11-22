package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zhizhishu/octopus/internal/requestsource"
)

const unitPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aOq0AAAAASUVORK5CYII="

func TestImageLoopback(t *testing.T) {
	for _, edit := range []bool{false, true} {
		t.Run(map[bool]string{false: "generate", true: "edit"}[edit], func(t *testing.T) {
			caller := httptest.NewRequest("POST", "/mcp", nil)
			caller.RemoteAddr = "192.0.2.5:7890"
			caller.Header.Set("Authorization", "Bearer generated-fake-test-token")
			caller.Header.Set("CF-Connecting-IP", "198.51.100.7")
			caller.Header.Set("X-Forwarded-For", "198.51.100.7")
			caller.Header.Set("User-Agent", "fake-client")
			input := map[string]any{"prompt": "test", "model": "fake-model", "size": "2048x1152", "quality": "high", "n": 2}
			if edit {
				input["images"] = []string{unitPNG, "data:image/png;base64," + unitPNG}
				input["mask"] = unitPNG
			} else {
				input["background"] = "transparent"
				input["output_format"] = "png"
			}
			raw, _ := json.Marshal(input)
			called := false
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				if r.Method != "POST" || r.RemoteAddr != caller.RemoteAddr || r.Header.Get("Authorization") != caller.Header.Get("Authorization") || r.Header.Get("CF-Connecting-IP") != "198.51.100.7" || r.Header.Get("X-Forwarded-For") != "198.51.100.7" || r.UserAgent() != "octopus-mcp/1.0 fake-client" {
					t.Fatalf("metadata not preserved: %+v", r)
				}
				if source := requestsource.FromContext(r.Context()); source == nil || *source != "mcp" {
					t.Fatal("source missing")
				}
				if edit {
					if r.URL.Path != "/v1/images/edits" {
						t.Fatal(r.URL.Path)
					}
					if err := r.ParseMultipartForm(1 << 20); err != nil {
						t.Fatal(err)
					}
					defer r.MultipartForm.RemoveAll()
					if len(r.MultipartForm.File["image"]) != 2 || len(r.MultipartForm.File["mask"]) != 1 || len(r.MultipartForm.File["model"]) != 0 || r.FormValue("model") != "fake-model" || r.FormValue("size") != "2048x1152" || r.FormValue("n") != "2" {
						t.Fatalf("multipart = %+v", r.MultipartForm)
					}
				} else {
					if r.URL.Path != "/v1/images/generations" {
						t.Fatal(r.URL.Path)
					}
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					if body["size"] != "2048x1152" || body["background"] != "transparent" || body["output_format"] != "png" || body["n"] != float64(2) {
						t.Fatalf("payload = %+v", body)
					}
				}
				_, _ = io.WriteString(w, `{"data":[{"b64_json":"`+unitPNG+`"},{"url":"https://images.example/fake.png"}]}`)
			})
			result := callImage(context.Background(), handler, caller, raw, edit)
			if result.IsError || !called || len(result.Content) != 3 {
				t.Fatalf("result = %+v called=%v", result, called)
			}
			if _, ok := result.Content[0].(*mcp.ImageContent); !ok {
				t.Fatal("missing image content")
			}
			if got := result.Content[1].(*mcp.TextContent).Text; got != "https://images.example/fake.png" {
				t.Fatal(got)
			}
		})
	}
}

func TestImageValidation(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		edit      bool
	}{
		{"empty", `{}`, false}, {"blank", `{"model":"fake","prompt":" "}`, false}, {"bad-json", `bad`, false}, {"unknown", `{"model":"fake","prompt":"a","bogus":true}`, false},
		{"zero-n", `{"model":"fake","prompt":"a","n":0}`, false}, {"too-many", `{"model":"fake","prompt":"a","n":5}`, false},
		{"empty-edit", `{"model":"fake","prompt":"a","images":[]}`, true}, {"local-path", `{"model":"fake","prompt":"a","images":["/etc/passwd"]}`, true},
		{"URL", `{"model":"fake","prompt":"a","images":["https://example.com/a.png"]}`, true}, {"bad-base64", `{"model":"fake","prompt":"a","images":["!!!"]}`, true},
		{"bad-data-url", `{"model":"fake","prompt":"a","images":["data:image/png,hello"]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := callImage(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("invalid input reached relay") }), httptest.NewRequest("POST", "/mcp", nil), json.RawMessage(tc.raw), tc.edit)
			if !result.IsError || !strings.HasPrefix(result.Content[0].(*mcp.TextContent).Text, "400:") {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}

func TestImageResponseErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body, message string
		status              int
	}{
		{"octopus", `{"code":403,"message":"model not supported"}`, "403: model not supported", 403},
		{"upstream", `{"error":{"message":"fake upstream error"}}`, "400: fake upstream error", 400},
		{"plain", "fake unavailable", "503: fake unavailable", 503},
		{"empty", `{"data":[]}`, "502:", 200}, {"malformed", "bad-json", "502:", 200}, {"bad-image", `{"data":[{"b64_json":"!!!"}]}`, "502:", 200},
		{"bad-url", `{"data":[{"url":"file:///etc/passwd"}]}`, "502:", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			result := callImage(context.Background(), handler, httptest.NewRequest("POST", "/mcp", nil), json.RawMessage(`{"model":"fake","prompt":"a"}`), false)
			if !result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, tc.message) {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestImageCancellation(t *testing.T) {
	for _, cancelCaller := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		caller := httptest.NewRequest("POST", "/mcp", nil)
		callCtx := ctx
		if cancelCaller {
			caller = caller.WithContext(ctx)
			callCtx = context.Background()
		}
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { cancel(); <-r.Context().Done(); w.WriteHeader(499) })
		result := callImage(callCtx, handler, caller, json.RawMessage(`{"model":"fake","prompt":"a"}`), false)
		if !result.IsError {
			t.Fatal("canceled call succeeded")
		}
	}
}
