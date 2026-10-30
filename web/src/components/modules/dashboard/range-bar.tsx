'use client';

import { useMemo, useState } from 'react';
import { CalendarDays, RefreshCw } from 'lucide-react';
import { useTranslations } from 'next-intl';
import type { DateRange as DayRange } from 'react-day-picker';
import { zhCN, zhTW } from 'react-day-picker/locale';
import { useSettingStore } from '@/stores/setting';
import { Calendar } from '@/components/ui/calendar';
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover';
import { cn } from '@/lib/utils';
import { InfoTip, Segmented } from './parts';
import {
    MAX_RANGE_DAYS,
    RANGE_PRESETS,
    addDays,
    isSingleDay,
    quickRanges,
    validateRange,
    type DateRange,
    type Granularity,
    type RangePreset,
} from './model';

function isoToDate(iso: string): Date {
    const [y, m, d] = iso.split('-').map(Number);
    return new Date(y, m - 1, d);
}

function dateToISO(date: Date): string {
    const y = date.getFullYear();
    const m = String(date.getMonth() + 1).padStart(2, '0');
    const d = String(date.getDate()).padStart(2, '0');
    return `${y}-${m}-${d}`;
}

function CustomRangePanel({ today, initial, onApply }: { today: string; initial: DateRange; onApply: (range: DateRange) => void }) {
    const t = useTranslations('dashboard.custom');
    const locale = useSettingStore((state) => state.locale);
    const calendarLocale = locale === 'zh_hans' ? zhCN : locale === 'zh_hant' ? zhTW : undefined;
    const [draft, setDraft] = useState<DateRange>(initial);
    const quick = useMemo(() => quickRanges(today), [today]);
    const problem = validateRange(draft, today);
    const earliest = isoToDate(addDays(today, -365));
    const selected: DayRange = { from: isoToDate(draft.from), to: isoToDate(draft.to) };

    const quickOptions: { key: keyof typeof quick; label: string }[] = [
        { key: 'last14', label: t('last14') },
        { key: 'lastMonth', label: t('lastMonth') },
        { key: 'last30', label: t('last30') },
        { key: 'last90', label: t('last90') },
    ];

    return (
        <div className="space-y-3">
            <div className="grid grid-cols-2 gap-2">
                <label className="space-y-1 text-xs text-muted-foreground">
                    <span>{t('from')}</span>
                    <input
                        type="date"
                        value={draft.from}
                        max={today}
                        onChange={(e) => setDraft((d) => ({ ...d, from: e.target.value }))}
                        className="h-9 w-full rounded-xl border border-input bg-muted/40 px-2.5 text-sm text-foreground"
                    />
                </label>
                <label className="space-y-1 text-xs text-muted-foreground">
                    <span>{t('to')}</span>
                    <input
                        type="date"
                        value={draft.to}
                        max={today}
                        onChange={(e) => setDraft((d) => ({ ...d, to: e.target.value }))}
                        className="h-9 w-full rounded-xl border border-input bg-muted/40 px-2.5 text-sm text-foreground"
                    />
                </label>
            </div>
            <div className="flex flex-wrap gap-1.5">
                {quickOptions.map((option) => {
                    const r = quick[option.key];
                    const active = r.from === draft.from && r.to === draft.to;
                    return (
                        <button
                            key={option.key}
                            type="button"
                            onClick={() => setDraft(r)}
                            className={cn(
                                'rounded-lg border px-2.5 py-1 text-xs transition-colors',
                                active ? 'border-primary/40 bg-primary/10 text-primary' : 'border-border bg-muted/40 text-muted-foreground hover:text-foreground',
                            )}
                        >
                            {option.label}
                        </button>
                    );
                })}
            </div>
            <Calendar
                locale={calendarLocale}
                mode="range"
                numberOfMonths={1}
                selected={selected}
                defaultMonth={isoToDate(draft.to)}
                onSelect={(value) => {
                    if (!value?.from) return;
                    setDraft({ from: dateToISO(value.from), to: dateToISO(value.to ?? value.from) });
                }}
                disabled={(date) => date > isoToDate(today) || date < earliest}
                className="mx-auto p-0"
            />
            <div className="flex items-center justify-between gap-2 border-t border-border pt-3">
                <span className={cn('text-xs', problem ? 'text-destructive' : 'text-muted-foreground')}>
                    {problem ? t(`problem.${problem}`, { days: MAX_RANGE_DAYS }) : t('max', { days: MAX_RANGE_DAYS })}
                </span>
                <button
                    type="button"
                    disabled={!!problem}
                    onClick={() => onApply(draft)}
                    className="rounded-xl bg-primary px-4 py-1.5 text-sm font-medium text-primary-foreground disabled:opacity-40"
                >
                    {t('apply')}
                </button>
            </div>
        </div>
    );
}

