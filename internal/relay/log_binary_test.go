package relay

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"

	transformerModel "github.com/zhizhishu/octopus/internal/transformer/model"
	openaiOutbound "github.com/zhizhishu/octopus/internal/transformer/outbound/openai"
)

const logCanaryKey = "sk-CANARYKEY00000000000000000000000000000000"

func TestStripInlineBinaryShapes(t *testing.T) {
	// 64 chars decodes to 48 bytes. Shorter fixtures are left in place: the
	// placeholder would make the log longer than the value it replaces.
	longRaw := strings.Repeat("A", 64)
	png := "data:image/png;base64," + longRaw
	jpeg := "data:image/jpeg;base64," + longRaw
	pdf := "data:application/pdf;base64," + longRaw
	httpsURL := "https://cdn.example.com/a.png?x=1&y=2"
	httpURL := "http://cdn.example.com/b.png"
	gsURL := "gs://bucket/obj.png"

	cases := []struct {
		name   string
		body   string
		want   []string
		keep   []string
		absent []string
		exact  bool
	}{
		{
			name:   "openai chat image_url data url keeps detail",
			body:   `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"` + png + `","detail":"high"}}]}]}`,
			want:   []string{`[image omitted: image/png, 48B]`, `"detail":"high"`, `"text":"look"`, `"model":"m"`},
			absent: []string{longRaw},
		},
		{
			name:  "https image url kept with query",
			body:  `{"type":"image_url","image_url":{"url":"` + httpsURL + `"}}`,
			exact: true,
			keep:  []string{httpsURL},
		},
		{
			name:  "http image url kept",
			body:  `{"type":"image_url","image_url":{"url":"` + httpURL + `"}}`,
			exact: true,
			keep:  []string{httpURL},
		},
		{
			name:  "gs file uri kept",
			body:  `{"fileData":{"mimeType":"image/png","fileUri":"` + gsURL + `"}}`,
			exact: true,
			keep:  []string{gsURL},
		},
		{
			name:  "short image_url token kept",
			body:  `{"image_url":"auto"}`,
			exact: true,
		},
		{
			name:   "responses input_image string data url",
			body:   `{"input":[{"type":"input_image","image_url":"` + jpeg + `","detail":"auto"}]}`,
			want:   []string{`[image omitted: image/jpeg, 48B]`, `"detail":"auto"`},
			absent: []string{longRaw},
		},
		{
			name:  "responses input_image https string kept",
			body:  `{"input":[{"type":"input_image","image_url":"` + httpsURL + `"}]}`,
			exact: true,
			keep:  []string{httpsURL},
		},
		{
			name:   "long raw base64 image_url string",
			body:   `{"type":"input_image","image_url":"` + longRaw + `"}`,
			want:   []string{`[image omitted: unknown, 48B]`},
			absent: []string{longRaw},
		},
		{
			name:   "input_audio",
			body:   `{"type":"input_audio","input_audio":{"format":"wav","data":"` + longRaw + `"}}`,
			want:   []string{`[audio omitted: audio/wav, 48B]`, `"format":"wav"`},
			absent: []string{longRaw},
		},
		{
			name:   "responses input_audio",
			body:   `{"type":"input_audio","input_audio":{"data":"` + longRaw + `","format":"mp3"}}`,
			want:   []string{`[audio omitted: audio/mp3, 48B]`, `"format":"mp3"`},
			absent: []string{longRaw},
		},
		{
			name:   "message audio keeps id and transcript",
			body:   `{"role":"assistant","audio":{"data":"` + longRaw + `","expires_at":9,"id":"aud_1","transcript":"hi"}}`,
			want:   []string{`[audio omitted: unknown, 48B]`, `"id":"aud_1"`, `"transcript":"hi"`, `"expires_at":9`},
			absent: []string{`"data":"` + longRaw + `"`},
		},
		{
			name:  "audio config without data is unchanged",
			body:  `{"audio":{"format":"wav","voice":"alloy"}}`,
			exact: true,
		},
		{
			name:   "file data url pdf keeps file_id and https file_url",
			body:   `{"type":"file","file":{"filename":"a.pdf","file_data":"` + pdf + `","media_type":"application/pdf","file_url":"` + httpsURL + `","file_id":"file_123"}}`,
			want:   []string{`[file omitted: application/pdf, 48B]`, `"filename":"a.pdf"`, `"file_id":"file_123"`, httpsURL, `"media_type":"application/pdf"`},
			absent: []string{longRaw},
		},
		{
			name:   "responses input_file raw base64",
			body:   `{"type":"input_file","filename":"a.pdf","file_data":"` + longRaw + `","file_id":"file_9"}`,
			want:   []string{`[file omitted: unknown, 48B]`, `"filename":"a.pdf"`, `"file_id":"file_9"`},
			absent: []string{`"file_data":"` + longRaw + `"`},
		},
		{
			name:   "anthropic image base64",
			body:   `{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/webp","data":"` + longRaw + `"}}]}`,
			want:   []string{`[image omitted: image/webp, 48B]`, `"type":"base64"`, `"media_type":"image/webp"`},
			absent: []string{`"data":"` + longRaw + `"`},
		},
		{
			name:  "anthropic image url kept",
			body:  `{"type":"image","source":{"type":"url","url":"` + httpsURL + `"}}`,
			exact: true,
			keep:  []string{httpsURL},
		},
		{
			name:   "anthropic document base64",
			body:   `{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"` + longRaw + `"},"title":"spec"}`,
			want:   []string{`[file omitted: application/pdf, 48B]`, `"title":"spec"`},
			absent: []string{`"data":"` + longRaw + `"`},
		},
		{
			name:  "anthropic document file id kept",
			body:  `{"type":"document","source":{"type":"file","file_id":"file_abc"}}`,
			exact: true,
			keep:  []string{`"file_id":"file_abc"`},
		},
		{
			name:   "anthropic source data before media_type",
			body:   `{"type":"image","source":{"data":"` + longRaw + `","media_type":"image/png","type":"base64"}}`,
			want:   []string{`[image omitted: image/png, 48B]`},
			absent: []string{`"data":"` + longRaw + `"`},
		},
		{
			name:   "gemini inlineData",
			body:   `{"contents":[{"parts":[{"inlineData":{"mimeType":"image/jpeg","data":"` + longRaw + `"}}]}]}`,
			want:   []string{`[image omitted: image/jpeg, 48B]`, `"mimeType":"image/jpeg"`},
			absent: []string{`"data":"` + longRaw + `"`},
		},
		{
			name:   "gemini inline_data snake case audio",
			body:   `{"parts":[{"inline_data":{"mime_type":"audio/wav","data":"` + longRaw + `"}}]}`,
			want:   []string{`[audio omitted: audio/wav, 48B]`},
			absent: []string{longRaw},
		},
		{
			name:   "gemini inline video",
			body:   `{"inlineData":{"mimeType":"video/mp4","data":"` + longRaw + `"}}`,
			want:   []string{`[video omitted: video/mp4, 48B]`},
			absent: []string{`"data":"` + longRaw + `"`},
		},
		{
			name:  "gemini fileData https kept",
			body:  `{"fileData":{"mimeType":"application/pdf","fileUri":"` + httpsURL + `"}}`,
			exact: true,
			keep:  []string{httpsURL},
		},
		{
			name:   "b64_json and bytesBase64Encoded",
			body:   `{"instances":[{"bytesBase64Encoded":"` + longRaw + `","mimeType":"image/png"}],"data":[{"b64_json":"` + longRaw + `"}]}`,
			want:   []string{`[image omitted: image/png, 48B]`, `[image omitted: unknown, 48B]`},
			absent: []string{`"bytesBase64Encoded":"` + longRaw + `"`, `"b64_json":"` + longRaw + `"`},
		},
		{
			name:  "response_format enum b64_json kept",
			body:  `{"response_format":"b64_json","model":"m"}`,
			exact: true,
		},
		{
			name:   "image generation result",
			body:   `{"type":"image_generation","result":"` + longRaw + `","output_format":"png"}`,
			want:   []string{`[image omitted: unknown, 48B]`, `"output_format":"png"`},
			absent: []string{longRaw},
		},
		{
			name:  "non image result kept",
			body:  `{"type":"function_call_output","result":"` + longRaw + `","image_url":"` + httpsURL + `"}`,
			exact: true,
			keep:  []string{longRaw, httpsURL},
		},
		{
			name:   "input_image_mask data url",
			body:   `{"tools":[{"type":"image_generation","image_generation":{"input_image_mask":{"image_url":"` + png + `"}}}]}`,
			want:   []string{`[image omitted: image/png, 48B]`},
			absent: []string{longRaw},
		},
		{
			name:   "pretty printed anthropic image",
			body:   "{\n  \"type\": \"image\",\n  \"source\": {\n    \"type\": \"base64\",\n    \"media_type\": \"image/png\",\n    \"data\": \"" + longRaw + "\"\n  }\n}",
			want:   []string{`[image omitted: image/png, 48B]`},
			absent: []string{longRaw},
		},
		{
			name:  "text that mentions a data url is kept",
			body:  `{"messages":[{"role":"user","content":"see data:image/png;base64,QUJDQUJD please"}]}`,
			exact: true,
			keep:  []string{"data:image/png;base64,QUJDQUJD"},
		},
		{
			name:  "tool arguments string is kept",
			body:  `{"tool_calls":[{"function":{"arguments":"{\"url\":\"data:image/png;base64,QUJD\"}"}}]}`,
			exact: true,
			keep:  []string{`data:image/png;base64,QUJD`},
		},
		{
			name:   "encrypted_content kept beside a stripped image",
			body:   `{"encrypted_content":"` + strings.Repeat("zZ1+", 40) + `","content":[{"type":"image_url","image_url":{"url":"` + png + `"}}]}`,
			want:   []string{strings.Repeat("zZ1+", 40), `[image omitted: image/png, 48B]`},
			absent: []string{longRaw},
		},
		{
			name:   "message images array",
			body:   `{"images":[{"type":"image_url","image_url":{"url":"` + png + `"}},{"type":"image_url","image_url":{"url":"` + httpsURL + `"}}]}`,
			want:   []string{`[image omitted: image/png, 48B]`, httpsURL},
			absent: []string{longRaw},
		},
		{
			name:  "invalid json unchanged",
			body:  `{"image_url":`,
			exact: true,
		},
		{
			name:  "non json unchanged",
			body:  strings.Repeat("x", 300),
			exact: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripInlineBinaryForLog(tc.body)
			if tc.exact && got != tc.body {
				t.Fatalf("expected original body\n got %s\nwant %s", got, tc.body)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Fatalf("missing %q in %s", w, got)
				}
			}
			for _, k := range tc.keep {
				if !strings.Contains(got, k) {
					t.Fatalf("dropped %q in %s", k, got)
				}
			}
			for _, a := range tc.absent {
				if strings.Contains(got, a) {
					t.Fatalf("still contains %q in %s", a, got)
				}
			}
		})
	}
}

