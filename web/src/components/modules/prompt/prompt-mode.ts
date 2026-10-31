import type { PromptOverrideMode } from '@/api/endpoints/access-plan';

// 系统提示词每层（全局 / 方案 / 模型路由 / 渠道）= 一个开关 + 一个位置。
//   开关关：这一层什么都不加，请求原样转发（文字保留）。
//   放在前面 / 放在后面：放在请求自带的系统提示词前面或后面；请求没带就直接用这段。
// 旧值 append_system / replace_system 都算「开 + 前面」（后端 migration 026 已改库）。

export type PromptPositionOption = { value: PromptOverrideMode; label: string; hint: string };

export const PROMPT_POSITION_OPTIONS: readonly PromptPositionOption[] = [
    {
        value: 'before',
        label: '放在前面',
        hint: '放在请求自带的系统提示词前面，原内容保留（文字用换行隔开；分块格式单独加一块，Claude Code 自带的计费头和身份块仍排第一）。请求没带就直接用这段。',
    },
    {
        value: 'after',
        label: '放在后面',
        hint: '放在请求自带的系统提示词后面，原内容保留（文字用换行隔开；分块格式在最后加一块）。请求没带就直接用这段。',
    },
];

export const PROMPT_ENABLED_LABEL = '启用';
export const PROMPT_ENABLED_HINT_ON = '已启用：按选的位置把这段加到系统提示词里。';
export const PROMPT_ENABLED_HINT_OFF = '已关闭：这一层什么都不加，请求原样转发；文字会保留，随时可以再打开。';

export function normalizePromptMode(value?: string | null): PromptOverrideMode {
    return value === 'after' ? 'after' : 'before';
}

export function promptPositionOption(value?: string | null): PromptPositionOption {
    const mode = normalizePromptMode(value);
    return PROMPT_POSITION_OPTIONS.find((option) => option.value === mode) ?? PROMPT_POSITION_OPTIONS[0];
}

/** 已保存的开关。旧数据（append_system / replace_system、没有开关字段）有文字就算开着。 */
export function promptEnabledFromSaved(enabled?: boolean | null, mode?: string | null, text?: string | null): boolean {
    if (enabled) return true;
    const legacy = mode === 'append_system' || mode === 'replace_system';
    return legacy && (text ?? '').trim().length > 0;
}

/** 一层是否真的会加东西：开着并且有文字。 */
export function promptLayerActive(enabled?: boolean | null, mode?: string | null, text?: string | null): boolean {
    return promptEnabledFromSaved(enabled, mode, text) && (text ?? '').trim().length > 0;
}

export type PromptStatus = { label: string; tone: 'on' | 'off' | 'empty'; hint: string };

/** 每层状态小标签：已启用 · 放在前面 / 已关闭 / 未填写。 */
export function promptStatus(enabled: boolean, mode?: string | null, text?: string | null): PromptStatus {
    if (!(text ?? '').trim()) return { label: '未填写', tone: 'empty', hint: '没有文字，这一层什么都不加。' };
    if (!enabled) return { label: '已关闭', tone: 'off', hint: PROMPT_ENABLED_HINT_OFF };
    const option = promptPositionOption(mode);
    return { label: `已启用 · ${option.label}`, tone: 'on', hint: option.hint };
}
