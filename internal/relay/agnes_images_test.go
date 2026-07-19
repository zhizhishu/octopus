package relay

import (
	"reflect"
	"testing"
)

// TestIsAgnesImagesModel pins the name gate: only agnes-image-2.* is affected,
// case/whitespace insensitively, and nothing else is.
func TestIsAgnesImagesModel(t *testing.T) {
	cases := map[string]bool{
		"agnes-image-2.0-flash": true,
		"agnes-image-2.1-flash": true,
		"  AGNES-IMAGE-2.0-Flash  ": true,
		"agnes-image-2":         true,
		"agnes-image-1.0-flash": false,
		"agnes-image-3.0-flash": false,
		"agnes-chat":            false,
		"grok-2-image":          false,
		"":                      false,
	}
	for model, want := range cases {
		if got := isAgnesImagesModel(model); got != want {
			t.Errorf("isAgnesImagesModel(%q) = %v, want %v", model, got, want)
		}
	}
}

// TestNormalizeAgnesImagesPayloadNestsFields verifies top-level response_format
// and image are moved into extra_body while every other field is left as-is.
func TestNormalizeAgnesImagesPayloadNestsFields(t *testing.T) {
	payload := map[string]any{
		"model":           "agnes-image-2.1-flash",
		"prompt":          "a cat",
		"size":            "1K",
		"ratio":           "16:9",
		"n":               2,
		"return_base64":   true,
		"response_format": "b64_json",
		"image":           []any{"https://example.test/a.png", "data:image/png;base64,AAAA"},
	}

	normalizeAgnesImagesPayload(payload)

	if _, ok := payload["response_format"]; ok {
		t.Fatalf("expected top-level response_format removed, still present: %#v", payload)
	}
	if _, ok := payload["image"]; ok {
		t.Fatalf("expected top-level image removed, still present: %#v", payload)
	}

	extra, ok := payload["extra_body"].(map[string]any)
	if !ok {
		t.Fatalf("expected extra_body map, got %#v", payload["extra_body"])
	}
	if extra["response_format"] != "b64_json" {
		t.Errorf("expected extra_body.response_format=b64_json, got %#v", extra["response_format"])
	}
	wantImage := []any{"https://example.test/a.png", "data:image/png;base64,AAAA"}
	if !reflect.DeepEqual(extra["image"], wantImage) {
		t.Errorf("expected extra_body.image=%#v, got %#v", wantImage, extra["image"])
	}

	// Untouched fields keep their original values.
	for key, want := range map[string]any{
		"model":         "agnes-image-2.1-flash",
		"prompt":        "a cat",
		"size":          "1K",
		"ratio":         "16:9",
		"n":             2,
		"return_base64": true,
	} {
		if payload[key] != want {
			t.Errorf("field %q changed: got %#v want %#v", key, payload[key], want)
		}
	}
}

// TestNormalizeAgnesImagesPayloadPreservesExistingExtraBody verifies pre-existing
// extra_body keys survive and moved fields are merged alongside them.
func TestNormalizeAgnesImagesPayloadPreservesExistingExtraBody(t *testing.T) {
	payload := map[string]any{
		"prompt": "a dog",
		"extra_body": map[string]any{
			"existing_key": "keep-me",
		},
		"response_format": "url",
	}

	normalizeAgnesImagesPayload(payload)

	extra, ok := payload["extra_body"].(map[string]any)
	if !ok {
		t.Fatalf("expected extra_body map, got %#v", payload["extra_body"])
	}
	if extra["existing_key"] != "keep-me" {
		t.Errorf("expected existing extra_body key preserved, got %#v", extra["existing_key"])
	}
	if extra["response_format"] != "url" {
		t.Errorf("expected response_format merged into extra_body, got %#v", extra["response_format"])
	}
}

// TestNormalizeAgnesImagesPayloadReplacesNonMapExtraBody verifies a non-object
// extra_body is replaced by a fresh map holding the moved fields.
func TestNormalizeAgnesImagesPayloadReplacesNonMapExtraBody(t *testing.T) {
	payload := map[string]any{
		"prompt":          "a bird",
		"extra_body":      "not-a-map",
		"response_format": "url",
	}

	normalizeAgnesImagesPayload(payload)

	extra, ok := payload["extra_body"].(map[string]any)
	if !ok {
		t.Fatalf("expected extra_body replaced with map, got %#v", payload["extra_body"])
	}
	if extra["response_format"] != "url" {
		t.Errorf("expected response_format in extra_body, got %#v", extra["response_format"])
	}
}

// TestNormalizeAgnesImagesPayloadIdempotent verifies a second pass is a no-op:
// once fields are nested (top-level absent) the payload is unchanged.
func TestNormalizeAgnesImagesPayloadIdempotent(t *testing.T) {
	payload := map[string]any{
		"prompt":          "a fish",
		"response_format": "b64_json",
		"image":           []any{"https://example.test/x.png"},
	}

	normalizeAgnesImagesPayload(payload)
	first := deepCopyMap(payload)
	normalizeAgnesImagesPayload(payload)

	if !reflect.DeepEqual(payload, first) {
		t.Fatalf("expected idempotent normalize, first=%#v second=%#v", first, payload)
	}
}

// TestNormalizeAgnesImagesPayloadNoTargetFields verifies a payload without
// response_format/image is left completely untouched (no extra_body created).
func TestNormalizeAgnesImagesPayloadNoTargetFields(t *testing.T) {
	payload := map[string]any{
		"model":  "agnes-image-2.0-flash",
		"prompt": "a tree",
		"size":   "1024x768",
	}
	before := deepCopyMap(payload)

	normalizeAgnesImagesPayload(payload)

	if _, ok := payload["extra_body"]; ok {
		t.Fatalf("expected no extra_body created, got %#v", payload["extra_body"])
	}
	if !reflect.DeepEqual(payload, before) {
		t.Fatalf("expected payload untouched, before=%#v after=%#v", before, payload)
	}
}

func deepCopyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if nested, ok := v.(map[string]any); ok {
			out[k] = deepCopyMap(nested)
			continue
		}
		out[k] = v
	}
	return out
}
