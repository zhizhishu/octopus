import { ChannelType } from '@/api/endpoints/channel';
import { isStreamRequiredModel } from './model-aliases';

export const MODEL_TEST_ENDPOINTS = [
    { value: 'openai_chat', label: 'Chat' },
    { value: 'openai_responses', label: 'Responses' },
    { value: 'anthropic_messages', label: 'Claude' },
    { value: 'gemini_generate_content', label: 'Gemini' },
    { value: 'openai_embeddings', label: 'Embeddings' },
    { value: 'openai_images_generations', label: 'Images' },
] as const;

export type ModelTestEndpoint = (typeof MODEL_TEST_ENDPOINTS)[number]['value'];

export const MODEL_TEST_ENDPOINT_LABELS: Record<ModelTestEndpoint, string> = {
    openai_chat: 'Chat',
    openai_responses: 'Responses',
    anthropic_messages: 'Claude',
    gemini_generate_content: 'Gemini',
    openai_embeddings: 'Embeddings',
    openai_images_generations: 'Images',
};

/**
 * 图片端点名（含后端 normalizeEndpoint 接受的别名）。
 *
 * 归一化逐条镜像 internal/modeltest/runner.go:499-506 的 normalizeEndpoint：
 * 小写、去首尾空白、`-` 与 `/` 一律换成 `_`。少了这一步，`OpenAI-Images`、
 * `images_generations` 这类写法会被判成"不是图片端点"，界面又开始按 codex 身份显示。
 */
const IMAGE_TEST_ENDPOINT_NAMES = new Set([
    'openai_images_generations', // runner.go:73 的端点名，UI 里 Images 那颗胶囊
    'images', // runner.go:535 的别名
    'images_generations',
    'openai_images',
]);

/**
 * 本次测试是不是走图片端点（/v1/images/generations，直通 relay.ImagesHandler）。
 *
 * 图片端点是渠道测试里唯一"自己的转发路径"的端点：后端 runner.go:674 在能力位 /
 * 形状逻辑**之前**就把它交给 tryImageChannel，此后所有关于流的判断都失效。
 * 所以凡是"是否锁流式 / 什么身份"的判断，都要先问这一句。
 */
export function isImageTestEndpoint(endpoint?: string): boolean {
    const name = (endpoint ?? '').toLowerCase().trim().replace(/[-/]/g, '_');
    return IMAGE_TEST_ENDPOINT_NAMES.has(name);
}

export const DEFAULT_MODEL_TEST_TIMEOUT_SECONDS = 180;

/**
 * Sanitize provider error text before channel/model-test UI exposure.
 * Keep code-like filenames readable while redacting upstream identity and credentials.
 */
