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
    anthropicContext1M?: boolean;
    cloakMode?: string;
}): boolean {
    if (args.models) {
        const models = Array.isArray(args.models) ? args.models : [args.models];
        if (models.some(isStreamRequiredModel)) return true;
    }
    if (args.endpoint === 'openai_responses' && channelUsesCodexIdentity(args.channelType)) return true;
    if (args.endpoint === 'anthropic_messages' && !!args.anthropicContext1M) return true;
    if (args.channelType === ChannelType.OpenAIResponse) return true;
    if (args.channelType === ChannelType.Anthropic && channelCloakApplies(args.cloakMode)) return true;
    return false;
}

/**
 * 该渠道的出站身份是不是 codex 形状（决定要不要强制流式 + 显示哪个身份）。
 *
 * 对应后端 runner.go:1149-1161 的两段判据：
 *   - OutboundTypeOpenAIResponse → 永远是 codex 形状，与端点无关
 *   - OpenAIChat / CustomOpenAIChat 只在 openai_responses 端点上才是 codex 形状
 *   - Volcengine 落 default → 永远不是
 *
 * channelType 为空（老调用方/未知）时返回 true，保持旧行为不误伤。
 */
export function channelUsesCodexIdentity(channelType?: ChannelType): boolean {
    if (channelType === undefined || channelType === null) return true;
    return channelType === ChannelType.OpenAIResponse ||
        channelType === ChannelType.OpenAIChat ||
        channelType === ChannelType.CustomOpenAIChat;
}

/**
 * 该端点+渠道组合对外呈现的身份。响应端点只在渠道真是 codex 形状时才叫 codex，
 * 否则就是通用 UA —— 界面必须和实际出站一致，不能因为端点名而撒谎。
 */
export function channelTestIdentityFor(
    endpoint: ModelTestEndpoint,
    channelType?: ChannelType,
): 'codex' | 'claude' | 'generic' {
    if (endpoint === 'openai_responses') {
        return channelUsesCodexIdentity(channelType) ? 'codex' : 'generic';
    }
    if (endpoint === 'anthropic_messages') return 'claude';
    return 'generic';
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
