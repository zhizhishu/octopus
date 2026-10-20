package relay

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/transformer/model"
	openaiOutbound "github.com/zhizhishu/octopus/internal/transformer/outbound/openai"
)

const raceContentTextJSON = `{"choices":[{"delta":{"content":"hello"}}]}`

type raceContentTestAdapter struct {
	model.Outbound
	stream   func(context.Context, []byte) (*model.InternalLLMResponse, error)
	response func(context.Context, *http.Response) (*model.InternalLLMResponse, error)
}

func (a *raceContentTestAdapter) TransformStream(ctx context.Context, data []byte) (*model.InternalLLMResponse, error) {
	if a.stream != nil {
		return a.stream(ctx, data)
	}
	return (&openaiOutbound.ChatOutbound{}).TransformStream(ctx, data)
}

func (a *raceContentTestAdapter) TransformResponse(ctx context.Context, response *http.Response) (*model.InternalLLMResponse, error) {
	if a.response != nil {
		return a.response(ctx, response)
	}
	return (&openaiOutbound.ChatOutbound{}).TransformResponse(ctx, response)
}

type raceContentTrackedBody struct {
	io.ReadCloser
	readBytes int
	closed    atomic.Bool
}

func (b *raceContentTrackedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.readBytes += n
	return n, err
}

func (b *raceContentTrackedBody) Close() error {
	b.closed.Store(true)
	return b.ReadCloser.Close()
}

func raceContentTestResponse(raw string) (*http.Response, *raceContentTrackedBody) {
	body := &raceContentTrackedBody{ReadCloser: io.NopCloser(strings.NewReader(raw))}
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, body
}

func raceContentAssertReplay(t *testing.T, response *http.Response, want []byte) {
	t.Helper()
	got, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read replay: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("replay differs: got %d bytes, want %d", len(got), len(want))
	}
}

func raceContentTestInternal(t *testing.T, raw string) *model.InternalLLMResponse {
	t.Helper()
	var response model.InternalLLMResponse
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		t.Fatalf("invalid test response: %v", err)
	}
	return &response
}

