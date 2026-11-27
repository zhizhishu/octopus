'use client';

// LiveActivityPanel 从 log/index.tsx 抽成独立文件（fix-ui1127a）：实时标签整页复用。
// 逻辑与原实现一致——在途请求 + 救援干预的展示与操作（中止/转救援/指定渠道重试）。

import { useEffect, useMemo, useRef, useState } from 'react';
import { useVirtualizer } from '@tanstack/react-virtual';
import { type RequestState } from '@/api/endpoints/log';
import { useAbortIntervention, useAbortRunningRequest, useRescueRunningRequest, useInterventionList, useRetryIntervention, type InterventionSnapshot } from '@/api/endpoints/intervention';
import { useChannelList } from '@/api/endpoints/channel';
import { useTranslations } from 'next-intl';
import { Button } from '@/components/ui/button';
import { Badge } from '@/components/ui/badge';
import { cn } from '@/lib/utils';
import { toast } from '@/components/common/Toast';
import { ChevronDown, ChevronRight, Loader2, RotateCw, WifiOff, X } from 'lucide-react';
import { LIVE_ACTIVITY_COLLAPSED_STORAGE_KEY } from '@/lib/log-auto-refresh';

type InterventionDraft = {
    channelID: string;
    keyID: string;
    modelName: string;
};

export interface LiveActivityPanelProps {
    isAdmin: boolean;
    enabled: boolean;
    requestStates: RequestState[];
    requestStateConnected: boolean;
    historyLogIDs: Set<number>;
    // fullPage=true：日志页「实时」标签的整页形态（fix-ui1127a）——不可折叠、
    // 不限高度、在途行做虚拟化（@tanstack/react-virtual），用户量大时也能用。
    fullPage?: boolean;
}

