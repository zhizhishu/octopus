package batchimage

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
)

// 这一组测试盯的是**契约**，不是覆盖率：
//  1. JSONL 的行格式（上游逐字认字段，大小写错了不报错、只是静默不生效）；
//  2. 结果解析（乱序、缺 key、坏 base64 都要有确定行为）；
//  3. 两个 provider 的请求形状（用一个假上游接住，看它到底发了什么）；
//  4. 安全闸（下载地址不能乱指、桶不能被越界删）。

func TestBuildJSONLUsesKeyNotLineOrder(t *testing.T) {
	jsonl, err := BuildJSONL(BuildRequest{
		Items: []SubmitItem{
			{CustomID: "a", Prompt: "画一只猫", OutputCount: 2},
			{CustomID: "b", Prompt: "画一条狗"},
		},
		AspectRatio: "1:1",
		ImageSize:   "2K",
	})
	if err != nil {
		t.Fatalf("BuildJSONL: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(jsonl)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 jsonl lines, got %d", len(lines))
	}

	var first jsonlLine
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line 1 is not valid json: %v", err)
	}
	if first.Key != "a" {
		t.Errorf("line 1 key = %q, want %q (results are matched by key, never by order)", first.Key, "a")
	}
	if len(first.Request.Contents) != 1 || len(first.Request.Contents[0].Parts) != 1 {
		t.Fatalf("line 1 must have exactly one content with one part")
	}
	if got := first.Request.Contents[0].Parts[0].Text; got != "画一只猫" {
		t.Errorf("prompt = %q, want the original text", got)
	}
	if first.Request.Contents[0].Role != "user" {
		t.Errorf("role = %q, want user", first.Request.Contents[0].Role)
	}

	config := first.Request.GenerationConfig
	if config == nil {
		t.Fatal("generationConfig must be present: without responseModalities the upstream returns text only")
	}
	if len(config.ResponseModalities) != 2 || config.ResponseModalities[1] != "IMAGE" {
		t.Errorf("responseModalities = %v, want [TEXT IMAGE]", config.ResponseModalities)
	}
	if config.CandidateCount != 2 {
		t.Errorf("candidateCount = %d, want 2", config.CandidateCount)
	}
	if config.ImageConfig == nil || config.ImageConfig.AspectRatio != "1:1" || config.ImageConfig.ImageSize != "2K" {
		t.Errorf("imageConfig = %+v, want aspectRatio 1:1 and imageSize 2K", config.ImageConfig)
	}
}

func TestBuildJSONLRejectsBadInput(t *testing.T) {
	cases := []struct {
		name  string
		req   BuildRequest
		match string
	}{
		{
			name:  "empty items",
			req:   BuildRequest{},
			match: "no items",
		},
		{
			name:  "empty custom id",
			req:   BuildRequest{Items: []SubmitItem{{Prompt: "x"}}},
			match: "empty custom_id",
		},
		{
			name: "duplicate custom id",
			req: BuildRequest{Items: []SubmitItem{
				{CustomID: "dup", Prompt: "a"},
				{CustomID: "dup", Prompt: "b"},
			}},
			match: "duplicate custom_id",
		},
		{
			name:  "empty prompt",
			req:   BuildRequest{Items: []SubmitItem{{CustomID: "a", Prompt: "   "}}},
			match: "empty prompt",
		},
		{
			name: "reference image with both data and uri",
			req: BuildRequest{Items: []SubmitItem{{
				CustomID:        "a",
				Prompt:          "x",
				ReferenceImages: []ReferenceImage{{MimeType: "image/png", Data: []byte{1}, FileURI: "gs://b/o"}},
			}}},
			match: "both data and file_uri",
		},
		{
			name: "reference image with neither",
			req: BuildRequest{Items: []SubmitItem{{
				CustomID:        "a",
				Prompt:          "x",
				ReferenceImages: []ReferenceImage{{MimeType: "image/png"}},
			}}},
			match: "neither data nor file_uri",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BuildJSONL(tc.req)
			if err == nil {
				t.Fatalf("want an error mentioning %q, got nil", tc.match)
			}
			if !strings.Contains(err.Error(), tc.match) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), tc.match)
			}
		})
	}
}

