import {
    OpenAI,
    Claude,
    Gemini,
    DeepSeek,
    Mistral,
    Qwen,
    Meta,
    Ollama,
    Groq,
    Cohere,
    Perplexity,
    Zhipu,
    Yi,
    Kimi,
    Minimax,
    Doubao,
    Hunyuan,
    Spark,
    Wenxin,
    Nvidia,
    Azure,
    Aws,
    Together,
    Fireworks,
    Replicate,
    HuggingFace,
    Grok,
    Google,
    Cerebras,
    SambaNova,
    Cloudflare,
    OpenRouter,
    Volcengine,
    SiliconCloud,
    Novita,
    InternLM,
    Stepfun,
    Gemma,
    Microsoft,
    KwaiKAT,
} from '@lobehub/icons';
import type { ComponentType, CSSProperties } from 'react';

/**
 * Vendor logos come from @lobehub/icons (MIT, already a dependency; inline SVG React
 * components, nothing is fetched at runtime). This is the same package new-api uses for
 * its model/vendor icons. Unknown vendors get a neutral letter avatar instead of a
 * borrowed logo. Logos are trademarks of their owners and only identify the vendor.
 */
export type AvatarComponent = ComponentType<{ size: number; className?: string; style?: CSSProperties }>;

type ModelIconConfig = {
    prefixes: string[];
    Avatar: AvatarComponent;
    color: string;
};

/**
 * Provider configurations with prefixes, Avatar components, and brand colors
 * Similar to Go's Provider array in internal/price/price.go
 */
const MODEL_ICON_PATTERNS: ModelIconConfig[] = [
    // OpenAI - GPT series
    { prefixes: ['gpt-', 'gpt', 'o1', 'o3', 'o4', 'chatgpt', 'text-embedding', 'dall-e', 'whisper', 'tts', 'openai'], Avatar: OpenAI.Avatar, color: '#10A37F' },
    // Anthropic - Claude series
    { prefixes: ['claude', 'anthropic'], Avatar: Claude.Avatar, color: '#D7765A' },
    // Google - Gemini series
    { prefixes: ['gemini'], Avatar: Gemini.Avatar, color: '#4285F4' },
    { prefixes: ['gemma'], Avatar: Gemma.Avatar, color: '#4285F4' },
    { prefixes: ['palm', 'google'], Avatar: Google.Avatar, color: '#4285F4' },
    // DeepSeek series
    { prefixes: ['deepseek'], Avatar: DeepSeek.Avatar, color: '#4D6BFE' },
    // xAI - Grok series
    { prefixes: ['grok', 'xai'], Avatar: Grok.Avatar, color: '#000000' },
    // Alibaba - Qwen series
    { prefixes: ['qwen', 'qwq', 'wanx', 'alibaba'], Avatar: Qwen.Avatar, color: '#6B4EFF' },
    // Zhipu - GLM series
    // Ox Alpha（牛来）匿名马甲真身是 GLM-5.3-Flash / 智谱，归 Zhipu 标
    { prefixes: ['glm', 'chatglm', 'codegeex', 'zhipu', 'z-ai', 'ox-', 'ox_', 'oxalpha', 'niulai', '牛来'], Avatar: Zhipu.Avatar, color: '#3C5BFC' },
    // MiniMax series
    { prefixes: ['minimax', 'abab'], Avatar: Minimax.Avatar, color: '#1A1A2E' },
    // Moonshot/Kimi series
    { prefixes: ['moonshot', 'kimi'], Avatar: Kimi.Avatar, color: '#000000' },
    // Mistral series
    { prefixes: ['mistral', 'mixtral', 'codestral', 'pixtral', 'ministral'], Avatar: Mistral.Avatar, color: '#F7D046' },
    // Meta - Llama series
    { prefixes: ['llama', 'meta-llama', 'meta-'], Avatar: Meta.Avatar, color: '#0668E1' },
    // ByteDance - Doubao series
    { prefixes: ['doubao', 'skylark', 'bytedance'], Avatar: Doubao.Avatar, color: '#00D6C2' },
    // Yi series
    { prefixes: ['yi-', '01-ai'], Avatar: Yi.Avatar, color: '#1B1464' },
    // Tencent - Hunyuan
    { prefixes: ['hunyuan'], Avatar: Hunyuan.Avatar, color: '#0052D9' },
    // iFlytek - Spark
    { prefixes: ['spark'], Avatar: Spark.Avatar, color: '#0078FF' },
    // Baidu - ERNIE/Wenxin
    { prefixes: ['ernie', 'wenxin', 'baidu'], Avatar: Wenxin.Avatar, color: '#2932E1' },
    // InternLM
    { prefixes: ['internlm'], Avatar: InternLM.Avatar, color: '#2F54EB' },
    // Stepfun
    { prefixes: ['stepfun', 'step-'], Avatar: Stepfun.Avatar, color: '#5B5CFF' },
    // Cloud providers
    { prefixes: ['nvidia', 'nemotron'], Avatar: Nvidia.Avatar, color: '#76B900' },
    { prefixes: ['azure'], Avatar: Azure.Avatar, color: '#0078D4' },
    { prefixes: ['aws', 'amazon', 'bedrock'], Avatar: Aws.Avatar, color: '#FF9900' },
    { prefixes: ['volcengine'], Avatar: Volcengine.Avatar, color: '#3370FF' },
    { prefixes: ['siliconflow'], Avatar: SiliconCloud.Avatar, color: '#7C3AED' },
    // Inference providers
    { prefixes: ['groq'], Avatar: Groq.Avatar, color: '#F55036' },
    { prefixes: ['together'], Avatar: Together.Avatar, color: '#0F6FFF' },
    { prefixes: ['fireworks'], Avatar: Fireworks.Avatar, color: '#FF6B00' },
    { prefixes: ['replicate'], Avatar: Replicate.Avatar, color: '#000000' },
    { prefixes: ['ollama'], Avatar: Ollama.Avatar, color: '#FFFFFF' },
    { prefixes: ['openrouter'], Avatar: OpenRouter.Avatar, color: '#6366F1' },
    { prefixes: ['cloudflare'], Avatar: Cloudflare.Avatar, color: '#F38020' },
    { prefixes: ['cerebras'], Avatar: Cerebras.Avatar, color: '#FF5722' },
    { prefixes: ['sambanova'], Avatar: SambaNova.Avatar, color: '#FF6B00' },
    { prefixes: ['novita'], Avatar: Novita.Avatar, color: '#7C3AED' },
    { prefixes: ['huggingface', 'hf'], Avatar: HuggingFace.Avatar, color: '#FFD21E' },
    // Other models
    { prefixes: ['cohere', 'command'], Avatar: Cohere.Avatar, color: '#39594D' },
    { prefixes: ['perplexity'], Avatar: Perplexity.Avatar, color: '#20B8CD' },
    { prefixes: ['phi-'], Avatar: Microsoft.Avatar, color: '#00BCF2' },
    { prefixes: ['kat'], Avatar: KwaiKAT.Avatar, color: '#1969FC' },
];

