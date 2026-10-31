'use client';

import { ArrowUp, ChevronDown, PauseCircle, RefreshCw } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { cn } from '@/lib/utils';
import {
    LOG_AUTO_REFRESH_CHOICES,
    parseLogAutoRefresh,
    type LogAutoRefresh,
    type LogAutoRefreshPauseReason,
} from '@/lib/log-auto-refresh';

const CHOICE_LABEL_KEY: Record<LogAutoRefresh, string> = {
    off: 'off',
    live: 'live',
    '5s': 's5',
    '10s': 's10',
    '30s': 's30',
};

const PAUSE_LABEL_KEY: Record<LogAutoRefreshPauseReason, string> = {
    hidden: 'pausedHidden',
    detail: 'pausedDetail',
    selecting: 'pausedSelecting',
    scrolled: 'pausedScrolled',
};

/**
 * 自动刷新开关：一个原生下拉（手机上弹系统选择器，拇指好点），左侧圆点表示连接状态。
 * 取代原来的「实时动态」开关：「实时」= 原开关打开时的行为。
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
    const on = mode !== 'off';
    return (
        <label
            title={t('hint')}
            className={cn(
                'relative inline-flex h-8 shrink-0 cursor-pointer items-center gap-1.5 rounded-lg border px-2 text-xs transition-colors max-sm:h-10 max-sm:px-2.5',
                on ? 'border-emerald-500/40 bg-emerald-500/[0.06] text-emerald-700 dark:text-emerald-300' : 'border-border bg-background text-muted-foreground'
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
                <RefreshCw className="size-3.5 shrink-0" aria-hidden />
            )}
            <span className="hidden sm:inline">{t('label')}</span>
            <select
                aria-label={t('label')}
                value={mode}
                onChange={(event) => onChange(parseLogAutoRefresh(event.target.value))}
                // 透明原生下拉叠在文字上：保留系统选择器（移动端好点），视觉上只露出当前值。
                className="cursor-pointer appearance-none bg-transparent pr-3.5 text-xs font-medium text-current outline-none focus-visible:underline"
            >
                {LOG_AUTO_REFRESH_CHOICES.map((choice) => (
                    <option key={choice} value={choice}>
                        {t(CHOICE_LABEL_KEY[choice])}
                    </option>
                ))}
            </select>
            <ChevronDown className="pointer-events-none absolute right-1.5 size-3 opacity-60 max-sm:right-2" aria-hidden />
        </label>
    );
}

/** 工具栏里的一行状态：在刷 / 已暂停（原因）/ 缓冲了 N 条新日志 + 「显示」。 */
export function AutoRefreshStatus({
    mode,
    onFirstPage,
    connected,
    pauseReason,
    pendingCount,
    onShowPending,
}: {
    mode: LogAutoRefresh;
    onFirstPage: boolean;
    connected: boolean;
    pauseReason: LogAutoRefreshPauseReason | null;
    pendingCount: number;
    onShowPending: () => void;
}) {
    const t = useTranslations('log.autoRefresh');
    if (mode === 'off') return null;
    if (!onFirstPage) {
        return <span className="text-xs text-muted-foreground">{t('onlyFirstPage')}</span>;
    }
    if (pauseReason) {
        return (
            <span className="inline-flex min-w-0 flex-wrap items-center gap-x-1.5 gap-y-1 text-xs text-amber-700 dark:text-amber-300">
                <PauseCircle className="size-3.5 shrink-0" aria-hidden />
                <span>{t('paused')} · {t(PAUSE_LABEL_KEY[pauseReason])}</span>
                {pendingCount > 0 && (
                    <button
                        type="button"
                        onClick={onShowPending}
                        className="inline-flex h-6 items-center gap-1 rounded-full bg-primary/10 px-2 font-medium text-primary transition-colors hover:bg-primary/15 max-sm:h-8 max-sm:px-3"
                    >
                        {t('newCount', { count: pendingCount })} · {t('showNew')}
                    </button>
                )}
            </span>
        );
    }
    return (
        <span className="text-xs text-emerald-700 dark:text-emerald-300">
            {!connected ? t('reconnecting') : mode === 'live' ? t('runningLive') : t('running')}
        </span>
    );
}

/** 列表上方悬浮的「↑ N 条新日志」：只在往下翻 / 选字暂停时出现，点了回顶并插入。 */
export function PendingLogsPill({ count, visible, onClick }: { count: number; visible: boolean; onClick: () => void }) {
    const t = useTranslations('log.autoRefresh');
    if (!visible || count <= 0) return null;
    return (
        <div className="pointer-events-none absolute inset-x-0 top-2 z-20 flex justify-center">
            <button
                type="button"
                onClick={onClick}
                className="pointer-events-auto inline-flex h-9 items-center gap-1.5 rounded-full border border-primary/30 bg-background/95 px-3.5 text-xs font-medium text-primary shadow-md backdrop-blur transition-colors hover:bg-primary/10 max-sm:h-10"
            >
                <ArrowUp className="size-3.5" aria-hidden />
                {t('newCount', { count })}
            </button>
        </div>
    );
}
