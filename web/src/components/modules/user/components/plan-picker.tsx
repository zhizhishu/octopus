'use client';

import { Check, ChevronDown } from 'lucide-react';
import { useTranslations } from 'next-intl';
import type { AccessPlan } from '@/api/endpoints/access-plan';
import { cn } from '@/lib/utils';
import { planLabel } from './shared';

// 能用的方案（点选）+ 默认用哪个。至少要留一个；默认必须在选中的里面。
export function PlanPicker({ plans, value, defaultID, onChange }: {
    plans: AccessPlan[];
    value: number[];
    defaultID?: number;
    onChange: (ids: number[], defaultID: number | undefined) => void;
}) {
    const t = useTranslations('user.plans');
    const toggle = (id: number) => {
        const next = value.includes(id) ? value.filter((x) => x !== id) : [...value, id];
        onChange(next, next.includes(defaultID ?? -1) ? defaultID : next[0]);
    };
    const effectiveDefault = defaultID && value.includes(defaultID) ? defaultID : value[0];
    if (plans.length === 0) return <div className="rounded-xl border border-dashed border-border px-3 py-4 text-center text-xs text-muted-foreground">{t('none')}</div>;
    return (
        <div>
            <div className="flex flex-wrap gap-2">
                {plans.map((p) => {
                    const on = value.includes(p.id);
                    return (
                        <button
                            key={p.id}
                            type="button"
                            aria-pressed={on}
                            title={p.slug}
                            onClick={() => toggle(p.id)}
                            className={cn(
                                'inline-flex max-w-[11rem] items-center gap-1 rounded-full border px-2.5 py-1 text-xs font-medium',
                                on ? 'border-transparent bg-primary text-primary-foreground' : 'border-border text-muted-foreground hover:text-foreground'
                            )}
                        >
                            {on && <Check className="size-3 shrink-0" />}
                            <span className="truncate">{planLabel(p)}</span>
                        </button>
                    );
                })}
            </div>
            {value.length === 0 && <div className="mt-2 text-xs text-destructive">{t('pickOne')}</div>}
            {value.length > 0 && (
                <div className="mt-3 grid grid-cols-[auto_1fr] items-center gap-3">
                    <div className="text-xs text-muted-foreground">{t('default')}</div>
                    <div className="relative">
                        <select
                            value={effectiveDefault ?? ''}
                            onChange={(e) => onChange(value, Number(e.target.value) || undefined)}
                            className="h-9 w-full appearance-none rounded-xl border border-input bg-background px-3 pr-8 text-sm outline-none focus:border-ring"
                        >
                            {plans.filter((p) => value.includes(p.id)).map((p) => <option key={p.id} value={p.id}>{planLabel(p)}</option>)}
                        </select>
                        <ChevronDown className="pointer-events-none absolute top-1/2 right-3 size-4 -translate-y-1/2 text-muted-foreground" />
                    </div>
                </div>
            )}
        </div>
    );
}
