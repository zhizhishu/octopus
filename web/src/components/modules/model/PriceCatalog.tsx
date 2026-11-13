'use client';

import { useId, useMemo, useState } from 'react';
import { useTranslations } from 'next-intl';
import { BadgeDollarSign, ChevronDown, Clock, CloudDownload, ExternalLink, Lock, RefreshCw, Settings2, TriangleAlert } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { useModelList, usePriceSyncStatus, useLastUpdateTime } from '@/api/endpoints/model';
import { parseServerTime, sourceCounts, type SyncTab } from './price-sync';
import { Chip, Hint, useDayTime } from './price-sync-ui';
import { PriceSyncDialog } from './PriceSyncDialog';
import { AutoSyncDialog, RunSummary } from './AutoSyncDialog';
import { cn } from '@/lib/utils';

/**
 * Single-row compact catalog (option A). Collapsed: one ~60px line with the
 * title, a short auto-period hint, source chips (xl) and only two actions:
 * update and expand. Expanded: short status, sources and the auto-update
 * setting; long copy lives in Hints. The held banner stays visible collapsed.
 */
export function ModelPriceCatalog() {
    const t = useTranslations('model.priceSync');
    const dayTime = useDayTime();
    const detailsId = useId();
    const { data: models } = useModelList();
    const { data: status } = usePriceSyncStatus();
    const { data: lastFetch } = useLastUpdateTime();
    const [expanded, setExpanded] = useState(false);
    const [previewOpen, setPreviewOpen] = useState(false);
    const [previewTab, setPreviewTab] = useState<SyncTab>('pending');
    const [autoOpen, setAutoOpen] = useState(false);

    // Unknown while loading: never pretend auto is on, never show fake zero counts.
    const counts = useMemo(() => (models ? sourceCounts(models) : null), [models]);
    const autoOn = status ? status.auto_enabled === true : null;
    const lastRun = status?.runs?.[0];
    const lastRunAt = parseServerTime(lastRun?.at);
    const fetchedAt = parseServerTime(status?.last_fetch_at ?? lastFetch);
    const nextRunAt = parseServerTime(status?.next_run_at);
    const held = status?.held_pending ?? [];
    const interval = status?.interval_hours ?? 24;

    const openPreview = (tab: SyncTab) => {
        setPreviewTab(tab);
        setPreviewOpen(true);
    };

    const sourceTip = (
        <div className="space-y-0.5">
            <div>{t('tipLibrary')}</div>
            <div>{t('tipManual')}</div>
            <div>{t('tipNone')}</div>
            <div>{t('tipEdit')}</div>
        </div>
    );

    const chipRow = counts ? (
        <>
            <Chip tone="library">{t('chipLibrary', { n: counts.library })}</Chip>
            <Chip tone="manual"><Lock className="size-3" />{t('chipManual', { n: counts.manual })}</Chip>
            <Chip tone="none">{t('chipNone', { n: counts.none })}</Chip>
        </>
    ) : (
        <span className="text-[11px] text-muted-foreground">{t('unknown')}</span>
    );

    const lastUpdateLabel = lastRunAt
        ? dayTime(lastRunAt)
        : fetchedAt
            ? dayTime(fetchedAt)
            : status == null && lastFetch == null
                ? t('unknown')
                : t('never');
    const autoLabel = status == null ? t('unknown') : autoOn ? t('autoShort', { h: interval }) : t('autoOff');

    const details = (
        <div
            id={detailsId}
            hidden={!expanded}
            className="mt-3 space-y-2 rounded-2xl border border-border/70 bg-muted/20 p-3 text-sm"
        >
            <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted-foreground">
                <span className="inline-flex items-center gap-1 whitespace-nowrap">
                    <Clock className="size-3.5" />
                    {t('lastUpdate')} · {lastUpdateLabel}
                </span>
                <span>
                    {lastRun ? <RunSummary run={lastRun} /> : fetchedAt ? t('fetchedOnly', { time: dayTime(fetchedAt) }) : null}
                </span>
                <span className="inline-flex items-center gap-1 whitespace-nowrap">
                    <RefreshCw className="size-3.5" />
                    {t('autoTile')} · {autoLabel}
                </span>
                {autoOn === true && (
                    <span className="whitespace-nowrap">{nextRunAt ? t('nextRun', { time: dayTime(nextRunAt) }) : t('nextRunSoon')}</span>
                )}
                {autoOn === false && <span>{t('autoOffHint')}</span>}
            </div>
            <div className="flex flex-wrap items-center gap-1.5">
                {chipRow}
                <Hint content={sourceTip} side="bottom" />
                <a
                    href="https://models.dev"
                    target="_blank"
                    rel="noreferrer"
                    className="inline-flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
                >
                    {t('sourceLink')}
                    <ExternalLink className="size-3" />
                </a>
            </div>
            <div>
                <Button variant="outline" className="h-8 rounded-xl px-3 text-xs" onClick={() => setAutoOpen(true)}>
                    <Settings2 className="size-3.5" />
                    {t('autoButton')}
                </Button>
            </div>
        </div>
    );

    return (
        <section className="shrink-0 rounded-3xl border border-border bg-card p-3 sm:px-4 sm:py-3">
            <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5">
                <h2 className="inline-flex shrink-0 items-center gap-2 text-base font-bold text-card-foreground">
                    <BadgeDollarSign className="size-5 shrink-0" />
                    {t('compactTitle')}
                    <Hint content={t('descriptionTip')} />
                </h2>
                <div className="flex min-w-0 flex-1 flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
                    <span className="inline-flex items-center gap-1 whitespace-nowrap max-sm:hidden">
                        <Clock className="size-3.5" />
                        {t('updatedAt', { time: lastUpdateLabel })}
                    </span>
                    <span className="inline-flex items-center gap-1 whitespace-nowrap">
                        <RefreshCw className="size-3.5" />
                        {autoLabel}
                    </span>
                    {!expanded && (
                        <div className="hidden items-center gap-1.5 xl:flex">
                            {chipRow}
                            <Hint content={sourceTip} side="bottom" />
                        </div>
                    )}
                </div>
                <div className="ml-auto flex shrink-0 items-center gap-2">
                    <Button className="h-9 rounded-xl max-sm:h-11" onClick={() => openPreview('pending')}>
                        <CloudDownload className="size-4" />
                        {t('updateButton')}
                    </Button>
                    <Button
                        variant="ghost"
                        className="h-9 rounded-xl px-3 max-sm:size-11 max-sm:px-0"
                        aria-expanded={expanded}
                        aria-controls={detailsId}
                        aria-label={expanded ? t('collapse') : t('expand')}
                        onClick={() => setExpanded((v) => !v)}
                    >
                        <ChevronDown className={cn('size-4 transition-transform', expanded && 'rotate-180')} />
                        <span className="max-sm:hidden">{expanded ? t('collapse') : t('expand')}</span>
                    </Button>
                </div>
            </div>

            {details}

            {held.length > 0 && (
                <div className="mt-3 flex flex-wrap items-center gap-2 rounded-2xl border border-amber-500/30 bg-amber-500/8 px-3 py-2 text-sm">
                    <TriangleAlert className="size-4 shrink-0 text-amber-600" />
                    <Hint content={t('bannerTip', { models: held.join(', ') })}>
                        <span className="min-w-0 flex-1">{t('banner', { n: held.length })}</span>
                    </Hint>
                    <button type="button" className="ml-auto text-sm font-medium text-primary hover:underline" onClick={() => openPreview('changed')}>
                        {t('bannerAction')}
                    </button>
                </div>
            )}

            <PriceSyncDialog open={previewOpen} onOpenChange={setPreviewOpen} initialTab={previewTab} />
            <AutoSyncDialog open={autoOpen} onOpenChange={setAutoOpen} status={status} />
        </section>
    );
}
