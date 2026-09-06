import { ChannelType } from '@/api/endpoints/channel';
import { isStreamRequiredModel } from './model-aliases';

export const MODEL_TEST_ENDPOINTS = [
    { value: 'openai_chat', label: 'Chat' },
    { value: 'openai_responses', label: 'Responses' },
    { value: 'anthropic_messages', label: 'Claude' },
    { value: 'gemini_generate_content', label: 'Gemini' },
] as const;

export type ModelTestEndpoint = (typeof MODEL_TEST_ENDPOINTS)[number]['value'];

export const MODEL_TEST_ENDPOINT_LABELS: Record<ModelTestEndpoint, string> = {
    openai_chat: 'Chat',
    openai_responses: 'Responses',
    anthropic_messages: 'Claude',
    gemini_generate_content: 'Gemini',
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
        .trim();
}

export function defaultModelTestEndpointForChannel(type: ChannelType): ModelTestEndpoint {
    switch (type) {
        case ChannelType.OpenAIResponse:
            return 'openai_responses';
        case ChannelType.Anthropic:
            return 'anthropic_messages';
        case ChannelType.Gemini:
            return 'gemini_generate_content';
        default:
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
 * 2) endpoint = openai_responses
 * 3) endpoint = anthropic_messages 且 1M
 * 4) channelType = OpenAIResponse
 * 5) channelType = Anthropic 且 cloak 生效
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
    if (args.endpoint === 'openai_responses') return true;
    if (args.endpoint === 'anthropic_messages' && !!args.anthropicContext1M) return true;
    if (args.channelType === ChannelType.OpenAIResponse) return true;
    if (args.channelType === ChannelType.Anthropic && channelCloakApplies(args.cloakMode)) return true;
    return false;
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