func TestRaceResponseHasContent(t *testing.T) {
	if raceResponseHasContent(nil) {
		t.Fatal("nil response won")
	}
	for _, tc := range []struct {
		name string
		raw  string
		want bool
	}{
		{"empty", `{}`, false},
		{"role", `{"choices":[{"delta":{"role":"assistant"}}]}`, false},
		{"usage", `{"usage":{"completion_tokens":5,"total_tokens":5}}`, false},
		{"embedding", `{"embedding_data":[{"embedding":[1]}]}`, false},
		{"whitespace", `{"choices":[{"message":{"content":" \n\t"}}]}`, false},
		{"done", `{"object":"[DONE]","choices":[{"delta":{"content":"text"}}]}`, false},
		{"message", `{"choices":[{"message":{"content":"text"}}]}`, true},
		{"delta", raceContentTextJSON, true},
		{"thinking", `{"choices":[{"delta":{"reasoning_content":"think"}}]}`, true},
		{"thinking_alias", `{"choices":[{"message":{"reasoning":"think"}}]}`, true},
		{"tool", `{"choices":[{"delta":{"tool_calls":[{"id":"call_1"}]}}]}`, true},
		{"refusal", `{"choices":[{"message":{"refusal":"cannot comply"}}]}`, true},
		{"later_choice", `{"choices":[{"delta":{"role":"assistant"}},{"message":{"content":"text"}}]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := raceResponseHasContent(raceContentTestInternal(t, tc.raw)); got != tc.want {
				t.Fatalf("has content = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAwaitRaceContentNilInputs(t *testing.T) {
	adapter := &openaiOutbound.ChatOutbound{}
	var typedNil *openaiOutbound.ChatOutbound
	for _, tc := range []struct {
		name     string
		response *http.Response
		adapter  model.Outbound
	}{
		{"response", nil, adapter},
		{"body", &http.Response{}, adapter},
		{"adapter", &http.Response{Body: http.NoBody}, nil},
		{"typed_nil_adapter", &http.Response{Body: http.NoBody}, typedNil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := awaitRaceContent(context.Background(), tc.response, tc.adapter, true, 0); err == nil {
				t.Fatal("nil input accepted")
			}
		})
	}
}

func TestAwaitRaceContentStreamReplayByteExact(t *testing.T) {
	first := ": comment\r\nid: cursor\r\nretry: 200\r\nevent: message\r\ndata: {\"choices\": [\r\ndata: {\"delta\":{\"content\":\"hello\"}}]}\r\n\r\n"
	raw := first + ": " + strings.Repeat("tail", 4096) + "\r\n\r\ndata: [DONE]\r\n\r\n"
	response, original := raceContentTestResponse(raw)
	if err := awaitRaceContent(context.Background(), response, &openaiOutbound.ChatOutbound{}, true, time.Second); err != nil {
		t.Fatal(err)
	}
	if original.readBytes <= len(first) || original.readBytes >= len(raw) {
		t.Fatalf("test requires read-ahead plus unread tail, consumed %d of %d", original.readBytes, len(raw))
	}
	if original.closed.Load() {
		t.Fatal("probe closed successful body")
	}
	raceContentAssertReplay(t, response, []byte(raw))
	if err := response.Body.Close(); err != nil || !original.closed.Load() {
		t.Fatalf("replay Close did not close original: %v", err)
	}
}

func TestAwaitRaceContentStreamAcceptsGeneratedContent(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"text", raceContentTextJSON},
		{"thinking", `{"choices":[{"delta":{"reasoning_content":"think"}}]}`},
		{"tool", `{"choices":[{"delta":{"tool_calls":[{"id":"call_1","function":{"name":"lookup","arguments":"{}"}}]}}]}`},
		{"refusal", `{"choices":[{"delta":{"refusal":"cannot comply"}}]}`},
		{"finish_with_text", `{"choices":[{"delta":{"content":"last"},"finish_reason":"stop"}]}`},
		{"completion_with_text", `{"type":"response.completed","choices":[{"message":{"content":"last"}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := "data: " + tc.raw + "\n\n"
			response, _ := raceContentTestResponse(raw)
			defer response.Body.Close()
			if err := awaitRaceContent(context.Background(), response, &openaiOutbound.ChatOutbound{}, true, time.Second); err != nil {
				t.Fatal(err)
			}
			raceContentAssertReplay(t, response, []byte(raw))
		})
	}
}

func TestAwaitRaceContentStreamRejectsEmptyAndError(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"role_eof", "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"},
		{"usage_done", "data: {\"usage\":{\"completion_tokens\":9}}\n\ndata: [DONE]\n\n"},
		{"role_done", "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: [DONE]\n\n"},
		{"opener_completed", "data: {\"type\":\"response.created\"}\n\ndata: {\"type\":\"response.completed\"}\n\n"},
		{"message_stop", "event: message_stop\ndata: {}\n\n"},
		{"finish", "data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\n"},
		{"whitespace", "data: {\"choices\":[{\"delta\":{\"content\":\"  \"}}]}\n\n"},
		{"invalid_json", "data: {invalid}\n\n"},
		{"error_payload", "data: {\"error\":{\"message\":\"failed\"}}\n\n"},
		{"error_event_with_content", "event: error\ndata: " + raceContentTextJSON + "\n\n"},
		{"failed_with_content", "data: {\"type\":\"response.failed\",\"choices\":[{\"delta\":{\"content\":\"not a win\"}}]}\n\n"},
		{"incomplete_with_content", "event: response.incomplete\ndata: " + raceContentTextJSON + "\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response, _ := raceContentTestResponse(tc.raw)
			defer response.Body.Close()
			if err := awaitRaceContent(context.Background(), response, &openaiOutbound.ChatOutbound{}, true, time.Second); err == nil {
				t.Fatal("empty/error stream won")
			}
		})
	}
}

// This body models headers arriving promptly while the body never makes progress.
// Close is the only way to release its synchronous Read.
type raceContentBlockingBody struct {
	started   chan struct{}
	closed    chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
}

func newRaceContentBlockingBody() *raceContentBlockingBody {
	return &raceContentBlockingBody{started: make(chan struct{}), closed: make(chan struct{})}
}

func (b *raceContentBlockingBody) Read([]byte) (int, error) {
	b.startOnce.Do(func() { close(b.started) })
	<-b.closed
	return 0, errors.New("test body closed")
}

func (b *raceContentBlockingBody) Close() error {
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

func raceContentWaitResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("probe did not finish after body Close")
		return nil
	}
}

