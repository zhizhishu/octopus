'use client';

import { useEffect, useState, useRef } from 'react';
import { useTranslations } from 'next-intl';
import { ScrollText, Calendar, Trash2, HardDrive, Database, Send, LifeBuoy } from 'lucide-react';
import { Input } from '@/components/ui/input';
import { Switch } from '@/components/ui/switch';
import { Button } from '@/components/ui/button';
import { Progress } from '@/components/ui/progress';
import { useSettingList, useSetSetting, SettingKey } from '@/api/endpoints/setting';
import { useClearLogs, useLogStorage, useLogQueueHealth, useLogCleanupNow } from '@/api/endpoints/log';
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

// 「记录发给上游的实际请求」(upstream_request_body_log_enabled) 在后端用 SettingGetInt
// (strconv.Atoi) 读取、非 0 即开。所以这里必须读写 "1"/"0"：写 "true" 会被后端
// 解析失败而当成关闭。读取时也按同一规则解释，界面显示与后端实际行为保持一致。
export function parseUpstreamRequestBodyLog(value: string | undefined): boolean {
    const trimmed = (value ?? '').trim();
    if (!/^[+-]?\d+$/.test(trimmed)) return false;
    return Number.parseInt(trimmed, 10) !== 0;
}

export function formatUpstreamRequestBodyLog(enabled: boolean): string {
    return enabled ? '1' : '0';
}

// 设置页按标签拆分后，这个组件分两块渲染：
//   'log'    → 日志设置卡（日志与备份 标签）
//   'rescue' → 自动救援卡（转发与容错 标签）
// 不传 section 时两块都渲染（保持原组件的完整行为，供测试与独立使用）。
export type SettingLogSection = 'log' | 'rescue';

