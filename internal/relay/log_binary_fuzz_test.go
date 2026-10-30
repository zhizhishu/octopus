package relay

import (
	"encoding/json"
	"strings"
	"testing"
)

// FuzzStripInlineBinaryForLog walks the hand-written JSON rewriter.
// Seed inputs come from the shape tests. go test runs those seeds;
// -fuzz mutates them.
//
// Invariants, for every input:
//   - no panic (a panic fails the test)
//   - strip returns (the walker is depth-capped and every scan advances)
//   - len(output) <= len(input)
//   - strip(strip(x)) == strip(x)
//   - valid JSON stays valid JSON
func FuzzStripInlineBinaryForLog(f *testing.F) {
	payload := strings.Repeat("A", 64)
	png := "data:image/png;base64," + payload
	httpsURL := "https://cdn.example.com/a.png?x=1&y=2"
	seeds := []string{
		"",
		" ",
		"{",
		"}",
		"[]",
		"{}",
		"null",
		"0",
		`"data:"`,
		strings.Repeat("x", 300),
		`{"image_url":"auto"}`,
		`{"image_url":"` + httpsURL + `"}`,
		`{"image_url":"` + png + `"}`,
		`{"type":"image_url","image_url":{"url":"` + png + `","detail":"high"}}`,
		`{"type":"input_image","image_url":"` + png + `"}`,
		`{"type":"input_audio","input_audio":{"format":"wav","data":"` + payload + `"}}`,
		`{"audio":{"data":"` + payload + `","id":"aud_1","transcript":"hi"}}`,
		`{"audio":{"format":"wav","voice":"alloy"}}`,
		`{"type":"file","file":{"file_data":"data:application/pdf;base64,` + payload + `","file_url":"` + httpsURL + `","file_id":"file_123"}}`,
		`{"type":"input_file","file_data":"` + payload + `"}`,
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + payload + `"}}`,
		`{"type":"image","source":{"type":"url","url":"` + httpsURL + `"}}`,
		`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"` + payload + `"}}`,
		`{"type":"document","source":{"type":"file","file_id":"file_abc"}}`,
		`{"inlineData":{"mimeType":"image/jpeg","data":"` + payload + `"}}`,
		`{"inline_data":{"mime_type":"audio/wav","data":"` + payload + `"}}`,
		`{"inlineData":{"mimeType":"video/mp4","data":"` + payload + `"}}`,
		`{"fileData":{"mimeType":"image/png","fileUri":"gs://bucket/obj.png"}}`,
		`{"bytesBase64Encoded":"` + payload + `","mimeType":"image/png"}`,
		`{"b64_json":"` + payload + `"}`,
		`{"response_format":"b64_json"}`,
		`{"type":"image_generation","result":"` + payload + `"}`,
		`{"type":"function_call_output","result":"` + payload + `"}`,
		`{"messages":[{"content":"see data:image/png;base64,QUJD please"}]}`,
		`{"tool_calls":[{"function":{"arguments":"{\"url\":\"data:image/png;base64,QUJD\"}"}}]}`,
		`{"image_url":"data:` + strings.Repeat("M", 4000) + `;base64,` + payload + `"}`,
		`{"image_url":"data:image/png;base64,QUJD"}`,
		"{\n  \"image_url\": \"" + png + "\"\n}",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	// Deep enough to pass the recursion cap.
	var deep strings.Builder
	deep.WriteString(`{"image_url":"` + png + `","nest":`)
	for i := 0; i < maxJSONWalkDepth+32; i++ {
		deep.WriteString(`{"a":`)
	}
	deep.WriteString(`1`)
	for i := 0; i < maxJSONWalkDepth+32; i++ {
		deep.WriteByte('}')
	}
	deep.WriteByte('}')
	f.Add(deep.String())

	f.Fuzz(func(t *testing.T, s string) {
		out := stripInlineBinaryForLog(s)
		if len(out) > len(s) {
			t.Fatalf("output longer than input: %d > %d", len(out), len(s))
		}
		again := stripInlineBinaryForLog(out)
		if again != out {
			t.Fatalf("not idempotent\n once %q\n twice %q", out, again)
		}
		if json.Valid([]byte(s)) && !json.Valid([]byte(out)) {
			t.Fatalf("valid JSON became invalid: %s", out)
		}
	})
}
