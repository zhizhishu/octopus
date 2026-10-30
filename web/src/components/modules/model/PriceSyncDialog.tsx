'use client';

import { useCallback, useEffect, useMemo, useState } from 'react';
import { useTranslations } from 'next-intl';
import { Check, Info, Loader, Lock, Minus, RefreshCw, Search, TriangleAlert } from 'lucide-react';
import { Dialog, DialogContent, DialogDescription, DialogTitle } from '@/components/ui/dialog';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { toast } from '@/components/common/Toast';
import { usePriceSyncApply, usePriceSyncPreview, type PriceSyncPreview } from '@/api/endpoints/model';
import { cn } from '@/lib/utils';
import {
    PRICE_FIELDS,
    biggestRatio,
    defaultSelected,
    formatPrice,
    itemsForTab,
    parseServerTime,
    percentChange,
    tabCounts,
    toApplyChanges,
    unselectedSummary,
    type PriceField,
    type SyncItem,
    type SyncTab,
} from './price-sync';
import { Chip, Hint, ModelLogo, useDayTime } from './price-sync-ui';

const TABS: SyncTab[] = ['pending', 'new', 'changed', 'manual', 'same'];

const TAB_LABEL: Record<SyncTab, string> = {
    pending: 'tabPending',
    new: 'tabNew',
    changed: 'tabChanged',
    manual: 'tabManual',
    same: 'tabSame',
};

const FIELD_LABEL: Record<PriceField, string> = {
    input: 'colInput',
    output: 'colOutput',
    cache_read: 'colCacheRead',
    cache_write: 'colCacheWrite',
};

function CheckBox({ state, onClick, disabled, label }: { state: 'on' | 'off' | 'some'; onClick?: () => void; disabled?: boolean; label: string }) {
    return (
        <button
            type="button"
            role="checkbox"
            aria-checked={state === 'some' ? 'mixed' : state === 'on'}
            aria-label={label}
            disabled={disabled}
            onClick={onClick}
            className={cn(
                'flex size-4 shrink-0 items-center justify-center rounded-[5px] border transition-colors disabled:cursor-not-allowed disabled:opacity-40',
                state === 'off' ? 'border-border bg-background' : 'border-primary bg-primary text-primary-foreground',
            )}
        >
            {state === 'on' && <Check className="size-3" strokeWidth={3} />}
            {state === 'some' && <Minus className="size-3" strokeWidth={3} />}
        </button>
    );
}

function PriceDelta({ item, field, align = 'left' }: { item: SyncItem; field: PriceField; align?: 'left' | 'right' }) {
    const t = useTranslations('model.priceSync.preview');
    const from = item.current[field];
    const to = item.next[field];
    if (item.kind === 'same' || from === to) {
        return <span className="tabular-nums text-muted-foreground">{formatPrice(to)}</span>;
    }
    if (item.kind === 'new') {
        return <span className="tabular-nums font-semibold text-foreground">{formatPrice(to)}</span>;
    }
    const pct = percentChange(from, to);
    return (
        <span className={cn('inline-flex flex-col leading-tight', align === 'right' ? 'items-end' : 'items-start')}>
            <span className="whitespace-nowrap tabular-nums">
                <span className="text-muted-foreground line-through decoration-muted-foreground/50">{formatPrice(from)}</span>
                <span className="mx-1 text-muted-foreground">→</span>
                <span className="font-semibold text-foreground">{formatPrice(to)}</span>
            </span>
            {align === 'left' && (
                <span className={cn('text-[11px] font-medium', pct === null || pct > 0 ? 'text-rose-600' : 'text-emerald-600')}>
                    {pct === null ? t('wasEmpty') : `${pct > 0 ? '+' : ''}${pct}%`}
                </span>
            )}
        </span>
    );
}

