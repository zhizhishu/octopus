package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

// D02 finding G5 regression: the *synthesized* terminal frame of a Responses
// stream must go through the same client-format restore gate as every
// upstream-derived frame.
//
// Why this case and not a hand-written terminal frame: the real capture (gwslbox
// candidate run 033a2a, case S03) shows an upstream that streams the placeholder
// as text deltas and then closes with a `response.completed` whose
// `response.output` is EMPTY, followed by `data: [DONE]`. oct's inbound Responses
// adapter therefore answers that terminal frame from its own accumulated (still
// redacted) state, and the frame it emits carries oct's own `sequence_number`,
// the client-facing model name and a filled `usage` — i.e. it is oct-built, not
// forwarded. Before the fix four frames left through the restore gate
// (`placeholder=false`) and this one left through the ungated synthesized path
// (`placeholder=true`), so the client saw `{{Redact:<64 hex>}}` in the terminal
// frame while the sibling copies of the same text were restored.
//
// Captured frame order this test pins (real capture, 51 frames / 166 lines;
// `_artifacts/runs/d02-gapline/run-033a2a/raw/S03-responses-stream-split-ph.txt`,
// delta count differs only because the upstream chunk size differs):
//
//	response.created -> response.in_progress -> response.output_item.added ->
//	response.content_part.added -> response.output_text.delta xN ->
//	response.output_text.done -> response.content_part.done ->
//	response.output_item.done -> response.completed -> [DONE]
func TestRelayRedactResponsesTerminalFrameRestored(t *testing.T) {
	setupRedactDB(t)

	// 64-hex high-entropy secret: detected by the H detector and (unlike a
	// repeated pattern such as cafebabe x4) it survives the core entropy gate.
	const secret = "9f3c1a7e5b2d8046cf13a9e7b4d6208fa5c8e10d3b7f9462ae08d5c13f7b2e94"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		token := redactTokenRe.FindString(string(buf[:n]))
		if token == "" {
			t.Errorf("upstream never received a placeholder; body=%s", string(buf[:n]))
		}

		w.Header().Set("Content-Type", "text/event-stream")
		writeEvent := func(ev string) {
			w.Write([]byte("event: " + ev + "\ndata: " + ev + "\n\n"))
		}
		echo := "leaked " + token + " tail"
		writeEvent(`{"type":"response.created","response":{"id":"resp_d02","object":"response","status":"in_progress","model":"d02","output":[]}}`)
		writeEvent(`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_d02","role":"assistant","status":"in_progress","content":[]}}`)
		writeEvent(`{"type":"response.content_part.added","item_id":"msg_d02","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`)
		// Split the placeholder across events, exactly like the captured case: the
		// stream restorer has to hold the partial prefix.
		for _, chunk := range splitChunks(echo, 12) {
			writeEvent(`{"type":"response.output_text.delta","item_id":"msg_d02","output_index":0,"content_index":0,"delta":` + jsonString(chunk) + `}`)
		}
		writeEvent(`{"type":"response.output_text.done","item_id":"msg_d02","output_index":0,"content_index":0,"text":` + jsonString(echo) + `}`)
		writeEvent(`{"type":"response.content_part.done","item_id":"msg_d02","output_index":0,"content_index":0,"part":{"type":"output_text","text":` + jsonString(echo) + `,"annotations":[]}}`)
		writeEvent(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_d02","role":"assistant","status":"completed","content":[{"type":"output_text","text":` + jsonString(echo) + `,"annotations":[]}]}}`)
		// The captured upstream closes with an EMPTY output array.
		writeEvent(`{"type":"response.completed","response":{"id":"resp_d02","object":"response","status":"completed","model":"d02","output":[]}}`)
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(upstream.Close)
	newRedactChannelFull(t, "redact-responses-terminal", upstream.URL, outbound.OutboundTypeOpenAIResponse, true, "HPSIBEG", 1)

	engine := newRedactGinEngine()
	engine.POST("/v1/responses", func(c *gin.Context) {
		c.Set("api_key_id", 0)
		c.Set("user_id", 0)
		c.Set("request_ip", "127.0.0.1")
		Handler(inbound.InboundTypeOpenAIResponse, c)
	})
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
		`{"model":"request-model","stream":true,"input":"my key is `+secret+`"}`))
	engine.ServeHTTP(rec, c.Request)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// 1. Nothing may leak, in any frame.
	if leftovers := redactTokenRe.FindAllString(body, -1); len(leftovers) > 0 {
		t.Fatalf("terminal frame leaked %d placeholder(s) to the client; first=%s\nbody:\n%s",
			len(leftovers), leftovers[0], body)
	}
	// 2. The synthesized terminal frame itself must carry the real secret.
	completed := frameOfType(body, "response.completed")
	if completed == "" {
		t.Fatalf("client never received a response.completed frame:\n%s", body)
	}
	if !strings.Contains(completed, secret) {
		t.Fatalf("terminal frame did not restore the secret: %s", completed)
	}
	// 3. The sibling completion frames must keep restoring (no regression).
	for _, ev := range []string{"response.output_text.done", "response.content_part.done", "response.output_item.done"} {
		frame := frameOfType(body, ev)
		if frame == "" {
			t.Fatalf("client never received %s:\n%s", ev, body)
		}
		if !strings.Contains(frame, secret) {
			t.Fatalf("%s did not restore the secret: %s", ev, frame)
		}
	}
	// 4. Frame order must match the captured client stream exactly, and the turn
	//    must still close with [DONE] after the terminal frame.
	want := []string{
		"response.created", "response.in_progress", "response.output_item.added",
		"response.content_part.added", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done",
		"response.output_item.done", "response.completed", "[DONE]",
	}
	got := clientFrameOrder(body)
	if strings.Join(got, " -> ") != strings.Join(want, " -> ") {
		t.Fatalf("client frame order changed:\n got: %s\nwant: %s\nbody:\n%s",
			strings.Join(got, " -> "), strings.Join(want, " -> "), body)
	}
}

