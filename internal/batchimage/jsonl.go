package batchimage

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// JSONL 的构建与解析——两家上游（Gemini 开发者 API / Vertex）共用同一套行格式，
// 差别只在"这行交给谁"。所以它单独成文件，不放任何一个 provider 里。
//
// 【行格式】每行一个 JSON 对象：
//
//	{"key":"<custom_id>","request":{"contents":[...],"generationConfig":{...}}}
//
// `key` 字段是**结果回填的唯一凭据**：上游不保证结果行的顺序与输入一致（官方
// issue 里明确有乱序 bug），所以绝不按行号对，一律按 key 回填。
//
// 【为什么不支持"一行多图"以外的形状】批处理生图的任务就是"给个 prompt 画图"。
// 把 chat 的完整多轮 / 工具 / 系统提示那套搬进来，会让这个文件变成一个半吊子
// chat 请求构造器，而正确的做法是将来单独做通用批处理，不是让这一个长歪。

// jsonlGenerationConfig 是 Gemini 的生成参数。字段名按上游文档逐字照抄，
// 大小写错了上游不报错、只是静默不生效，这比报错难查得多。
type jsonlGenerationConfig struct {
	ResponseModalities []string        `json:"responseModalities,omitempty"`
	ResponseMimeType   string          `json:"responseMimeType,omitempty"`
	CandidateCount     int             `json:"candidateCount,omitempty"`
	ImageConfig        *jsonlImageConf `json:"imageConfig,omitempty"`
}

// jsonlImageConf 是图片尺寸/比例的配置位。
type jsonlImageConf struct {
	AspectRatio string `json:"aspectRatio,omitempty"`
	ImageSize   string `json:"imageSize,omitempty"`
}

// jsonlPart 是 contents[].parts[] 里的一项。四种形态互斥（见 batchImagePart）。
type jsonlPart struct {
	Text       string           `json:"text,omitempty"`
	InlineData *jsonlInlineData `json:"inlineData,omitempty"`
	FileData   *jsonlFileData   `json:"fileData,omitempty"`
}

// jsonlInlineData 是内联字节（base64）。
type jsonlInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

// jsonlFileData 是上游文件引用。
type jsonlFileData struct {
	MimeType string `json:"mimeType"`
	FileURI  string `json:"fileUri"`
}

// jsonlContent 是一条消息。
type jsonlContent struct {
	Role  string      `json:"role,omitempty"`
	Parts []jsonlPart `json:"parts"`
}

// jsonlRequest 是每行的 request 体。
type jsonlRequest struct {
	Contents         []jsonlContent         `json:"contents"`
	GenerationConfig *jsonlGenerationConfig `json:"generationConfig,omitempty"`
}

// jsonlLine 是一整行。
type jsonlLine struct {
	Key     string       `json:"key"`
	Request jsonlRequest `json:"request"`
}

// ReferenceImage 是一条子请求里附带的参考图。
//
// 二选一：Data（内联字节）或 FileURI（上游已有的文件引用）。两个都给或都不给都是
// 错误——静默挑一个会让客户端以为另一张也生效了。
type ReferenceImage struct {
	MimeType string
	Data     []byte
	FileURI  string
}

// SubmitItem 是构建 JSONL 需要的单条输入。
type SubmitItem struct {
	CustomID        string
	Prompt          string
	OutputCount     int
	ReferenceImages []ReferenceImage
}

// BuildRequest 是构建一整份 JSONL 需要的全部输入（批次级参数 + 逐条）。
type BuildRequest struct {
	Items            []SubmitItem
	ResponseMimeType string
	AspectRatio      string
	ImageSize        string
}

// maxReferenceImageBytes 是**单张**参考图的字节上限（解码后）。
//
// 参考图会以 base64 进 JSONL，base64 会放大 4/3，再乘上"每批 1000 条 × 14 张"，
// 一次提交理论上能到几百 GB —— 而构建 JSONL 是**整批在内存里拼**的。
// 所以条数与单张大小必须都有上限，只限条数挡不住"条数很少但每张巨大"。
const maxReferenceImageBytes = 8 << 20

// maxReferenceImagesPerItem 是单条子请求允许的参考图数量上限。
//
// 定这个数不是为了省事：Gemini 对单次请求的图片数量有上限，超了上游报错；
// 而这个错误要等批次跑起来才看到，用户已经等了几十分钟。**在提交时就拒绝**。
const maxReferenceImagesPerItem = 14

