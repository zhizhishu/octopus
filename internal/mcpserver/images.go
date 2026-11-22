package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/requestsource"
)

type imageInput struct {
	Prompt       string   `json:"prompt"`
	Model        string   `json:"model,omitempty"`
	Size         string   `json:"size,omitempty"`
	Quality      string   `json:"quality,omitempty"`
	N            *int     `json:"n,omitempty"`
	Background   string   `json:"background,omitempty"`
	OutputFormat string   `json:"output_format,omitempty"`
	Images       []string `json:"images,omitempty"`
	Mask         string   `json:"mask,omitempty"`
}

func imageTool(edit bool) *mcp.Tool {
	properties := map[string]any{}
	for _, name := range []string{"prompt", "model", "size", "quality"} {
		properties[name] = map[string]any{"type": "string"}
	}
	properties["n"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 4}
	required := []string{"prompt"}
	name, description := "generate_image", "Generate images from a prompt. Billed to your Octopus API key."
	if edit {
		name, description = "edit_image", "Edit 1–16 images supplied as base64 or data URLs. Billed to your Octopus API key."
		properties["images"] = map[string]any{"type": "array", "minItems": 1, "maxItems": 16, "items": map[string]any{"type": "string"}}
		properties["mask"] = map[string]any{"type": "string"}
		required = append(required, "images")
	} else {
		properties["background"] = map[string]any{"type": "string"}
		properties["output_format"] = map[string]any{"type": "string"}
	}
	return &mcp.Tool{Name: name, Description: description, InputSchema: map[string]any{
		"type": "object", "properties": properties, "required": required, "additionalProperties": false,
	}}
}

func toolError(status int, message string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("%d: %s", status, message)}}}
}

func decodeImage(value string) ([]byte, string, error) {
	encoded := value
	if strings.HasPrefix(value, "data:") {
		var ok bool
		_, encoded, ok = strings.Cut(value, ";base64,")
		if !ok {
			return nil, "", fmt.Errorf("image must be a base64 data URL")
		}
	}
	if len(encoded) > 28<<20 {
		return nil, "", fmt.Errorf("image exceeds 20 MiB")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) == 0 || len(data) > 20<<20 {
		return nil, "", fmt.Errorf("image must contain valid base64 (max 20 MiB); URLs and local paths are not supported")
	}
	typ := http.DetectContentType(data)
	if typ != "image/png" && typ != "image/jpeg" && typ != "image/webp" && typ != "image/gif" {
		return nil, "", fmt.Errorf("unsupported image type")
	}
	return data, typ, nil
}

func callImage(ctx context.Context, loopback http.Handler, caller *http.Request, raw json.RawMessage, edit bool) *mcp.CallToolResult {
	var input imageInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return toolError(400, "invalid tool arguments")
	}
	if strings.TrimSpace(input.Prompt) == "" || input.N != nil && (*input.N < 1 || *input.N > 4) {
		return toolError(400, "prompt is required; n must be between 1 and 4")
	}
	if edit && (len(input.Images) < 1 || len(input.Images) > 16 || input.Background != "" || input.OutputFormat != "") || !edit && (len(input.Images) > 0 || input.Mask != "") {
		return toolError(400, "edit_image requires 1–16 images; invalid fields for this tool")
	}
	if input.Model == "" {
		var err error
		input.Model, err = op.SettingGetString(model.SettingKeyMCPDefaultModel)
		if err != nil || input.Model == "" {
			return toolError(503, "MCP default model is unavailable")
		}
	}
	var body bytes.Buffer
	path, contentType := "/v1/images/generations", "application/json"
	if edit {
		path = "/v1/images/edits"
		writer := multipart.NewWriter(&body)
		fields := map[string]string{"model": input.Model, "prompt": input.Prompt, "size": input.Size, "quality": input.Quality}
		if input.N != nil {
			fields["n"] = strconv.Itoa(*input.N)
		}
		for name, value := range fields {
			if value != "" {
				if err := writer.WriteField(name, value); err != nil {
					return toolError(500, "cannot encode image request")
				}
			}
		}
		images := append([]string(nil), input.Images...)
		if input.Mask != "" {
			images = append(images, input.Mask)
		}
		for i, image := range images {
			data, typ, err := decodeImage(image)
			if err != nil {
				return toolError(400, err.Error())
			}
			name := "image"
			if i == len(input.Images) {
				name = "mask"
			}
			header := make(textproto.MIMEHeader)
			header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s.%s"`, name, name, strings.TrimPrefix(typ, "image/")))
			header.Set("Content-Type", typ)
			part, err := writer.CreatePart(header)
			if err != nil {
				return toolError(500, "cannot encode image")
			}
			if _, err := part.Write(data); err != nil {
				return toolError(500, "cannot encode image")
			}
		}
		if err := writer.Close(); err != nil {
			return toolError(500, "cannot encode image request")
		}
		contentType = writer.FormDataContentType()
	} else {
		if err := json.NewEncoder(&body).Encode(input); err != nil {
			return toolError(500, "cannot encode image request")
		}
	}
	// SDK sessions outlive initialization. Couple each call to its own HTTP
	// request as well as to the SDK cancellation notification context.
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(caller.Context(), cancel)
	defer stop()
	defer cancel()
	inner, err := http.NewRequestWithContext(requestsource.WithMCP(ctx), http.MethodPost, path, &body)
	if err != nil {
		return toolError(500, "cannot create image request")
	}
	inner.RemoteAddr = caller.RemoteAddr
	for _, name := range []string{"Authorization", "CF-Connecting-IP", "X-Forwarded-For", "X-Real-IP", "X-Access-Plan", "X-Session-ID", "X-Conversation-ID"} {
		for _, value := range caller.Header.Values(name) {
			inner.Header.Add(name, value)
		}
	}
	inner.Header.Set("User-Agent", "octopus-mcp/1.0 "+caller.UserAgent())
	inner.Header.Set("Content-Type", contentType)
	recorder := httptest.NewRecorder()
	loopback.ServeHTTP(recorder, inner)
	if recorder.Code < 200 || recorder.Code >= 300 {
		message := strings.TrimSpace(recorder.Body.String())
		var failure struct {
			Message string `json:"message"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(recorder.Body.Bytes(), &failure) == nil {
			if failure.Message != "" {
				message = failure.Message
			} else if failure.Error.Message != "" {
				message = failure.Error.Message
			}
		}
		return toolError(recorder.Code, message)
	}
	var response struct {
		Data []struct {
			Base64 string `json:"b64_json"`
			URL    string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || len(response.Data) == 0 {
		return toolError(502, "image response contained no images")
	}
	result := &mcp.CallToolResult{Content: []mcp.Content{}}
	for _, image := range response.Data {
		if image.Base64 != "" {
			data, err := base64.StdEncoding.DecodeString(image.Base64)
			if err != nil || len(data) == 0 {
				return toolError(502, "invalid base64 image response")
			}
			typ := http.DetectContentType(data)
			if !strings.HasPrefix(typ, "image/") {
				return toolError(502, "invalid image response type")
			}
			result.Content = append(result.Content, &mcp.ImageContent{Data: data, MIMEType: typ})
		} else if u, err := url.Parse(image.URL); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
			result.Content = append(result.Content, &mcp.TextContent{Text: image.URL})
		} else {
			return toolError(502, "image response contained an invalid image URL")
		}
	}
	result.Content = append(result.Content, &mcp.TextContent{Text: fmt.Sprintf("Returned %d image(s) using %s. Billed to your Octopus API key.", len(response.Data), input.Model)})
	return result
}
