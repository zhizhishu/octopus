'use client';

import { useEffect, useRef, useState } from 'react';
import { useTranslations } from 'next-intl';
import { Clock, Database, DollarSign, ExternalLink, RefreshCw } from 'lucide-react';
import { Input } from '@/components/ui/input';
import { Button } from '@/components/ui/button';
import { useSettingList, useSetSetting, SettingKey } from '@/api/endpoints/setting';
import { useUpdateModelPrice, useLastUpdateTime } from '@/api/endpoints/model';
import { toast } from '@/components/common/Toast';

export function ModelPriceCatalog() {
    const t = useTranslations('setting');
    const { data: settings } = useSettingList();
    const setSetting = useSetSetting();
    const updatePrice = useUpdateModelPrice();
    const lastUpdateQuery = useLastUpdateTime();
    const { data: lastUpdateTime } = lastUpdateQuery;

    const [updateInterval, setUpdateInterval] = useState('');
    const initialUpdateInterval = useRef('');

    useEffect(() => {
        if (!settings) return;

        const interval = settings.find((s) => s.key === SettingKey.ModelInfoUpdateInterval);
        if (!interval) return;

        queueMicrotask(() => setUpdateInterval(interval.value));
        initialUpdateInterval.current = interval.value;
    }, [settings]);

    const handleSave = (key: string, value: string, initialValue: string) => {
        if (value === initialValue) return;

        setSetting.mutate({ key, value }, {
            onSuccess: () => {
                toast.success(t('saved'));
                initialUpdateInterval.current = value;
            },
        });
    };

    const handleManualUpdate = () => {
        updatePrice.mutate(undefined, {
            onSuccess: () => {
                void lastUpdateQuery.refetch();
                toast.success(t('llmPrice.updateSuccess'));
            },
            onError: () => {
                toast.error(t('llmPrice.updateFailed'));
            },
        });
    };

    const formatLastUpdateTime = (timeStr: string | undefined) => {
        if (!timeStr) return t('llmPrice.neverUpdated');

        const date = new Date(timeStr);
        if (date.getFullYear() === 1) return t('llmPrice.neverUpdated');

        return date.toLocaleString();
    };

    return (
        <section className="rounded-3xl border border-border bg-card p-4 sm:p-6">
            <div className="flex min-w-0 flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
                <div className="min-w-0">
                    <h2 className="flex min-w-0 items-center gap-2 text-lg font-bold text-card-foreground">
                        <DollarSign className="h-5 w-5 shrink-0" />
                        <span className="min-w-0 break-words">{t('llmPrice.title')}</span>
                    </h2>
                    <p className="mt-1 text-xs leading-5 text-muted-foreground">{t('llmPrice.description')}</p>
                </div>
                <a
                    href="https://models.dev"
                    target="_blank"
                    rel="noreferrer"
                    className="inline-flex w-fit max-w-full items-center gap-1.5 rounded-full border border-border bg-muted/30 px-3 py-1 text-xs text-muted-foreground hover:text-foreground"
                >
                    <span className="truncate">{t('llmPrice.sourceName')}</span>
                    <ExternalLink className="size-3 shrink-0" />
                </a>
            </div>

            <div className="mt-4 grid gap-3 md:grid-cols-3">
                <div className="min-w-0 rounded-2xl border border-border/70 bg-muted/20 px-4 py-3">
                    <div className="flex min-w-0 items-center gap-2 text-xs text-muted-foreground">
                        <Database className="size-4 shrink-0" />
                        <span className="min-w-0 break-words">{t('llmPrice.source')}</span>
                    </div>
                    <div className="mt-1 min-w-0 break-words text-sm font-semibold">{t('llmPrice.sourceName')}</div>
                </div>
                <div className="min-w-0 rounded-2xl border border-border/70 bg-muted/20 px-4 py-3 md:col-span-2">
                    <div className="flex min-w-0 items-center gap-2 text-xs text-muted-foreground">
                        <Clock className="size-4 shrink-0" />
                        <span className="min-w-0 break-words">{t('llmPrice.lastUpdate')}</span>
                    </div>
                    <div className="mt-1 min-w-0 break-words text-sm font-semibold">
                        {lastUpdateQuery.isFetching ? t('llmPrice.checking') : formatLastUpdateTime(lastUpdateTime)}
                    </div>
                </div>
            </div>

            <div className="mt-4 flex min-w-0 flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
                <div className="flex min-w-0 items-center gap-3">
                    <Clock className="h-5 w-5 shrink-0 text-muted-foreground" />
                    <span className="min-w-0 break-words text-sm font-medium">{t('llmPrice.updateInterval.label')}</span>
                </div>
                <Input
                    type="number"
                    value={updateInterval}
                    onChange={(e) => setUpdateInterval(e.target.value)}
                    onBlur={() => handleSave(SettingKey.ModelInfoUpdateInterval, updateInterval, initialUpdateInterval.current)}
                    placeholder={t('llmPrice.updateInterval.placeholder')}
                    className="w-full rounded-xl sm:w-48"
                />
            </div>

            <div className="mt-4 flex min-w-0 flex-col gap-3 rounded-2xl border border-border/70 bg-muted/20 p-4 sm:flex-row sm:items-center sm:justify-between">
                <div className="flex min-w-0 flex-col gap-1">
                    <div className="flex min-w-0 items-center gap-3">
                        <RefreshCw className="h-5 w-5 shrink-0 text-muted-foreground" />
                        <span className="min-w-0 break-words text-sm font-medium">{t('llmPrice.manualUpdate.label')}</span>
                    </div>
                    <span className="min-w-0 break-words text-xs leading-5 text-muted-foreground sm:ml-8">
                        {t('llmPrice.manualUpdate.hint')}
                    </span>
                </div>
                <Button
                    variant="outline"
                    size="sm"
                    onClick={handleManualUpdate}
                    disabled={updatePrice.isPending}
                    className="w-full rounded-xl sm:w-auto"
                >
                    {updatePrice.isPending ? (
                        <>
                            <RefreshCw className="size-4 animate-spin" />
                            {t('llmPrice.manualUpdate.updating')}
                        </>
                    ) : (
                        <>
                            <RefreshCw className="size-4" />
                            {t('llmPrice.manualUpdate.button')}
                        </>
                    )}
                </Button>
            </div>
        </section>
    );
}
