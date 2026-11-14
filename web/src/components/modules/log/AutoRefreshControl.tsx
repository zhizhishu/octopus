'use client';

import { ArrowUp, PauseCircle, Radio } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { cn } from '@/lib/utils';
import type { LogAutoRefresh, LogAutoRefreshPauseReason } from '@/lib/log-auto-refresh';
import { pendingUnreadLabel } from '@/lib/log-live-buffer';

const PAUSE_LABEL_KEY: Record<LogAutoRefreshPauseReason, string> = {
    hidden: 'pausedHidden',
    detail: 'pausedDetail',
    selecting: 'pausedSelecting',
    scrolled: 'pausedScrolled',
};

/**
 * 「实时」开关：点一下开、再点一下关（aria-pressed 表示状态）。
 * 外观与旁边的导出/隐藏按钮同一套描边方块；开着时变绿，圆点表示连接状态。
 */
export function AutoRefreshControl({
    mode,
    onChange,
    connected,
    errored,
    paused,
}: {
    mode: LogAutoRefresh;
    onChange: (mode: LogAutoRefresh) => void;
    connected: boolean;
    errored: boolean;
    paused: boolean;
}) {
    const t = useTranslations('log.autoRefresh');
    const on = mode === 'live';
    return (
        <button
            type="button"
            aria-pressed={on}
            title={t('hint')}
            onClick={() => onChange(on ? 'off' : 'live')}
            className={cn(
                'inline-flex h-8 shrink-0 items-center gap-1.5 rounded-lg border px-2.5 text-xs font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring max-sm:h-10 max-sm:px-3',
                on
                    ? 'border-emerald-500/40 bg-emerald-500/[0.08] text-emerald-700 hover:bg-emerald-500/[0.14] dark:text-emerald-300'
                    : 'border-border bg-background text-muted-foreground hover:text-foreground dark:bg-card'
            )}
        >
            {on ? (
                <span className="relative flex size-2 shrink-0" aria-hidden>
                    {connected && !paused && (
                        <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-emerald-400 opacity-75" />
                    )}
                    <span
                        className={cn(
                            'relative inline-flex size-2 rounded-full',
                            paused ? 'bg-amber-500' : connected ? 'bg-emerald-500' : errored ? 'bg-destructive' : 'bg-amber-500'
                        )}
                    />
                </span>
            ) : (
                <Radio className="size-3.5 shrink-0" aria-hidden />
            )}
            <span>{t('live')}</span>
        </button>
    );
}

/** 工具栏里的一行状态：在刷 / 已暂停（原因）/ 缓冲了 N 条新日志 + 「显示」。 */
export function AutoRefreshStatus({
    mode,
    onFirstPage,
    connected,
    pauseReason,
    pendingCount,
    pendingOverflow = false,
    frozen = false,
    busy = false,
    onShowPending,
}: {
    mode: LogAutoRefresh;
    onFirstPage: boolean;
    connected: boolean;
    pauseReason: LogAutoRefreshPauseReason | null;
    pendingCount: number;
    /** 缓冲曾满过：徽标只说「200+」，不假装知道确切总数。 */
    pendingOverflow?: boolean;
    /** 详情关闭后的冻结闩仍在：插入依旧暂停，等用户点「显示」或手动刷新。 */
    frozen?: boolean;
    /** 恢复 GET 在飞：按钮先禁用，不叠第二次请求（单飞）。 */
    busy?: boolean;
    onShowPending: () => void;
}) {
    const t = useTranslations('log.autoRefresh');
    if (mode === 'off') return null;
    if (!onFirstPage) {
        return <span className="text-xs text-muted-foreground">{t('onlyFirstPage')}</span>;
    }
    // 冻结闩没有对应的「原因」（详情可能已经关掉），所以只显示「已暂停」+ 恢复入口，
    // 不借用「正在看详情」那句——那是假的。
    if (pauseReason || frozen) {
        return (
            <span className="inline-flex min-w-0 flex-wrap items-center gap-x-1.5 gap-y-1 text-xs text-amber-700 dark:text-amber-300">
                <PauseCircle className="size-3.5 shrink-0" aria-hidden />
                <span>{t('paused')}{pauseReason ? ` · ${t(PAUSE_LABEL_KEY[pauseReason])}` : ''}</span>
                {pendingCount > 0 && (
                    <button
                        type="button"
                        onClick={onShowPending}
                        disabled={busy}
                        aria-busy={busy}
                        className="inline-flex h-6 items-center gap-1 rounded-full bg-primary/10 px-2 font-medium text-primary transition-colors hover:bg-primary/15 disabled:opacity-60 max-sm:h-8 max-sm:px-3"
                    >
                        {t('newCount', { count: pendingUnreadLabel(pendingCount, pendingOverflow) })} · {t('showNew')}
                    </button>
                )}
            </span>
        );
    }
    return (
        <span className="text-xs text-emerald-700 dark:text-emerald-300">
            {connected ? t('runningLive') : t('reconnecting')}
        </span>
    );
}

/** 列表上方悬浮的「↑ N 条新日志」：暂停/冻结时出现，点了回顶并按显式恢复插入。 */
export function PendingLogsPill({ count, visible, overflowed = false, busy = false, onClick }: { count: number; visible: boolean; overflowed?: boolean; busy?: boolean; onClick: () => void }) {
    const t = useTranslations('log.autoRefresh');
    if (!visible || count <= 0) return null;
    return (
        <div className="pointer-events-none absolute inset-x-0 top-2 z-20 flex justify-center">
            <button
                type="button"
                onClick={onClick}
                disabled={busy}
                aria-busy={busy}
                className="pointer-events-auto inline-flex h-9 items-center gap-1.5 rounded-full border border-primary/30 bg-background/95 px-3.5 text-xs font-medium text-primary shadow-md backdrop-blur transition-colors hover:bg-primary/10 disabled:opacity-60 max-sm:h-10"
            >
                <ArrowUp className="size-3.5" aria-hidden />
                {t('newCount', { count: pendingUnreadLabel(count, overflowed) })}
            </button>
        </div>
    );
}
