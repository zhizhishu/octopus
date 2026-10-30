'use client';

import { useMemo, useState } from 'react';
import { useTranslations } from 'next-intl';
import { BadgeDollarSign, Clock, CloudDownload, ExternalLink, Lock, RefreshCw, Settings2, TriangleAlert } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { useModelList, usePriceSyncStatus, useLastUpdateTime } from '@/api/endpoints/model';
import { parseServerTime, sourceCounts, type SyncTab } from './price-sync';
import { Chip, Hint, useDayTime } from './price-sync-ui';
import { PriceSyncDialog } from './PriceSyncDialog';
import { AutoSyncDialog, RunSummary } from './AutoSyncDialog';

export function ModelPriceCatalog() {
    const t = useTranslations('model.priceSync');
    const dayTime = useDayTime();
    const { data: models } = useModelList();
    const { data: status } = usePriceSyncStatus();
    const { data: lastFetch } = useLastUpdateTime();
    const [previewOpen, setPreviewOpen] = useState(false);
    const [previewTab, setPreviewTab] = useState<SyncTab>('pending');
    const [autoOpen, setAutoOpen] = useState(false);

    const counts = useMemo(() => sourceCounts(models ?? []), [models]);
    const lastRun = status?.runs?.[0];
    const lastRunAt = parseServerTime(lastRun?.at);
    const fetchedAt = parseServerTime(status?.last_fetch_at ?? lastFetch);
    const nextRunAt = parseServerTime(status?.next_run_at);
    const held = status?.held_pending ?? [];
    const interval = status?.interval_hours ?? 24;
    const autoOn = status?.auto_enabled ?? true;

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

    const sourceChips = (
        <div className="flex flex-wrap items-center gap-1.5">
            <Chip tone="library">{t('chipLibrary', { n: counts.library })}</Chip>
            <Chip tone="manual"><Lock className="size-3" />{t('chipManual', { n: counts.manual })}</Chip>
            <Chip tone="none">{t('chipNone', { n: counts.none })}</Chip>
        </div>
    );

    const lastUpdateLabel = lastRunAt ? dayTime(lastRunAt) : fetchedAt ? dayTime(fetchedAt) : t('never');

    return (
        <section className="rounded-3xl border border-border bg-card p-4 sm:p-6">
            <div className="flex min-w-0 items-start justify-between gap-3">
                <div className="min-w-0">
                    <h2 className="flex items-center gap-2 text-lg font-bold text-card-foreground">
                        <BadgeDollarSign className="size-5 shrink-0" />
                        {t('title')}
                    </h2>
                    <p className="mt-1 flex items-center gap-1 text-xs leading-5 text-muted-foreground">
                        <span>{t('description')}</span>
                        <Hint content={t('descriptionTip')} />
                    </p>
                </div>
                <a
                    href="https://models.dev"
                    target="_blank"
                    rel="noreferrer"
                    className="inline-flex shrink-0 items-center gap-1.5 rounded-full border border-border bg-muted/30 px-3 py-1 text-xs text-muted-foreground hover:text-foreground max-sm:border-0 max-sm:bg-transparent max-sm:px-0"
                >
                    {t('sourceLink')}
                    <ExternalLink className="size-3 max-sm:hidden" />
                </a>
            </div>

            {/* desktop / tablet tiles */}
            <div className="mt-4 grid gap-3 max-sm:hidden md:grid-cols-3">
                <div className="min-w-0 rounded-2xl border border-border/70 bg-muted/20 px-4 py-3">
                    <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
                        <Clock className="size-3.5" />
                        {t('lastUpdate')}
                    </div>
                    <div className="mt-1 text-sm font-semibold tabular-nums">{lastUpdateLabel}</div>
                    <div className="mt-0.5 text-xs text-muted-foreground">
                        {lastRun ? <RunSummary run={lastRun} /> : fetchedAt ? t('fetchedOnly', { time: dayTime(fetchedAt) }) : null}
                    </div>
                </div>
                <div className="min-w-0 rounded-2xl border border-border/70 bg-muted/20 px-4 py-3">
                    <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
                        <RefreshCw className="size-3.5" />
                        {t('autoTile')}
                    </div>
                    <div className="mt-1 text-sm font-semibold">{autoOn ? t('autoOn', { h: interval }) : t('autoOff')}</div>
                    <div className="mt-0.5 text-xs text-muted-foreground">
                        {autoOn ? (nextRunAt ? t('nextRun', { time: dayTime(nextRunAt) }) : t('nextRunSoon')) : t('autoOffHint')}
                    </div>
                </div>
                <div className="min-w-0 rounded-2xl border border-border/70 bg-muted/20 px-4 py-3">
                    <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
                        <BadgeDollarSign className="size-3.5" />
                        {t('sourceTile')}
                        <Hint content={sourceTip} side="bottom" />
                    </div>
                    <div className="mt-1.5">{sourceChips}</div>
                </div>
            </div>

            {/* phone: one compact line + chips */}
            <div className="mt-3 space-y-2 sm:hidden">
                <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted-foreground">
                    <span className="inline-flex items-center gap-1"><Clock className="size-3.5" />{t('updatedAt', { time: lastUpdateLabel })}</span>
                    <span className="inline-flex items-center gap-1"><RefreshCw className="size-3.5" />{autoOn ? t('autoShort', { h: interval }) : t('autoOff')}</span>
                </div>
                <div className="flex items-center gap-1.5">
                    {sourceChips}
                    <Hint content={sourceTip} side="bottom" />
                </div>
            </div>

            {held.length > 0 && (
                <div className="mt-4 flex flex-wrap items-center gap-2 rounded-2xl border border-amber-500/30 bg-amber-500/8 px-4 py-2.5 text-sm">
                    <TriangleAlert className="size-4 shrink-0 text-amber-600" />
                    <Hint content={t('bannerTip', { models: held.join(', ') })}>
                        <span className="min-w-0 flex-1">
                            <span className="max-sm:hidden">{t('banner', { n: held.length })}</span>
                            <span className="sm:hidden">{t('bannerMobile', { n: held.length })}</span>
                        </span>
                    </Hint>
                    <button type="button" className="ml-auto text-sm font-medium text-primary hover:underline" onClick={() => openPreview('changed')}>
                        {t('bannerAction')}
                    </button>
                </div>
            )}

            <div className="mt-4 flex items-center gap-2">
                <Button className="h-9 rounded-xl max-sm:h-11 max-sm:flex-1" onClick={() => openPreview('pending')}>
                    <CloudDownload className="size-4" />
                    {t('syncButton')}
                </Button>
                <Button variant="outline" className="h-9 rounded-xl max-sm:size-11 max-sm:px-0" onClick={() => setAutoOpen(true)} aria-label={t('autoButton')}>
                    <Settings2 className="size-4" />
                    <span className="max-sm:hidden">{t('autoButton')}</span>
                </Button>
                <span className="ml-1 text-xs text-muted-foreground max-sm:hidden">{t('syncHint')}</span>
            </div>

            <PriceSyncDialog open={previewOpen} onOpenChange={setPreviewOpen} initialTab={previewTab} />
            <AutoSyncDialog open={autoOpen} onOpenChange={setAutoOpen} status={status} />
        </section>
    );
}
