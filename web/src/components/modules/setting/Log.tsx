'use client';

import { useEffect, useState, useRef } from 'react';
import { useTranslations } from 'next-intl';
import { ScrollText, Calendar, Trash2, HardDrive } from 'lucide-react';
import { Input } from '@/components/ui/input';
import { Switch } from '@/components/ui/switch';
import { Button } from '@/components/ui/button';
import { Progress } from '@/components/ui/progress';
import { useSettingList, useSetSetting, SettingKey } from '@/api/endpoints/setting';
import { useClearLogs, useLogStorage } from '@/api/endpoints/log';
import { toast } from '@/components/common/Toast';

function formatBytes(bytes: number): string {
    if (!Number.isFinite(bytes) || bytes <= 0) return '0 B';
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    let value = bytes;
    let unitIndex = 0;
    while (value >= 1024 && unitIndex < units.length - 1) {
        value /= 1024;
        unitIndex += 1;
    }
    const digits = value >= 100 || unitIndex === 0 ? 0 : value >= 10 ? 1 : 2;
    return `${value.toFixed(digits)} ${units[unitIndex]}`;
}

export function SettingLog() {
    const t = useTranslations('setting');
    const { data: settings } = useSettingList();
    const { data: storage } = useLogStorage();
    const setSetting = useSetSetting();
    const clearLogs = useClearLogs();

    const [enabled, setEnabled] = useState(true);
    const [keepPeriod, setKeepPeriod] = useState('7');
    const [maxStorageGB, setMaxStorageGB] = useState('0');
    const [interventionEnabled, setInterventionEnabled] = useState(false);
    const [interventionTimeout, setInterventionTimeout] = useState('1800');
    const [noBreakerRetryBudget, setNoBreakerRetryBudget] = useState('300');
    const [isClearing, setIsClearing] = useState(false);

    const initialEnabled = useRef(true);
    const initialKeepPeriod = useRef('7');
    const initialMaxStorageGB = useRef('0');
    const initialInterventionEnabled = useRef(false);
    const initialInterventionTimeout = useRef('1800');
    const initialNoBreakerRetryBudget = useRef('300');

    useEffect(() => {
        if (settings) {
            const enabledSetting = settings.find(s => s.key === SettingKey.RelayLogKeepEnabled);
            const periodSetting = settings.find(s => s.key === SettingKey.RelayLogKeepPeriod);
            const maxStorageSetting = settings.find(s => s.key === SettingKey.RelayLogMaxStorageGB);
            const interventionEnabledSetting = settings.find(s => s.key === SettingKey.RelayInterventionEnabled);
            const interventionTimeoutSetting = settings.find(s => s.key === SettingKey.RelayInterventionTimeoutSeconds);
            const noBreakerRetryBudgetSetting = settings.find(s => s.key === SettingKey.RelayNoBreakerRetryBudgetSeconds);
            if (enabledSetting) {
                const isEnabled = enabledSetting.value === 'true';
                queueMicrotask(() => setEnabled(isEnabled));
                initialEnabled.current = isEnabled;
            }
            if (periodSetting) {
                queueMicrotask(() => setKeepPeriod(periodSetting.value));
                initialKeepPeriod.current = periodSetting.value;
            }
            if (maxStorageSetting) {
                queueMicrotask(() => setMaxStorageGB(maxStorageSetting.value));
                initialMaxStorageGB.current = maxStorageSetting.value;
            }
            if (interventionEnabledSetting) {
                const isInterventionEnabled = interventionEnabledSetting.value === 'true';
                queueMicrotask(() => setInterventionEnabled(isInterventionEnabled));
                initialInterventionEnabled.current = isInterventionEnabled;
            }
            if (interventionTimeoutSetting) {
                queueMicrotask(() => setInterventionTimeout(interventionTimeoutSetting.value));
                initialInterventionTimeout.current = interventionTimeoutSetting.value;
            }
            if (noBreakerRetryBudgetSetting) {
                queueMicrotask(() => setNoBreakerRetryBudget(noBreakerRetryBudgetSetting.value));
                initialNoBreakerRetryBudget.current = noBreakerRetryBudgetSetting.value;
            }
        }
    }, [settings]);

    const handleEnabledChange = (checked: boolean) => {
        setEnabled(checked);
        setSetting.mutate(
            { key: SettingKey.RelayLogKeepEnabled, value: checked ? 'true' : 'false' },
            {
                onSuccess: () => {
                    toast.success(t('saved'));
                    initialEnabled.current = checked;
                }
            }
        );
    };

    const handleKeepPeriodSave = () => {
        if (keepPeriod === initialKeepPeriod.current) return;

        setSetting.mutate(
            { key: SettingKey.RelayLogKeepPeriod, value: keepPeriod },
            {
                onSuccess: () => {
                    toast.success(t('saved'));
                    initialKeepPeriod.current = keepPeriod;
                }
            }
        );
    };

    const handleMaxStorageSave = () => {
        if (maxStorageGB === initialMaxStorageGB.current) return;

        setSetting.mutate(
            { key: SettingKey.RelayLogMaxStorageGB, value: maxStorageGB },
            {
                onSuccess: () => {
                    toast.success(t('saved'));
                    initialMaxStorageGB.current = maxStorageGB;
                }
            }
        );
    };

    const handleInterventionEnabledChange = (checked: boolean) => {
        setInterventionEnabled(checked);
        setSetting.mutate(
            { key: SettingKey.RelayInterventionEnabled, value: checked ? 'true' : 'false' },
            {
                onSuccess: () => {
                    toast.success(t('saved'));
                    initialInterventionEnabled.current = checked;
                }
            }
        );
    };

    const handleInterventionTimeoutSave = () => {
        if (interventionTimeout === initialInterventionTimeout.current) return;

        setSetting.mutate(
            { key: SettingKey.RelayInterventionTimeoutSeconds, value: interventionTimeout },
            {
                onSuccess: () => {
                    toast.success(t('saved'));
                    initialInterventionTimeout.current = interventionTimeout;
                }
            }
        );
    };

    const handleNoBreakerRetryBudgetSave = () => {
        const parsed = Number.parseInt(noBreakerRetryBudget, 10);
        const normalized = String(Number.isFinite(parsed) ? Math.min(600, Math.max(0, parsed)) : 300);
        setNoBreakerRetryBudget(normalized);
        if (normalized === initialNoBreakerRetryBudget.current) return;
        setSetting.mutate(
            { key: SettingKey.RelayNoBreakerRetryBudgetSeconds, value: normalized },
            {
                onSuccess: () => {
                    toast.success(t('saved'));
                    initialNoBreakerRetryBudget.current = normalized;
                }
            }
        );
    };

    const handleClearLogs = () => {
        setIsClearing(true);
        clearLogs.mutate(undefined, {
            onSuccess: () => {
                toast.success(t('log.clearSuccess'));
                setIsClearing(false);
            },
            onError: () => {
                toast.error(t('log.clearFailed'));
                setIsClearing(false);
            }
        });
    };

    return (
        <div className="rounded-3xl border border-border bg-card p-6 space-y-5">
            <h2 className="text-lg font-bold text-card-foreground flex items-center gap-2">
                <ScrollText className="h-5 w-5" />
                {t('log.title')}
            </h2>

            {/* 是否启用历史日志 */}
            <div className="flex items-center justify-between gap-4">
                <div className="flex items-center gap-3">
                    <ScrollText className="h-5 w-5 text-muted-foreground" />
                    <span className="text-sm font-medium">{t('log.enabled.label')}</span>
                </div>
                <Switch
                    checked={enabled}
                    onCheckedChange={handleEnabledChange}
                />
            </div>

            {/* 历史日志保存范围 */}
            <div className="flex items-center justify-between gap-4">
                <div className="flex items-center gap-3">
                    <Calendar className="h-5 w-5 text-muted-foreground" />
                    <span className="text-sm font-medium">{t('log.keepPeriod.label')}</span>
                </div>
                <Input
                    type="number"
                    value={keepPeriod}
                    onChange={(e) => setKeepPeriod(e.target.value)}
                    onBlur={handleKeepPeriodSave}
                    placeholder={t('log.keepPeriod.placeholder')}
                    className="w-48 rounded-xl"
                    disabled={!enabled}
                />
            </div>

            {/* 历史日志容量上限 */}
            <div className="space-y-3">
                <div className="flex items-center justify-between gap-4">
                    <div className="flex items-center gap-3">
                        <HardDrive className="h-5 w-5 text-muted-foreground" />
                        <div className="flex flex-col gap-0.5">
                            <span className="text-sm font-medium">{t('log.maxStorage.label')}</span>
                            <span className="text-xs text-muted-foreground">{t('log.maxStorage.hint')}</span>
                        </div>
                    </div>
                    <Input
                        type="number"
                        min="0"
                        step="0.1"
                        value={maxStorageGB}
                        onChange={(e) => setMaxStorageGB(e.target.value)}
                        onBlur={handleMaxStorageSave}
                        placeholder={t('log.maxStorage.placeholder')}
                        className="w-48 rounded-xl"
                        disabled={!enabled}
                    />
                </div>
                <div className="pl-8 space-y-2">
                    <Progress
                        value={storage?.max_bytes ? Math.min(100, (storage.stored_bytes / storage.max_bytes) * 100) : 0}
                        className="h-1.5"
                    />
                    <div className="flex justify-between text-xs text-muted-foreground">
                        <span>{t('log.maxStorage.current')}</span>
                        <span>
                            {formatBytes(storage?.stored_bytes ?? 0)}
                            {storage?.max_bytes ? ` / ${formatBytes(storage.max_bytes)}` : ` / ${t('log.maxStorage.unlimited')}`}
                        </span>
                    </div>
                </div>
            </div>

            {/* 上游错误自动救援 */}
            <div className="space-y-3 rounded-2xl border border-sky-500/30 bg-sky-500/5 p-4">
                <div className="flex items-center justify-between gap-4">
                    <div className="flex flex-col gap-1">
                        <span className="text-sm font-medium">上游错误自动救援</span>
                        <span className="text-xs text-muted-foreground">开启后，流式请求在所有自动渠道失败时会保持客户端连接，机器按画布顺序自动切换渠道重试，无需人工干预。</span>
                    </div>
                    <Switch
                        checked={interventionEnabled}
                        onCheckedChange={handleInterventionEnabledChange}
                    />
                </div>
                <div className="flex items-center justify-between gap-4">
                    <span className="text-sm font-medium">自动救援等待上限（秒）</span>
                    <Input
                        type="number"
                        min="1"
                        value={interventionTimeout}
                        onChange={(e) => setInterventionTimeout(e.target.value)}
                        onBlur={handleInterventionTimeoutSave}
                        className="w-48 rounded-xl"
                        disabled={!interventionEnabled}
                    />
                </div>
                <div className="flex items-center justify-between gap-4 border-t border-amber-500/20 pt-3">
                    <div className="flex flex-col gap-1">
                        <span className="text-sm font-medium">无熔断渠道自动猛打（秒）</span>
                        <span className="text-xs text-muted-foreground">默认 300、最大 600。失败时拦截下游错误，按无限画布既定优先/轮询顺序每秒重新选路；0 表示关闭。开启熔断的渠道仍照常计失败并进入熔断。</span>
                    </div>
                    <Input
                        type="number"
                        min="0"
                        max="600"
                        value={noBreakerRetryBudget}
                        onChange={(e) => setNoBreakerRetryBudget(e.target.value)}
                        onBlur={handleNoBreakerRetryBudgetSave}
                        className="w-48 shrink-0 rounded-xl"
                    />
                </div>
            </div>

            {/* 清空历史日志 */}
            <div className="flex items-center justify-between gap-4">
                <div className="flex items-center gap-3">
                    <Trash2 className="h-5 w-5 text-muted-foreground" />
                    <span className="text-sm font-medium">{t('log.clear.label')}</span>
                </div>
                <Button
                    variant="destructive"
                    size="sm"
                    onClick={handleClearLogs}
                    disabled={isClearing}
                    className="rounded-xl"
                >
                    {isClearing ? t('log.clear.clearing') : t('log.clear.button')}
                </Button>
            </div>
        </div>
    );
}