func TestAwaitRaceContentHeadersFastBodySlowTimeout(t *testing.T) {
	body := newRaceContentBlockingBody()
	defer body.Close()
	response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}
	result := make(chan error, 1)
	go func() {
		result <- awaitRaceContent(context.Background(), response, &openaiOutbound.ChatOutbound{}, true, 20*time.Millisecond)
	}()
	if err := raceContentWaitResult(t, result); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected content timeout, got %v", err)
	}
	select {
	case <-body.closed:
	default:
		t.Fatal("timeout did not close body")
	}
}

func TestAwaitRaceContentCancelAndZeroBudget(t *testing.T) {
	for _, stream := range []bool{true, false} {
		name := "json"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			body := newRaceContentBlockingBody()
			defer body.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			response := &http.Response{Header: make(http.Header), Body: body}
			result := make(chan error, 1)
			go func() {
				result <- awaitRaceContent(ctx, response, &openaiOutbound.ChatOutbound{}, stream, 0)
			}()
			select {
			case <-body.started:
			case <-time.After(time.Second):
				t.Fatal("probe never started reading")
			}
			select {
			case err := <-result:
				t.Fatalf("zero budget ended before cancellation: %v", err)
			default:
			}
			cancel()
			if err := raceContentWaitResult(t, result); !errors.Is(err, context.Canceled) {
				t.Fatalf("expected cancellation, got %v", err)
			}
		})
	}
}

func TestAwaitRaceContentAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	response, body := raceContentTestResponse("data: " + raceContentTextJSON + "\n\n")
	if err := awaitRaceContent(ctx, response, &openaiOutbound.ChatOutbound{}, true, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if !body.closed.Load() || body.readBytes != 0 {
		t.Fatal("canceled probe read or left body open")
	}
}

func TestAwaitRaceContentSuccessStopsTimerAndCancellationWatcher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	raw := "data: " + raceContentTextJSON + "\n\ndata: [DONE]\n\n"
	response, body := raceContentTestResponse(raw)
	if err := awaitRaceContent(ctx, response, &openaiOutbound.ChatOutbound{}, true, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	cancel()
	<-time.After(200 * time.Millisecond)
	if body.closed.Load() {
		t.Fatal("stopped timeout/cancel watcher closed successful body")
	}
	raceContentAssertReplay(t, response, []byte(raw))
	_ = response.Body.Close()
}

type raceContentCloseGateBody struct {
	io.ReadCloser
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *raceContentCloseGateBody) Close() error {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return b.ReadCloser.Close()
}

func TestAwaitRaceContentJoinsRunningTimeoutBeforeReturn(t *testing.T) {
	body := &raceContentCloseGateBody{
		ReadCloser: io.NopCloser(strings.NewReader("data: {}\n\n")),
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(body.release) }) }
	defer release()
	adapter := &raceContentTestAdapter{stream: func(context.Context, []byte) (*model.InternalLLMResponse, error) {
		<-body.entered
		text := "arrived too late"
		return &model.InternalLLMResponse{Choices: []model.Choice{{Delta: &model.Message{Content: model.MessageContent{Content: &text}}}}}, nil
	}}
	result := make(chan error, 1)
	go func() {
		result <- awaitRaceContent(context.Background(), &http.Response{Header: make(http.Header), Body: body}, adapter, true, 20*time.Millisecond)
	}()
	select {
	case <-body.entered:
	case <-time.After(time.Second):
		t.Fatal("timeout callback did not start")
	}
	select {
	case err := <-result:
		t.Fatalf("probe returned before Close callback completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	release()
	if err := raceContentWaitResult(t, result); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("late content won against running timeout: %v", err)
	}
}