export function RangeBar({
    today,
    range,
    preset,
    granularity,
    refreshing,
    onPreset,
    onCustom,
    onGranularity,
    onRefresh,
}: {
    today: string;
    range: DateRange;
    preset: RangePreset;
    granularity: Granularity;
    refreshing: boolean;
    onPreset: (preset: Exclude<RangePreset, 'custom'>) => void;
    onCustom: (range: DateRange) => void;
    onGranularity: (g: Granularity) => void;
    onRefresh: () => void;
}) {
    const t = useTranslations('dashboard');
    const [customOpen, setCustomOpen] = useState(false);
    const oneDay = isSingleDay(range);

    return (
        <div className="flex min-w-0 items-center gap-2 rounded-3xl border border-card-border bg-card p-2 text-card-foreground custom-shadow md:p-3">
            <Segmented
                ariaLabel={t('range.custom')}
                value={preset === 'custom' ? null : preset}
                onChange={(value) => onPreset(value)}
                className="min-w-0 overflow-x-auto"
                options={RANGE_PRESETS.map((p) => ({ value: p, label: t(`range.${p}`) }))}
            />
            <Popover open={customOpen} onOpenChange={setCustomOpen}>
                <PopoverTrigger
                    aria-label={t('range.custom')}
                    className={cn(
                        'inline-flex h-9 shrink-0 items-center gap-1.5 rounded-xl border px-2.5 text-sm font-medium transition-colors md:px-3',
                        preset === 'custom' ? 'border-primary/40 bg-primary/10 text-primary' : 'border-transparent bg-muted text-muted-foreground hover:text-foreground',
                    )}
                >
                    <CalendarDays className="size-4" />
                    <span className="hidden md:inline">{preset === 'custom' ? `${range.from} ~ ${range.to}` : t('range.custom')}</span>
                </PopoverTrigger>
                <PopoverContent align="start" className="w-[min(20rem,calc(100vw-1rem))] rounded-2xl p-3">
                    <CustomRangePanel
                        key={`${range.from}-${range.to}-${customOpen}`}
                        today={today}
                        initial={range}
                        onApply={(r) => {
                            onCustom(r);
                            setCustomOpen(false);
                        }}
                    />
                </PopoverContent>
            </Popover>
            <div className="ml-auto hidden shrink-0 items-center gap-2 md:flex">
                <span className="text-xs text-muted-foreground">{t('granularity.label')}</span>
                <Segmented
                    size="sm"
                    value={granularity}
                    onChange={onGranularity}
                    options={[
                        { value: 'hour', label: t('granularity.hour'), disabled: !oneDay },
                        { value: 'day', label: t('granularity.day') },
                    ]}
                />
                {!oneDay ? <InfoTip>{t('granularity.hourOnlyOneDay')}</InfoTip> : null}
                <button
                    type="button"
                    onClick={onRefresh}
                    aria-label={t('refresh')}
                    title={t('refresh')}
                    className="inline-flex size-9 items-center justify-center rounded-xl border border-border bg-muted/40 text-muted-foreground transition-colors hover:text-foreground"
                >
                    <RefreshCw className={cn('size-4', refreshing && 'animate-spin')} />
                </button>
            </div>
        </div>
    );
}