func TestBuildJSONLReferenceImageShapes(t *testing.T) {
	jsonl, err := BuildJSONL(BuildRequest{Items: []SubmitItem{{
		CustomID: "a",
		Prompt:   "x",
		ReferenceImages: []ReferenceImage{
			{MimeType: "image/png", Data: []byte("raw-bytes")},
			{MimeType: "image/jpeg", FileURI: "gs://bucket/ref.jpg"},
		},
	}}})
	if err != nil {
		t.Fatalf("BuildJSONL: %v", err)
	}

	var line jsonlLine
	if err := json.Unmarshal(bytes.TrimSpace(jsonl), &line); err != nil {
		t.Fatalf("decode: %v", err)
	}
	parts := line.Request.Contents[0].Parts
	if len(parts) != 3 {
		t.Fatalf("want 3 parts (prompt + 2 refs), got %d", len(parts))
	}
	if parts[1].InlineData == nil {
		t.Fatal("first reference must be inlineData")
	}
	if got, _ := base64.StdEncoding.DecodeString(parts[1].InlineData.Data); string(got) != "raw-bytes" {
		t.Errorf("inline data round-trip = %q", got)
	}
	if parts[2].FileData == nil || parts[2].FileData.FileURI != "gs://bucket/ref.jpg" {
		t.Errorf("second reference must be fileData with the uri, got %+v", parts[2])
	}
}

func TestParseResultsHandlesShapes(t *testing.T) {
	const pngB64 = "iVBORw0KGgo=" // 只是几个字节，够验证 base64 解出来
	body := strings.Join([]string{
		// 1: Gemini 形状，带 key
		`{"key":"a","response":{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"` + pngB64 + `"}}]}}]}}`,
		// 2: 空行（跳过多余空行）
		``,
		// 3: Vertex 形状，key 在 instance 里
		`{"instance":{"key":"b"},"prediction":{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/jpeg","data":"` + pngB64 + `"}}]}}]}}`,
		// 4: 上游对单条报错
		`{"key":"c","error":{"code":400,"message":"bad prompt","status":"INVALID_ARGUMENT"}}`,
		// 5: 有响应但没有图
		`{"key":"d","response":{"candidates":[{"content":{"parts":[{"text":"I cannot do that"}]}}]}}`,
		// 6: 完全不是 JSON
		`not json at all`,
	}, "\n")

	type heard struct {
		line   int
		result ParsedResult
	}
	var heardLines []heard
	err := ParseResults(strings.NewReader(body), func(lineNumber int, result ParsedResult) {
		heardLines = append(heardLines, heard{line: lineNumber, result: result})
	})
	if err != nil {
		t.Fatalf("ParseResults: %v", err)
	}

	if len(heardLines) != 5 {
		t.Fatalf("want 5 non-empty lines, got %d", len(heardLines))
	}

	// 行号必须反映**结果对象里的真实行号**（中间那个空行要算进去）。
	if heardLines[1].line != 3 {
		t.Errorf("third physical line reported as line %d, want 3", heardLines[1].line)
	}

	if heardLines[0].result.Key != "a" || len(heardLines[0].result.Images) != 1 {
		t.Errorf("line 1 = %+v, want key a with 1 image", heardLines[0].result)
	}
	if got := heardLines[0].result.Images[0].MimeType; got != "image/png" {
		t.Errorf("line 1 mime = %q", got)
	}

	if heardLines[1].result.Key != "b" {
		t.Errorf("vertex line key = %q, want b extracted from instance", heardLines[1].result.Key)
	}
	if len(heardLines[1].result.Images) != 1 {
		t.Errorf("vertex line must yield an image")
	}

	if heardLines[2].result.Error == nil || heardLines[2].result.Error.Code != "INVALID_ARGUMENT" {
		t.Errorf("line 4 = %+v, want an INVALID_ARGUMENT error", heardLines[2].result)
	}

	if heardLines[3].result.Error == nil {
		t.Fatal("a response with no image must become an error, otherwise the client sees success with zero images")
	}
	if !strings.Contains(heardLines[3].result.Error.Message, "I cannot do that") {
		t.Errorf("no-image error should carry the model text, got %q", heardLines[3].result.Error.Message)
	}

	if heardLines[4].result.Error == nil || heardLines[4].result.Error.Code != "invalid_result_line" {
		t.Errorf("a non-json line must become an error, got %+v", heardLines[4].result)
	}
}