export function sanitizeChannelTestError(text: string): string {
    const fileExtensions = 'js|mjs|cjs|jsx|ts|tsx|go|py|rs|rb|php|java|kt|c|h|cc|cpp|hpp|cs|swift|json|ya?ml|toml|ini|env|md|txt|csv|log|html?|css|scss|sh|bash|zsh|sql|proto|lock|png|jpe?g|gif|svg|webp|ico|pdf|zip|tar|gz|exe|dll|so|dylib|bin|db|sqlite';
    return text
        .replace(/https?:\/\/[^\s"'）)\]]+/gi, '上游')
        .replace(new RegExp(`\\b(?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\\.)+(?!(?:${fileExtensions})\\b)[a-z]{2,}(?::\\d{2,5})?\\b`, 'gi'), '上游')
        .replace(/\b\d{1,3}(?:\.\d{1,3}){3}(?::\d{2,5})?\b/g, '上游')
        .replace(/(authorization\s*:\s*bearer|x-api-key|api[_-]?key|token|secret)\s*[=:]?\s*[^\s,;]+/gi, '$1 [已隐藏]')
        .replace(/\bsk-[a-z0-9_-]+\b/gi, '[已隐藏]')
        .replace(/\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b/g, '[已隐藏]')
        .replace(/\b[A-Fa-f0-9]{32,}\b/g, '[已隐藏]')
        .trim();
}

export function defaultModelTestEndpointForChannel(type: ChannelType): ModelTestEndpoint {
    switch (type) {
        case ChannelType.OpenAIResponse:
            return 'openai_responses';
        case ChannelType.Volcengine:
            // 火山渠道的出站**固定**是 /responses（见 outbound/volcengine/response.go:107
            // `parsedUrl.Path = parsedUrl.Path + "/responses"`），它根本不发
            // /v1/chat/completions。早先落进 default 分支按 openai_chat 测，
            // 测的是一条这个渠道永远不会走的路径——测出来的绿是假的。
            //
            // 注意这里用 openai_responses 只是为了对上"真实出站是 responses 形状"，
            // 但火山渠道的 OutboundType 是 Volcengine 而不是 OpenAIResponse，
            // 所以 runner 里那条"codex 渠道强制流式"的分支不会误伤它。
            return 'openai_responses';
        case ChannelType.Anthropic:
            return 'anthropic_messages';
        case ChannelType.Gemini:
            return 'gemini_generate_content';
        case ChannelType.OpenAIEmbedding:
            // 嵌入渠道只有 /v1/embeddings 一个可用端点：chat 形状会被出站适配器
            // 直接拒收（EmbeddingOutbound 只认 EmbeddingInput）。
            return 'openai_embeddings';
        default:
            // OpenAI Chat 与 Custom OpenAI Chat 都是 chat 形状。
            return 'openai_chat';
    }
}

/**
 * Mirror backend shouldApplyChannelCloak: cloak applies unless explicitly turned off.
 */
export function channelCloakApplies(mode?: string): boolean {
    const m = (mode ?? 'auto').toLowerCase().trim();
    return !(m === 'never' || m === 'off' || m === 'false' || m === 'disabled');
}

/**
 * 统一「测试是否强制流式」判断：
 * 0) endpoint 是图片端点 → **永不**强制流式（见下面第 0 条，后端写死非流式）
 * 1) 任一 isStreamRequiredModel
 * 2) endpoint = openai_responses **且渠道真是 codex 形状**
 * 3) endpoint = anthropic_messages 且 1M
 * 4) channelType = OpenAIResponse
 * 5) channelType = Anthropic 且 cloak 生效
 *
 * 🔴 第 2 条为什么不能只看端点：强制流式来源于"真 codex 客户端永远流式"这条
 * shape 约束（后端 runner.go:763 那条分支）。它绑的是 OutboundType 而不是端点名。
 * 火山渠道的默认测试端点是 openai_responses（它的出站固定 /responses），但它的
 * OutboundType 是 Volcengine —— 后端既不给它强制流式（runner.go:763 只匹配
 * OutboundTypeOpenAIResponse），也不套 codex 指纹（runner.go:1156-1161 的
 * switch 里 Volcengine 落 default 返回 false）。只看端点会让前端锁死流式开关、
 * 还显示"codex CLI"身份，而实际出站是通用 UA 的非流式 —— 界面在撒谎。
 *
 * 所以判据是：端点选了 responses **并且**渠道本身是 codex 形状（或渠道类型未知，
 * 沿用旧行为）。Volcengine / 普通 chat 渠道选 responses 端点时不锁流。
 */
export function shouldForceChannelTestStream(args: {
    models?: string | string[];
    endpoint?: string;
    channelType?: ChannelType;
    cloakMode?: string;
    cloakShape?: string;
    anthropicContext1M?: boolean;
}): boolean {
    // 🔴 这里只返回"用户无权关闭流式"的情形，必须逐条对应 runner 里**真正覆盖
    // 显式 Stream=false** 的分支。审计三轮把这里越修越窄，最后收敛成两条：
    //
    //   1. Responses 渠道（OutboundTypeOpenAIResponse）—— runner.go:763-772
    //      无条件 `stream := true`，不看端点、不看 shape。
    //   2. cloak 生效的 Anthropic 渠道 —— runner.go:1241-1254
    //      forceClaudeModelTestBodyShape 里无条件 `stream := true`，其调用点是
    //      `if shouldApplyChannelCloak(channel.Cloak)`。
    //
    // **不再包含**的两类，都是没有后端对应的：
    //   - 模型名（`isStreamRequiredModel`：gpt-5.5 / [1m] / context-1m）：
    //     runner.go:911-914 明确"显式给了 stream 就听它的"，:959 的
    //     `if !streamExplicit { stream = true }` 只是**未指定时默认流式**，
    //     客户端仍可传 false。原来在这里锁死，等于把一个后端允许的选项藏起来。
    //   - `anthropic_messages && anthropicContext1M`：1M helper
    //     (runner.go:1240-1254) 的流式来自上面的 cloak 门，不来自 1M 本身。
    //
    // 🔴 第 0 条必须排在渠道类型判据**之前**，否则第 4 条会把图片端点重新锁死。
    //
    // 图片探针根本不进这条判据链：后端 runner.go:674 在能力位/形状逻辑之前就把
    // 图片端点转给 tryImageChannel，而 image_channel.go:112 的 `stream := false`
    // 是写死的、**不读 request.Stream**（Images 的流式响应没有统一判定口径，宁可
    // 少测一种模式，也不许把好渠道报成"上游没给图片数据"的假红）。请求里就算带了
    // stream=true，图片分支也不看。所以界面对图片端点宣称"强制流式"是纯撒谎。
    if (isImageTestEndpoint(args.endpoint)) return false;
    if (args.channelType === ChannelType.OpenAIResponse) return true;
    if (args.channelType === ChannelType.Anthropic && channelCloakApplies(args.cloakMode)) return true;
    // 渠道类型未知（老调用方）：沿用旧行为，避免行为突变。
    if ((args.channelType === undefined || args.channelType === null) &&
        args.endpoint === 'openai_responses') return true;
    return false;
}

/**
 * 该渠道的真实出站身份族：'claude' | 'codex' | 'generic'。
 *
 * 必须镜像后端 `header_defaults.go:78-247` 的实际判据，而不是"端点名 + 渠道类型"
 * 这种想当然的对应。逐条对照：
 *
 *   0. endpoint 是图片端点 → 通用 UA（relay/images.go:1012 只套 genericUAForChannel，
 *      不走 applyHeaderDefaults，也没有任何 CLI 指纹分支）。
 *   1. cloak 关了（`!shouldApplyChannelCloak`，见 header_defaults.go:81-90）：
 *      → 强制通用 UA，并剥掉 codex 头。**和渠道类型无关。**
 *   2. Anthropic 渠道 → claude CLI。
 *   3. Responses 渠道 → codex CLI（协议本身就是 codex 的）。
 *   4. 其它渠道（含普通 Chat / Volcengine）→ 看 `cloak.shape`：
 *      - 'claude' → claude CLI
 *      - 'codex'  → codex CLI
 *      - 其它/空 → 通用 UA（header_defaults.go:240-246 的 default 分支）
 *
 * 注意 Volcengine 落在最后一条：它出站是 /responses 形状，但**身份是通用 UA**。
 * 早先版本只看端点名，把火山标成 codex —— 界面撒谎。
 */
export function channelTestIdentityFor(
    // 收 string 而不是 ModelTestEndpoint：后端 normalizeEndpoint 还认 'images' /
    // 'images_generations' / 'openai_images' 这些别名，别名进来也要能被认出来。
    endpoint: string,
    channelType?: ChannelType,
    cloak?: { mode?: string; shape?: string },
): 'codex' | 'claude' | 'generic' {
    // 0. 图片端点：出站身份**固定通用 UA**，与渠道类型 / cloak.shape 无关。
    //
    // 图片转发走 relay.ImagesHandler，它的发请求路径没有 applyHeaderDefaults，
    // UA 只有一句 relay/images.go:1012
    //   setHeaderIfMissing(req.Header, "User-Agent", genericUAForChannel(channel))
    // —— genericUAForChannel 取指纹 profile 的 GenericUA，没有任何 codex / claude
    // 分支（images.go 全文只有一处提到 "codex"，且是 :1011 那句注释，说明
    // /responses/compact 子路径之后会覆盖 UA，不是图片路径分支）。所以一条 Responses 渠道测图片
    // 端点，实际发的是通用 UA：界面显示 codex CLI 是撒谎。这条同样排在渠道类型判据之前。
    if (isImageTestEndpoint(endpoint)) return 'generic';

    // 1. cloak 关掉：后端明确强制通用身份，优先于一切。
    if (cloak && !channelCloakApplies(cloak.mode)) return 'generic';

    // 2./3. 渠道协议本身就是某个 CLI 的，身份不可选。（Gemini 无 CLI 身份）
    if (channelType === ChannelType.Anthropic) return 'claude';
    if (channelType === ChannelType.OpenAIResponse) return 'codex';

    // 4. **只有 OpenAI Chat / Custom OpenAI Chat 两类**读 cloak.shape。
    //
    // header_defaults.go:210 的 `case OutboundTypeOpenAIChat,
    // OutboundTypeCustomOpenAIChat` 才进 `switch chatChannelShape(channel)`；
    // Gemini / Volcengine / Embedding 全部落到 :239 的 default，直接走
    // applyGenericHeaderDefaultsForChannel，根本不看 shape。
    // channelTLSFace（:279-291）同样只让这两类读 shape。
    //
    // 所以不能"对剩下的渠道一律读 shape"：Form.tsx:766 切渠道类型时保留原
    // cloak_shape，一个配过 codex 的 Chat 渠道改成火山后，shape 还留在配置里，
    // 前端会显示 codex 而实际发的是通用 UA —— 又一次界面撒谎。
    const isChatFamily = channelType === ChannelType.OpenAIChat ||
        channelType === ChannelType.CustomOpenAIChat;
    if (isChatFamily) {
        const shape = (cloak?.shape ?? '').toLowerCase().trim();
        if (shape === 'claude') return 'claude';
        if (shape === 'codex') return 'codex';
    }

    // 端点只影响"测的是什么协议"，不影响身份族——除非渠道类型未知
    // （老调用方），此时沿用旧的按端点推断，避免行为突变。
    if (channelType === undefined || channelType === null) {
        if (endpoint === 'openai_responses') return 'codex';
        if (endpoint === 'anthropic_messages') return 'claude';
    }
    return 'generic';
}

/**
 * 该渠道的出站身份是不是 codex 形状。
 * 供流式判断与显示复用，避免两处各推一遍。
 */
export function channelUsesCodexIdentity(
    channelType?: ChannelType,
    cloak?: { mode?: string; shape?: string },
): boolean {
    return channelTestIdentityFor('openai_chat', channelType, cloak) === 'codex';
}

/**
 * 模型/渠道测试统一默认 prompt——四位数加法（沿用原模型测试页），只回答结果不解释，
 * 快速判通道是否真能出正文；每次测试现随机一道，避免上游缓存命中。
 */
export function makeModelTestPrompt(): string {
    const left = 1000 + Math.floor(Math.random() * 9000);
    const right = 1000 + Math.floor(Math.random() * 9000);
    return `请只回答算式结果，不要解释：${left} + ${right} = ?`;
}
