import { ChannelType } from '@/api/endpoints/channel';
import { isStreamRequiredModel } from './model-aliases';

export const MODEL_TEST_ENDPOINTS = [
    { value: 'openai_chat', label: 'Chat' },
    { value: 'openai_responses', label: 'Responses' },
    { value: 'anthropic_messages', label: 'Claude' },
    { value: 'gemini_generate_content', label: 'Gemini' },
    { value: 'openai_embeddings', label: 'Embeddings' },
] as const;

export type ModelTestEndpoint = (typeof MODEL_TEST_ENDPOINTS)[number]['value'];

export const MODEL_TEST_ENDPOINT_LABELS: Record<ModelTestEndpoint, string> = {
    openai_chat: 'Chat',
    openai_responses: 'Responses',
    anthropic_messages: 'Claude',
    gemini_generate_content: 'Gemini',
    openai_embeddings: 'Embeddings',
};

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
    if (args.models) {
        const models = Array.isArray(args.models) ? args.models : [args.models];
        if (models.some(isStreamRequiredModel)) return true;
    }
    // 🔴 强制流式的唯一来源是"真 codex 渠道"，而后端只对 OutboundTypeOpenAIResponse
    // 这么做（runner.go:763）。它**不**看端点名，也**不**看 cloak.shape：
    // 一个 shape=codex 的普通 Chat 渠道，后端不会强制流式。
    // 早先这里写 `endpoint === 'openai_responses' → true`，导致把火山渠道（出站
    // /responses 但不是 codex）的流式开关锁死，测不了非流式。
    if (args.channelType === ChannelType.OpenAIResponse) return true;
    if (args.endpoint === 'anthropic_messages' && !!args.anthropicContext1M) return true;
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
    endpoint: ModelTestEndpoint,
    channelType?: ChannelType,
    cloak?: { mode?: string; shape?: string },
): 'codex' | 'claude' | 'generic' {
    // 1. cloak 关掉：后端明确强制通用身份，优先于一切。
    if (cloak && !channelCloakApplies(cloak.mode)) return 'generic';

    // 2./3. 渠道协议本身就是某个 CLI 的，身份不可选。（Gemini 无 CLI 身份）
    if (channelType === ChannelType.Anthropic) return 'claude';
    if (channelType === ChannelType.OpenAIResponse) return 'codex';

    // 4. Chat 族由 cloak.shape 显式 opt-in，否则通用。
    const shape = (cloak?.shape ?? '').toLowerCase().trim();
    if (shape === 'claude') return 'claude';
    if (shape === 'codex') return 'codex';

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
