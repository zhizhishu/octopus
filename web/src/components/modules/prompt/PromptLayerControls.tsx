'use client';

import type { PromptOverrideMode } from '@/api/endpoints/access-plan';
import { Switch } from '@/components/ui/switch';
import { cn } from '@/lib/utils';
import {
    PROMPT_ENABLED_HINT_OFF,
    PROMPT_ENABLED_HINT_ON,
    PROMPT_ENABLED_LABEL,
    PROMPT_POSITION_OPTIONS,
    normalizePromptMode,
} from './prompt-mode';

export type PromptLayerText = {
    enabled: string;
    enabledHintOn: string;
    enabledHintOff: string;
    before: string;
    beforeHint: string;
    after: string;
    afterHint: string;
    position: string;
};

const DEFAULT_TEXT: PromptLayerText = {
    enabled: PROMPT_ENABLED_LABEL,
    enabledHintOn: PROMPT_ENABLED_HINT_ON,
    enabledHintOff: PROMPT_ENABLED_HINT_OFF,
    before: PROMPT_POSITION_OPTIONS[0].label,
    beforeHint: PROMPT_POSITION_OPTIONS[0].hint,
    after: PROMPT_POSITION_OPTIONS[1].label,
    afterHint: PROMPT_POSITION_OPTIONS[1].hint,
    position: '位置',
};

/** 一层系统提示词的「启用」开关 + 「放在前面 / 放在后面」二选一。关掉时位置变灰，但还能先选好。 */
export function PromptLayerControls({
    enabled,
    onEnabledChange,
    mode,
    onModeChange,
    label,
    className,
    text = DEFAULT_TEXT,
}: {
    enabled: boolean;
    onEnabledChange: (value: boolean) => void;
    mode: PromptOverrideMode | string | undefined | null;
    onModeChange: (value: PromptOverrideMode) => void;
    label: string;
    className?: string;
    /** 渠道表单走 i18n，传自己的文案；其余地方用默认中文。 */
    text?: PromptLayerText;
}) {
    const current = normalizePromptMode(mode);
    const options = PROMPT_POSITION_OPTIONS.map((option) =>
        option.value === 'before'
            ? { ...option, label: text.before, hint: text.beforeHint }
            : { ...option, label: text.after, hint: text.afterHint }
    );
    return (
        <div className={cn('flex flex-wrap items-center gap-2', className)}>
            <label
                className="inline-flex cursor-pointer items-center gap-1.5 text-xs text-muted-foreground"
                title={enabled ? text.enabledHintOn : text.enabledHintOff}
            >
                <Switch checked={enabled} onCheckedChange={onEnabledChange} aria-label={`${label} ${text.enabled}`} />
                {text.enabled}
            </label>
            <div
                role="radiogroup"
                aria-label={`${label} ${text.position}`}
                className={cn(
                    'inline-flex h-8 items-center rounded-xl border border-input bg-muted/40 p-0.5 text-xs',
                    !enabled && 'opacity-50'
                )}
            >
                {options.map((option) => (
                    <button
                        key={option.value}
                        type="button"
                        role="radio"
                        aria-checked={current === option.value}
                        title={option.hint}
                        onClick={() => onModeChange(option.value)}
                        className={cn(
                            'h-7 rounded-lg px-2.5 transition-colors',
                            current === option.value
                                ? 'bg-background font-medium text-foreground shadow-sm'
                                : 'text-muted-foreground hover:text-foreground'
                        )}
                    >
                        {option.label}
                    </button>
                ))}
            </div>
        </div>
    );
}