func TestStripInlineBinaryDecodedSize(t *testing.T) {
	raw := bytes.Repeat([]byte{0x01}, 412*1024)
	b64 := base64.StdEncoding.EncodeToString(raw)
	body := `{"image_url":{"url":"data:image/png;base64,` + b64 + `"}}`
	got := stripInlineBinaryForLog(body)
	if !strings.Contains(got, "[image omitted: image/png, 412KB]") {
		t.Fatalf("placeholder = %s", got)
	}
	if strings.Contains(got, b64[:32]) {
		t.Fatal("base64 leaked")
	}
}

func TestRequestContentStripsInternalShapesAndKeepsRequest(t *testing.T) {
	payload := strings.Repeat("A", 64)
	png := "data:image/png;base64," + payload
	detail := "high"
	text := "hello " + logCanaryKey
	audioData := payload
	req := &transformerModel.InternalLLMRequest{
		Model: "gpt-img",
		Messages: []transformerModel.Message{{
			Role: "user",
			Content: transformerModel.MessageContent{MultipleContent: []transformerModel.MessageContentPart{
				{Type: "text", Text: &text},
				{Type: "image_url", ImageURL: &transformerModel.ImageURL{URL: png, Detail: &detail}},
				{Type: "input_audio", Audio: &transformerModel.Audio{Format: "wav", Data: audioData}},
				{Type: "file", File: &transformerModel.File{
					Filename: "a.pdf",
					FileData: "data:application/pdf;base64," + payload,
					FileURL:  "https://cdn.example.com/a.pdf",
					FileID:   "file_123",
				}},
			}},
			Images: []transformerModel.MessageContentPart{{
				Type:     "image_url",
				ImageURL: &transformerModel.ImageURL{URL: png},
			}},
			Audio: &struct {
				Data       string `json:"data,omitempty"`
				ExpiresAt  int64  `json:"expires_at,omitempty"`
				ID         string `json:"id,omitempty"`
				Transcript string `json:"transcript,omitempty"`
			}{Data: payload, ID: "aud", Transcript: "said"},
		}},
		ExtraBody: json.RawMessage(`{"inlineData":{"mimeType":"image/png","data":"` + payload + `"}}`),
		// Not serialized into request_content. Must survive on the struct.
		ResponsesInputRaw: json.RawMessage(`[{"type":"input_image","image_url":"` + png + `"}]`),
	}

	rawInput := append(json.RawMessage(nil), req.ResponsesInputRaw...)
	before, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	logged := requestContentForLog(req, "")
	after, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("requestContentForLog mutated InternalRequest")
	}
	if !strings.Contains(string(after), png) {
		t.Fatal("internal request lost its data url")
	}
	if !bytes.Equal(req.ResponsesInputRaw, rawInput) {
		t.Fatal("ResponsesInputRaw was cleared")
	}
	for _, absent := range []string{payload, logCanaryKey, "data:image/png", "data:application/pdf"} {
		if strings.Contains(logged, absent) {
			t.Fatalf("request log still contains %q: %s", absent, logged)
		}
	}
	for _, want := range []string{
		"[image omitted: image/png, 48B]",
		"[audio omitted: audio/wav, 48B]",
		"[file omitted: application/pdf, 48B]",
		"[audio omitted: unknown, 48B]",
		"https://cdn.example.com/a.pdf",
		"file_123",
		"[redacted]",
		"gpt-img",
		"hello",
		`"transcript":"said"`,
	} {
		if !strings.Contains(logged, want) {
			t.Fatalf("request log missing %q: %s", want, logged)
		}
	}
	// json:"-" raw passthrough is not part of request_content.
	if strings.Contains(logged, "input_image") {
		t.Fatalf("ResponsesInputRaw leaked into request_content: %s", logged)
	}
}