// BuildJSONL 把一批输入摊成上游要的 JSONL。
//
// 【严格校验，不做部分成功】任何一条不合法就整体拒绝并指出是哪一条：批处理是
// 异步的，一条坏行意味着几十分钟后整批里有一条莫名失败，用户还得自己猜是哪条。
// 提交那一刻报错，是唯一能让人立刻改正的时机。
func BuildJSONL(req BuildRequest) ([]byte, error) {
	if len(req.Items) == 0 {
		return nil, fmt.Errorf("batch image: no items to submit")
	}

	seen := make(map[string]struct{}, len(req.Items))
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	// 关掉 HTML 转义：prompt 里的中文/引号原样交给上游，
	// 转义只会让 JSONL 变大且更难对照原始请求排查。
	encoder.SetEscapeHTML(false)

	for index, item := range req.Items {
		customID := strings.TrimSpace(item.CustomID)
		if customID == "" {
			return nil, fmt.Errorf("batch image: item #%d has empty custom_id", index+1)
		}
		if _, dup := seen[customID]; dup {
			// 重复 custom_id 会让结果回填指向错误的那一条，是**静默的数据错位**，
			// 比直接报错严重得多。
			return nil, fmt.Errorf("batch image: duplicate custom_id %q", customID)
		}
		seen[customID] = struct{}{}

		prompt := strings.TrimSpace(item.Prompt)
		if prompt == "" {
			return nil, fmt.Errorf("batch image: item %q has empty prompt", customID)
		}
		if item.OutputCount < 0 {
			return nil, fmt.Errorf("batch image: item %q has negative output_count", customID)
		}
		for _, image := range item.ReferenceImages {
			if len(image.Data) > maxReferenceImageBytes {
				return nil, fmt.Errorf("batch image: item %q has a %d byte reference image, at most %d allowed",
					customID, len(image.Data), maxReferenceImageBytes)
			}
		}
		if len(item.ReferenceImages) > maxReferenceImagesPerItem {
			return nil, fmt.Errorf("batch image: item %q has %d reference images, at most %d allowed",
				customID, len(item.ReferenceImages), maxReferenceImagesPerItem)
		}

		parts := make([]jsonlPart, 0, len(item.ReferenceImages)+1)
		parts = append(parts, jsonlPart{Text: prompt})
		for imageIndex, image := range item.ReferenceImages {
			part, err := buildReferencePart(customID, imageIndex, image)
			if err != nil {
				return nil, err
			}
			parts = append(parts, part)
		}

		config := &jsonlGenerationConfig{
			// 生图必须显式要 IMAGE，否则上游按纯文本回，回来一张图都没有。
			ResponseModalities: []string{"TEXT", "IMAGE"},
		}
		if mime := strings.TrimSpace(req.ResponseMimeType); mime != "" {
			config.ResponseMimeType = mime
		}
		if item.OutputCount > 0 {
			config.CandidateCount = item.OutputCount
		}
		if strings.TrimSpace(req.AspectRatio) != "" || strings.TrimSpace(req.ImageSize) != "" {
			config.ImageConfig = &jsonlImageConf{
				AspectRatio: strings.TrimSpace(req.AspectRatio),
				ImageSize:   strings.TrimSpace(req.ImageSize),
			}
		}

		line := jsonlLine{
			Key: customID,
			Request: jsonlRequest{
				Contents: []jsonlContent{{
					// 角色显式写 user：不写时部分上游按 unknown 处理并拒绝。
					Role:  "user",
					Parts: parts,
				}},
				GenerationConfig: config,
			},
		}
		if err := encoder.Encode(line); err != nil {
			return nil, fmt.Errorf("batch image: encode item %q: %w", customID, err)
		}
	}
	return buf.Bytes(), nil
}

