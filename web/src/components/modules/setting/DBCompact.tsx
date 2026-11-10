'use client';

import { useTranslations } from 'next-intl';
import { Archive, HelpCircle } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from '@/components/animate-ui/components/animate/tooltip';
import { useDBCompactInfo, useDBCompactNow } from '@/api/endpoints/setting';
import { toast } from '@/components/common/Toast';

export function formatCompactBytes(bytes: number): string {
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

/**
 * 「压缩数据库」一行：当前大小 + 可回收量 + 按钮。
 *
 * - 仅 SQLite 显示（后端 supported=false 时整行不渲染：MySQL/PostgreSQL 由数据库自身维护）；
 * - VACUUM 期间库被独占，点之前先确认；首次（还不是增量模式）要重建整个文件，提示更重；
 * - 后端单飞：已有压缩或自动空间回收在跑时回 409，这里提示"稍后再试"而不是报失败。
 */
export function DBCompactRow() {
    const t = useTranslations('setting');
    const query = useDBCompactInfo();
    const { data: info } = query;
    const compactNow = useDBCompactNow();
    const refresh = <Button variant="outline" size="sm" disabled={query.isFetching}
        onClick={() => { if (!query.isFetching) void query.refetch(); }}>{t('log.status.refresh')}</Button>;

    if (query.isError || !info) return (
        <div className="flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground" role="status">
            <span>{t('log.compact.label')} · {t(query.isFetching ? 'log.status.loading' : query.isError ? 'log.status.failed' : 'log.status.unknown')}</span>
            {refresh}
        </div>
    );
    if (!info.supported) return null;

    const busy = compactNow.isPending || info.running;
    const statsValid = info.stats_valid === true && !info.error_code;

    const handleCompact = () => {
        if (busy) return;
        const message = info.needs_full
            ? `${t('log.compact.confirm')}\n\n${t('log.compact.fullWarning')}`
            : t('log.compact.confirm');
        if (typeof window !== 'undefined' && !window.confirm(message)) return;
        compactNow.mutate(undefined, {
            onSuccess: (r) => {
                if (r.stats_valid === false || r.error_code ||
                    !Number.isFinite(r.before_bytes) || r.before_bytes < 0 ||
                    !Number.isFinite(r.after_bytes) || r.after_bytes < 0) {
                    toast.warning(t('log.compact.measurementUnknown'));
                    return;
                }
                if (r.checkpoint_error_code ||
                    (r.checkpoint_status !== 'truncated' && r.checkpoint_status !== 'not_needed')) {
                    toast.warning(t('log.compact.checkpointIncomplete'));
                    return;
                }
                toast.success(t('log.compact.done', {
                    before: formatCompactBytes(r.before_bytes),
                    after: formatCompactBytes(r.after_bytes),
                }));
            },
            onError: (error: unknown) => {
                const code = (error as { code?: number } | null)?.code;
                toast.error(code === 409 ? t('log.compact.busy') : t('log.compact.failed'));
            },
        });
    };

    return (
        <div className="flex items-center justify-between gap-4">
            <div className="flex min-w-0 flex-1 items-center gap-3">
                <Archive className="h-5 w-5 shrink-0 text-muted-foreground" />
                <div className="flex min-w-0 flex-col gap-0.5">
                    <span className="flex items-center gap-1.5 text-sm font-medium">
                        {t('log.compact.label')}
                        <TooltipProvider>
                            <Tooltip>
                                <TooltipTrigger asChild>
                                    <HelpCircle className="size-4 text-muted-foreground cursor-help" aria-label={t('log.compact.tooltip')} />
                                </TooltipTrigger>
                                <TooltipContent className="max-w-xs">{t('log.compact.tooltip')}</TooltipContent>
                            </Tooltip>
                        </TooltipProvider>
                    </span>
                    <span className="text-xs text-muted-foreground tabular-nums">
                        {`${t('log.compact.size')} ${formatCompactBytes(info.file_bytes)}`}
                        {statsValid ? ` · ${t('log.compact.reclaimable', { size: formatCompactBytes(info.free_bytes) })}` : ` · ${t(info.stats_valid === false || info.error_code ? 'log.status.failed' : 'log.status.unknown')}`}
                    </span>
                    <span className="text-xs text-muted-foreground tabular-nums">
                        {(['main_bytes', 'wal_bytes', 'shm_bytes'] as const).filter(key => typeof info[key] === 'number').map(key => `${t(`log.compact.${key}`)} ${formatCompactBytes(info[key]!)}`).join(' · ')}
                    </span>
                    <span className="text-xs text-muted-foreground">{t(info.needs_full ? 'log.compact.fullMode' : 'log.compact.incrementalMode')}</span>
                    {!!query.dataUpdatedAt && <span className="text-xs text-muted-foreground">{t('log.status.updated', { time: new Date(query.dataUpdatedAt).toLocaleTimeString() })}</span>}
                    <span className="text-xs text-amber-600 dark:text-amber-500">
                        {info.needs_full ? t('log.compact.fullWarning') : t('log.compact.hint')}
                    </span>
                </div>
            </div>
            <div className="flex flex-col gap-2">
            {refresh}
            <Button
                variant="outline"
                size="sm"
                onClick={handleCompact}
                disabled={busy}
                className="shrink-0 rounded-xl"
            >
                {busy ? t('log.compact.running') : t('log.compact.button')}
            </Button>
            </div>
        </div>
    );
}