func TestRequestContentParamOverrideMerge(t *testing.T) {
	text := "keep-me"
	req := &transformerModel.InternalLLMRequest{
		Model: "m",
		Messages: []transformerModel.Message{{
			Role:    "user",
			Content: transformerModel.MessageContent{Content: &text},
		}},
	}
	before, _ := json.Marshal(req)

	payload := strings.Repeat("A", 64)
	override := `{
		"temperature": 0.5,
		"user": null,
		"extra_body": {"inlineData":{"mimeType":"image/png","data":"` + payload + `"}}
	}`
	logged := requestContentForLog(req, override)
	after, _ := json.Marshal(req)
	if !bytes.Equal(before, after) {
		t.Fatal("override merge mutated the request")
	}
	if !strings.Contains(logged, `"temperature":0.5`) && !strings.Contains(logged, `"temperature": 0.5`) {
		t.Fatalf("temperature override missing: %s", logged)
	}
	if !strings.Contains(logged, `"user":null`) {
		t.Fatalf("null override must be stored as null (maps.Copy), got %s", logged)
	}
	if !strings.Contains(logged, "keep-me") || !strings.Contains(logged, `"model":"m"`) {
		t.Fatalf("original fields dropped: %s", logged)
	}
	if strings.Contains(logged, payload) {
		t.Fatalf("override image leaked: %s", logged)
	}
	if !strings.Contains(logged, "[image omitted: image/png, 48B]") {
		t.Fatalf("override image not stripped: %s", logged)
	}

	// Invalid override falls back to the marshaled request, which is then stripped.
	png := "data:image/png;base64," + payload
	req.Messages[0].Content = transformerModel.MessageContent{MultipleContent: []transformerModel.MessageContentPart{{
		Type: "image_url", ImageURL: &transformerModel.ImageURL{URL: png},
	}}}
	fallback := requestContentForLog(req, `{`)
	if strings.Contains(fallback, payload) || !strings.Contains(fallback, "[image omitted: image/png, 48B]") {
		t.Fatalf("invalid override fallback = %s", fallback)
	}

	// A valid override still replaces the whole messages key.
	replaced := requestContentForLog(req, `{"messages":[{"role":"user","content":"only-override"}]}`)
	if strings.Contains(replaced, "image omitted") || !strings.Contains(replaced, "only-override") {
		t.Fatalf("override did not replace messages: %s", replaced)
	}
}