function ItemBadges({ item }: { item: SyncItem }) {
    const t = useTranslations('model.priceSync.preview');
    const manualPrice = `${item.current.input.toFixed(2)} / ${item.current.output.toFixed(2)}`;
    const ratio = biggestRatio(item.current, item.next);
    const bigText = ratio > 0
        ? t('bigUp', { x: String(Number(ratio.toFixed(1))) })
        : t('bigDown', { pct: Math.round(-ratio * 100) });
    return (
        <div className="min-w-0">
            <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
                <span className="min-w-0 truncate font-medium">{item.name}</span>
                {item.kind === 'new' && (
                    <Hint content={item.exists ? t('newTip') : t('newRowTip')}>
                        <span><Chip tone="new">{t('chipNew')}</Chip></span>
                    </Hint>
                )}
                {item.kind === 'changed' && !item.big && <Chip tone="changed">{t('chipChanged')}</Chip>}
                {item.kind === 'changed' && item.big && (
                    <Hint content={t('bigTip')}>
                        <span><Chip tone="warn"><TriangleAlert className="size-3" />{t('chipBig')}</Chip></span>
                    </Hint>
                )}
                {item.kind === 'manual' && (
                    <Hint content={t('manualTip', { price: manualPrice })}>
                        <span><Chip tone="manual"><Lock className="size-3" />{t('chipManual')}</Chip></span>
                    </Hint>
                )}
                {item.has_per_call && (
                    <Hint content={t('perCallKept')}>
                        <span className="inline-flex text-muted-foreground"><Info className="size-3.5" /></span>
                    </Hint>
                )}
            </div>
            {item.kind === 'manual' && <div className="mt-0.5 text-xs text-muted-foreground">{t('manualSub')}</div>}
            {item.kind === 'changed' && item.big && <div className="mt-0.5 text-xs text-amber-700 dark:text-amber-400">{bigText}</div>}
        </div>
    );
}