func TestParseResultsRejectsBadBase64(t *testing.T) {
	body := `{"key":"a","response":{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"!!!not base64!!!"}}]}}]}}`
	var got ParsedResult
	err := ParseResults(strings.NewReader(body), func(_ int, result ParsedResult) { got = result })
	if err != nil {
		t.Fatalf("ParseResults: %v", err)
	}
	if got.Error == nil || got.Error.Code != "invalid_image_data" {
		t.Errorf("want an invalid_image_data error, got %+v", got)
	}
	if len(got.Images) != 0 {
		t.Error("a corrupt image must not be handed to the caller")
	}
}

// TestGeminiSubmitShape 用一个假上游接住请求，检查我们到底发了什么。
func TestGeminiSubmitShape(t *testing.T) {
	var uploadPath, batchPath string
	var batchBody map[string]any
	var uploadContentType string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/upload/v1beta/files"):
			uploadPath = r.URL.Path
			uploadContentType = r.Header.Get("Content-Type")
			if got := r.Header.Get("x-goog-api-key"); got != "test-key" {
				t.Errorf("upload must carry the api key header, got %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"file":{"name":"files/abc123"}}`))
		case strings.Contains(r.URL.Path, ":batchGenerateContent"):
			batchPath = r.URL.Path
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &batchBody)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"batches/xyz","state":"JOB_STATE_PENDING"}`))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	provider := &geminiProvider{client: server.Client()}
	result, err := provider.Submit(t.Context(), SubmitRequest{
		Channel: ChannelContext{APIKey: "test-key", BaseURL: server.URL},
		BatchID: "imgbatch_test",
		Model:   "gemini-2.5-flash-image",
		JSONL:   []byte(`{"key":"a","request":{}}` + "\n"),
		Items:   []SubmitItem{{CustomID: "a", Prompt: "x"}},
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if !strings.Contains(uploadContentType, "multipart/form-data") {
		t.Errorf("upload content type = %q, want multipart/form-data", uploadContentType)
	}
	if uploadPath == "" {
		t.Error("no upload happened")
	}
	// 模型名必须被转义进路径里，且方法后缀要在。
	if !strings.HasSuffix(batchPath, ":batchGenerateContent") {
		t.Errorf("batch path = %q, want a :batchGenerateContent suffix", batchPath)
	}
	if strings.Contains(batchPath, "models/models/") {
		t.Errorf("batch path = %q, the model prefix was joined twice", batchPath)
	}
	inputConfig, _ := batchBody["batch"].(map[string]any)
	if inputConfig == nil {
		t.Fatalf("body must be wrapped in a batch object, got %+v", batchBody)
	}
	inner, _ := inputConfig["inputConfig"].(map[string]any)
	if inner == nil || inner["fileName"] != "files/abc123" {
		t.Errorf("inputConfig.fileName = %+v, want the uploaded file ref", inner)
	}

	if result.ProviderJobName != "batches/xyz" {
		t.Errorf("job name = %q", result.ProviderJobName)
	}
	if result.ProviderInputRef != "files/abc123" {
		t.Errorf("input ref = %q", result.ProviderInputRef)
	}
}

func TestGeminiAuthErrorIsNotTemporary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":401,"message":"API key not valid","status":"UNAUTHENTICATED"}}`))
	}))
	defer server.Close()

	provider := &geminiProvider{client: server.Client()}
	_, err := provider.Submit(t.Context(), SubmitRequest{
		Channel: ChannelContext{APIKey: "bad", BaseURL: server.URL},
		BatchID: "imgbatch_test",
		Model:   "m",
		JSONL:   []byte("{}\n"),
	})
	if err == nil {
		t.Fatal("want an error for a 401")
	}
	var upstream *upstreamError
	if !errors.As(err, &upstream) {
		t.Fatalf("want an *upstreamError, got %T", err)
	}
	if upstream.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", upstream.StatusCode)
	}
	// 4xx 不该被当成可重试：重试只会把上游打得更狠。
	if upstream.Temporary() {
		t.Error("401 must not be treated as temporary")
	}
	if strings.Contains(err.Error(), "bad") && !strings.Contains(err.Error(), "not valid") {
		t.Errorf("error should carry the upstream message, got %q", err.Error())
	}
}

