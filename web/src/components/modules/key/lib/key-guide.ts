// 「接入说明」里各客户端要复制的那一段（照 sub2api 的 UseKeyModal）。
// 纯字符串拼接，不带界面文字：说明文字在组件里按语言给。
// 组件会拼两遍：显示用遮住的 Key，复制按钮用完整 Key。

import type { KeyFamily } from './key-filters';

export type GuideClient = 'curl' | 'claude' | 'codex' | 'gemini' | 'video';
export const GUIDE_CLIENTS: GuideClient[] = ['curl', 'claude', 'codex', 'gemini', 'video'];
export type GuideShell = 'unix' | 'powershell';
export type CurlFamily = 'openai-compatible' | 'anthropic' | 'gemini';
export const CURL_FAMILIES: CurlFamily[] = ['openai-compatible', 'anthropic', 'gemini'];

/** 每个客户端走哪条协议：Key 没开这条协议就用不了，界面上会提示先去打开。 */
export const CLIENT_FAMILY: Record<Exclude<GuideClient, 'curl'>, KeyFamily> = {
    claude: 'anthropic',
    codex: 'openai-compatible',
    gemini: 'gemini',
    video: 'video',
};

/** 示例里的模型：Key 只限某些模型时，挑一个名字对得上的；不限就用常见的名字当占位。 */
const MODEL_HINTS: Record<'openai' | 'claude' | 'gemini' | 'video' | 'codex', { match: RegExp; fallback: string }> = {
    openai: { match: /gpt|o\d|deepseek|qwen/i, fallback: 'gpt-4o' },
    codex: { match: /codex|gpt-5/i, fallback: 'gpt-5-codex' },
    claude: { match: /claude/i, fallback: 'claude-sonnet-4.5' },
    gemini: { match: /gemini/i, fallback: 'gemini-2.5-flash' },
    video: { match: /sora|veo|video|kling/i, fallback: 'sora-2' },
};

export function pickModel(limited: string[], kind: keyof typeof MODEL_HINTS): string {
    const hint = MODEL_HINTS[kind];
    if (limited.length === 0) return hint.fallback;
    return limited.find((m) => hint.match.test(m)) ?? limited[0];
}

export interface GuideInput {
    baseUrl: string;
    apiKey: string;
    /** 要走的方案（X-Octopus-Plan）；不填 = 用 Key 的默认方案，不加这个头。 */
    plan?: string;
    /** Key 限定的模型；空 = 不限。 */
    models: string[];
    shell: GuideShell;
}

export interface GuideBlock {
    /** 这一段是什么：组件按它取说明文字。 */
    id: 'request' | 'env' | 'settings' | 'config' | 'status';
    /** 要写进哪个文件（有的话显示在标题上）。 */
    file?: string;
    code: string;
}

function trimBase(baseUrl: string) {
    return baseUrl.replace(/\/+$/, '');
}

function envLines(vars: Array<[string, string]>, shell: GuideShell): string {
    return vars.map(([k, v]) => (shell === 'powershell' ? `$env:${k}="${v}"` : `export ${k}="${v}"`)).join('\n');
}

export function buildCurl(family: CurlFamily | 'video', input: GuideInput): string {
    const base = trimBase(input.baseUrl);
    const plan = input.plan ? `  -H "X-Octopus-Plan: ${input.plan}" \\\n` : '';
    switch (family) {
        case 'gemini':
            return `curl "${base}/v1beta/models/${pickModel(input.models, 'gemini')}:generateContent" \\
  -H "x-goog-api-key: ${input.apiKey}" \\
${plan}  -H "Content-Type: application/json" \\
  -d '{"contents":[{"parts":[{"text":"Hello"}]}]}'`;
        case 'anthropic':
            return `curl "${base}/v1/messages" \\
  -H "x-api-key: ${input.apiKey}" \\
${plan}  -H "anthropic-version: 2023-06-01" \\
  -H "Content-Type: application/json" \\
  -d '{"model":"${pickModel(input.models, 'claude')}","max_tokens":256,"messages":[{"role":"user","content":"Hello"}]}'`;
        case 'video':
            return `curl "${base}/v1/videos" \\
  -H "Authorization: Bearer ${input.apiKey}" \\
${plan}  -H "Content-Type: application/json" \\
  -d '{"model":"${pickModel(input.models, 'video')}","prompt":"A calm river at dawn"}'`;
        case 'openai-compatible':
        default:
            return `curl "${base}/v1/chat/completions" \\
  -H "Authorization: Bearer ${input.apiKey}" \\
${plan}  -H "Content-Type: application/json" \\
  -d '{"model":"${pickModel(input.models, 'openai')}","messages":[{"role":"user","content":"Hello"}]}'`;
    }
}

export function buildGuide(client: GuideClient, input: GuideInput, curlFamily: CurlFamily = 'openai-compatible'): GuideBlock[] {
    const base = trimBase(input.baseUrl);
    switch (client) {
        case 'curl':
            return [{ id: 'request', code: buildCurl(curlFamily, input) }];
        case 'claude': {
            const vars: Array<[string, string]> = [
                ['ANTHROPIC_BASE_URL', base],
                ['ANTHROPIC_AUTH_TOKEN', input.apiKey],
                ['CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC', '1'],
            ];
            if (input.plan) vars.push(['ANTHROPIC_CUSTOM_HEADERS', `X-Octopus-Plan: ${input.plan}`]);
            if (input.models.length > 0) vars.push(['ANTHROPIC_MODEL', pickModel(input.models, 'claude')]);
            const settings = JSON.stringify({ env: Object.fromEntries(vars) }, null, 2);
            return [
                { id: 'env', code: `${envLines(vars, input.shell)}\nclaude` },
                { id: 'settings', file: input.shell === 'powershell' ? '%USERPROFILE%\\.claude\\settings.json' : '~/.claude/settings.json', code: settings },
            ];
        }
        case 'codex': {
            const headers = input.plan ? `\nhttp_headers = { "X-Octopus-Plan" = "${input.plan}" }` : '';
            const config = `model_provider = "octopus"
model = "${pickModel(input.models, 'codex')}"

[model_providers.octopus]
name = "Octopus"
base_url = "${base}/v1"
wire_api = "responses"
env_key = "OCTOPUS_API_KEY"${headers}`;
            return [
                { id: 'config', file: input.shell === 'powershell' ? '%USERPROFILE%\\.codex\\config.toml' : '~/.codex/config.toml', code: config },
                { id: 'env', code: `${envLines([['OCTOPUS_API_KEY', input.apiKey]], input.shell)}\ncodex` },
            ];
        }
        case 'gemini': {
            const vars: Array<[string, string]> = [
                ['GOOGLE_GEMINI_BASE_URL', base],
                ['GEMINI_API_KEY', input.apiKey],
            ];
            if (input.models.length > 0) vars.push(['GEMINI_MODEL', pickModel(input.models, 'gemini')]);
            return [{ id: 'env', code: `${envLines(vars, input.shell)}\ngemini` }];
        }
        case 'video':
            return [
                { id: 'request', code: buildCurl('video', input) },
                { id: 'status', code: `curl "${base}/v1/videos/<id>" \\\n  -H "Authorization: Bearer ${input.apiKey}"` },
            ];
    }
}
