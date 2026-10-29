package relay

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/client"
	"github.com/zhizhishu/octopus/internal/helper"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 明文 chat 出站的头序必须跟渠道 cloak 走，且走的是生产客户端，不是测试里手搓的拨号。
//
// sendRequest 把 channelTLSFace 放进 context（WithTLSFace），ChannelHttpClientContext
// 在 face 非空时选 uTLS 客户端。http:// 上游不握 TLS，请求落到明文 fallback。
// 这里把这条链跑通：shape=claude/codex 必须排出对应 CLI 头序；shape 空/未知、
// cloak never/off 在 /v1/chat/completions 上必须保持 Go 的顺序（nil）。
func TestPlaintextChatCloakDrivesHTTP1WireOrder(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	cases := []struct {
		name    string
		channel *dbmodel.Channel
		path    string
		want    string // "claude" | "codex" | "go"
	}{
		{
			name:    "shape_claude",
			channel: &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIChat, Cloak: dbmodel.ChannelCloak{Shape: "claude"}},
			path:    "/v1/chat/completions",
			want:    "claude",
		},
		{
			name:    "shape_codex",
			channel: &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIChat, Cloak: dbmodel.ChannelCloak{Shape: "codex"}},
			path:    "/v1/chat/completions",
			want:    "codex",
		},
		{
			name:    "custom_chat_shape_claude",
			channel: &dbmodel.Channel{Type: outbound.OutboundTypeCustomOpenAIChat, Cloak: dbmodel.ChannelCloak{Shape: "claude"}},
			path:    "/v1/chat/completions",
			want:    "claude",
		},
		{
			name:    "shape_off",
			channel: &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIChat},
			path:    "/v1/chat/completions",
			want:    "go",
		},
		{
			name:    "shape_unknown",
			channel: &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIChat, Cloak: dbmodel.ChannelCloak{Shape: "gemini-ish"}},
			path:    "/v1/chat/completions",
			want:    "go",
		},
		{
			name: "cloak_never",
			channel: &dbmodel.Channel{
				Type:  outbound.OutboundTypeOpenAIChat,
				Cloak: dbmodel.ChannelCloak{Mode: "never", Shape: "claude"},
			},
			path: "/v1/chat/completions",
			want: "go",
		},
		{
			name: "cloak_off",
			channel: &dbmodel.Channel{
				Type:  outbound.OutboundTypeOpenAIChat,
				Cloak: dbmodel.ChannelCloak{Mode: "off", Shape: "codex"},
			},
			path: "/v1/chat/completions",
			want: "go",
		},
		{
			// Anthropic 渠道本身就是 claude face；路径规则给出同一张表。
			name:    "anthropic_channel",
			channel: &dbmodel.Channel{Type: outbound.OutboundTypeAnthropic},
			path:    "/v1/messages",
			want:    "claude",
		},
		{
			name:    "responses_channel",
			channel: &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIResponse},
			path:    "/v1/responses",
			want:    "codex",
		},
		{
			// cloak never 拿掉 face，但 /v1/messages 仍按路径规则排 claude 头序，
			// 与改动前的 http1RequestHeaderOrder 一致。
			name: "anthropic_cloak_never_keeps_path_rule",
			channel: &dbmodel.Channel{
				Type:  outbound.OutboundTypeAnthropic,
				Cloak: dbmodel.ChannelCloak{Mode: "never"},
			},
			path: "/v1/messages",
			want: "claude",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines := capturePlaintextChannel(t, ln, tc.channel, tc.path)
			got := classifyPlainWire(lines)
			if got != tc.want {
				t.Fatalf("wire class = %s, want %s\nheaders=%q", got, tc.want, lines)
			}
		})
	}
}

// capturePlaintextChannel 复刻 sendRequest 把 face 放上 context 的那两步，然后用
// 生产的 ChannelHttpClientContext 把请求打到本机明文端口。
func capturePlaintextChannel(t *testing.T, ln net.Listener, channel *dbmodel.Channel, path string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+ln.Addr().String()+path, strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ua-test")
	if face := channelTLSFace(channel); face != "" {
		req = req.WithContext(client.WithTLSFace(req.Context(), face))
	}

	httpClient, err := helper.ChannelHttpClientContext(req.Context(), channel)
	if err != nil {
		t.Fatalf("http client: %v", err)
	}

	type result struct {
		lines []string
		err   error
	}
	linesCh := make(chan result, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			linesCh <- result{err: err}
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		br := bufio.NewReader(conn)
		var lines []string
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				linesCh <- result{lines: lines, err: err}
				return
			}
			lines = append(lines, line)
			if line == "\r\n" {
				break
			}
		}
		linesCh <- result{lines: lines}
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
	}()

	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	select {
	case got := <-linesCh:
		if got.err != nil {
			t.Fatalf("read header block: %v", got.err)
		}
		return got.lines
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the captured request header block")
		return nil
	}
}

// classifyPlainWire 把一条明文头块收成三档里的一档。三档彼此的差异是稳定的：
// claude 会补 Connection 并把 Host 排到 User-Agent 之后；codex 把头名全部小写且
// 没有 Connection；Go 把 Host 放在第一行。
func classifyPlainWire(lines []string) string {
	hostIdx, uaIdx := -1, -1
	var hostName, uaName string
	hasConnection := false
	for i, line := range lines {
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "host:") && hostIdx < 0 {
			hostIdx = i
			hostName = strings.TrimRight(strings.SplitN(line, ":", 2)[0], "\r\n")
		}
		if strings.HasPrefix(lower, "user-agent:") && uaIdx < 0 {
			uaIdx = i
			uaName = strings.TrimRight(strings.SplitN(line, ":", 2)[0], "\r\n")
		}
		if strings.HasPrefix(lower, "connection:") {
			hasConnection = true
		}
	}
	switch {
	case hasConnection && hostIdx > uaIdx && hostName == "Host":
		return "claude"
	case !hasConnection && hostIdx > uaIdx && hostName == "host" && uaName == "user-agent":
		return "codex"
	case !hasConnection && hostIdx >= 0 && (uaIdx < 0 || hostIdx < uaIdx) && hostName == "Host":
		return "go"
	default:
		return "other"
	}
}