func TestGeminiMissingKeyFailsFast(t *testing.T) {
	provider := &geminiProvider{}
	_, err := provider.Submit(t.Context(), SubmitRequest{Channel: ChannelContext{}})
	if err == nil {
		t.Fatal("want an error when the api key is missing")
	}
	if !errors.Is(err, ErrProviderNotConfigured) {
		t.Errorf("error = %v, want ErrProviderNotConfigured so operators can tell config from outage", err)
	}
}

// TestValidateDownloadHost 是安全边界：下载会带着密钥，不能指到任意主机。
func TestValidateDownloadHost(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		baseURL string
		wantErr bool
	}{
		{"googleapis content host", "https://storage.googleapis.com/x", "", false},
		{"same host as channel", "https://my-relay.example/file", "https://my-relay.example", false},
		{"foreign host", "https://evil.example/file", "https://my-relay.example", true},
		{"not a url", "::::", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDownloadHost(tc.url, tc.baseURL)
			if tc.wantErr && err == nil {
				t.Fatalf("want an error for %q", tc.url)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestGCSURIAndPrefixRoundTrip(t *testing.T) {
	settings := vertexSettings{bucket: "my-bucket", prefix: "octopus-batch-image"}
	object := settings.objectName("imgbatch_1", "input/batch.jsonl")
	if object != "octopus-batch-image/imgbatch_1/input/batch.jsonl" {
		t.Fatalf("object name = %q", object)
	}
	uri := settings.gcsURI(object)
	if uri != "gs://my-bucket/octopus-batch-image/imgbatch_1/input/batch.jsonl" {
		t.Fatalf("uri = %q", uri)
	}

	prefix, err := gcsPrefixFromURI("gs://my-bucket/octopus-batch-image/imgbatch_1/output/", "my-bucket")
	if err != nil {
		t.Fatalf("gcsPrefixFromURI: %v", err)
	}
	if prefix != "octopus-batch-image/imgbatch_1/output/" {
		t.Errorf("prefix = %q", prefix)
	}

	// 上游给了别的桶时必须拒绝：那等于带着 token 去读用户没授权的数据。
	if _, err := gcsPrefixFromURI("gs://someone-else/prefix/", "my-bucket"); err == nil {
		t.Error("reading a foreign bucket must be refused")
	}
}

func TestVertexSettingsRequireBucketAndServiceAccount(t *testing.T) {
	provider := &vertexProvider{tokenCache: map[string]cachedVertexToken{}}

	cases := []struct {
		name    string
		channel ChannelContext
		match   string
	}{
		{"no key", ChannelContext{}, "service account json"},
		{"not json", ChannelContext{APIKey: "not-json"}, "not valid json"},
		{
			name:    "missing private key",
			channel: ChannelContext{APIKey: `{"client_email":"a@b","project_id":"p"}`},
			match:   "client_email or private_key",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := provider.resolveSettings(tc.channel)
			if err == nil {
				t.Fatalf("want an error mentioning %q", tc.match)
			}
			if !errors.Is(err, ErrProviderNotConfigured) {
				t.Errorf("error must wrap ErrProviderNotConfigured, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.match) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), tc.match)
			}
		})
	}

	// 有服务账号但没配桶：必须明确报"缺桶"，因为 Vertex 只认 gs://。
	bucketKeyPEM := testPrivateKeyPEM(t)
	accountJSON := `{"client_email":"a@b.iam.gserviceaccount.com","project_id":"p","private_key":"` +
		strings.ReplaceAll(bucketKeyPEM, "\n", `\n`) + `"}`
	_, err := provider.resolveSettings(ChannelContext{APIKey: accountJSON})
	if err == nil || !strings.Contains(err.Error(), "gcs bucket") {
		t.Fatalf("want a missing-bucket error, got %v", err)
	}
}

func TestSignRS256JWTIsVerifiable(t *testing.T) {
	keyPEM := testPrivateKeyPEM(t)
	key, err := parseRSAPrivateKey(keyPEM)
	if err != nil {
		t.Fatalf("parse test key: %v", err)
	}
	assertion, err := signRS256JWT(map[string]any{"iss": "a@b"}, keyPEM)
	if err != nil {
		t.Fatalf("signRS256JWT: %v", err)
	}
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt must have 3 segments, got %d", len(parts))
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("signature is not base64url: %v", err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], signature); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
}

func TestVertexSubmitShape(t *testing.T) {
	var uploaded bool
	var tokenRequested bool
	var jobBody map[string]any
	var jobPath string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/token"):
			tokenRequested = true
			if err := r.ParseForm(); err != nil {
				t.Errorf("token request form: %v", err)
			}
			if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
				t.Errorf("grant_type = %q", r.Form.Get("grant_type"))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":3600}`))
		case strings.HasPrefix(r.URL.Path, "/upload/storage/v1/b/"):
			uploaded = true
			if got := r.Header.Get("Authorization"); got != "Bearer tok" {
				t.Errorf("upload auth = %q, want Bearer tok", got)
			}
			if got := r.Header.Get("Content-Type"); got != "application/jsonl" {
				t.Errorf("upload content type = %q, want application/jsonl", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.Path, "/batchPredictionJobs"):
			jobPath = r.URL.Path
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &jobBody)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"projects/p/locations/l/batchPredictionJobs/1","state":"JOB_STATE_PENDING"}`))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	account, _ := json.Marshal(map[string]string{
		"type":         "service_account",
		"project_id":   "proj",
		"client_email": "svc@proj.iam.gserviceaccount.com",
		"private_key":  testPrivateKeyPEM(t),
		"token_uri":    server.URL + "/token",
	})

	provider := &vertexProvider{client: server.Client(), tokenCache: map[string]cachedVertexToken{}}
	result, err := provider.Submit(t.Context(), SubmitRequest{
		Channel: ChannelContext{
			APIKey:    string(account),
			BaseURL:   server.URL,
			GCSBucket: "my-bucket",
			Location:  "global",
		},
		BatchID: "imgbatch_1",
		Model:   "gemini-2.5-flash-image",
		JSONL:   []byte("{}\n"),
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !tokenRequested {
		t.Error("no token was requested; the service account jwt path did not run")
	}
	if !uploaded {
		t.Error("the jsonl was never uploaded to gcs")
	}
	if jobPath == "" {
		t.Fatal("no batchPredictionJob was created")
	}
	if !strings.Contains(jobPath, "/projects/proj/locations/global/") {
		t.Errorf("job path = %q, want the project/location in the path", jobPath)
	}

	inputConfig, _ := jobBody["inputConfig"].(map[string]any)
	if inputConfig == nil {
		t.Fatalf("job body must have inputConfig, got %+v", jobBody)
	}
	if inputConfig["instancesFormat"] != "jsonl" {
		t.Errorf("instancesFormat = %v, want jsonl", inputConfig["instancesFormat"])
	}
	instanceConfig, _ := jobBody["instanceConfig"].(map[string]any)
	if instanceConfig == nil || instanceConfig["keyField"] != "key" {
		t.Errorf("instanceConfig = %+v, want keyField=key (otherwise results cannot be matched)", instanceConfig)
	}
	if got, _ := jobBody["model"].(string); !strings.Contains(got, "publishers/google/models/gemini-2.5-flash-image") {
		t.Errorf("model = %q, want a full publishers/google/models path", got)
	}

	if result.ProviderJobName != "projects/p/locations/l/batchPredictionJobs/1" {
		t.Errorf("job name = %q", result.ProviderJobName)
	}
	if !strings.HasPrefix(result.ProviderInputRef, "gs://my-bucket/") {
		t.Errorf("input ref = %q, want a gs:// uri", result.ProviderInputRef)
	}
}

func TestVertexModelResourceVariants(t *testing.T) {
	cases := map[string]string{
		"gemini-2.5-flash-image":                                "projects/p/locations/l/publishers/google/models/gemini-2.5-flash-image",
		"publishers/google/models/gemini-x":                     "projects/p/locations/l/publishers/google/models/gemini-x",
		"projects/other/locations/x/publishers/google/models/m": "projects/other/locations/x/publishers/google/models/m",
	}
	for input, want := range cases {
		if got := vertexModelResource("p", "l", input); got != want {
			t.Errorf("vertexModelResource(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestVertexAiplatformHost(t *testing.T) {
	if got := vertexAiplatformHost("", "global"); got != "https://aiplatform.googleapis.com" {
		t.Errorf("global host = %q", got)
	}
	if got := vertexAiplatformHost("", "us-central1"); got != "https://us-central1-aiplatform.googleapis.com" {
		t.Errorf("regional host = %q", got)
	}
	if got := vertexAiplatformHost("https://proxy.example/", "global"); got != "https://proxy.example" {
		t.Errorf("override host = %q", got)
	}
}

func TestProviderForUsesChannelConfig(t *testing.T) {
	if _, err := ProviderFor(nil); err == nil {
		t.Error("a nil channel must not produce a provider")
	}
	if _, err := ProviderFor(&model.Channel{}); !errors.Is(err, ErrProviderNotConfigured) {
		t.Errorf("a channel without batch config must report ErrProviderNotConfigured, got %v", err)
	}
	channel := &model.Channel{BatchImage: &model.ChannelBatchImage{Provider: model.BatchImageProviderGeminiAPI}}
	provider, err := ProviderFor(channel)
	if err != nil {
		t.Fatalf("ProviderFor: %v", err)
	}
	if provider.Name() != model.BatchImageProviderGeminiAPI {
		t.Errorf("provider name = %q", provider.Name())
	}

	vertexChannel := &model.Channel{BatchImage: &model.ChannelBatchImage{Provider: model.BatchImageProviderVertex}}
	provider, err = ProviderFor(vertexChannel)
	if err != nil {
		t.Fatalf("ProviderFor vertex: %v", err)
	}
	if provider.Name() != model.BatchImageProviderVertex {
		t.Errorf("provider name = %q", provider.Name())
	}
}

func TestImageExtension(t *testing.T) {
	cases := map[string]string{
		"image/png":  "png",
		"image/jpeg": "jpg",
		"image/webp": "webp",
		"":           "png",
		"weird":      "png",
	}
	for mime, want := range cases {
		if got := ImageExtension(mime); got != want {
			t.Errorf("ImageExtension(%q) = %q, want %q", mime, got, want)
		}
	}
}

// testPrivateKeyPEM 每次调用生成一把一次性 2048 位 RSA 私钥（PEM/PKCS8）。
//
// 刻意在测试里现生成而不是写死一段：写死的 PEM 容易被人当成"某个真实服务账号的
// 钥匙"而不敢动，生成一次只花几十毫秒，语义也更诚实——它不指向任何真实账号。
func testPrivateKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate test rsa key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal test rsa key: %v", err)
	}
	block := &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	return string(pem.EncodeToMemory(block))
}