func TestAwaitRaceContentGzipReplay(t *testing.T) {
	for _, stream := range []bool{true, false} {
		raw := " \n" + raceContentTextJSON + "\r\n"
		if stream {
			raw = ": comment\r\ndata: " + raceContentTextJSON + "\r\n\r\ndata: [DONE]\r\n\r\n"
		}
		var compressed bytes.Buffer
		writer := gzip.NewWriter(&compressed)
		if _, err := writer.Write([]byte(raw)); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		body := &raceContentTrackedBody{ReadCloser: io.NopCloser(bytes.NewReader(compressed.Bytes()))}
		response := &http.Response{Header: make(http.Header), Body: body}
		response.Header.Set("Content-Encoding", "gzip")
		response.Header.Set("Content-Length", "123")
		if err := awaitRaceContent(context.Background(), response, &openaiOutbound.ChatOutbound{}, stream, time.Second); err != nil {
			t.Fatal(err)
		}
		if response.Header.Get("Content-Encoding") != "" || response.Header.Get("Content-Length") != "" {
			t.Fatal("encoding headers were not cleared after decoding")
		}
		raceContentAssertReplay(t, response, []byte(raw))
		_ = response.Body.Close()
		if !body.closed.Load() {
			t.Fatal("decoded replay Close did not close compressed source")
		}
	}
}

func TestAwaitRaceContentGzipInitializationCanceled(t *testing.T) {
	body := newRaceContentBlockingBody()
	defer body.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	response := &http.Response{Header: make(http.Header), Body: body}
	response.Header.Set("Content-Encoding", "gzip")
	result := make(chan error, 1)
	go func() {
		result <- awaitRaceContent(ctx, response, &openaiOutbound.ChatOutbound{}, true, 0)
	}()
	select {
	case <-body.started:
	case <-time.After(time.Second):
		t.Fatal("gzip initialization did not read source")
	}
	cancel()
	if err := raceContentWaitResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("gzip initialization did not honor cancellation: %v", err)
	}
}

func TestAwaitRaceContentStreamCaptureCapDoesNotOverread(t *testing.T) {
	raw := ":" + strings.Repeat("x", raceStreamCaptureLimit) + "\n\ndata: " + raceContentTextJSON + "\n\n"
	response, body := raceContentTestResponse(raw)
	defer response.Body.Close()
	err := awaitRaceContent(context.Background(), response, &openaiOutbound.ChatOutbound{}, true, time.Second)
	if !errors.Is(err, errRaceCaptureTooLarge) {
		t.Fatalf("expected capture cap, got %v", err)
	}
	if body.readBytes != raceStreamCaptureLimit {
		t.Fatalf("read %d bytes, limit %d", body.readBytes, raceStreamCaptureLimit)
	}
}

func TestAwaitRaceContentStreamExactCaptureCapCanWin(t *testing.T) {
	prefix := `data: {"choices":[{"delta":{"content":"`
	suffix := `"}}]}` + "\n\n"
	raw := prefix + strings.Repeat("x", raceStreamCaptureLimit-len(prefix)-len(suffix)) + suffix
	response, body := raceContentTestResponse(raw)
	defer response.Body.Close()
	if err := awaitRaceContent(context.Background(), response, &openaiOutbound.ChatOutbound{}, true, time.Second); err != nil {
		t.Fatalf("complete content at exact cap rejected: %v", err)
	}
	if body.readBytes != raceStreamCaptureLimit {
		t.Fatalf("read %d bytes at exact cap", body.readBytes)
	}
	raceContentAssertReplay(t, response, []byte(raw))
}

type raceContentReadError struct {
	data []byte
	err  error
}

func (r *raceContentReadError) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, r.err
}

func TestAwaitRaceContentReadErrorWithContentFails(t *testing.T) {
	wantErr := errors.New("test read failure")
	response := &http.Response{Header: make(http.Header), Body: io.NopCloser(&raceContentReadError{
		data: []byte("data: " + raceContentTextJSON + "\n\n"), err: wantErr,
	})}
	defer response.Body.Close()
	if err := awaitRaceContent(context.Background(), response, &openaiOutbound.ChatOutbound{}, true, time.Second); !errors.Is(err, wantErr) {
		t.Fatalf("read error was hidden by buffered content: %v", err)
	}
}

func TestAwaitRaceContentJSONReplayAndFreshAdapter(t *testing.T) {
	raw := " \r\n{\n\t\"choices\": [{\"message\": {\"content\": \"hello\"}}]\n}\t\r\n"
	response, body := raceContentTestResponse(raw)
	if err := awaitRaceContent(context.Background(), response, &openaiOutbound.ChatOutbound{}, false, 0); err != nil {
		t.Fatal(err)
	}
	// A second adapter parses the full replay rather than inheriting probe state.
	internal, err := (&openaiOutbound.ChatOutbound{}).TransformResponse(context.Background(), response)
	if err != nil || !raceResponseHasContent(internal) {
		t.Fatalf("fresh adapter could not parse replay: %v", err)
	}
	_ = response.Body.Close()
	if !body.closed.Load() {
		t.Fatal("JSON replay Close did not close original")
	}
	response, _ = raceContentTestResponse(raw)
	defer response.Body.Close()
	if err := awaitRaceContent(context.Background(), response, &openaiOutbound.ChatOutbound{}, false, 0); err != nil {
		t.Fatal(err)
	}
	raceContentAssertReplay(t, response, []byte(raw))
}