// TestRelayRedactResponsesTerminalFrameHeldTailIsSafe: the write-order change
// (synthesized frame ingested BEFORE the flush) must never let the synthesized
// terminal frame jump ahead of content the restorer is still holding. Here the
// upstream ends mid-placeholder, so the restorer is holding an indeterminate
// prefix when the stream closes: the hard invariant is that nothing raw and no
// half placeholder may reach the client — a fail-closed error is acceptable, a
// silent success with a leaked fragment is not.
func TestRelayRedactResponsesTerminalFrameHeldTailIsSafe(t *testing.T) {
	setupRedactDB(t)

	const secret = "5b8e0d2a4f7c1396ab6e4d05c87f2a913d64b80e5a2c7f4918360edb25a9c147"

	var upstreamToken string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		token := redactTokenRe.FindString(string(buf[:n]))
		if token == "" {
			t.Errorf("upstream never received a placeholder; body=%s", string(buf[:n]))
		}
		upstreamToken = token

		w.Header().Set("Content-Type", "text/event-stream")
		writeEvent := func(ev string) {
			w.Write([]byte("event: " + ev + "\ndata: " + ev + "\n\n"))
		}
		writeEvent(`{"type":"response.created","response":{"id":"resp_d02","object":"response","status":"in_progress","model":"d02","output":[]}}`)
		writeEvent(`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_d02","role":"assistant","status":"in_progress","content":[]}}`)
		writeEvent(`{"type":"response.content_part.added","item_id":"msg_d02","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`)
		// A complete half plus an incomplete opening brace pair: the restorer ends
		// the stream holding an ambiguous registered prefix.
		writeEvent(`{"type":"response.output_text.delta","item_id":"msg_d02","output_index":0,"content_index":0,"delta":"prefix "}`)
		writeEvent(`{"type":"response.output_text.delta","item_id":"msg_d02","output_index":0,"content_index":0,"delta":` + jsonString(token[:20]) + `}`)
		writeEvent(`{"type":"response.completed","response":{"id":"resp_d02","object":"response","status":"completed","model":"d02","output":[]}}`)
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(upstream.Close)
	newRedactChannelFull(t, "redact-responses-held-tail", upstream.URL, outbound.OutboundTypeOpenAIResponse, true, "HPSIBEG", 1)

	engine := newRedactGinEngine()
	engine.POST("/v1/responses", func(c *gin.Context) {
		c.Set("api_key_id", 0)
		c.Set("user_id", 0)
		c.Set("request_ip", "127.0.0.1")
		Handler(inbound.InboundTypeOpenAIResponse, c)
	})
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
		`{"model":"request-model","stream":true,"input":"my key is `+secret+`"}`))
	engine.ServeHTTP(rec, c.Request)

	body := rec.Body.String()
	// Hard invariant: no raw placeholder and no long token fragment may be written.
	if leftovers := redactTokenRe.FindAllString(body, -1); len(leftovers) > 0 {
		t.Fatalf("held tail leaked %d placeholder(s): %s\nbody:\n%s", len(leftovers), leftovers[0], body)
	}
	if upstreamToken != "" && strings.Contains(body, upstreamToken[:40]) {
		t.Fatalf("held tail wrote a raw token/secret fragment:\n%s", body)
	}
	// Nothing may be swallowed either: if the turn reports success it must not
	// pretend the truncated text was complete.
	if rec.Code == http.StatusOK && strings.Contains(body, "response.completed") {
		if !strings.Contains(body, `"status":"completed"`) {
			t.Fatalf("terminal frame inconsistent:\n%s", body)
		}
	}
	t.Logf("held-tail outcome: status=%d body=%s", rec.Code, truncateForLog(body, 400))
}

// clientFrameOrder returns the client-visible frame types in order, collapsing
// consecutive repeats (delta runs) to a single entry, and the terminal [DONE]
// marker as its own entry — i.e. the frame ORDER, not the chunk count.
func clientFrameOrder(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := line[len("data: "):]
		if strings.TrimSpace(payload) == "[DONE]" {
			out = append(out, "[DONE]")
			continue
		}
		if !strings.Contains(payload, `"type":"`) {
			continue
		}
		start := strings.Index(payload, `"type":"`) + len(`"type":"`)
		end := strings.Index(payload[start:], `"`)
		if end < 0 {
			continue
		}
		ev := payload[start : start+end]
		if len(out) > 0 && out[len(out)-1] == ev {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// frameOfType returns the `data:` line of the first frame whose JSON carries the
// given `"type"`.
func frameOfType(body, eventType string) string {
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		if strings.Contains(line, `"type":"`+eventType+`"`) {
			return line
		}
	}
	return ""
}

// splitChunks cuts s into size-byte pieces, mirroring the fake upstream's chunker.
func splitChunks(s string, size int) []string {
	var out []string
	for len(s) > size {
		out = append(out, s[:size])
		s = s[size:]
	}
	if len(s) > 0 {
		out = append(out, s)
	}
	return out
}

// jsonString quotes s as a JSON string (test payloads are ASCII only).
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