// buildReferencePart 把一张参考图变成 part，并校验"二选一"。
func buildReferencePart(customID string, index int, image ReferenceImage) (jsonlPart, error) {
	mime := strings.TrimSpace(image.MimeType)
	fileURI := strings.TrimSpace(image.FileURI)
	hasData := len(image.Data) > 0
	hasURI := fileURI != ""

	switch {
	case hasData && hasURI:
		return jsonlPart{}, fmt.Errorf(
			"batch image: item %q reference image #%d gives both data and file_uri; pick one",
			customID, index+1)
	case !hasData && !hasURI:
		return jsonlPart{}, fmt.Errorf(
			"batch image: item %q reference image #%d gives neither data nor file_uri",
			customID, index+1)
	case mime == "":
		return jsonlPart{}, fmt.Errorf(
			"batch image: item %q reference image #%d has empty mime_type", customID, index+1)
	}

	if hasData {
		return jsonlPart{InlineData: &jsonlInlineData{
			MimeType: mime,
			Data:     base64.StdEncoding.EncodeToString(image.Data),
		}}, nil
	}
	return jsonlPart{FileData: &jsonlFileData{
		MimeType: mime,
		FileURI:  fileURI,
	}}, nil
}

// ResultImage 是从结果里解析出来的一张图。
type ResultImage struct {
	MimeType string
	Data     []byte
}

// ParsedResult 是一条子请求的结果。
type ParsedResult struct {
	// Key 是结果行回带的 key。**可能为空**——部分上游不回填，此时由调用方按行序兜底。
	Key    string
	Images []ResultImage
	// Text 是随图一起回来的文本（模型有时会多说一句），仅用于排查。
	Text string
	// Error 非空表示这一条在上游侧失败了。
	Error *ResultError
}

// ResultError 是上游对单条子请求给出的错误。
type ResultError struct {
	Code    string
	Message string
}

// resultEnvelope 是结果对象里的一行。
//
// 上游可能给三种包法，都要认：
//  1. {"key":..,"response":{...}}            —— Gemini File API 批处理
//  2. {"key":..,"response":{...},"error":{}} —— 同上的失败行
//  3. {"instance":{...},"prediction":{...}}  —— Vertex 批处理
//
// 认不出来就当失败行处理并带上原因，**绝不跳过**：跳过会让这个 custom_id
// 永远停在 pending，客户端一直等。
type resultEnvelope struct {
	Key      string           `json:"key"`
	Response *geminiResponse  `json:"response"`
	Error    *resultErrorBody `json:"error"`
	// Vertex 形状
	Prediction *geminiResponse `json:"prediction"`
	Instance   json.RawMessage `json:"instance"`
}

type resultErrorBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