func TestAwaitRaceContentJSONProbeUsesResponseCopy(t *testing.T) {
	raw := `{"choices":[{"message":{"content":"hello"}}]}`
	response, original := raceContentTestResponse(raw)
	response.Header.Set("X-Probe", "original")
	adapter := &raceContentTestAdapter{response: func(ctx context.Context, probe *http.Response) (*model.InternalLLMResponse, error) {
		if probe == response || probe.Body == original {
			t.Fatal("probe adapter received live response/body")
		}
		probe.Header.Set("X-Probe", "changed")
		defer probe.Body.Close()
		return (&openaiOutbound.ChatOutbound{}).TransformResponse(ctx, probe)
	}}
	if err := awaitRaceContent(context.Background(), response, adapter, false, time.Second); err != nil {
		t.Fatal(err)
	}
	if response.Header.Get("X-Probe") != "original" || original.closed.Load() {
		t.Fatal("probe mutated response header or closed original body")
	}
	raceContentAssertReplay(t, response, []byte(raw))
	_ = response.Body.Close()
}

func TestAwaitRaceContentJSONRejectsInvalidEmptyAndErrors(t *testing.T) {
	for _, raw := range []string{
		"", "{invalid}", "null", "{}", "[]", "<html>not JSON</html>",
		`{"usage":{"completion_tokens":10}}`,
		`{"choices":[{"message":{"role":"assistant"}}]}`,
		`{"error":{"message":"failure"},"choices":[{"message":{"content":"not a win"}}]}`,
	} {
		response, _ := raceContentTestResponse(raw)
		err := awaitRaceContent(context.Background(), response, &openaiOutbound.ChatOutbound{}, false, 0)
		_ = response.Body.Close()
		if err == nil {
			t.Fatalf("invalid/empty/error JSON won: %q", raw)
		}
	}
}

func TestAwaitRaceContentJSONSizeLimit(t *testing.T) {
	base := `{"choices":[{"message":{"content":"hello"}}]}`
	for _, extra := range []int{0, 1} {
		raw := base + strings.Repeat(" ", raceJSONBodyLimit-len(base)+extra)
		response, body := raceContentTestResponse(raw)
		called := false
		adapter := &raceContentTestAdapter{response: func(ctx context.Context, response *http.Response) (*model.InternalLLMResponse, error) {
			called = true
			return (&openaiOutbound.ChatOutbound{}).TransformResponse(ctx, response)
		}}
		err := awaitRaceContent(context.Background(), response, adapter, false, 0)
		if extra == 0 {
			if err != nil || !called {
				t.Fatalf("valid JSON at 32 MiB rejected: %v", err)
			}
			raceContentAssertReplay(t, response, []byte(raw))
		} else if err == nil || called || body.readBytes != raceJSONBodyLimit+1 {
			t.Fatalf("overflow: err=%v, adapter called=%v, bytes=%d", err, called, body.readBytes)
		}
		_ = response.Body.Close()
	}
}

func TestAwaitRaceContentAdapterErrors(t *testing.T) {
	wantErr := errors.New("test adapter failure")
	for _, stream := range []bool{true, false} {
		adapter := &raceContentTestAdapter{
			stream: func(context.Context, []byte) (*model.InternalLLMResponse, error) { return nil, wantErr },
			response: func(context.Context, *http.Response) (*model.InternalLLMResponse, error) {
				return nil, wantErr
			},
		}
		raw := raceContentTextJSON
		if stream {
			raw = "data: " + raw + "\n\n"
		}
		response, _ := raceContentTestResponse(raw)
		err := awaitRaceContent(context.Background(), response, adapter, stream, 0)
		_ = response.Body.Close()
		if !errors.Is(err, wantErr) {
			t.Fatalf("adapter error lost: %v", err)
		}
	}
}