export function PriceSyncDialog({
    open,
    onOpenChange,
    initialTab = 'pending',
}: {
    open: boolean;
    onOpenChange: (open: boolean) => void;
    initialTab?: SyncTab;
}) {
    const t = useTranslations('model.priceSync.preview');
    const dayTime = useDayTime();
    const previewMutation = usePriceSyncPreview();
    const applyMutation = usePriceSyncApply();
    const [preview, setPreview] = useState<PriceSyncPreview | null>(null);
    const [selected, setSelected] = useState<Set<string>>(new Set());
    const [tab, setTab] = useState<SyncTab>(initialTab);
    const [search, setSearch] = useState('');
    const { mutate: runPreview } = previewMutation;

    const load = useCallback(() => {
        runPreview(undefined, {
            onSuccess: (data) => {
                const items = data.items ?? [];
                setPreview({ ...data, items });
                setSelected(defaultSelected(items));
            },
        });
    }, [runPreview]);

    useEffect(() => {
        if (!open) return;
        queueMicrotask(() => {
            setTab(initialTab);
            setSearch('');
            setPreview(null);
        });
        load();
    }, [open, initialTab, load]);

    const items = useMemo(() => preview?.items ?? [], [preview]);
    const counts = useMemo(() => tabCounts(items), [items]);
    const visible = useMemo(() => itemsForTab(items, tab, search), [items, tab, search]);
    const selectable = useMemo(() => visible.filter((it) => it.kind !== 'same'), [visible]);
    const selectedCount = useMemo(() => items.filter((it) => it.kind !== 'same' && selected.has(it.name)).length, [items, selected]);
    const left = useMemo(() => unselectedSummary(items, selected), [items, selected]);
    const pageState: 'on' | 'off' | 'some' = selectable.length === 0
        ? 'off'
        : selectable.every((it) => selected.has(it.name)) ? 'on' : selectable.some((it) => selected.has(it.name)) ? 'some' : 'off';

    const toggle = (name: string) => {
        setSelected((prev) => {
            const next = new Set(prev);
            if (next.has(name)) next.delete(name);
            else next.add(name);
            return next;
        });
    };
    const togglePage = () => {
        setSelected((prev) => {
            const next = new Set(prev);
            if (pageState === 'on') selectable.forEach((it) => next.delete(it.name));
            else selectable.forEach((it) => next.add(it.name));
            return next;
        });
    };

    const apply = () => {
        const changes = toApplyChanges(items, selected);
        if (changes.length === 0) return;
        applyMutation.mutate(changes, {
            onSuccess: (res) => {
                toast.success(t('toastApplied', { n: res.added + res.changed }));
                onOpenChange(false);
            },
            onError: (error) => {
                const code = (error as { code?: number }).code;
                if (code === 409) {
                    toast.warning(t('toastConflict'));
                    load();
                    return;
                }
                toast.error(t('toastFailed'), { description: error.message });
            },
        });
    };

    const fetchedAt = parseServerTime(preview?.fetched_at);
    const fetchedLabel = !fetchedAt || Date.now() - fetchedAt.getTime() < 60_000 ? t('justNow') : `${dayTime(fetchedAt)} `;

    return (
        <Dialog open={open} onOpenChange={onOpenChange}>
            <DialogContent
                showCloseButton
                // Don't land focus on a hint icon: that would pop its tooltip open on every open.
                onOpenAutoFocus={(e) => e.preventDefault()}
                className={cn(
                    'flex flex-col gap-0 overflow-hidden rounded-3xl bg-card p-0',
                    'sm:max-h-[86vh] sm:w-[min(980px,calc(100vw-2rem))] sm:max-w-[980px]',
                    'max-sm:top-auto max-sm:bottom-0 max-sm:max-h-[92dvh] max-sm:w-full max-sm:max-w-full max-sm:translate-y-0 max-sm:rounded-b-none',
                )}
            >
                <div className="mx-auto mt-2 h-1 w-10 rounded-full bg-muted sm:hidden" />
                <div className="border-b border-border/70 px-5 pb-4 pt-4 sm:px-6 sm:pt-5">
                    <DialogTitle className="text-lg font-bold">{t('title')}</DialogTitle>
                    <DialogDescription asChild>
                        <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-muted-foreground">
                            <span>{t('fetched', { time: fetchedLabel })}</span>
                            {preview && (
                                <>
                                    <span className="max-sm:hidden">·</span>
                                    <span className="inline-flex items-center gap-1 max-sm:hidden">
                                        {t('scope', { n: preview.total })}
                                        <Hint content={t('scopeTip')} />
                                    </span>
                                </>
                            )}
                            <span>·</span>
                            <span className="inline-flex items-center gap-1">
                                {t('unit')}
                                <Hint content={t('unitTip')} />
                            </span>
                        </div>
                    </DialogDescription>
                </div>

                {previewMutation.isPending && !preview ? (
                    <div className="flex min-h-48 flex-1 items-center justify-center gap-2 text-sm text-muted-foreground">
                        <Loader className="size-4 animate-spin" />
                        {t('loading')}
                    </div>
                ) : previewMutation.isError && !preview ? (
                    <div className="flex min-h-48 flex-1 flex-col items-center justify-center gap-3 px-6 text-center text-sm">
                        <span className="text-destructive">{t('failed', { msg: previewMutation.error?.message ?? '' })}</span>
                        <Button variant="outline" size="sm" className="rounded-xl" onClick={load}>
                            <RefreshCw className="size-4" />
                            {t('retry')}
                        </Button>
                    </div>
                ) : (
                    <>
                        <div className="flex flex-wrap items-center justify-between gap-3 px-4 pt-4 sm:px-6">
                            <div className="inline-flex max-w-full gap-1 overflow-x-auto rounded-2xl bg-muted/50 p-1">
                                {TABS.map((key) => (
                                    <button
                                        key={key}
                                        type="button"
                                        onClick={() => setTab(key)}
                                        className={cn(
                                            'inline-flex shrink-0 items-center gap-1.5 whitespace-nowrap rounded-xl px-3 py-1.5 text-sm text-muted-foreground transition-colors',
                                            tab === key && 'bg-background text-foreground shadow-sm',
                                            key === 'same' && 'max-sm:hidden',
                                        )}
                                    >
                                        {key === 'manual' && <Lock className="size-3.5" />}
                                        {key === 'manual' ? (
                                            <>
                                                <span className="max-sm:hidden">{t(TAB_LABEL[key])}</span>
                                                <span className="sm:hidden">{t('tabManualShort')}</span>
                                            </>
                                        ) : t(TAB_LABEL[key])}
                                        <b className="text-xs font-semibold tabular-nums opacity-70">{counts[key]}</b>
                                    </button>
                                ))}
                            </div>
                            <div className="relative w-56 max-sm:hidden">
                                <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                                <Input value={search} onChange={(e) => setSearch(e.target.value)} placeholder={t('search')} className="h-9 rounded-xl pl-9" />
                            </div>
                        </div>

                        <div className="min-h-0 flex-1 overflow-y-auto px-4 py-4 sm:px-6">
                            {visible.length === 0 ? (
                                <div className="py-12 text-center text-sm text-muted-foreground">{t('empty')}</div>
                            ) : (
                                <>
                                    {/* desktop: table */}
                                    <div className="overflow-hidden rounded-2xl border border-border max-sm:hidden">
                                        <table className="w-full table-fixed">
                                            <thead className="bg-muted/40">
                                                <tr>
                                                    <th className="w-10 px-3 py-2.5 text-left">
                                                        <CheckBox state={pageState} onClick={togglePage} disabled={selectable.length === 0} label={t('selectAll')} />
                                                    </th>
                                                    <th className="w-[34%] px-3 py-2.5 text-left text-xs font-medium text-muted-foreground">{t('colModel')}</th>
                                                    {PRICE_FIELDS.map((f) => (
                                                        <th key={f} className="px-3 py-2.5 text-left text-xs font-medium text-muted-foreground">{t(FIELD_LABEL[f])}</th>
                                                    ))}
                                                </tr>
                                            </thead>
                                            <tbody>
                                                {visible.map((it) => {
                                                    const dim = it.kind === 'manual' || it.big;
                                                    return (
                                                        <tr key={it.name} className={cn('border-t border-border/60', dim && 'bg-muted/25')}>
                                                            <td className="px-3 py-2.5 align-middle">
                                                                <CheckBox
                                                                    state={selected.has(it.name) ? 'on' : 'off'}
                                                                    onClick={() => toggle(it.name)}
                                                                    disabled={it.kind === 'same'}
                                                                    label={it.name}
                                                                />
                                                            </td>
                                                            <td className="px-3 py-2.5 align-middle text-sm">
                                                                <div className="flex min-w-0 items-center gap-2.5">
                                                                    <ModelLogo name={it.name} size={28} />
                                                                    <ItemBadges item={it} />
                                                                </div>
                                                            </td>
                                                            {PRICE_FIELDS.map((f) => (
                                                                <td key={f} className="px-3 py-2.5 align-middle text-sm">
                                                                    <PriceDelta item={it} field={f} />
                                                                </td>
                                                            ))}
                                                        </tr>
                                                    );
                                                })}
                                            </tbody>
                                        </table>
                                    </div>

                                    {/* mobile: cards */}
                                    <div className="space-y-3 sm:hidden">
                                        {visible.map((it) => (
                                            <div
                                                key={it.name}
                                                className={cn('rounded-2xl border border-border p-4', (it.kind === 'manual' || it.big) && 'bg-muted/25')}
                                            >
                                                <div className="flex min-w-0 items-start gap-2.5">
                                                    <div className="pt-1.5">
                                                        <CheckBox
                                                            state={selected.has(it.name) ? 'on' : 'off'}
                                                            onClick={() => toggle(it.name)}
                                                            disabled={it.kind === 'same'}
                                                            label={it.name}
                                                        />
                                                    </div>
                                                    <ModelLogo name={it.name} size={28} />
                                                    <div className="min-w-0 flex-1 pt-0.5 text-[15px]"><ItemBadges item={it} /></div>
                                                </div>
                                                <div className="mt-3 grid grid-cols-2 gap-x-4 gap-y-2 text-sm">
                                                    {PRICE_FIELDS.map((f) => (
                                                        <div key={f} className="flex min-w-0 items-center justify-between gap-2">
                                                            <span className="text-muted-foreground">{t(FIELD_LABEL[f])}</span>
                                                            <PriceDelta item={it} field={f} align="right" />
                                                        </div>
                                                    ))}
                                                </div>
                                            </div>
                                        ))}
                                    </div>
                                </>
                            )}
                            <p className="mt-3 flex flex-wrap items-center gap-x-1.5 gap-y-1 text-xs text-muted-foreground">
                                <Info className="size-3.5 shrink-0" />
                                <span>{t('footNote')}</span>
                                {preview && preview.no_library > 0 && <span>{t('noLibrary', { n: preview.no_library })}</span>}
                            </p>
                        </div>
                    </>
                )}

                <div className="flex items-center gap-3 border-t border-border/70 px-5 py-4 sm:px-6">
                    <div className="min-w-0 flex-1">
                        <span className="text-sm">{t('selected', { n: selectedCount })}</span>
                        {(left.manual > 0 || left.big > 0) && (
                            <span className="ml-2 text-xs text-muted-foreground max-sm:hidden">{t('unselected', left)}</span>
                        )}
                    </div>
                    <Button variant="outline" className="rounded-xl" onClick={() => onOpenChange(false)}>
                        {t('cancel')}
                    </Button>
                    <Button className="rounded-xl" onClick={apply} disabled={selectedCount === 0 || applyMutation.isPending || previewMutation.isPending}>
                        {applyMutation.isPending ? <Loader className="size-4 animate-spin" /> : <Check className="size-4" />}
                        {applyMutation.isPending ? t('applying') : (
                            <>
                                <span className="max-sm:hidden">{t('apply', { n: selectedCount })}</span>
                                <span className="sm:hidden">{t('applyShort', { n: selectedCount })}</span>
                            </>
                        )}
                    </Button>
                </div>
            </DialogContent>
        </Dialog>
    );
}
