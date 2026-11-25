'use client';

import { useMemo, useState } from 'react';
import { Check, ChevronDown, Search } from 'lucide-react';
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover';
import { cn } from '@/lib/utils';

export type MultiPickerOption = {
    value: string;
    label: string;
    /** 搜索时额外匹配的文字（渠道名、编号）。 */
    keywords?: string;
    /** 第二行小字（例如所属渠道）。 */
    hint?: string;
};

/**
 * 多选下拉：搜索 + 勾选。检测页选模型、鹈鹕测试选「渠道 · 模型」都用它。
 *
 * 不会自己清空选择：选中的值不在 options 里（换了渠道）由调用方负责收拾。
 */
export function MultiPicker({
    value,
    onChange,
    options,
    placeholder = '请选择',
    summary,
    searchPlaceholder = '搜索…',
    emptyText = '没有匹配项',
    className,
    disabled,
    max,
    ariaLabel,
}: {
    value: string[];
    onChange: (next: string[]) => void;
    options: MultiPickerOption[];
    placeholder?: string;
    /** 触发按钮上的字；不给就显示「第一个 +N」。 */
    summary?: string;
    searchPlaceholder?: string;
    emptyText?: string;
    className?: string;
    disabled?: boolean;
    /** 最多选几个（超出的勾不上）。 */
    max?: number;
    ariaLabel?: string;
}) {
    const [open, setOpen] = useState(false);
    const [query, setQuery] = useState('');
    const chosen = new Set(value);
    const filtered = useMemo(() => {
        const terms = query.toLowerCase().split(/\s+/).filter(Boolean);
        if (terms.length === 0) return options;
        return options.filter((option) => {
            const hay = `${option.label} ${option.keywords ?? ''} ${option.hint ?? ''}`.toLowerCase();
            return terms.every((term) => hay.includes(term));
        });
    }, [options, query]);

    const firstLabel = options.find((option) => option.value === value[0])?.label ?? value[0];
    const label = summary ?? (value.length === 0 ? placeholder : value.length === 1 ? firstLabel : `${firstLabel} +${value.length - 1}`);
    const full = typeof max === 'number' && value.length >= max;

    const toggle = (option: MultiPickerOption) => {
        if (chosen.has(option.value)) onChange(value.filter((item) => item !== option.value));
        else if (!full) onChange([...value, option.value]);
    };

    return (
        <Popover open={open} onOpenChange={(next) => { setOpen(next); if (!next) setQuery(''); }}>
            <PopoverTrigger asChild>
                <button
                    type="button"
                    disabled={disabled}
                    aria-label={ariaLabel}
                    className={cn(
                        'flex h-10 min-w-0 items-center justify-between gap-2 rounded-xl border border-input bg-background px-3 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-50',
                        className,
                    )}
                >
                    <span className={cn('min-w-0 truncate text-left', value.length === 0 && 'text-muted-foreground')}>{label}</span>
                    <ChevronDown className="size-4 shrink-0 opacity-50" />
                </button>
            </PopoverTrigger>
            <PopoverContent
                align="start"
                sideOffset={6}
                className="w-[max(var(--radix-popover-trigger-width),16rem)] max-w-[calc(100vw-1rem)] overflow-hidden rounded-xl border border-border/60 bg-popover p-0 shadow-xl"
            >
                <div className="flex items-center gap-2 border-b border-border/60 px-3">
                    <Search className="size-4 shrink-0 text-muted-foreground" />
                    <input
                        autoFocus
                        value={query}
                        onChange={(event) => setQuery(event.target.value)}
                        placeholder={searchPlaceholder}
                        className="h-10 w-full min-w-0 bg-transparent text-base outline-none placeholder:text-muted-foreground sm:h-9 sm:text-sm"
                    />
                </div>
                <div className="flex items-center justify-between gap-2 border-b border-border/60 px-3 py-1.5 text-[11px] text-muted-foreground">
                    <span>已选 {value.length}{typeof max === 'number' ? ` / 最多 ${max}` : ''}</span>
                    {value.length > 0 && (
                        <button type="button" className="text-primary hover:underline" onClick={() => onChange([])}>
                            全不选
                        </button>
                    )}
                </div>
                <div className="max-h-[50vh] overflow-y-auto overscroll-contain p-1 sm:max-h-72">
                    {filtered.length === 0 ? (
                        <div className="px-3 py-6 text-center text-sm text-muted-foreground">{emptyText}</div>
                    ) : (
                        filtered.map((option) => {
                            const on = chosen.has(option.value);
                            const blocked = !on && full;
                            return (
                                <button
                                    key={option.value}
                                    type="button"
                                    role="checkbox"
                                    aria-checked={on}
                                    disabled={blocked}
                                    onClick={() => toggle(option)}
                                    className={cn(
                                        'flex w-full min-w-0 items-center gap-2 rounded-lg px-2.5 py-2.5 text-left text-sm hover:bg-accent sm:py-2',
                                        blocked && 'opacity-40',
                                    )}
                                >
                                    <span
                                        className={cn(
                                            'flex size-4 shrink-0 items-center justify-center rounded border',
                                            on ? 'border-primary bg-primary text-primary-foreground' : 'border-border',
                                        )}
                                    >
                                        {on && <Check className="size-3" />}
                                    </span>
                                    <span className="min-w-0 flex-1">
                                        <span className="block truncate">{option.label}</span>
                                        {option.hint && <span className="block truncate text-[11px] text-muted-foreground">{option.hint}</span>}
                                    </span>
                                </button>
                            );
                        })
                    )}
                </div>
            </PopoverContent>
        </Popover>
    );
}