// Neutral fallback for vendors we do not know: the model's first letter on a muted
// circle (new-api renders unknown icons the same way). Cached per letter so callers that
// resolve the icon during render keep a stable component type.
const NEUTRAL_COLOR = '#71717A';
const letterAvatars = new Map<string, AvatarComponent>();

export function modelInitial(modelName: string): string {
    const cleaned = (modelName || '').trim().replace(/^[^/:]*[/:]/, '');
    const match = cleaned.match(/[\p{L}\p{N}]/u);
    return match ? match[0].toUpperCase() : '?';
}

function letterAvatar(letter: string): AvatarComponent {
    const cached = letterAvatars.get(letter);
    if (cached) return cached;
    const LetterAvatar: AvatarComponent = ({ size, className, style }) => (
        <span
            aria-hidden="true"
            data-letter-avatar={letter}
            className={['inline-flex shrink-0 select-none items-center justify-center rounded-full bg-muted font-semibold text-muted-foreground', className].filter(Boolean).join(' ')}
            style={{ width: size, height: size, fontSize: Math.max(10, Math.round(size * 0.42)), lineHeight: 1, ...style }}
        >
            {letter}
        </span>
    );
    LetterAvatar.displayName = `LetterAvatar(${letter})`;
    letterAvatars.set(letter, LetterAvatar);
    return LetterAvatar;
}

/**
 * Get the Avatar component and color for a given model name
 * @param modelName - The name of the model
 * @returns Object containing Avatar component and brand color
 */
export function getModelIcon(modelName: string): { Avatar: AvatarComponent; color: string } {
    // Extract the part after the first '/' or ':' if it exists
    // e.g., "qwen/gpt-5.2" -> "gpt-5.2", "openai:gpt-4o" -> "gpt-4o"
    let nameToMatch = (modelName || '').trim();
    if (nameToMatch.includes('/')) {
        nameToMatch = nameToMatch.split('/')[1] || nameToMatch;
    } else if (nameToMatch.includes(':')) {
        nameToMatch = nameToMatch.split(':')[1] || nameToMatch;
    }
    const lowerName = nameToMatch.toLowerCase();
    for (const { prefixes, Avatar, color } of MODEL_ICON_PATTERNS) {
        if (prefixes.some(prefix => lowerName.startsWith(prefix) || lowerName.includes(prefix))) {
            return { Avatar, color };
        }
    }
    return { Avatar: letterAvatar(modelInitial(modelName)), color: NEUTRAL_COLOR };
}