export function SettingLog({ section }: { section?: SettingLogSection } = {}) {
    const t = useTranslations('setting');
    const { data: settings } = useSettingList();
    const { data: storage } = useLogStorage();
    const { data: queueHealth } = useLogQueueHealth();
    const setSetting = useSetSetting();
    const clearLogs = useClearLogs();
    const cleanupNow = useLogCleanupNow();

    const [enabled, setEnabled] = useState(true);
    const [keepPeriod, setKeepPeriod] = useState('7');
    const [maxStorageGB, setMaxStorageGB] = useState('0');
    const [interventionEnabled, setInterventionEnabled] = useState(true);
    const [noBreakerRetryBudget, setNoBreakerRetryBudget] = useState('300');
    const [isClearing, setIsClearing] = useState(false);
    // 死空间回收开关：关掉=只有本轮删了行才回收，用于 VACUUM 卡住时应急止血。
    const [reclaimEnabled, setReclaimEnabled] = useState(true);
    // 记录真实出站请求体：默认关（存储成本与请求体同量级，且按原样保存、不打码）。
    const [upstreamBodyLog, setUpstreamBodyLog] = useState(false);

    const initialEnabled = useRef(true);
    const initialKeepPeriod = useRef('7');
    const initialMaxStorageGB = useRef('0');
    const initialInterventionEnabled = useRef(true);
    const initialNoBreakerRetryBudget = useRef('300');
    const initialReclaimEnabled = useRef(true);
    const initialUpstreamBodyLog = useRef(false);

    useEffect(() => {
        if (settings) {
            const enabledSetting = settings.find(s => s.key === SettingKey.RelayLogKeepEnabled);
            const periodSetting = settings.find(s => s.key === SettingKey.RelayLogKeepPeriod);
            const maxStorageSetting = settings.find(s => s.key === SettingKey.RelayLogMaxStorageGB);
            const interventionEnabledSetting = settings.find(s => s.key === SettingKey.RelayInterventionEnabled);
            const noBreakerRetryBudgetSetting = settings.find(s => s.key === SettingKey.RelayNoBreakerRetryBudgetSeconds);
            const reclaimSetting = settings.find(s => s.key === SettingKey.RelayLogReclaimEnabled);
            const upstreamBodyLogSetting = settings.find(s => s.key === SettingKey.UpstreamRequestBodyLog);
            if (enabledSetting) {
                const isEnabled = enabledSetting.value === 'true';
                queueMicrotask(() => setEnabled(isEnabled));
                initialEnabled.current = isEnabled;
            }
            if (periodSetting) {
                if (periodSetting.value !== initialKeepPeriod.current) {
                    queueMicrotask(() => setKeepPeriod(periodSetting.value));
                    initialKeepPeriod.current = periodSetting.value;
                }
            }
            if (maxStorageSetting) {
                if (maxStorageSetting.value !== initialMaxStorageGB.current) {
                    queueMicrotask(() => setMaxStorageGB(maxStorageSetting.value));
                    initialMaxStorageGB.current = maxStorageSetting.value;
                }
            }
            if (interventionEnabledSetting) {
                const isInterventionEnabled = interventionEnabledSetting.value === 'true';
                queueMicrotask(() => setInterventionEnabled(isInterventionEnabled));
                initialInterventionEnabled.current = isInterventionEnabled;
            }
            if (reclaimSetting) {
                const isReclaimEnabled = reclaimSetting.value === 'true';
                queueMicrotask(() => setReclaimEnabled(isReclaimEnabled));
                initialReclaimEnabled.current = isReclaimEnabled;
            }
            if (upstreamBodyLogSetting) {
                const isUpstreamBodyLog = parseUpstreamRequestBodyLog(upstreamBodyLogSetting.value);
                queueMicrotask(() => setUpstreamBodyLog(isUpstreamBodyLog));
                initialUpstreamBodyLog.current = isUpstreamBodyLog;
            }
            if (noBreakerRetryBudgetSetting) {
                if (noBreakerRetryBudgetSetting.value !== initialNoBreakerRetryBudget.current) {
                    queueMicrotask(() => setNoBreakerRetryBudget(noBreakerRetryBudgetSetting.value));
                    initialNoBreakerRetryBudget.current = noBreakerRetryBudgetSetting.value;
                }
            }
        }
    }, [settings]);

    const handleEnabledChange = (checked: boolean) => {
        if (setSetting.isPending) return;
        setEnabled(checked);
        setSetting.mutate(
            { key: SettingKey.RelayLogKeepEnabled, value: checked ? 'true' : 'false' },
            {
                onSuccess: () => {
                    toast.success(t('saved'));
                    initialEnabled.current = checked;
                },
                onError: () => {
                    toast.error(t('saveFailed'));
                    setEnabled(initialEnabled.current);
                }
            }
        );
    };

    const handleReclaimEnabledChange = (checked: boolean) => {
        if (setSetting.isPending) return;
        // 乐观更新 + 失败回滚：这是应急开关，点了必须立刻生效或立刻复位。
        setReclaimEnabled(checked);
        setSetting.mutate(
            { key: SettingKey.RelayLogReclaimEnabled, value: checked ? 'true' : 'false' },
            {
                onSuccess: () => {
                    toast.success(t('saved'));
                    initialReclaimEnabled.current = checked;
                },
                onError: () => {
                    toast.error(t('saveFailed'));
                    setReclaimEnabled(initialReclaimEnabled.current);
                }
            }
        );
    };

    const handleUpstreamBodyLogChange = (checked: boolean) => {
        if (setSetting.isPending) return;
        // 与其它开关一致：乐观更新，失败回滚到已确认值。
        setUpstreamBodyLog(checked);
        setSetting.mutate(
            { key: SettingKey.UpstreamRequestBodyLog, value: formatUpstreamRequestBodyLog(checked) },
            {
                onSuccess: () => {
                    toast.success(t('saved'));
                    initialUpstreamBodyLog.current = checked;
                },
                onError: () => {
                    toast.error(t('saveFailed'));
                    setUpstreamBodyLog(initialUpstreamBodyLog.current);
                }
            }
        );
    };

    const handleKeepPeriodSave = () => {
        if (keepPeriod === initialKeepPeriod.current) return;
        if (setSetting.isPending) return;

        setSetting.mutate(
            { key: SettingKey.RelayLogKeepPeriod, value: keepPeriod },
            {
                onSuccess: () => {
                    toast.success(t('saved'));
                    initialKeepPeriod.current = keepPeriod;
                },
                onError: () => {
                    // 保留草稿不回滚：再次失焦即可重试。
                    toast.error(t('saveFailed'));
                }
            }
        );
    };

    const handleMaxStorageSave = () => {
        if (maxStorageGB === initialMaxStorageGB.current) return;
        if (setSetting.isPending) return;

        setSetting.mutate(
            { key: SettingKey.RelayLogMaxStorageGB, value: maxStorageGB },
            {
                onSuccess: () => {
                    toast.success(t('saved'));
                    initialMaxStorageGB.current = maxStorageGB;
                },
                onError: () => {
                    // 保留草稿不回滚：再次失焦即可重试。
                    toast.error(t('saveFailed'));
                }
            }
        );
    };

    const handleInterventionEnabledChange = (checked: boolean) => {
        if (setSetting.isPending) return;
        setInterventionEnabled(checked);
        setSetting.mutate(
            { key: SettingKey.RelayInterventionEnabled, value: checked ? 'true' : 'false' },
            {
                onSuccess: () => {
                    toast.success(t('saved'));
                    initialInterventionEnabled.current = checked;
                },
                onError: () => {
                    toast.error(t('saveFailed'));
                    setInterventionEnabled(initialInterventionEnabled.current);
                }
            }
        );
    };

    const handleNoBreakerRetryBudgetSave = () => {
        if (setSetting.isPending) return;
        const parsed = Number.parseInt(noBreakerRetryBudget, 10);
        const normalized = String(Number.isFinite(parsed) ? Math.min(300, Math.max(0, parsed)) : 300);
        setNoBreakerRetryBudget(normalized);
        if (normalized === initialNoBreakerRetryBudget.current) return;
        setSetting.mutate(
            { key: SettingKey.RelayNoBreakerRetryBudgetSeconds, value: normalized },
            {
                onSuccess: () => {
                    toast.success(t('saved'));
                    initialNoBreakerRetryBudget.current = normalized;
                },
                onError: () => {
                    // 保留钳制后的草稿不回滚：再次失焦即可重试。
                    toast.error(t('saveFailed'));
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

    const logCard = (
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

            {/* 记录发给上游的实际请求：历史日志关闭时不落库，开了也没用，所以跟着置灰 */}
            <div className="flex items-center justify-between gap-4">
                <div className="flex items-center gap-3">
                    <Send className="h-5 w-5 shrink-0 text-muted-foreground" />
                    <div className="flex flex-col">
                        <span className="text-sm font-medium">{t('log.upstreamRequestBody.label')}</span>
                        <span className="text-xs text-muted-foreground">{t('log.upstreamRequestBody.hint')}</span>
                    </div>
                </div>
                <Switch
                    checked={upstreamBodyLog}
                    onCheckedChange={handleUpstreamBodyLogChange}
                    disabled={!enabled}
                    aria-label={t('log.upstreamRequestBody.label')}
                />
            </div>

            {/* 死空间回收 */}
            <div className="flex items-center justify-between gap-4">
                <div className="flex items-center gap-3">
                    <Database className="h-5 w-5 shrink-0 text-muted-foreground" />
                    <div className="flex flex-col">
                        <span className="text-sm font-medium">{t('log.reclaim.label')}</span>
                        <span className="text-xs text-muted-foreground">{t('log.reclaim.hint')}</span>
                    </div>
                </div>
                <Switch
                    checked={reclaimEnabled}
                    onCheckedChange={handleReclaimEnabledChange}
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
                        <HardDrive className="h-5 w-5 shrink-0 text-muted-foreground" />
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
                    <div className="flex items-center justify-between gap-2 text-xs text-muted-foreground">
                        <span>{t('log.maxStorage.current')}</span>
                        <div className="flex items-center gap-2">
                            <span className="tabular-nums">
                                {formatBytes(storage?.stored_bytes ?? 0)}
                                {storage?.max_bytes ? ` / ${formatBytes(storage.max_bytes)}` : ` / ${t('log.maxStorage.unlimited')}`}
                            </span>
                            <Button
                                size="sm"
                                variant="outline"
                                className="h-7 rounded-lg px-2 text-xs"
                                disabled={!enabled || cleanupNow.isPending}
                                onClick={() => {
                                    cleanupNow.mutate(undefined, {
                                        onSuccess: (r) => {
                                            // 只删了行但 VACUUM 失败时不能报"成功"，否则用户会以为空间回来了
                                            if (r.freed_bytes > 0 && r.vacuumed) toast.success(t('log.maxStorage.cleanupFreed', { size: formatBytes(r.freed_bytes) }));
                                            else if (r.freed_bytes > 0) toast.warning(r.note || t('log.maxStorage.cleanupVacuumFailed'));
                                            else toast.info(r.note || t('log.maxStorage.cleanupNothing'));
                                        },
                                        onError: () => toast.error(t('log.maxStorage.cleanupFailed')),
                                    });
                                }}
                            >
                                {cleanupNow.isPending ? t('log.maxStorage.cleaning') : t('log.maxStorage.cleanupNow')}
                            </Button>
                        </div>
                    </div>
                    {/* 说明"已用"含哪些东西：不说清楚的话，主表删了、副本还在长，
                        用户会以为上限没用。 */}
                    <p className="text-xs text-muted-foreground/80">{t('log.maxStorage.usageHint')}</p>
                    {/* 落库队列健康: 没有这一行时, “日志里查不到这条”和“请求没发生”看着一模一样。
                        计数是进程内的(重启归零), 关闭持久化是主动设置、不算故障。 */}
                    {queueHealth?.persistence_enabled && (
                        <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
                            <span className="text-muted-foreground">
                                {t('log.queueHealth.pending')}
                                <span className="ml-1 tabular-nums text-foreground">
                                    {queueHealth.pending_count} / {queueHealth.pending_capacity}
                                </span>
                            </span>
                            {queueHealth.dropped_total > 0 && (
                                <span className="text-amber-600 dark:text-amber-500">
                                    {t('log.queueHealth.dropped')}
                                    <span className="ml-1 tabular-nums">{queueHealth.dropped_total}</span>
                                </span>
                            )}
                            {queueHealth.write_failure_total > 0 && (
                                <span className="text-rose-600 dark:text-rose-500">
                                    {t('log.queueHealth.writeFailed')}
                                    <span className="ml-1 tabular-nums">{queueHealth.write_failure_total}</span>
                                    {queueHealth.last_write_failure_at > 0 &&
                                        ` · ${new Date(queueHealth.last_write_failure_at * 1000).toLocaleString('zh-CN')}`}
                                </span>
                            )}
                            {queueHealth.dropped_total === 0 && queueHealth.write_failure_total === 0 && (
                                <span className="text-muted-foreground">{t('log.queueHealth.healthy')}</span>
                            )}
                        </div>
                    )}
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

    // 上游错误自动救援：与日志无关，单独成卡放到「转发与容错」。
    const rescueCard = (
        <div className="rounded-3xl border border-border bg-card p-6 space-y-5">
            <h2 className="text-lg font-bold text-card-foreground flex items-center gap-2">
                <LifeBuoy className="h-5 w-5" />
                {t('log.rescue.title')}
            </h2>
            <div className="space-y-3 rounded-2xl border border-sky-500/30 bg-sky-500/5 p-4">
                <div className="flex items-center justify-between gap-4">
                    <div className="flex flex-col gap-1">
                        <span className="text-sm font-medium">{t('log.intervention.label')}</span>
                        <span className="text-xs text-muted-foreground">{t('log.intervention.description')}</span>
                    </div>
                    <Switch
                        checked={interventionEnabled}
                        onCheckedChange={handleInterventionEnabledChange}
                    />
                </div>
                <div className="flex items-center justify-between gap-4 border-t border-amber-500/20 pt-3">
                    <div className="flex flex-col gap-1">
                        <span className="text-sm font-medium">{t('log.noBreaker.label')}</span>
                        <span className="text-xs text-muted-foreground">{t('log.noBreaker.hint')}</span>
                    </div>
                    <Input
                        type="number"
                        min="0"
                        max="300"
                        value={noBreakerRetryBudget}
                        onChange={(e) => setNoBreakerRetryBudget(e.target.value)}
                        onBlur={handleNoBreakerRetryBudgetSave}
                        className="w-28 shrink-0 rounded-xl sm:w-32"
                    />
                </div>
            </div>
        </div>
    );

    if (section === 'log') return logCard;
    if (section === 'rescue') return rescueCard;
    return (
        <>
            {logCard}
            {rescueCard}
        </>
    );
}
