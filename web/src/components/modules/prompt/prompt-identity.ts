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
        `面向终端用户时，你的助手显示名称为「${name}」。可使用这一名称介绍你的对话角色。`,
        style ? `回答风格：${style}。` : '',
        '这只是用户可见的助手称呼，不代表底层模型的实际提供商或型号。不得伪造真实模型、工具能力、调用记录或技术信息。',
    ].filter(Boolean).join('\n');
}

/** 保留已有规则，只追加一段；已存在完全相同的模板时不重复追加。 */
export function appendIdentityPreset(existing: string, preset: string): string {
    const addition = preset.trim();
    if (!addition || existing.includes(addition)) return existing;
    if (!existing.trim()) return addition;
    return `${existing.trimEnd()}\n\n${addition}`;
}
