/** 管理员编辑用的纯文字模板；不触碰出站身份、模型元数据或响应。 */
export type IdentityPresetMode = 'brand' | 'neutral';

export type IdentityPresetInput = {
    mode: IdentityPresetMode;
    displayName: string;
    style: string;
};

function plainLine(value: string, maxLength: number): string {
    return value.replace(/[\r\n\t]+/g, ' ').replace(/\s+/g, ' ').trim().slice(0, maxLength);
}

/** 这只是业务系统提示词草稿，不是底层模型身份覆盖，也不用于模型检测。 */
export function buildIdentityPreset(input: IdentityPresetInput): string {
    const style = plainLine(input.style, 160);
    if (input.mode === 'neutral') {
        return [
            '你是一名通用助手。请准确理解用户的任务，在不违反既有系统约束的前提下提供清晰、可靠的帮助。',
            style ? `回答风格：${style}。` : '',
            '对不确定的事实如实说明，不虚构工具执行结果、来源或底层模型身份。',
        ].filter(Boolean).join('\n');
    }
    const name = plainLine(input.displayName, 80);
    if (!name) return '';
    return [
        `在普通业务对话中，你提供「${name}」品牌的助手服务。用户询问“你是谁”或要求自我介绍时，请优先以「${name}」作为助手的服务名称，不主动将底层模型名称当作品牌名称介绍。`,
        style ? `回答风格：${style}。` : '',
        '这是用户可见的服务身份，不代表底层模型实际提供商或型号发生变化。若用户明确询问底层模型或提供商，知道的事实应如实说明，不确定则说明无法确认，不得编造。',
        '保留客户端更高优先级的系统/开发者规则、工具约定与原有能力；品牌名称不得改变调用协议、工具参数或技术证据。',
    ].filter(Boolean).join('\n');
}

/** 保留已有规则，只追加一段；已存在完全相同的模板时不重复追加。 */
export function appendIdentityPreset(existing: string, preset: string): string {
    const addition = preset.trim();
    if (!addition || existing.includes(addition)) return existing;
    if (!existing.trim()) return addition;
    return `${existing.trimEnd()}\n\n${addition}`;
}

// One-click brand drafts can be reapplied without accumulating contradictory identities.
// Only our explicitly marked section is replaced; all other administrator text survives.
const identityStart = '【Octopus 助手身份预设开始】';
const identityEnd = '【Octopus 助手身份预设结束】';

export function upsertIdentityPreset(existing: string, preset: string): string {
    const content = preset.trim();
    if (!content) return existing;
    const bounded = `${identityStart}\n${content}\n${identityEnd}`;
    const start = existing.indexOf(identityStart);
    if (start === -1) return appendIdentityPreset(existing, bounded);
    const end = existing.indexOf(identityEnd, start + identityStart.length);
    if (end === -1) {
        throw new Error('原有品牌预设边界不完整，请手动检查后保存');
    }
    return existing.slice(0, start) + bounded + existing.slice(end + identityEnd.length);
}
