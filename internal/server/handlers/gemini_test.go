package handlers

import "testing"

func TestParseGeminiModelAction(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantModel  string
		wantStream bool
		wantOK     bool
	}{
		{name: "generate", raw: "/gemini-2.5-flash:generateContent", wantModel: "gemini-2.5-flash", wantOK: true},
		{name: "stream", raw: "/gemini-2.5-flash:streamGenerateContent", wantModel: "gemini-2.5-flash", wantStream: true, wantOK: true},
		{name: "publisher model", raw: "/publishers/google/models/gemini-pro:generateContent", wantModel: "publishers/google/models/gemini-pro", wantOK: true},
		// A client that echoes the Google-style "models/<id>" name from GET
		// /v1beta/models doubles the prefix here; the redundant leading "models/" must
		// be stripped so routing matches the bare model name (mid-path "models/" as in
		// the publisher case above stays intact).
		{name: "redundant models prefix stream", raw: "/models/gemini-3.1-pro:streamGenerateContent", wantModel: "gemini-3.1-pro", wantStream: true, wantOK: true},
		{name: "redundant models prefix generate", raw: "/models/gemini-3.1-pro:generateContent", wantModel: "gemini-3.1-pro", wantOK: true},
		{name: "missing action", raw: "/gemini-2.5-flash", wantOK: false},
		{name: "unknown action", raw: "/gemini-2.5-flash:countTokens", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotModel, gotStream, gotOK := parseGeminiModelAction(tt.raw)
			if gotOK != tt.wantOK {
				t.Fatalf("ok: got %t want %t", gotOK, tt.wantOK)
			}
			if gotModel != tt.wantModel {
				t.Fatalf("model: got %q want %q", gotModel, tt.wantModel)
			}
			if gotStream != tt.wantStream {
				t.Fatalf("stream: got %t want %t", gotStream, tt.wantStream)
			}
		})
	}
}
