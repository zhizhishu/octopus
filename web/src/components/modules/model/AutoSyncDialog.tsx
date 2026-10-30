'use client';

import { useEffect, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslations } from 'next-intl';
import { History, Loader, Lock } from 'lucide-react';
import { Dialog, DialogContent, DialogDescription, DialogTitle } from '@/components/ui/dialog';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Switch } from '@/components/ui/switch';
import { toast } from '@/components/common/Toast';
import { SettingKey, useSetSetting } from '@/api/endpoints/setting';
import type { PriceSyncRun, PriceSyncStatus } from '@/api/endpoints/model';
import { cn } from '@/lib/utils';
import { parseServerTime } from './price-sync';
import { Chip, Hint, useDayTime } from './price-sync-ui';

export function RunSummary({ run }: { run: PriceSyncRun }) {
    const t = useTranslations('model.priceSync');
    if (run.status === 'failed') return <span className="text-destructive">{t('runFailed')}</span>;
    if (run.status === 'aborted') return <span className="text-destructive">{t('runAborted')}</span>;
    if (run.status === 'conflict') return <span className="text-amber-700 dark:text-amber-400">{t('runConflict')}</span>;
    if (!run.added && !run.changed && !run.held) return <span className="text-muted-foreground">{t('summaryNone')}</span>;
    return (
        <span className="flex flex-wrap items-center gap-1.5">
            {run.added > 0 && <Chip tone="new">{t('summaryAdded', { n: run.added })}</Chip>}
            {run.changed > 0 && <Chip tone="changed">{t('summaryChanged', { n: run.changed })}</Chip>}
            {run.held > 0 && <Chip tone="warn">{t('summaryHeld', { n: run.held })}</Chip>}
        </span>
    );
}

export function AutoSyncDialog({
    open,
    onOpenChange,
    status,
}: {
    open: boolean;
    onOpenChange: (open: boolean) => void;
    status: PriceSyncStatus | undefined;
}) {
    const t = useTranslations('model.priceSync.auto');
    const tp = useTranslations('model.priceSync');
    const dayTime = useDayTime();
    const setSetting = useSetSetting();
    const queryClient = useQueryClient();
    const [enabled, setEnabled] = useState(true);
    const [hold, setHold] = useState(true);
    const [interval, setIntervalValue] = useState('24');
    const [saving, setSaving] = useState(false);

    useEffect(() => {
        if (!open || !status) return;
        queueMicrotask(() => {
            setEnabled(status.auto_enabled);
            setHold(status.hold_big_change);
            setIntervalValue(String(status.interval_hours || 24));
        });
    }, [open, status]);

    const save = async () => {
        const hours = Number(interval);
        if (!Number.isInteger(hours) || hours <= 0) {
            toast.error(t('intervalInvalid'));
            return;
        }
        const writes: { key: string; value: string }[] = [];
        if (!status || status.auto_enabled !== enabled) writes.push({ key: SettingKey.PriceAutoSyncEnabled, value: String(enabled) });
        if (!status || status.hold_big_change !== hold) writes.push({ key: SettingKey.PriceSyncHoldBigChange, value: String(hold) });
        if (!status || status.interval_hours !== hours) writes.push({ key: SettingKey.ModelInfoUpdateInterval, value: String(hours) });
        setSaving(true);
        try {
            for (const w of writes) {
                await setSetting.mutateAsync(w);
            }
            await queryClient.invalidateQueries({ queryKey: ['models', 'price-sync-status'] });
            toast.success(t('saved'));
            onOpenChange(false);
        } catch (error) {
            toast.error(t('saveFailed'), { description: (error as Error).message });
        } finally {
            setSaving(false);
        }
    };

    const runs = (status?.runs ?? []).slice(0, 5);

    return (
        <Dialog open={open} onOpenChange={onOpenChange}>
            <DialogContent onOpenAutoFocus={(e) => e.preventDefault()} className="gap-0 rounded-3xl bg-card p-0 sm:max-w-[520px]">
                <div className="px-6 pb-2 pt-5">
                    <DialogTitle className="text-lg font-bold">{t('title')}</DialogTitle>
                    <DialogDescription className="mt-1 text-xs">{t('description')}</DialogDescription>
                </div>
                <div className="space-y-1 px-4 py-3 sm:px-6">
                    <label className="flex cursor-pointer items-center gap-3 rounded-2xl bg-muted/30 px-4 py-3">
                        <div className="min-w-0 flex-1">
                            <div className="flex items-center gap-1.5 text-sm font-medium">
                                {t('enabled')}
                                <Hint content={t('enabledTip')} />
                            </div>
                            <div className="text-xs text-muted-foreground">{t('enabledSub')}</div>
                        </div>
                        <Switch checked={enabled} onCheckedChange={setEnabled} />
                    </label>
                    <div className="flex items-center gap-3 px-4 py-3">
                        <div className="flex-1 text-sm font-medium">{t('interval')}</div>
                        <Input
                            type="number"
                            min={1}
                            step={1}
                            value={interval}
                            onChange={(e) => setIntervalValue(e.target.value)}
                            className="h-9 w-24 rounded-xl tabular-nums"
                        />
                        <span className="text-sm text-muted-foreground">{t('hours')}</span>
                    </div>
                    <div className="flex items-center gap-3 px-4 py-3">
                        <div className="min-w-0 flex-1">
                            <div className="flex items-center gap-1.5 text-sm font-medium">
                                <Lock className="size-3.5" />
                                {t('manualLocked')}
                            </div>
                            <div className="text-xs text-muted-foreground">{t('manualLockedSub')}</div>
                        </div>
                        <Switch checked disabled aria-readonly />
                    </div>
                    <label className={cn('flex cursor-pointer items-center gap-3 px-4 py-3', !enabled && 'opacity-60')}>
                        <div className="min-w-0 flex-1">
                            <div className="flex items-center gap-1.5 text-sm font-medium">
                                {t('hold')}
                                <Hint content={t('holdTip')} />
                            </div>
                            <div className="text-xs text-muted-foreground">{t('holdSub')}</div>
                        </div>
                        <Switch checked={hold} onCheckedChange={setHold} />
                    </label>
                </div>
                <div className="mx-4 rounded-2xl border border-border/70 p-4 sm:mx-6">
                    <div className="mb-2 flex items-center gap-1.5 text-xs font-medium text-muted-foreground">
                        <History className="size-3.5" />
                        {t('history')}
                    </div>
                    {runs.length === 0 ? (
                        <div className="text-sm text-muted-foreground">{t('historyEmpty')}</div>
                    ) : (
                        <div className="space-y-2 text-sm">
                            {runs.map((run) => {
                                const at = parseServerTime(run.at);
                                return (
                                    <div key={run.id} className="flex min-w-0 items-center gap-2">
                                        <span className="w-24 shrink-0 tabular-nums text-muted-foreground">{at ? dayTime(at) : ''}</span>
                                        <div className="min-w-0 flex-1"><RunSummary run={run} /></div>
                                        {run.trigger === 'manual' && <span className="shrink-0 text-xs text-muted-foreground">{tp('runManual')}</span>}
                                    </div>
                                );
                            })}
                        </div>
                    )}
                </div>
                <div className="flex items-center justify-end gap-2 px-6 py-5">
                    <Button variant="outline" className="rounded-xl" onClick={() => onOpenChange(false)}>
                        {t('cancel')}
                    </Button>
                    <Button className="rounded-xl" onClick={save} disabled={saving}>
                        {saving && <Loader className="size-4 animate-spin" />}
                        {t('save')}
                    </Button>
                </div>
            </DialogContent>
        </Dialog>
    );
}
