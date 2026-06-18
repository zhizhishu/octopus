package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestParseGeminiModelGet(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "empty means list", raw: "", want: ""},
		{name: "slash only means list", raw: "/", want: ""},
		{name: "single model", raw: "/gemini-2.5-flash", want: "gemini-2.5-flash"},
		{name: "strips action suffix", raw: "/gemini-2.5-flash:generateContent", want: "gemini-2.5-flash"},
		{name: "publisher path", raw: "/publishers/google/models/gemini-pro", want: "publishers/google/models/gemini-pro"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseGeminiModelGet(tt.raw); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestNewGeminiModel(t *testing.T) {
	m := newGeminiModel("gemini-2.5-flash")
	if m.Name != "models/gemini-2.5-flash" {
		t.Fatalf("name: got %q want models/gemini-2.5-flash", m.Name)
	}
	if m.DisplayName != "gemini-2.5-flash" {
		t.Fatalf("displayName: got %q", m.DisplayName)
	}
	want := map[string]bool{"generateContent": true, "streamGenerateContent": true}
	if len(m.SupportedGenerationMethods) != len(want) {
		t.Fatalf("methods: got %v", m.SupportedGenerationMethods)
	}
	for _, method := range m.SupportedGenerationMethods {
		if !want[method] {
			t.Fatalf("unexpected method %q", method)
		}
	}
}

// TestGeminiModelRouteDispatch wires the same route shapes used in init() with
// stub handlers to confirm GET list, GET single-model, and POST generateContent
// all dispatch correctly and the new GET routes do not regress the POST route.
func TestGeminiModelRouteDispatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	grp := engine.Group("/v1beta")
	grp.POST("/models/*modelAction", func(c *gin.Context) { c.String(http.StatusOK, "post:%s", c.Param("modelAction")) })
	grp.GET("/models", func(c *gin.Context) { c.String(http.StatusOK, "list") })
	grp.GET("/models/*modelAction", func(c *gin.Context) {
		c.String(http.StatusOK, "get:%s", parseGeminiModelGet(c.Param("modelAction")))
	})

	cases := []struct {
		method string
		path   string
		want   string
	}{
		{http.MethodGet, "/v1beta/models", "list"},
		{http.MethodGet, "/v1beta/models/gemini-2.5-flash", "get:gemini-2.5-flash"},
		{http.MethodPost, "/v1beta/models/gemini-2.5-flash:generateContent", "post:/gemini-2.5-flash:generateContent"},
		{http.MethodPost, "/v1beta/models/gemini-2.5-flash:streamGenerateContent", "post:/gemini-2.5-flash:streamGenerateContent"},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, nil)
		engine.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s %s: status %d body=%s", tc.method, tc.path, w.Code, w.Body.String())
		}
		if w.Body.String() != tc.want {
			t.Fatalf("%s %s: got %q want %q", tc.method, tc.path, w.Body.String(), tc.want)
		}
	}
}

// TestGeminiListModelsResponseShape verifies the JSON shape emitted for a model
// set, independent of the DB-backed allow-list resolution.
func TestGeminiListModelsResponseShape(t *testing.T) {
	models := []string{"gemini-2.5-flash", "gemini-2.5-pro"}
	list := geminiModelListResponse{Models: make([]geminiModel, 0, len(models))}
	for _, m := range models {
		list.Models = append(list.Models, newGeminiModel(m))
	}
	data, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		Models []struct {
			Name                       string   `json:"name"`
			SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.Models) != 2 {
		t.Fatalf("models count: got %d want 2", len(decoded.Models))
	}
	if decoded.Models[0].Name != "models/gemini-2.5-flash" {
		t.Fatalf("first name: got %q", decoded.Models[0].Name)
	}
	if len(decoded.Models[0].SupportedGenerationMethods) != 2 {
		t.Fatalf("methods: got %v", decoded.Models[0].SupportedGenerationMethods)
	}
}