export function LiveActivityPanel({
    isAdmin,
    enabled,
    requestStates,
    requestStateConnected,
    historyLogIDs,
    fullPage = false,
}: LiveActivityPanelProps) {
    const t = useTranslations('log.live');
    const { data: interventions = [], isSuccess } = useInterventionList({ enabled: isAdmin && enabled });
    const { data: channelRows = [] } = useChannelList({ enabled: isAdmin && enabled && interventions.length > 0 });
    const retryIntervention = useRetryIntervention();
    const abortIntervention = useAbortIntervention();
    const abortRunningRequest = useAbortRunningRequest();
    const rescueRunningRequest = useRescueRunningRequest();
    const [rescuingIDs, setRescuingIDs] = useState<Set<number>>(() => new Set());
    const [abortingIDs, setAbortingIDs] = useState<Set<number>>(() => new Set());
    const [drafts, setDrafts] = useState<Record<string, InterventionDraft>>({});
    // 可折叠，本地记住：手机上三五条实时请求就能占掉三分之一屏，收起后只留一行计数。
    const [collapsedStateValue, setCollapsedState] = useState<boolean>(() => {
        if (typeof window === 'undefined') return false;
        try {
            return window.localStorage.getItem(LIVE_ACTIVITY_COLLAPSED_STORAGE_KEY) === '1';
        } catch {
            return false;
        }
    });
    const collapsed = fullPage ? false : collapsedStateValue;
    const toggleCollapsed = () => {
        if (fullPage) return;
        setCollapsedState((previous) => {
            const next = !previous;
            try {
                window.localStorage.setItem(LIVE_ACTIVITY_COLLAPSED_STORAGE_KEY, next ? '1' : '0');
            } catch {
                // 写不进本地存储时只在本次会话生效。
            }
            return next;
        });
    };

    useEffect(() => {
        if (!enabled || !isSuccess) return;
        const activeIDs = new Set(interventions.map((item) => item.id));
        setDrafts((previous) => {
            if (Object.keys(previous).every((id) => activeIDs.has(id))) return previous;
            return Object.fromEntries(Object.entries(previous).filter(([id]) => activeIDs.has(id)));
        });
    }, [enabled, interventions, isSuccess]);

    const scrollParentRef = useRef<HTMLDivElement | null>(null);

    const enabledChannels = useMemo(
        () => channelRows.map((row) => row.raw).filter((channel) => channel.enabled),
        [channelRows]
    );

    const activeInterventions = useMemo(() => {
        if (!isAdmin || !enabled) return [];
        return interventions.filter(
            (item) => item.log_id === undefined || !historyLogIDs.has(item.log_id)
        );
    }, [enabled, historyLogIDs, interventions, isAdmin]);

    const activeInterventionIDs = useMemo(() => {
        return new Set(activeInterventions.map((item) => item.id));
    }, [activeInterventions]);

    const runningStates = useMemo(() => {
        if (!enabled) return [];
        return requestStates.filter(
            (state) =>
                state.status === 'running' &&
                (!state.intervention_id || !activeInterventionIDs.has(state.intervention_id))
        );
    }, [activeInterventionIDs, enabled, requestStates]);

    // 整页形态下的在途行虚拟化：行高不固定（details 可展开），用 measureElement 实测。
    const rowVirtualizer = useVirtualizer({
        count: fullPage ? runningStates.length : 0,
        getScrollElement: () => scrollParentRef.current,
        estimateSize: () => 56,
        overscan: 6,
    });

    if (!enabled) return null;
    if (runningStates.length === 0 && activeInterventions.length === 0) return null;

    const updateDraft = (id: string, patch: Partial<InterventionDraft>) => {
        setDrafts((previousDrafts) => {
            const currentDraft = previousDrafts[id] ?? { channelID: '', keyID: '', modelName: '' };
            return { ...previousDrafts, [id]: { ...currentDraft, ...patch } };
        });
    };

    const retryHeldRequest = (intervention: InterventionSnapshot) => {
        const draft = drafts[intervention.id];
        const channelID = Number(draft?.channelID);
        if (!Number.isFinite(channelID) || channelID <= 0) {
            toast.error('先选一个要重试的渠道');
            return;
        }
        const keyID = Number(draft?.keyID);
        retryIntervention.mutate(
            {
                id: intervention.id,
                request: {
                    channel_id: channelID,
                    key_id: Number.isFinite(keyID) && keyID > 0 ? keyID : undefined,
                    model_name: draft?.modelName.trim() || undefined,
                },
            },
            {
                onSuccess: () => toast.success('已指定渠道立即重试'),
                onError: (error) => toast.error('人工指定重试失败', { description: error instanceof Error ? error.message : String(error) }),
            }
        );
    };

    const abortHeldRequest = (intervention: InterventionSnapshot) => {
        abortIntervention.mutate(intervention.id, {
            onSuccess: () => toast.success(t('stopSent')),
            onError: (error) => toast.error(t('stopFailed'), { description: error instanceof Error ? error.message : String(error) }),
        });
    };

    const abortRunning = async (state: RequestState) => {
        if (!isAdmin) return;
        setAbortingIDs((previous) => new Set(previous).add(state.id));
        try {
            await abortRunningRequest.mutateAsync({
                id: state.id,
                started_at: state.started_at,
            });
            toast.success(t('stopSent'));
        } catch (error) {
            toast.error(t('stopFailed'), {
                description: error instanceof Error ? error.message : String(error),
            });
        } finally {
            setAbortingIDs((previous) => {
                const next = new Set(previous);
                next.delete(state.id);
                return next;
            });
        }
    };

    const rescueRunning = async (state: RequestState) => {
        if (!isAdmin || !state.rescuable) return;
        setRescuingIDs(previous => new Set(previous).add(state.id));
        try {
            await rescueRunningRequest.mutateAsync({ id: state.id, started_at: state.started_at });
            toast.success('已转入自动救援');
        } catch (error) {
            toast.error('转入自动救援失败', { description: error instanceof Error ? error.message : String(error) });
        } finally {
            setRescuingIDs(previous => {
                const next = new Set(previous);
                next.delete(state.id);
                return next;
            });
        }
    };

    // renderStateRow 是单条在途请求行的渲染体：整页形态的虚拟化行与普通分支共用，
    // 行为与抽取前完全一致。
    const renderStateRow = (state: RequestState) => {
const stopping = abortingIDs.has(state.id);
                    const startedTimestamp = new Date(state.started_at).getTime();
                    const elapsedSeconds = Number.isFinite(startedTimestamp)
                        ? Math.max(0, Math.round(((state.finished_at ? new Date(state.finished_at).getTime() : Date.now()) - startedTimestamp) / 1000))
                        : null;
                    const isRescue = Boolean(state.intervention_id) || state.round > 1;
                    const totalAttempts = Math.max(state.round, state.attempts?.length ?? 1);
                    const retries = Math.max(0, totalAttempts - 1);

                    return (
                        <div key={state.id} className="flex items-start gap-2 py-1.5 first:pt-0">
                            <details className="group min-w-0 flex-1">
                                <summary className="grid min-h-10 min-w-0 cursor-pointer list-none grid-cols-[minmax(0,1fr)_auto] items-center gap-x-2 gap-y-1 rounded-md px-2 py-1.5 transition-[background-color,box-shadow,transform] duration-200 hover:bg-muted/70 hover:shadow-sm active:scale-[0.99] outline-none focus-visible:ring-2 focus-visible:ring-ring sm:min-h-8 sm:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_auto] sm:px-1.5 sm:py-1">
                                    <span className="flex min-w-0 items-center gap-1.5">
                                        <ChevronRight className="size-3.5 shrink-0 text-muted-foreground transition-transform duration-200 ease-out group-open:rotate-90 group-hover:text-foreground" />
                                        {isRescue ? (
                                            <Badge variant="outline" className="shrink-0 border-amber-500/40 bg-amber-500/10 text-[10px] text-amber-700 dark:text-amber-300">
                                                {t('autoRescue')}
                                            </Badge>
                                        ) : (
                                            <Badge variant="outline" className="shrink-0 border-sky-500/40 bg-sky-500/10 text-[10px] text-sky-700 dark:text-sky-300 animate-pulse">
                                                {t('running')}
                                            </Badge>
                                        )}
                                        <span className="truncate font-mono text-xs" title={state.model}>
                                            {state.model || 'unknown'}
                                        </span>
                                    </span>
                                    <span className={cn('truncate text-xs', isRescue ? 'text-amber-600 dark:text-amber-300' : 'text-sky-600 dark:text-sky-300')}>
                                        {isAdmin && state.sending && state.target_channel
                                            ? state.target_channel
                                            : state.round > 1
                                                ? t('retryRound', { count: state.round })
                                                : t('requesting')}
                                    </span>
                                    <time dateTime={state.started_at} className="col-span-2 text-xs tabular-nums text-muted-foreground sm:col-span-1 sm:text-right">
                                        {new Date(state.started_at).toLocaleTimeString()}
                                    </time>
                                </summary>
                                <div className="details-content space-y-2 px-3 pb-2 pt-1 text-xs sm:px-2">
                                    {isAdmin && (
                                        <Button variant="outline" size="sm" className="h-9 text-xs sm:h-7"
                                            disabled={!state.rescuable || stopping || rescuingIDs.has(state.id)}
                                            onClick={(event) => { event.stopPropagation(); rescueRunning(state); }}
                                            title={state.rescuable ? '停止当前尝试，保留请求并交给自动救援' : '仅未开始返回内容的运行请求可转救援'}
                                            aria-busy={rescuingIDs.has(state.id)}>
                                            {rescuingIDs.has(state.id) ? (<><Loader2 className="size-3.5 animate-spin" />正在转入…</>) : '转自动救援'}
                                        </Button>
                                    )}
                                    <div className="grid grid-cols-2 gap-3 rounded-md border border-border/60 bg-muted/20 p-3 sm:grid-cols-4 sm:gap-2 sm:p-2">
                                        <div className="min-w-0">
                                            <span className="text-[10px] text-muted-foreground">端点</span>
                                            <p className="truncate font-mono text-foreground" title={state.endpoint}>{state.endpoint || '-'}</p>
                                        </div>
                                        <div className="min-w-0">
                                            <span className="text-[10px] text-muted-foreground">模型</span>
                                            <p className="truncate font-mono text-foreground" title={state.model}>{state.model || '-'}</p>
                                        </div>
                                        <div className="min-w-0">
                                            <span className="text-[10px] text-muted-foreground">开始时间</span>
                                            <p className="truncate tabular-nums text-foreground">{new Date(state.started_at).toLocaleTimeString()}</p>
                                        </div>
                                        <div className="min-w-0">
                                            <span className="text-[10px] text-muted-foreground">状态 / 耗时</span>
                                            <p className="truncate tabular-nums text-foreground">
                                                <span className={state.status === 'running' ? 'font-medium text-sky-600 dark:text-sky-400' : 'text-muted-foreground'}>
                                                    {state.status}
                                                </span>
                                                {elapsedSeconds !== null && (
                                                    <span className={elapsedSeconds > 30 ? 'font-bold text-amber-600 dark:text-amber-400' : 'text-muted-foreground'}>
                                                        {' · '}{elapsedSeconds}s
                                                    </span>
                                                )}
                                            </p>
                                        </div>
                                        {isAdmin && state.intervention_id && (
                                            <div className="col-span-2 min-w-0 sm:col-span-4">
                                                <span className="text-[10px] text-muted-foreground">救援 ID</span>
                                                <p className="truncate font-mono text-amber-600 dark:text-amber-400" title={state.intervention_id}>
                                                    {state.intervention_id}
                                                </p>
                                            </div>
                                        )}
                                    </div>

                                    {state.error && <p className="break-words text-destructive">{state.error}</p>}

                                    {isAdmin && (
                                        <div className="divide-y divide-border/60 rounded border border-border/60 bg-muted/20 px-2 py-1">
                                            <div className="flex items-center justify-between py-1 text-[11px] font-medium text-muted-foreground">
                                                <span>渠道尝试 ({state.attempts?.length ?? 0})</span>
                                                <span className="tabular-nums">总调用 {totalAttempts} 次 · 重试 {retries} 次</span>
                                            </div>
                                            {(state.attempts ?? []).map((attempt, index) => (
                                                <div key={`${attempt.round}-${index}`} className="flex min-w-0 flex-wrap items-center justify-between gap-2 py-1 text-xs">
                                                    <span className="min-w-0 truncate">
                                                        #{attempt.round} {attempt.channel_name || 'Channel'}
                                                        {attempt.upstream_model && (
                                                            <span className="ml-1 font-mono text-[11px] text-muted-foreground">
                                                                ({attempt.upstream_model})
                                                            </span>
                                                        )}
                                                    </span>
                                                    <span className="shrink-0 tabular-nums text-muted-foreground">
                                                        {attempt.status} {attempt.latency_ms !== undefined ? `${attempt.latency_ms}ms` : ''}
                                                    </span>
                                                    {attempt.error_msg && (
                                                        <p className="w-full break-words text-[11px] text-destructive">{attempt.error_msg}</p>
                                                    )}
                                                </div>
                                            ))}
                                        </div>
                                    )}
                                </div>
                            </details>
                            {isAdmin && (
                                <Button
                                    variant="ghost"
                                    size="icon"
                                    className="size-10 shrink-0 text-destructive transition-all duration-200 hover:scale-105 hover:text-destructive sm:size-8"
                                    disabled={stopping}
                                    onClick={(event) => { event.stopPropagation(); abortRunning(state); }}
                                    title={t('stop')}
                                    aria-label={t('stop')}
                                    aria-busy={stopping}
                                >
                                    {stopping ? <Loader2 className="size-4 animate-spin sm:size-3.5" /> : <X className="size-4 transition-transform duration-200 hover:scale-110 sm:size-3.5" />}
                                </Button>
                            )}
                        </div>
                    );
    };

    const totalCount = runningStates.length + activeInterventions.length;
    const rescueCount = activeInterventions.length;
    const retryingCount = runningStates.filter((state) => Boolean(state.intervention_id) || state.round > 1).length;

    return (
        <section aria-label={t('activeActivity')} className={cn('live-activity-panel flex min-h-0 flex-none flex-col rounded-lg border border-border bg-card', collapsed ? 'px-3 py-1' : 'gap-2 p-3')}>
            <div className={cn('flex items-center justify-between gap-2', !collapsed && 'border-b border-border pb-2')}>
                <button
                    type="button"
                    onClick={toggleCollapsed}
                    aria-expanded={!collapsed}
                    aria-label={collapsed ? t('expand') : t('collapse')}
                    title={collapsed ? t('expand') : t('collapse')}
                    className="-mx-1 flex min-h-9 min-w-0 flex-1 items-center gap-2 rounded-md px-1 text-left text-sm font-medium outline-none transition-colors hover:bg-muted/50 focus-visible:ring-2 focus-visible:ring-ring max-sm:min-h-10"
                >
                    <ChevronDown className={cn('size-4 shrink-0 text-muted-foreground transition-transform duration-200', collapsed && '-rotate-90')} aria-hidden />
                    <Loader2 className="size-4 shrink-0 animate-spin text-sky-500" aria-hidden />
                    <span className="shrink-0">{t('activeActivity')}</span>
                    <Badge variant="secondary" className="shrink-0 tabular-nums px-1.5 py-0 text-xs">
                        {totalCount}
                    </Badge>
                    {collapsed && (rescueCount > 0 || retryingCount > 0) && (
                        <span className="flex min-w-0 items-center gap-1.5 truncate text-xs font-normal text-amber-700 dark:text-amber-300">
                            {rescueCount > 0 && <span className="shrink-0">{t('collapsedRescue', { count: rescueCount })}</span>}
                            {retryingCount > 0 && <span className="shrink-0">{t('collapsedRetrying', { count: retryingCount })}</span>}
                        </span>
                    )}
                </button>
                {!requestStateConnected && (
                    <span className="flex shrink-0 items-center gap-1 text-xs text-amber-600 dark:text-amber-400">
                        <WifiOff className="size-3" />
                        {t('reconnecting')}
                    </span>
                )}
            </div>

            <style jsx>{`
                .live-activity-panel details > summary {
                    user-select: none;
                    -webkit-tap-highlight-color: transparent;
                }
                .live-activity-panel details[open] > summary {
                    background-color: color-mix(in srgb, var(--muted) 50%, transparent);
                }
                .live-activity-panel details > .details-content {
                    animation: slideDown 250ms cubic-bezier(0.4, 0, 0.2, 1);
                }
                @keyframes slideDown {
                    from {
                        opacity: 0;
                        transform: translateY(-8px);
                        max-height: 0;
                    }
                    to {
                        opacity: 1;
                        transform: translateY(0);
                        max-height: 500px;
                    }
                }
                @media (max-width: 640px) {
                    .live-activity-panel details > .details-content {
                        font-size: 0.8125rem;
                    }
                }
                @media (prefers-reduced-motion: reduce) {
                    .live-activity-panel details > .details-content {
                        animation: none;
                    }
                    .live-activity-panel details > summary,
                    .live-activity-panel details > summary :global(svg) {
                        transition: none;
                    }
                    .live-activity-panel details > summary:active {
                        transform: none;
                        scale: none;
                    }
                }
            `}</style>
            {!collapsed && (
            <div ref={scrollParentRef} className={fullPage ? "h-full space-y-1.5 overflow-y-auto overscroll-contain divide-y divide-border/60" : "max-h-60 space-y-1.5 overflow-y-auto overscroll-contain divide-y divide-border/60 sm:max-h-48"}>
                {/* 救援中请求 (仅管理员可见) */}
                {activeInterventions.map((intervention) => {
                    const draft = drafts[intervention.id] ?? { channelID: '', keyID: '', modelName: '' };
                    const selectedChannel = enabledChannels.find((channel) => channel.id === Number(draft.channelID));
                    const enabledKeys = selectedChannel?.keys?.filter((key) => key.enabled) ?? [];
                    const stopping = abortIntervention.isPending && abortIntervention.variables === intervention.id;

                    return (
                        <div key={intervention.id} className="flex items-start gap-2 pt-1.5 first:pt-0">
                            <details className="group min-w-0 flex-1">
                                <summary className="grid min-h-10 min-w-0 cursor-pointer list-none grid-cols-[minmax(0,1fr)_auto] items-center gap-x-2 gap-y-1 rounded-md px-2 py-1.5 transition-[background-color,box-shadow,transform] duration-200 hover:bg-muted/70 hover:shadow-sm active:scale-[0.99] outline-none focus-visible:ring-2 focus-visible:ring-ring sm:min-h-8 sm:grid-cols-[minmax(0,1fr)_auto_auto] sm:gap-x-3 sm:px-1.5 sm:py-1">
                                    <span className="flex min-w-0 items-center gap-1.5">
                                        <ChevronRight className="size-3.5 shrink-0 text-muted-foreground transition-transform duration-200 ease-out group-open:rotate-90 group-hover:text-foreground" />
                                        <Badge variant="outline" className="shrink-0 border-amber-500/40 bg-amber-500/10 text-[10px] text-amber-700 dark:text-amber-300">
                                            {t('rescue')}
                                        </Badge>
                                        <span className="truncate font-mono text-xs" title={intervention.request_model}>
                                            {intervention.request_model || 'unknown'}
                                        </span>
                                    </span>
                                    <span className="text-xs text-amber-600 dark:text-amber-300">
                                        {t('retryRound', { count: intervention.rescue_round ?? 1 })}
                                    </span>
                                    <span className="col-span-2 text-xs tabular-nums text-muted-foreground sm:col-span-1 sm:text-right">
                                        {intervention.waiting_for}
                                    </span>
                                </summary>
                                <div className="details-content space-y-2 px-3 pb-2 pt-1 text-xs sm:px-2">
                                    <p className="text-muted-foreground">{intervention.endpoint} · {intervention.id}{intervention.status ? ` · ${intervention.status}` : ''}</p>
                                    {intervention.next_retry_at && (
                                        <p className="tabular-nums text-muted-foreground">下次重试 {new Date(intervention.next_retry_at).toLocaleTimeString()}</p>
                                    )}
                                    {intervention.last_error && (
                                        <p className="break-words text-destructive">{intervention.last_error}</p>
                                    )}
                                    {intervention.attempts?.length > 0 && (
                                        <div className="divide-y divide-border/60 rounded border border-border/60 bg-muted/20 px-2 py-1">
                                            {intervention.attempts.slice(-3).map((attempt, index) => (
                                                <div key={`${attempt.channel_id}-${index}`} className="flex min-w-0 flex-wrap justify-between gap-2 py-1">
                                                    <span className="min-w-0 truncate">
                                                        {attempt.channel_name || `ch#${attempt.channel_id}`} · {attempt.status}
                                                    </span>
                                                    <span className="tabular-nums text-muted-foreground">{attempt.duration}ms</span>
                                                </div>
                                            ))}
                                        </div>
                                    )}
                                    <details className="space-y-2">
                                        <summary className="cursor-pointer font-medium text-muted-foreground hover:text-foreground">
                                            {t('override')}
                                        </summary>
                                        <div className="grid grid-cols-1 gap-2 pt-1 sm:grid-cols-2">
                                            <select
                                                aria-label={t('channel')}
                                                value={draft.channelID}
                                                onChange={(event) => updateDraft(intervention.id, { channelID: event.target.value, keyID: '' })}
                                                className="h-8 min-w-0 rounded-md border border-input bg-background px-2 text-xs"
                                            >
                                                <option value="">{t('channel')}</option>
                                                {enabledChannels.map((channel) => (
                                                    <option key={channel.id} value={channel.id}>{channel.name}</option>
                                                ))}
                                            </select>
                                            <select
                                                aria-label={t('key')}
                                                value={draft.keyID}
                                                onChange={(event) => updateDraft(intervention.id, { keyID: event.target.value })}
                                                disabled={!selectedChannel}
                                                className="h-8 min-w-0 rounded-md border border-input bg-background px-2 text-xs"
                                            >
                                                <option value="">{t('key')}</option>
                                                {enabledKeys.map((key) => (
                                                    <option key={key.id} value={key.id}>{key.remark || `Key #${key.id}`}</option>
                                                ))}
                                            </select>
                                            <input
                                                aria-label={t('model')}
                                                value={draft.modelName}
                                                onChange={(event) => updateDraft(intervention.id, { modelName: event.target.value })}
                                                placeholder={intervention.request_model}
                                                className="h-8 min-w-0 rounded-md border border-input bg-background px-2 text-xs"
                                            />
                                            <Button
                                                size="sm"
                                                variant="outline"
                                                className="h-8"
                                                onClick={() => retryHeldRequest(intervention)}
                                                disabled={retryIntervention.isPending && retryIntervention.variables?.id === intervention.id}
                                            >
                                                <RotateCw className="size-3.5" />
                                                {t('retry')}
                                            </Button>
                                        </div>
                                    </details>
                                </div>
                            </details>
                            <Button
                                variant="ghost"
                                size="icon"
                                className="size-10 shrink-0 text-destructive transition-all duration-200 hover:scale-105 hover:text-destructive sm:size-8"
                                disabled={stopping}
                                onClick={(event) => { event.stopPropagation(); abortHeldRequest(intervention); }}
                                title={t('stop')}
                                aria-label={t('stop')}
                                aria-busy={stopping}
                            >
                                {stopping ? <Loader2 className="size-4 animate-spin sm:size-3.5" /> : <X className="size-4 transition-transform duration-200 hover:scale-110 sm:size-3.5" />}
                            </Button>
                        </div>
                    );
                })}

                {/* 正在运行中的请求（整页形态虚拟化：只渲染可视行，占位高度撑出滚动区间） */}
                {fullPage ? (
                    <div style={{ height: rowVirtualizer.getTotalSize(), position: 'relative' }}>
                        {rowVirtualizer.getVirtualItems().map((virtualRow) => {
                            const state = runningStates[virtualRow.index];
                            if (!state) return null;
                            return (
                                <div
                                    key={state.id}
                                    ref={rowVirtualizer.measureElement}
                                    data-index={virtualRow.index}
                                    style={{ position: 'absolute', top: 0, left: 0, width: '100%', transform: `translateY(${virtualRow.start}px)` }}
                                >
                                    {renderStateRow(state)}
                                </div>
                            );
                        })}
                    </div>
                ) : runningStates.map((state) => renderStateRow(state))}
            </div>
            )}
        </section>
    );
}

