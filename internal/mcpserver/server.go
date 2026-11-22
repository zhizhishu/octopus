package mcpserver

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/server/middleware"
)

const maxBodyBytes = 192 << 20

func enabled(key model.SettingKey) bool {
	on, err := op.SettingGetBool(key)
	return err == nil && on
}

type legacyEntry struct {
	handler *mcp.SSEHandler
	readers int
}

func NewHandler(loopback http.Handler) http.Handler {
	getServer := func(r *http.Request) *mcp.Server { return newServer(loopback, r) }
	streamable := mcp.NewStreamableHTTPHandler(getServer, &mcp.StreamableHTTPOptions{
		SessionTimeout: 30 * time.Minute, MaxRequestBodyBytes: maxBodyBytes,
		PropagateRequestCancellation: true,
	})
	// Legacy SSE has no SDK token/session binding. Separate its session registry
	// by key, and discard the registry when its last GET stream ends.
	var mu sync.Mutex
	legacy := make(map[string]*legacyEntry)
	transport := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcp" {
			streamable.ServeHTTP(w, r)
			return
		}
		id := auth.TokenInfoFromContext(r.Context()).UserID
		mu.Lock()
		entry := legacy[id]
		if r.Method == http.MethodGet {
			if entry == nil {
				entry = &legacyEntry{handler: mcp.NewSSEHandler(getServer, &mcp.SSEOptions{MaxRequestBodyBytes: maxBodyBytes})}
				legacy[id] = entry
			}
			entry.readers++
		}
		mu.Unlock()
		if entry == nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		if r.Method == http.MethodGet {
			defer func() {
				mu.Lock()
				defer mu.Unlock()
				entry.readers--
				if entry.readers == 0 {
					delete(legacy, id)
				}
			}()
		}
		entry.handler.ServeHTTP(w, r)
	})
	verified := auth.RequireBearerToken(func(ctx context.Context, token string, r *http.Request) (*auth.TokenInfo, error) {
		key, _, err := middleware.ValidateAPIKey(ctx, token)
		if err != nil {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{UserID: strconv.Itoa(key.ID), Extra: map[string]any{"request": r}}, nil
	}, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(transport)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !enabled(model.SettingKeyMCPEnabled) {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path != "/mcp" && r.URL.Path != "/mcp/sse" {
			http.NotFound(w, r)
			return
		}
		fields := strings.Fields(r.Header.Get("Authorization"))
		if len(fields) != 2 || fields[0] != "Bearer" || r.URL.Query().Has("key") || r.URL.Query().Has("api_key") || r.URL.Query().Has("access_token") {
			http.Error(w, "Authorization: Bearer API key required", http.StatusUnauthorized)
			return
		}
		verified.ServeHTTP(w, r)
	})
}

func newServer(loopback http.Handler, initial *http.Request) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "octopus-image", Version: "1.0.0"}, nil)
	tools := []*mcp.Tool{imageTool(false), imageTool(true)}
	for i, tool := range tools {
		edit := i == 1
		s.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			key := model.SettingKeyMCPGenerateEnabled
			if edit {
				key = model.SettingKeyMCPEditEnabled
			}
			if !enabled(model.SettingKeyMCPEnabled) || !enabled(key) {
				return toolError(403, "tool is disabled"), nil
			}
			caller := initial
			if req.Extra != nil && req.Extra.TokenInfo != nil {
				if r, ok := req.Extra.TokenInfo.Extra["request"].(*http.Request); ok {
					caller = r
				}
			}
			return callImage(ctx, loopback, caller, req.Params.Arguments, edit), nil
		})
	}
	s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/list" {
				list := &mcp.ListToolsResult{Tools: []*mcp.Tool{}}
				if enabled(model.SettingKeyMCPEnabled) {
					if enabled(model.SettingKeyMCPGenerateEnabled) {
						list.Tools = append(list.Tools, tools[0])
					}
					if enabled(model.SettingKeyMCPEditEnabled) {
						list.Tools = append(list.Tools, tools[1])
					}
				}
				return list, nil
			}
			return next(ctx, method, req)
		}
	})
	return s
}
