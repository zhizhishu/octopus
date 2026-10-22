package modeltest

import "strings"

// imageGenerationModelPatterns 是"哪些模型该走图片端点"的唯一真源。
//
// 来源：new-api 的 common/model.go `ImageGenerationModels`（2026-10-22 抄录，逐条照抄，
// 顺序也照抄）。语义与 new-api 一致：
//   - "prefix:" 开头 → 按**前缀**匹配；
//   - 其余 → 按**子串**匹配；
//   - 两边都先转小写再比。
//
// 别在别处再写一份类似的清单（前端也一样）：改一次要能同时影响所有调用点，
// 否则"该不该测图片"会随一处漏改而悄悄漂移。
var imageGenerationModelPatterns = []string{
	"dall-e-3",
	"dall-e-2",
	"prefix:dall-e",
	"gpt-image-",
	"qwen-image",
	"z-image",
	"wan2.7-image-pro",
	"wan2.7-image",
	"wan2.6-image",
	"wan2.6-t2i",
	"wan2.5-t2i-preview",
	"wan2.2-t2i-flash",
	"wan2.2-t2i-plus",
	"wanx2.1-t2i-turbo",
	"wanx2.1-t2i-plus",
	"wanx2.0-t2i-turbo",
	"prefix:imagen-",
	"flux-",
	"flux.1-",
}

// imageGenerationModelPrefixMarker 是清单里"按前缀匹配"的标记，与 new-api 拼写一致。
const imageGenerationModelPrefixMarker = "prefix:"

// IsImageGenerationModel 判断模型名是否属于图片生成模型（镜像 new-api 的同名函数）。
//
// 空模型名一律判否：调用方拿到空串通常意味着"用户还没选模型"，那不是图片模型。
func IsImageGenerationModel(modelName string) bool {
	name := strings.ToLower(strings.TrimSpace(modelName))
	if name == "" {
		return false
	}
	for _, pattern := range imageGenerationModelPatterns {
		if strings.HasPrefix(pattern, imageGenerationModelPrefixMarker) {
			prefix := strings.TrimPrefix(pattern, imageGenerationModelPrefixMarker)
			if prefix == "" {
				continue
			}
			if strings.HasPrefix(name, prefix) {
				return true
			}
			continue
		}
		if strings.Contains(name, pattern) {
			return true
		}
	}
	return false
}
