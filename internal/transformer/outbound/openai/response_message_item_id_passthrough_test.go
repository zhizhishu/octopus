package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	inboundOpenAI "github.com/zhizhishu/octopus/internal/transformer/inbound/openai"
	"github.com/zhizhishu/octopus/internal/transformer/model"
)

// upstreamMsgItemID is the long, upstream-minted Responses message item id that a
// real CLI saw on its direct leg (msg_ + 48 hex). oct must echo exactly this id to
// the client instead of minting a short one, so the client's next-turn replay hands
// the upstream back the id format the upstream itself minted.
const upstreamMsgItemID = "msg_0e71a8f0e7a69059016ace1fdfc904819591f49f6fa4093e44"

// runResponsesStreamRoundTrip drives the real upstream -> internal -> client path:
// each upstream Responses SSE event goes through the outbound transformer (upstream
// -> internal) and the resulting internal chunk through the inbound transformer
// (internal -> client). It returns the concatenated client-facing SSE bytes.
func runResponsesStreamRoundTrip(t *testing.T, upstreamEvents []string) string {
	t.Helper()

	out := &ResponseOutbound{}
	in := &inboundOpenAI.ResponseInbound{}

	var client strings.Builder
	for _, ev := range upstreamEvents {
		internal, err := out.TransformStream(context.Background(), []byte(ev))
		if err != nil {
			t.Fatalf("outbound TransformStream(%s): %v", ev, err)
		}
		if internal == nil {
			continue
		}
		chunk, err := in.TransformStream(context.Background(), internal)
		if err != nil {
			t.Fatalf("inbound TransformStream: %v", err)
		}
		client.Write(chunk)
	}

	// [DONE] flushes any still-open items so every message item finalizes.
	done, err := in.TransformStream(context.Background(), &model.InternalLLMResponse{Object: "[DONE]"})
	if err != nil {
		t.Fatalf("inbound TransformStream([DONE]): %v", err)
	}
	client.Write(done)

	return client.String()
}

// runResponsesNonStreamRoundTrip drives the real upstream -> internal -> client
// non-stream path and returns the decoded client-facing Responses body.
func runResponsesNonStreamRoundTrip(t *testing.T, upstreamBody string) ResponsesResponse {
	t.Helper()

	out := &ResponseOutbound{}
	internal, err := out.TransformResponse(context.Background(), &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	})
	if err != nil {
		t.Fatalf("outbound TransformResponse: %v", err)
	}

	body, err := (&inboundOpenAI.ResponseInbound{}).TransformResponse(context.Background(), internal)
	if err != nil {
		t.Fatalf("inbound TransformResponse: %v", err)
	}

	var resp ResponsesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode client Responses body %q: %v", string(body), err)
	}
	return resp
}

// clientMessageItemIDs returns every distinct client-facing message output item id
// seen on response.output_item.added / response.output_item.done.
func clientMessageItemIDs(t *testing.T, raw string) []string {
	t.Helper()

	var ids []string
	seen := map[string]bool{}
	for _, block := range strings.Split(raw, "\n\n") {
		line := strings.TrimSpace(block)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" || payload == "" {
			continue
		}
		var ev ResponsesStreamEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			t.Fatalf("decode client SSE event %q: %v", payload, err)
		}
		if ev.Item == nil || ev.Item.Type != "message" || ev.Item.ID == "" {
			continue
		}
		if seen[ev.Item.ID] {
			continue
		}
		seen[ev.Item.ID] = true
		ids = append(ids, ev.Item.ID)
	}
	return ids
}

// clientMessageItemID returns the id of the first message output item in a client body.
func clientMessageItemID(resp ResponsesResponse) string {
	for _, item := range resp.Output {
		if item.Type == "message" {
			return item.ID
		}
	}
	return ""
}

// TestResponsesMessageItemIDPassthroughStream locks the upstream message item id
// through the streaming relay: when the upstream's message output_item.added carries
// an id, the client-facing message item events carry that exact id; when the upstream
// sends no id, oct mints a msg_ + 16 fallback.
func TestResponsesMessageItemIDPassthroughStream(t *testing.T) {
	t.Run("upstream id preserved", func(t *testing.T) {
		client := runResponsesStreamRoundTrip(t, []string{
			`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.6","status":"in_progress","output":[]}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"id":"` + upstreamMsgItemID + `","type":"message","role":"assistant","content":[]}}`,
			`{"type":"response.output_text.delta","output_index":0,"item_id":"` + upstreamMsgItemID + `","content_index":0,"delta":"hello"}`,
			`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.6","status":"completed","output":[{"id":"` + upstreamMsgItemID + `","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}}`,
		})

		ids := clientMessageItemIDs(t, client)
		if len(ids) != 1 || ids[0] != upstreamMsgItemID {
			t.Fatalf("client must receive the upstream message item id %q verbatim, got %v\nSSE:\n%s", upstreamMsgItemID, ids, client)
		}
	})

	t.Run("empty upstream id falls back to msg_+16", func(t *testing.T) {
		client := runResponsesStreamRoundTrip(t, []string{
			`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.6","status":"in_progress","output":[]}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant","content":[]}}`,
			`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hello"}`,
			`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.6","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}}`,
		})

		ids := clientMessageItemIDs(t, client)
		if len(ids) != 1 {
			t.Fatalf("expected exactly one client message item, got %v\nSSE:\n%s", ids, client)
		}
		if !strings.HasPrefix(ids[0], "msg_") || len(ids[0]) != len("msg_")+16 {
			t.Fatalf("fallback message item id must be msg_ + 16 chars, got %q", ids[0])
		}
	})
}

// TestResponsesMessageItemIDPassthroughNonStream is the non-stream sibling: the
// upstream output message item's id survives onto the client-facing response, and an
// id-less upstream message item gets a msg_ + 16 fallback.
func TestResponsesMessageItemIDPassthroughNonStream(t *testing.T) {
	t.Run("upstream id preserved", func(t *testing.T) {
		resp := runResponsesNonStreamRoundTrip(t,
			`{"id":"resp_1","model":"gpt-5.6","status":"completed","output":[{"id":"`+upstreamMsgItemID+`","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}`)

		if got := clientMessageItemID(resp); got != upstreamMsgItemID {
			t.Fatalf("client message item id must equal the upstream id %q, got %q", upstreamMsgItemID, got)
		}
	})

	t.Run("empty upstream id falls back to msg_+16", func(t *testing.T) {
		resp := runResponsesNonStreamRoundTrip(t,
			`{"id":"resp_1","model":"gpt-5.6","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}`)

		got := clientMessageItemID(resp)
		if !strings.HasPrefix(got, "msg_") || len(got) != len("msg_")+16 {
			t.Fatalf("fallback message item id must be msg_ + 16 chars, got %q", got)
		}
	})
}
