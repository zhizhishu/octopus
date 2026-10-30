'use client';

import { CirclePlus, X } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover';
import { cn } from '@/lib/utils';
import { CheckMark, chip, chipOn } from './ui-bits';

// 照 new-api 的 DataTableFacetedFilter：一个虚线胶囊，点开是可多选的清单，选了几项就显示几项。
export function FacetFilter<T extends string | number>({ title, options, selected, onChange, className }: {
    title: string;
    options: { value: T; label: string; count?: number }[];
    selected: T[];
    onChange: (next: T[]) => void;
    className?: string;
}) {
    const t = useTranslations('user.toolbar');
    const toggle = (v: T) => onChange(selected.includes(v) ? selected.filter((x) => x !== v) : [...selected, v]);
    const chosen = options.filter((o) => selected.includes(o.value));
    return (
        <Popover>
            <PopoverTrigger asChild>
                <button type="button" className={cn(chip, selected.length > 0 && chipOn, className)}>
                    <CirclePlus className="size-3.5" />
                    {title}
                    {chosen.length > 0 && (
                        <span className="ml-0.5 max-w-[8rem] truncate border-l border-current/30 pl-1.5">
                            {chosen.length > 2 ? t('selectedN', { n: chosen.length }) : chosen.map((o) => o.label).join('、')}
                        </span>
                    )}
                </button>
            </PopoverTrigger>
            <PopoverContent align="start" className="w-56 p-1">
                <div className="max-h-72 overflow-y-auto">
                    {options.map((o) => (
                        <button
                            key={String(o.value)}
                            type="button"
                            onClick={() => toggle(o.value)}
                            role="menuitemcheckbox"
                            aria-checked={selected.includes(o.value)}
                            className="flex w-full items-center gap-2 rounded-lg px-2 py-1.5 text-left text-sm hover:bg-muted"
                        >
                            <CheckMark checked={selected.includes(o.value)} />
                            <span className="min-w-0 flex-1 truncate">{o.label}</span>
                            {o.count !== undefined && <span className="text-xs tabular-nums text-muted-foreground">{o.count}</span>}
                        </button>
                    ))}
                    {options.length === 0 && <div className="px-2 py-3 text-center text-xs text-muted-foreground">{t('noOptions')}</div>}
                </div>
                {selected.length > 0 && (
                    <>
                        <div className="my-1 h-px bg-border" />
                        <button type="button" onClick={() => onChange([])} className="flex w-full items-center justify-center gap-1 rounded-lg px-2 py-1.5 text-xs text-muted-foreground hover:bg-muted">
                            <X className="size-3.5" />
                            {t('clear')}
                        </button>
                    </>
                )}
            </PopoverContent>
        </Popover>
    );
}