func TestUpstreamRequestBodyStripsWithoutMutatingOutbound(t *testing.T) {
	png := "data:image/png;base64," + strings.Repeat("A", 80)
	text := "see " + logCanaryKey
	req := &transformerModel.InternalLLMRequest{
		Model: "m",
		Messages: []transformerModel.Message{{
			Role: "user",
			Content: transformerModel.MessageContent{MultipleContent: []transformerModel.MessageContentPart{
				{Type: "text", Text: &text},
				{Type: "image_url", ImageURL: &transformerModel.ImageURL{URL: png}},
			}},
		}},
	}
	out := &openaiOutbound.CustomChatOutbound{}
	first, err := out.TransformRequest(context.Background(), req, "http://upstream.example", "k")
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	firstBody, err := io.ReadAll(first.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(firstBody, []byte(png)) {
		t.Fatalf("outbound transform dropped the image: %s", firstBody)
	}

	// Same backing array the relay re-mounts and then logs.
	raw := append([]byte(nil), firstBody...)
	httpReq, err := http.NewRequest(http.MethodPost, "http://upstream.example/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := snapshotOutboundRequestBody(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(snap, firstBody) {
		t.Fatal("snapshot differs from outbound body")
	}
	metrics := NewRelayMetrics(0, 0, "", "m", req)
	metrics.SetUpstreamRequestBody(snap)
	_ = requestContentForLog(req, "")

	got, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, firstBody) {
		t.Fatal("logging changed the outbound body")
	}
	if !bytes.Equal(snap, firstBody) {
		t.Fatal("SetUpstreamRequestBody mutated the snapshot buffer")
	}
	again, err := out.TransformRequest(context.Background(), req, "http://upstream.example", "k")
	if err != nil {
		t.Fatal(err)
	}
	secondBody, err := io.ReadAll(again.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(secondBody, firstBody) {
		t.Fatal("second transform differs after log filtering")
	}
	if strings.Contains(metrics.upstreamRequestBody, strings.Repeat("A", 40)) {
		t.Fatal("upstream log still has the image")
	}
	if strings.Contains(metrics.upstreamRequestBody, logCanaryKey) {
		t.Fatal("upstream log still has the key")
	}
	if !strings.Contains(metrics.upstreamRequestBody, "[image omitted: image/png,") {
		t.Fatalf("upstream log placeholder missing: %s", metrics.upstreamRequestBody)
	}
	if !strings.Contains(metrics.upstreamRequestBody, "[redacted]") {
		t.Fatalf("redaction missing: %s", metrics.upstreamRequestBody)
	}
}

func TestUpstreamBodyStripKeepsTailPastFormerTruncationPoint(t *testing.T) {
	payload := strings.Repeat("A", 200*1024)
	body := `{"model":"keep-model","messages":[{"content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + payload + `"}}]}],"tail":"KEEP_ME"}`
	raw := []byte(body)
	before := append([]byte(nil), raw...)
	metrics := NewRelayMetrics(0, 0, "", "", nil)
	metrics.SetUpstreamRequestBody(raw)
	if !bytes.Equal(raw, before) {
		t.Fatal("input buffer was mutated")
	}
	if strings.Contains(metrics.upstreamRequestBody, payload[:64]) {
		t.Fatal("payload leaked into upstream log")
	}
	if !strings.Contains(metrics.upstreamRequestBody, "KEEP_ME") || !strings.Contains(metrics.upstreamRequestBody, "keep-model") {
		t.Fatalf("fields after the image were lost: %s", metrics.upstreamRequestBody)
	}
	if strings.Contains(metrics.upstreamRequestBody, "truncated") {
		t.Fatalf("stripped body should fit under the cap, got %s", metrics.upstreamRequestBody)
	}
	if !strings.Contains(metrics.upstreamRequestBody, "[image omitted: image/png,") {
		t.Fatalf("placeholder missing: %s", metrics.upstreamRequestBody)
	}
}

func TestResponsesInputRawStrippedOnlyWhenSerializedUpstream(t *testing.T) {
	png := "data:image/png;base64," + strings.Repeat("B", 80)
	text := "hi"
	req := &transformerModel.InternalLLMRequest{
		Model: "m",
		Messages: []transformerModel.Message{{
			Role:    "user",
			Content: transformerModel.MessageContent{Content: &text},
		}},
		ResponsesInputRaw: json.RawMessage(`[{"type":"input_image","image_url":"` + png + `"}]`),
	}
	logged := requestContentForLog(req, "")
	if strings.Contains(logged, strings.Repeat("B", 40)) {
		t.Fatalf("unserialized ResponsesInputRaw landed in request_content: %s", logged)
	}
	upstream := `{"model":"m","input":` + string(req.ResponsesInputRaw) + `}`
	metrics := NewRelayMetrics(0, 0, "", "", req)
	metrics.SetUpstreamRequestBody([]byte(upstream))
	if strings.Contains(metrics.upstreamRequestBody, strings.Repeat("B", 40)) {
		t.Fatalf("upstream body kept responses image: %s", metrics.upstreamRequestBody)
	}
	if !strings.Contains(metrics.upstreamRequestBody, "[image omitted: image/png,") {
		t.Fatalf("upstream placeholder missing: %s", metrics.upstreamRequestBody)
	}
}

func TestStripInlineBinaryLargeInputDoesNotCopyPayload(t *testing.T) {
	const n = 2 << 20
	payload := strings.Repeat("A", n)
	body := `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + payload + `"}}]}]}`
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got := stripInlineBinaryForLog(body)
	runtime.ReadMemStats(&after)
	delta := after.TotalAlloc - before.TotalAlloc
	// One copy of the 2MB payload would already exceed this. The placeholder
	// path must skip the bytes instead of decoding them into another string.
	if delta > uint64(len(body))/2 {
		t.Fatalf("allocated %d bytes for %d byte input", delta, len(body))
	}
	if strings.Contains(got, payload[:64]) {
		t.Fatal("payload leaked")
	}
	if !strings.Contains(got, "[image omitted: image/png,") || !strings.Contains(got, `"model":"m"`) {
		t.Fatalf("unexpected log: %s", got)
	}
}

func TestStripInlineBinaryMIMEIsSanitized(t *testing.T) {
	if got := sanitizeLogMIME("image/svg+xml"); got != "image/svg+xml" {
		t.Fatalf("token mime = %q", got)
	}
	exact := "image/" + strings.Repeat("a", maxLogMIMELen-len("image/"))
	if len(exact) != maxLogMIMELen || sanitizeLogMIME(exact) != exact {
		t.Fatalf("64-char mime = %q (len %d)", sanitizeLogMIME(exact), len(exact))
	}
	for _, bad := range []string{
		"",
		exact + "a",
		"image/png;charset=utf-8",
		"image/\npng",
		"image/png evil",
		strings.Repeat("M", 4000),
	} {
		if got := sanitizeLogMIME(bad); got != "unknown" {
			t.Fatalf("sanitize(%q) = %q", bad, got)
		}
	}

	payload := strings.Repeat("A", 64)
	evil := "image/" + strings.Repeat("a", 200)
	body := `{"image_url":"data:` + evil + `;base64,` + payload + `"}`
	got := stripInlineBinaryForLog(body)
	if strings.Contains(got, strings.Repeat("a", 80)) || strings.Contains(got, payload) {
		t.Fatalf("crafted mime or payload landed in the log: %s", got)
	}
	if !strings.Contains(got, "[image omitted: unknown, 48B]") {
		t.Fatalf("placeholder = %s", got)
	}
	// A media_type field is client-controlled too; only the placeholder copy is capped.
	media := strings.Repeat("Z", 300)
	body = `{"type":"image","source":{"type":"base64","media_type":"` + media + `","data":"` + payload + `"}}`
	got = stripInlineBinaryForLog(body)
	if strings.Contains(got, "[image omitted: "+media) || strings.Contains(got, "[file omitted: "+media) {
		t.Fatalf("media_type copied into placeholder: %s", got)
	}
	if !strings.Contains(got, "omitted: unknown,") {
		t.Fatalf("expected unknown mime, got %s", got)
	}
}

func TestStripInlineBinaryDoesNotGrowOrRewriteTwice(t *testing.T) {
	short := `{"image_url":"data:image/png;base64,QUJD"}`
	if got := stripInlineBinaryForLog(short); got != short {
		t.Fatalf("short data URL must stay, got %s", got)
	}
	body := `{"image_url":{"url":"data:image/png;base64,` + strings.Repeat("A", 64) + `","detail":"high"}}`
	once := stripInlineBinaryForLog(body)
	if len(once) > len(body) {
		t.Fatalf("log grew %d -> %d", len(body), len(once))
	}
	if stripInlineBinaryForLog(once) != once {
		t.Fatalf("not idempotent: %s", once)
	}
	if !json.Valid([]byte(once)) {
		t.Fatalf("not JSON: %s", once)
	}
}

func TestStripDeepNestDoesNotPanic(t *testing.T) {
	var b strings.Builder
	const depth = maxJSONWalkDepth + 100
	b.WriteString(`{"image_url":"data:image/png;base64,`)
	b.WriteString(strings.Repeat("A", 64))
	b.WriteString(`","nest":`)
	for i := 0; i < depth; i++ {
		b.WriteString(`{"a":`)
	}
	b.WriteString(`1`)
	for i := 0; i < depth; i++ {
		b.WriteByte('}')
	}
	b.WriteByte('}')
	body := b.String()
	got := stripInlineBinaryForLog(body)
	if len(got) > len(body) {
		t.Fatalf("grew %d -> %d", len(body), len(got))
	}
	if !json.Valid([]byte(got)) {
		t.Fatal("deep nest became invalid JSON")
	}
	if stripInlineBinaryForLog(got) != got {
		t.Fatal("deep nest is not idempotent")
	}
	if strings.Contains(got, strings.Repeat("A", 64)) {
		t.Fatal("top-level image was not stripped")
	}
}

func TestStripInlineBinaryLargeTextAllocatesNothing(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("hello ", 200000) + `"}]}`
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got := stripInlineBinaryForLog(body)
	runtime.ReadMemStats(&after)
	delta := after.TotalAlloc - before.TotalAlloc
	if got != body {
		t.Fatal("text-only body was rewritten")
	}
	if delta > 8192 {
		t.Fatalf("text-only body allocated %d bytes", delta)
	}
}