// geminiResponse 是 GenerateContentResponse 里我们关心的部分。
type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text       string `json:"text"`
				InlineData *struct {
					MimeType string `json:"mimeType"`
					Data     string `json:"data"`
				} `json:"inlineData"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	// PromptFeedback 是整条请求被拒时的形状（安全策略等）。
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
}

// ParseResults 逐行解析结果对象。
//
// 【流式解析，不整个读进内存】一批 200 条、每条几张图，整份读进来可能是几百 MB。
// 用 bufio.Scanner 逐行处理，内存占用只跟单行大小有关。
//
// onLine 回调拿到的 lineNumber 是**结果对象里的行号（1-based）**，调用方拿它做索引，
// 也拿它做"对不上时到底哪一行坏了"的排查线索。
// onLine 返回 errStopResultScan 可让解析**立刻停止且不算失败**。
// 这是给"找到目标就收工"的单图取图路径用的：结果流可能几百 MB，
// 目标行已经在手上还继续读，等于把整批下载一遍只为丢弃。
func ParseResults(reader io.Reader, onLine func(lineNumber int, result ParsedResult) error) error {
	if reader == nil {
		return fmt.Errorf("batch image: nil result reader")
	}
	scanner := bufio.NewScanner(reader)
	// 单行可能很大（一张 base64 图就可能几 MB）。默认 64KB 的 token 上限会直接
	// 报 "token too long"，而那个错误信息完全看不出是图太大。
	scanner.Buffer(make([]byte, 0, 256*1024), maxResultLineBytes)

	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		if err := onLine(lineNumber, parseResultLine(raw)); err != nil {
			if errors.Is(err, errStopResultScan) {
				return nil
			}
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("batch image: read result line %d: %w", lineNumber+1, err)
	}
	return nil
}

// errStopResultScan 是 onLine 用来"提前收工"的哨兵。故意不导出：
// 它表达的是"我已拿到想要的东西"，不是业务错误，不该出现在对外错误里。
var errStopResultScan = errors.New("batch image: stop result scan")

// maxResultLineBytes 是单行结果的上限。
//
// 一张 4K PNG 的 base64 大约 20-40MB。给到 64MB 是留足余量而不是鼓励大图：
// 真要更大的图，正确做法是让上游给 fileUri 而不是把字节塞进行里。
const maxResultLineBytes = 64 * 1024 * 1024

// parseResultLine 解析一行，任何解析失败都变成一条带错误的 ParsedResult。
func parseResultLine(raw []byte) ParsedResult {
	var envelope resultEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return ParsedResult{Error: &ResultError{
			Code:    "invalid_result_line",
			Message: "upstream result line is not valid JSON",
		}}
	}

	parsed := ParsedResult{Key: strings.TrimSpace(envelope.Key)}

	if envelope.Error != nil {
		code := strings.TrimSpace(envelope.Error.Status)
		if code == "" {
			code = fmt.Sprintf("upstream_%d", envelope.Error.Code)
		}
		message := strings.TrimSpace(envelope.Error.Message)
		if message == "" {
			message = "upstream reported an error for this item"
		}
		parsed.Error = &ResultError{Code: code, Message: message}
		return parsed
	}

	// Vertex 形状：prediction 里放 response，instance 里带 key。
	response := envelope.Response
	if response == nil && envelope.Prediction != nil {
		response = envelope.Prediction
		if parsed.Key == "" && len(envelope.Instance) > 0 {
			var instance struct {
				Key string `json:"key"`
			}
			if err := json.Unmarshal(envelope.Instance, &instance); err == nil {
				parsed.Key = strings.TrimSpace(instance.Key)
			}
		}
	}

	if response == nil {
		parsed.Error = &ResultError{
			Code:    "missing_response",
			Message: "upstream result line has neither response nor prediction",
		}
		return parsed
	}

	if response.PromptFeedback != nil && strings.TrimSpace(response.PromptFeedback.BlockReason) != "" {
		parsed.Error = &ResultError{
			Code:    "blocked",
			Message: "request was blocked: " + strings.TrimSpace(response.PromptFeedback.BlockReason),
		}
		return parsed
	}

	for _, candidate := range response.Candidates {
		for _, part := range candidate.Content.Parts {
			if part.InlineData != nil && part.InlineData.Data != "" {
				data, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
				if err != nil {
					// 一条坏图不该让整批失败：把它记成这一条的错误，其余照常。
					parsed.Error = &ResultError{
						Code:    "invalid_image_data",
						Message: "upstream returned image data that is not valid base64",
					}
					parsed.Images = nil
					return parsed
				}
				parsed.Images = append(parsed.Images, ResultImage{
					MimeType: normalizeImageMime(part.InlineData.MimeType),
					Data:     data,
				})
				continue
			}
			if text := strings.TrimSpace(part.Text); text != "" {
				if parsed.Text == "" {
					parsed.Text = text
				} else {
					parsed.Text += "\n" + text
				}
			}
		}
	}

	if len(parsed.Images) == 0 {
		// 有响应但没有图：多半是被安全策略挡了或模型只回了文字。
		// 必须变成错误，否则这一条会显示"成功但零张图"。
		message := "upstream returned no image"
		if parsed.Text != "" {
			message += ": " + truncateText(parsed.Text, 200)
		}
		parsed.Error = &ResultError{Code: "no_image_returned", Message: message}
	}
	return parsed
}

// normalizeImageMime 兜底推断图片类型。
//
// 上游偶尔不回 mimeType；这时按 PNG 处理并把类型串补全，因为后面取图/打包
// 都要靠它决定 Content-Type 和扩展名，空着会让客户端拿不到可用的文件。
func normalizeImageMime(mime string) string {
	mime = strings.ToLower(strings.TrimSpace(mime))
	if mime == "" {
		return "image/png"
	}
	return mime
}

// ImageExtension 把 mime 映射成扩展名（用于文件名与 ZIP 条目）。
func ImageExtension(mime string) string {
	switch normalizeImageMime(mime) {
	case "image/png":
		return "png"
	case "image/jpeg", "image/jpg":
		return "jpg"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	case "image/heic":
		return "heic"
	default:
		// 未知类型用 png 作为扩展名比留空好：留空会让下载下来的文件打不开，
		// 而 Content-Type 仍然如实反映真实类型。
		return "png"
	}
}

// truncateText 按**字节**安全截断，不切坏多字节字符。
func truncateText(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	// 从 limit 往回退，直到落在一个 UTF-8 字符边界上。
	cut := limit
	for cut > 0 && (text[cut]&0xC0) == 0x80 {
		cut--
	}
	return text[:cut] + "..."
}
