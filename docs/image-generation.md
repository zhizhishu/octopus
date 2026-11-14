# 图片生成与档位选择

Octopus 的图片接口是 `POST /v1/images/generations`。请求使用渠道已配置的模型名称；
渠道是否启用、模型是否被允许，仍由后台配置决定。本文中的
`https://upstream.example` 是占位地址，请替换成自己的 Octopus 地址。

## Images 接口

Gemini 图片渠道可通过 `quality` 选择 `1K`、`2K` 或 `4K`。
`size` 用于选择画幅，最终像素尺寸由上游模型决定；两者分别设置。

| 目标档位 | 请求中的 quality |
| --- | --- |
| 1K | `"1K"` |
| 2K | `"2K"` |
| 4K | `"4K"` |

例如，一张横向 4K 图片：

```sh
curl https://upstream.example/v1/images/generations \
  -H "Authorization: Bearer $OCTOPUS_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-3-pro-image",
    "prompt": "A blue ceramic cup on a wooden table",
    "n": 1,
    "size": "1792x1024",
    "quality": "4K"
  }'
```

`quality` 的取值随模型而异。上表是 Gemini 图像桥的档位；
其他图片模型应使用其支持的质量值，例如 `low`、`medium`、`high`
或 `standard`、`hd`。Octopus 不把一个模型的枚举强加给另一个模型。

渠道测试和草稿渠道测试都可在选择图片接口后设置尺寸与质量。未选择时保持原有
测试请求：`model`、`n: 1`、固定测试提示词和 `size: "1024x1024"`，
不额外发送 `quality`。只有用户显式选择时才带上对应字段。
这些选择仅用于本次图片测试，不会修改渠道配置，也不会进入同时选择的其他接口测试。

图片测试会真实生成一张图片并消耗上游额度；界面测试按钮的行为与生产图片链路一致。

## Chat 接口

`/v1/chat/completions` 中只写“画一张图”不会为请求补入分辨率档位。
没有有效的档位参数时，Octopus 保留上游默认行为，不强制注入 1K、2K 或 4K。
已验证的 Gemini 3 图像上游在这种情况下返回默认档；不能把它理解为所有模型都保证
同一像素尺寸。

对于支持自定义请求体字段的客户端，可将以下内容合并到 chat 请求的**顶层**：

```json
{
  "image_config": {
    "aspect_ratio": "16:9",
    "image_size": "4K"
  }
}
```

有的客户端将这个设置称为“extra body”。这里指客户端把字段合并进请求体，
不是再套一个名为 `extra_body` 的 JSON 层。也可以使用明确的
`image_generation` 工具质量参数；同时存在时，显式 `image_config` 优先。

仅设置宽高比而没有分辨率时，也不会补一个默认 `image_size`。
后台服务日志会在符合条件的 chat 生图请求旁记录：

```text
image tier: upstream default (client sent none)
```

这是一条服务端观测日志，不会被加入模型响应、请求正文或中继日志 API 字段。

## Pi Agent Desktop

Pi Agent Desktop 的生图模型可以独立于对话模型绑定：

1. 在设置中的模型／提供商页面添加 OpenAI Compatible 提供商。
2. Base URL 填到 `/v1`，例如 `https://upstream.example/v1`。
3. 选择后台已允许的图片模型，例如 `gemini-3-pro-image` 或
   `gemini-3.1-flash-image`，并绑定为生图模型。
4. 如果提供商类型支持选择，使用直接走 Images 的类型，以明确访问
   `/v1/images/generations`。

该客户端的生图流程可能先尝试 Responses 内置的 `image_generation` 工具，
在不支持该端点或工具时才回退到 Images。Octopus 本身有 `/v1/responses`
路由，因此不能仅凭“配置了 Base URL”就认定这次调用已经走 Images。
需要固定接口时，应在客户端选择对应的提供商类型或接口模式。

## Cherry Studio 等 Chat 客户端

使用纯 chat 调用的客户端默认沿用上游图片档位。如果客户端支持自定义请求体，
可以填写上面的顶层 `image_config`；需要明确测试 2K／4K 时，也可以使用
Images 接口。Base URL 同样填写 `https://upstream.example/v1`，
模型名称应与后台允许的名称一致，不要自行加上 `-preview` 等后缀。
