package relay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/tmaxmax/go-sse"
)

const (
	raceStreamCaptureLimit = 1 << 20
	raceJSONBodyLimit      = 32 << 20
)

var (
	errRaceContentMissing  = errors.New("upstream response ended without race content")
	errRaceCaptureTooLarge = errors.New("upstream race content probe exceeded 1 MiB")
)

// awaitRaceContent consumes only a probe adapter. The caller must use another
// adapter for the real parse: every byte read here, including SSE read-ahead, is
// replayed without rewriting the upstream framing or JSON.
func awaitRaceContent(ctx context.Context, response *http.Response, adapter model.Outbound, stream bool, budget time.Duration) (err error) {
	if response == nil || response.Body == nil {
		return errors.New("upstream race response has no body")
	}
	adapterValue := reflect.ValueOf(adapter)
	if !adapterValue.IsValid() || (adapterValue.Kind() == reflect.Pointer && adapterValue.IsNil()) {
		return errors.New("upstream race probe has no adapter")
	}

	original := response.Body
	if err := ctx.Err(); err != nil {
		_ = original.Close()
		return err
	}

	// Close the original transport body, not a decoder that may be in Read or
	// even still being initialized by unwrapResponseEncoding. No background
	// reader is needed; closing the transport interrupts the synchronous read.
	var abortOnce sync.Once
	var abortErr error
	abort := func(reason error) {
		abortOnce.Do(func() {
			abortErr = reason
			_ = original.Close()
		})
	}
	contextDone := make(chan struct{})
	stopContext := context.AfterFunc(ctx, func() {
		defer close(contextDone)
		abort(ctx.Err())
	})
	var timer *time.Timer
	var timerDone chan struct{}
	if budget > 0 {
		timerDone = make(chan struct{})
		timer = time.AfterFunc(budget, func() {
			defer close(timerDone)
			abort(fmt.Errorf("upstream race content timeout after %s: %w", budget, context.DeadlineExceeded))
		})
	}

	var captured []byte
	var decoded io.ReadCloser
	defer func() {
		// Stop alone is insufficient: an already-running callback could close a
		// successful winner after handoff. Join every callback that won its race.
		if !stopContext() {
			<-contextDone
		}
		if timer != nil && !timer.Stop() {
			<-timerDone
		}
		if ctx.Err() != nil {
			err = ctx.Err()
		} else if abortErr != nil {
			err = abortErr
		}
		if err == nil {
			response.Body = &raceReplayBody{
				Reader: io.MultiReader(bytes.NewReader(captured), decoded),
				Closer: decoded,
			}
		}
	}()

	if err := unwrapResponseEncoding(response); err != nil {
		return fmt.Errorf("failed to unwrap race response encoding: %w", err)
	}
	decoded = response.Body

	if stream {
		recorder := &recordingReader{reader: decoded}
		for event, readErr := range sse.Read(recorder, &sse.ReadConfig{MaxEventSize: maxSSEEventSize}) {
			if readErr != nil {
				// The parser can surface its own framing error (for example an unexpected EOF) for
				// the very read that hit the recorder's limit. The recorder's own error is the real
				// cause, so it must win instead of being masked by the parser's EOF.
				if recorder.err != nil {
					return fmt.Errorf("failed to read race stream: %w", recorder.err)
				}
				return fmt.Errorf("failed to read race stream: %w", readErr)
			}
			// Buffered SSE parsing must not hide a non-EOF error delivered with
			// the same Read as an otherwise meaningful event.
			if recorder.err != nil {
				return fmt.Errorf("failed to read race stream: %w", recorder.err)
			}
			class := classifyStreamEvent(event.Data)
			for _, eventType := range []string{event.Type, class.eventType} {
				switch strings.TrimSpace(eventType) {
				case "error", "response.failed", "response.incomplete":
					return errors.New("upstream race stream returned an error event")
				}
			}
			internal, transformErr := adapter.TransformStream(ctx, []byte(event.Data))
			if transformErr != nil {
				return fmt.Errorf("failed to transform race stream: %w", transformErr)
			}
			if internal != nil && internal.Error != nil {
				return errors.New("upstream race stream returned an error response")
			}
			if raceResponseHasContent(internal) {
				captured = recorder.captured
				return nil
			}
			// A terminal event can itself carry the first real content, so it
			// is transformed above before rejecting an empty terminal.
			if class.isTerminal(event.Type) || internalStreamHasFinish(internal) {
				return errRaceContentMissing
			}
		}
		if recorder.err != nil {
			return fmt.Errorf("failed to read race stream: %w", recorder.err)
		}
		return errRaceContentMissing
	}

	captured, err = io.ReadAll(io.LimitReader(decoded, raceJSONBodyLimit+1))
	if err != nil {
		return fmt.Errorf("failed to read race response: %w", err)
	}
	if len(captured) > raceJSONBodyLimit {
		return errors.New("upstream race response exceeded 32 MiB")
	}
	probe := *response
	probe.Header = response.Header.Clone()
	probe.Body = io.NopCloser(bytes.NewReader(captured))
	probe.ContentLength = int64(len(captured))
	internal, err := adapter.TransformResponse(ctx, &probe)
	if err != nil {
		return fmt.Errorf("failed to transform race response: %w", err)
	}
	if internal != nil && internal.Error != nil {
		return errors.New("upstream race response returned an error")
	}
	if !raceResponseHasContent(internal) {
		return errRaceContentMissing
	}
	return nil
}

// raceResponseHasContent deliberately excludes usage, roles and envelopes.
// Unlike internalStreamHasMeaningfulResponse, only generated message content
// can win a race; the shared message helper also recognizes thinking and tools.
func raceResponseHasContent(response *model.InternalLLMResponse) bool {
	if response == nil || response.Object == "[DONE]" {
		return false
	}
	for _, choice := range response.Choices {
		if messageHasMeaningfulResponse(choice.Message) || messageHasMeaningfulResponse(choice.Delta) {
			return true
		}
	}
	return false
}

// recordingReader bounds all actual reads, including the SSE parser's read-ahead.
// At the ceiling it returns an error without consuming an unrecorded probe byte.
// A complete meaningful event ending exactly at the ceiling may still succeed.
type recordingReader struct {
	reader   io.Reader
	captured []byte
	err      error
}

func (r *recordingReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	remaining := raceStreamCaptureLimit - len(r.captured)
	if remaining <= 0 {
		r.err = errRaceCaptureTooLarge
		return 0, r.err
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	n, err := r.reader.Read(p)
	r.captured = append(r.captured, p[:n]...)
	if err != nil && !errors.Is(err, io.EOF) {
		r.err = err
	}
	return n, err
}

type raceReplayBody struct {
	io.Reader
	io.Closer
}
