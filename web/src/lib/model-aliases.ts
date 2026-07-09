export function isStreamRequiredModel(model: string) {
    const value = model.trim().toLowerCase();
    if (!value) return false;
    return /^gpt-5\.5(?:$|[-_:])/i.test(value)
        || value === 'opus[1m]'
        || value.endsWith('[1m]')
        || value.includes('context-1m');
}

/** 模型/渠道测试统一默认超时（秒）——三处测试入口（渠道卡片/渠道表单/模型测试页）共用，
 *  避免渠道卡片曾经非 Anthropic 用 30s 的分叉。 */
export const DEFAULT_MODEL_TEST_TIMEOUT_SECONDS = 180;

/** 统一「测试是否强制流式」判断：任一 stream-required 模型 / responses 端点 / anthropic-1M 端点 → 强制流。
 *  三处测试入口共用同一规则，避免各写一套导致行为漂移。endpoint 用字符串（避免和 ChannelType 循环依赖）。 */
export function shouldForceTestStream(args: {
    models: string | string[];
    endpoint: string;
    anthropicContext1M?: boolean;
}): boolean {
    const models = Array.isArray(args.models) ? args.models : [args.models];
    if (models.some(isStreamRequiredModel)) return true;
    if (args.endpoint === 'openai_responses') return true;
    if (args.endpoint === 'anthropic_messages' && !!args.anthropicContext1M) return true;
    return false;
}

/** 模型/渠道测试统一默认 prompt——四位数加法（沿用原模型测试页），只回答结果不解释，
 *  快速判通道是否真能出正文；每次测试现随机一道，避免上游缓存命中。 */
export function makeModelTestPrompt(): string {
    const left = 1000 + Math.floor(Math.random() * 9000);
    const right = 1000 + Math.floor(Math.random() * 9000);
    return `请只回答算式结果，不要解释：${left} + ${right} = ?`;
}

export function cleanOneMillionModelName(model: string) {
    const trimmed = model.trim();
    const value = trimmed.toLowerCase();
    if (!value) return '';
    if (value === 'opus[1m]' || value === 'claude-opus-4-8[1m]' || value === 'claude-opus-4.8[1m]' || value === 'claude-opus-4-7[1m]' || value === 'claude-opus-4.7[1m]') {
        return 'claude-opus-4-8';
    }
    if (value === 'fable[1m]' || value === 'claude-fable-5[1m]') {
        return 'claude-fable-5';
    }
    return trimmed.replace(/\[1m\]$/i, '');
}

export function expandOneMillionModelAliases(models: string[]) {
    const result: string[] = [];
    const seen = new Set<string>();
    const add = (model: string) => {
        const trimmed = cleanOneMillionModelName(model);
        if (!trimmed) return;
        const key = trimmed.toLowerCase();
        if (seen.has(key)) return;
        seen.add(key);
        result.push(trimmed);
    };

    for (const model of models) {
        add(model);
    }

    return result;
}
