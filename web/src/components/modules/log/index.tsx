'use client';

import { useCallback, useDeferredValue, useEffect, useMemo, useState } from 'react';
import { getRelayLogSeverity, type RelayLog, type RelayLogSeverity, type RequestState, useExportLogs, useLogSeverityCounts, useLogs, useRequestStateStream } from '@/api/endpoints/log';
import { LogCard, useSensitiveStore } from './Item';
import { AlertCircle, AlertTriangle, CheckCircle2, ChevronDown, ChevronLeft, ChevronRight, ChevronUp, Circle, Download, Eye, EyeOff, Loader2, RefreshCw, RotateCcw, RotateCw, ScrollText, Search, SlidersHorizontal, WifiOff, X } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { VirtualizedGrid } from '@/components/common/VirtualizedGrid';
import { PageWrapper } from '@/components/common/PageWrapper';
import { MobileFilterCollapse } from '@/components/common/MobileFilterCollapse';
import { useAPIKeyList } from '@/api/endpoints/apikey';
import { useChannelList } from '@/api/endpoints/channel';
import { useModelList } from '@/api/endpoints/model';
import { useAbortIntervention, useAbortRunningRequest, useRescueRunningRequest, useInterventionList, useRetryIntervention, type InterventionSnapshot } from '@/api/endpoints/intervention';
import { useAuthStore, useUserList } from '@/api/endpoints/user';
import { Button } from '@/components/ui/button';
import { Badge } from '@/components/ui/badge';
import { cn } from '@/lib/utils';
import { toast } from '@/components/common/Toast';
import { TooltipProvider } from '@/components/animate-ui/components/animate/tooltip';
import { useSettingList, useSetSetting, SettingKey } from '@/api/endpoints/setting';

type LogSeverityFilter = RelayLogSeverity | 'all';
type LogDateRangeShortcut = 'today' | 'last7Days' | 'lastMonth' | 'all';

const severityFilters: Array<{ id: LogSeverityFilter; icon: typeof Circle; className: string }> = [
    { id: 'all', icon: Circle, className: 'text-muted-foreground' },
    { id: 'success', icon: CheckCircle2, className: 'text-emerald-600 dark:text-emerald-400' },
    { id: 'warn', icon: AlertCircle, className: 'text-amber-600 dark:text-amber-300' },
    { id: 'error', icon: AlertCircle, className: 'text-destructive' },
];

// value = endpoint family prefix; backend matches by family (exact or
// "<value>_<variant>"), so e.g. 'gemini' catches gemini_generate_content /
// gemini_stream_generate_content, 'images' catches images_generations, etc.
const endpointFilters = [
    { value: '', label: '全部端点' },
    { value: 'chat', label: 'chat' },
    { value: 'responses', label: 'responses' },
    { value: 'messages', label: 'messages' },
    { value: 'gemini', label: 'gemini' },
    { value: 'embeddings', label: 'embeddings' },
    { value: 'images', label: 'images' },
    { value: 'audio', label: 'audio' },
    { value: 'videos', label: 'videos' },
    { value: 'completions', label: 'completions' },
    { value: 'edits', label: 'edits' },
    { value: 'moderations', label: 'moderations' },
    { value: 'rerank', label: 'rerank' },
    { value: 'model_test_chat', label: 'model test chat' },
    { value: 'model_test_responses', label: 'model test responses' },
    { value: 'model_test_anthropic_messages', label: 'model test messages' },
    { value: 'model_test_gemini', label: 'model test gemini' },
] as const;

// Unified provider taxonomy options
const providerFilters = [
    { value: '', label: '全部厂商' },
    { value: 'openai', label: 'OpenAI (GPT/o1/o3/o4)' },
    { value: 'anthropic', label: 'Anthropic (Claude)' },
    { value: 'google', label: 'Google (Gemini)' },
    { value: 'deepseek', label: 'DeepSeek' },
    { value: 'xai', label: 'xAI (Grok)' },
    { value: 'alibaba', label: 'Alibaba (Qwen/通义)' },
    { value: 'zhipuai', label: 'Zhipu (GLM/智谱)' },
    { value: 'minimax', label: 'MiniMax (海螺)' },
    { value: 'moonshotai', label: 'Moonshot (Kimi)' },
    { value: 'mistral', label: 'Mistral' },
    { value: 'meta', label: 'Meta (Llama)' },
    { value: 'bytedance', label: 'ByteDance (豆包)' },
    { value: 'baidu', label: 'Baidu (文心)' },
    { value: 'tencent', label: 'Tencent (混元)' },
] as const;

const dateRangeShortcuts: Array<{ id: LogDateRangeShortcut; label: string }> = [
    { id: 'today', label: '今天' },
    { id: 'last7Days', label: '7天' },
    { id: 'lastMonth', label: '1个月' },
    { id: 'all', label: '全部' },
];

function localDateInput(date: Date) {
    const pad = (n: number) => String(n).padStart(2, '0');
    return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
}

function parseLocalDateInput(value: string) {
    if (!value) return undefined;
    const [year, month, day] = value.split('-').map(Number);
    if (!Number.isFinite(year) || !Number.isFinite(month) || !Number.isFinite(day)) return undefined;
    return new Date(year, month - 1, day);
}

function addLocalDays(date: Date, days: number) {
    const nextDate = new Date(date);
    nextDate.setDate(nextDate.getDate() + days);
    return nextDate;
}

function addLocalMonths(date: Date, months: number) {
    const nextDate = new Date(date);
    const originalDay = nextDate.getDate();
    nextDate.setDate(1);
    nextDate.setMonth(nextDate.getMonth() + months);
    const lastDayOfTargetMonth = new Date(nextDate.getFullYear(), nextDate.getMonth() + 1, 0).getDate();
    nextDate.setDate(Math.min(originalDay, lastDayOfTargetMonth));
    return nextDate;
}

function resolveDateRangeShortcut(shortcut: LogDateRangeShortcut, todayLabel: string) {
    const todayDate = parseLocalDateInput(todayLabel) ?? new Date();

    if (shortcut === 'all') {
        return { startDate: '', endDate: '' };
    }

    if (shortcut === 'last7Days') {
        return { startDate: localDateInput(addLocalDays(todayDate, -6)), endDate: todayLabel };
    }

    if (shortcut === 'lastMonth') {
        return { startDate: localDateInput(addLocalMonths(todayDate, -1)), endDate: todayLabel };
    }

    return { startDate: todayLabel, endDate: todayLabel };
}

function startOfLocalDayUnix(value: string) {
    const date = parseLocalDateInput(value);
    if (!date) return undefined;
    date.setHours(0, 0, 0, 0);
    return Math.floor(date.getTime() / 1000);
}

function endOfLocalDayUnix(value: string) {
    const date = parseLocalDateInput(value);
    if (!date) return undefined;
    date.setHours(23, 59, 59, 999);
    return Math.floor(date.getTime() / 1000);
}

function resolveLogTimeRange(startDate: string, endDate: string) {
    if (!startDate && !endDate) {
        return { startTime: undefined, endTime: undefined };
    }

    const resolvedStartDate = startDate || endDate;
    const resolvedEndDate = endDate || startDate;

    return {
        startTime: startOfLocalDayUnix(resolvedStartDate),
        endTime: endOfLocalDayUnix(resolvedEndDate),
    };
}

/** 生效筛选的可删除小药丸：点 × 清掉这一维筛选，让「现在到底在看什么」一目了然。 */
function FilterPill({ label, onClear }: { label: string; onClear: () => void }) {
    return (
        <span className="inline-flex items-center gap-1 rounded-full border border-primary/30 bg-primary/10 py-0.5 pl-2.5 pr-1 text-xs font-medium text-primary">
            <span className="max-w-[14rem] truncate">{label}</span>
            <button
                type="button"
                onClick={onClear}
                aria-label="清除该筛选"
                className="grid size-4 place-items-center rounded-full text-primary/70 transition-colors hover:bg-primary/20 hover:text-primary"
            >
                <X className="size-3" />
            </button>
        </span>
    );
}


type InterventionDraft = {
    channelID: string;
    keyID: string;
    modelName: string;
};

interface LiveActivityPanelProps {
    isAdmin: boolean;
    enabled: boolean;
    requestStates: RequestState[];
    requestStateConnected: boolean;
    historyLogIDs: Set<number>;
}

function LiveActivityPanel({
    isAdmin,
    enabled,
    requestStates,
    requestStateConnected,
    historyLogIDs,
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

    useEffect(() => {
        if (!enabled || !isSuccess) return;
        const activeIDs = new Set(interventions.map((item) => item.id));
        setDrafts((previous) => {
            if (Object.keys(previous).every((id) => activeIDs.has(id))) return previous;
            return Object.fromEntries(Object.entries(previous).filter(([id]) => activeIDs.has(id)));
        });
    }, [enabled, interventions, isSuccess]);

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

    const totalCount = runningStates.length + activeInterventions.length;

    return (
        <section aria-label={t('activeActivity')} className="flex min-h-0 flex-none flex-col gap-2 rounded-lg border border-border bg-card p-3">
            <div className="flex items-center justify-between gap-2 border-b border-border pb-2">
                <div className="flex items-center gap-2 text-sm font-medium">
                    <Loader2 className="size-4 animate-spin text-sky-500" />
                    <span>{t('activeActivity')}</span>
                    <Badge variant="secondary" className="tabular-nums px-1.5 py-0 text-xs">
                        {totalCount}
                    </Badge>
                </div>
                {!requestStateConnected && (
                    <span className="flex items-center gap-1 text-xs text-amber-600 dark:text-amber-400">
                        <WifiOff className="size-3" />
                        {t('reconnecting')}
                    </span>
                )}
            </div>

            <div className="max-h-60 space-y-1.5 overflow-y-auto overscroll-contain divide-y divide-border/60">
                {/* 救援中请求 (仅管理员可见) */}
                {activeInterventions.map((intervention) => {
                    const draft = drafts[intervention.id] ?? { channelID: '', keyID: '', modelName: '' };
                    const selectedChannel = enabledChannels.find((channel) => channel.id === Number(draft.channelID));
                    const enabledKeys = selectedChannel?.keys?.filter((key) => key.enabled) ?? [];
                    const stopping = abortIntervention.isPending && abortIntervention.variables === intervention.id;

                    return (
                        <div key={intervention.id} className="flex items-start gap-2 pt-1.5 first:pt-0">
                            <details className="group min-w-0 flex-1">
                                <summary className="grid min-h-8 cursor-pointer list-none grid-cols-[minmax(0,1fr)_auto] items-center gap-x-3 gap-y-1 rounded-md px-1.5 py-1 transition-colors hover:bg-muted/50 outline-none focus-visible:ring-2 focus-visible:ring-ring sm:grid-cols-[minmax(0,1fr)_auto_auto]">
                                    <span className="flex min-w-0 items-center gap-1.5">
                                        <ChevronRight className="size-3.5 shrink-0 transition-transform group-open:rotate-90 text-muted-foreground" />
                                        <Badge variant="outline" className="border-amber-500/40 bg-amber-500/10 text-[10px] text-amber-700 dark:text-amber-300">
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
                                <div className="space-y-2 px-2 pb-2 pt-1 text-xs">
                                    <p className="text-muted-foreground">{intervention.endpoint} · {intervention.id}</p>
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
                                className="size-8 shrink-0 text-destructive"
                                disabled={stopping}
                                onClick={() => abortHeldRequest(intervention)}
                                title={t('stop')}
                                aria-label={t('stop')}
                            >
                                {stopping ? <Loader2 className="size-3.5 animate-spin" /> : <X className="size-3.5" />}
                            </Button>
                        </div>
                    );
                })}

                {/* 正在运行中的请求 */}
                {runningStates.map((state) => {
                    const stopping = abortingIDs.has(state.id);
                    const startedTimestamp = new Date(state.started_at).getTime();
                    const elapsedSeconds = Number.isFinite(startedTimestamp)
                        ? Math.max(0, Math.round(((state.finished_at ? new Date(state.finished_at).getTime() : Date.now()) - startedTimestamp) / 1000))
                        : null;

                    return (
                        <div key={state.id} className="flex items-start gap-2 py-1.5 first:pt-0">
                            <details className="group min-w-0 flex-1">
                                <summary className="grid min-h-8 cursor-pointer list-none grid-cols-[minmax(0,1fr)_auto] items-center gap-x-2 gap-y-1 rounded-md px-1.5 py-1 transition-colors hover:bg-muted/50 outline-none focus-visible:ring-2 focus-visible:ring-ring sm:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_auto]">
                                    <span className="flex min-w-0 items-center gap-1.5">
                                        <ChevronRight className="size-3.5 shrink-0 transition-transform group-open:rotate-90 text-muted-foreground" />
                                        <Badge variant="outline" className="border-sky-500/40 bg-sky-500/10 text-[10px] text-sky-700 dark:text-sky-300">
                                            {t('running')}
                                        </Badge>
                                        <span className="truncate font-mono text-xs" title={state.model}>
                                            {state.model || 'unknown'}
                                        </span>
                                    </span>
                                    <span className="truncate text-xs text-sky-600 dark:text-sky-300">
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
                                <div className="space-y-2 px-2 pb-2 pt-1 text-xs">
                                    {isAdmin && (
                                        <Button variant="outline" size="sm" className="h-7 text-xs"
                                            disabled={!state.rescuable || stopping || rescuingIDs.has(state.id)}
                                            onClick={() => rescueRunning(state)}
                                            title={state.rescuable ? '停止当前尝试，保留请求并交给自动救援' : '仅未开始返回内容的运行请求可转救援'}>
                                            {rescuingIDs.has(state.id) ? '正在转入…' : '转自动救援'}
                                        </Button>
                                    )}
                                    <div className="grid grid-cols-2 gap-2 rounded-md border border-border/60 bg-muted/20 p-2 sm:grid-cols-4">
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
                                                {elapsedSeconds !== null && <span className="text-muted-foreground"> · {elapsedSeconds}s</span>}
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

                                    {isAdmin && (state.attempts?.length ?? 0) > 0 && (
                                        <div className="divide-y divide-border/60 rounded border border-border/60 bg-muted/20 px-2 py-1">
                                            <div className="py-1 text-[11px] font-medium text-muted-foreground">
                                                渠道尝试 ({state.attempts?.length})
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
                                    className="size-8 shrink-0 text-destructive transition-colors hover:text-destructive"
                                    disabled={stopping}
                                    onClick={() => abortRunning(state)}
                                    title={t('stop')}
                                    aria-label={t('stop')}
                                >
                                    {stopping ? <Loader2 className="size-3.5 animate-spin" /> : <X className="size-3.5" />}
                                </Button>
                            )}
                        </div>
                    );
                })}
            </div>
        </section>
    );
}

/**
 * 日志页面组件
 * - 初始加载 pageSize 条历史日志
 * - SSE 实时推送新日志
 * - 滚动自动加载更多
 */
export function Log() {
    const t = useTranslations('log');
    const isAdmin = useAuthStore((state) => state.user?.role === 'admin');
    const todayLabel = useMemo(() => localDateInput(new Date()), []);
    // 默认区间放宽到「近 7 天」而非「今天」：日志常是前一两天产生的，默认只查今天会让页面一开屏就空、
    // 显得「筛选无效」。近 7 天在「够聚焦」和「开屏能看到东西」之间取平衡。
    const defaultRange = useMemo(() => resolveDateRangeShortcut('last7Days', todayLabel), [todayLabel]);
    const [viewMode, setViewMode] = useState<'history' | 'live'>('history');
    // 所有筛选都「即选即生效」——改了立刻查，不再需要点「搜索」。这正是过去让人觉得「筛选无效」的
    // 另一半原因：改了接口下拉/用户/Key 却要再点搜索才生效，看起来像没反应。
    const [selectedUserID, setSelectedUserID] = useState<number | undefined>();
    const [selectedAPIKeyID, setSelectedAPIKeyID] = useState<number | undefined>();
    const [selectedEndpoint, setSelectedEndpoint] = useState('');
    const [selectedProvider, setSelectedProvider] = useState('');
    const [selectedModel, setSelectedModel] = useState('');
    const [startDate, setStartDate] = useState(defaultRange.startDate);
    const [endDate, setEndDate] = useState(defaultRange.endDate);
    // 严重程度 + 「只看有重试」都是服务端过滤，翻页/总数都对得上。
    const [severityFilter, setSeverityFilter] = useState<LogSeverityFilter>('all');
    const [retriedOnly, setRetriedOnly] = useState(false);
    const [hideModelTest, setHideModelTest] = useState(false);
    const [searchKeyword, setSearchKeyword] = useState('');
    const deferredSearch = useDeferredValue(searchKeyword.trim());
    const isLiveMode = viewMode === 'live';
    const [advancedOpen, setAdvancedOpen] = useState(false);
    // 分页状态：当前页（从 1 开始）+ 跳页输入框草稿值。
    const [currentPage, setCurrentPage] = useState(1);
    const [pageJumpInput, setPageJumpInput] = useState('');
    const sensitiveVisible = useSensitiveStore((state) => state.sensitiveVisible);
    const setSensitiveVisible = useSensitiveStore((state) => state.setSensitiveVisible);
    const { data: users = [] } = useUserList({ enabled: isAdmin });
    const { data: apiKeys = [] } = useAPIKeyList();
    const { data: modelList = [] } = useModelList();
    const { data: channelRows = [] } = useChannelList({ enabled: isAdmin });
    const exportLogs = useExportLogs();
    const { states: requestStates, isConnected: requestStateConnected } = useRequestStateStream(isLiveMode);
    // 历史日志持久化状态：关闭时后端只留最近 ~100 条内存记录、重启即失，按日期查历史必然是空的。
    // 日志页过去对此零提示（静默显示内存缓存），用户会误以为“日志功能坏了”。这里显式暴露 + 一键开启。
    const { data: settings } = useSettingList({ enabled: isAdmin });
    const setSetting = useSetSetting();
    const persistenceOff = isAdmin && (settings?.some((s) => s.key === SettingKey.RelayLogKeepEnabled && s.value === 'false') ?? false);
    const handleEnablePersistence = useCallback(() => {
        setSetting.mutate(
            { key: SettingKey.RelayLogKeepEnabled, value: 'true' },
            {
                onSuccess: () => toast.success('已开启历史日志持久化，此后新日志会写入数据库、可按日期回查'),
                onError: (e) => toast.error(e instanceof Error ? e.message : String(e)),
            }
        );
    }, [setSetting]);

    const apiKeysForSelectedUser = useMemo(() => {
        return apiKeys
            .filter((apiKey) => !selectedUserID || apiKey.user_id === selectedUserID)
            .sort((a, b) => a.name.localeCompare(b.name));
    }, [apiKeys, selectedUserID]);

    const selectedAPIKey = useMemo(() => {
        if (!selectedAPIKeyID) return undefined;
        return apiKeys.find((apiKey) => apiKey.id === selectedAPIKeyID);
    }, [apiKeys, selectedAPIKeyID]);

    const effectiveSelectedAPIKeyID = selectedAPIKey && (!selectedUserID || selectedAPIKey.user_id === selectedUserID)
        ? selectedAPIKeyID
        : undefined;
    const { startTime, endTime } = resolveLogTimeRange(startDate, endDate);

    // 全量严重程度计数（成功/警告/错误 + 总数）。与列表查询共享过滤参数，不含
    // page/page_size/severity，所以徽章数字与分页页数都是"全量"、不受当前页限制。
    const { data: severityCounts } = useLogSeverityCounts({
        userID: selectedUserID,
        apiKeyID: effectiveSelectedAPIKeyID,
        endpoint: selectedEndpoint || undefined,
        provider: selectedProvider || undefined,
        model: selectedModel || undefined,
        startTime,
        endTime,
        retried: retriedOnly,
        hideModelTest,
        search: deferredSearch || undefined,
    });
    const LOG_PAGE_SIZE = 20;
    // 当前生效筛选下的总条数：全部→total，否则取该严重程度的计数。分页页数据此算。
    const activeTotal = severityFilter === 'all'
        ? (severityCounts?.total ?? 0)
        : (severityCounts?.[severityFilter] ?? 0);
    const totalPages = Math.max(1, Math.ceil(activeTotal / LOG_PAGE_SIZE));

    const {
        logs,
        hasMore,
        isLoading,
        isLoadingMore,
        isRefreshing,
        isConnected,
        error: streamError,
        loadMore,
        refresh,
    } = useLogs({
        pageSize: LOG_PAGE_SIZE,
        // 始终分页；实时刷新只是往第 1 页实时插入新日志，分页导航永远保留。
        page: currentPage,
        // 严重程度改为服务端过滤：翻页/总数都对得上，不再是"只筛当前页"。
        severity: severityFilter === 'all' ? undefined : severityFilter,
        userID: selectedUserID,
        apiKeyID: effectiveSelectedAPIKeyID,
        endpoint: selectedEndpoint || undefined,
        provider: selectedProvider || undefined,
        model: selectedModel || undefined,
        startTime,
        endTime,
        retried: retriedOnly,
        hideModelTest,
        search: deferredSearch || undefined,
        // 实时刷新只在实时模式 + 第 1 页（最新）生效；翻到历史页自然暂停，回第 1 页恢复。
        live: isLiveMode && currentPage === 1,
    });

    // 收集当前可用模型候选：来自 modelList、channelRows、当前选中的模型、以及当前已经拉到的日志里的模型名
    const availableModelOptions = useMemo(() => {
        const set = new Set<string>();
        if (selectedModel?.trim()) {
            set.add(selectedModel.trim());
        }
        for (const m of modelList) {
            if (m.name?.trim()) set.add(m.name.trim());
        }
        for (const ch of channelRows) {
            const raw = ch.raw;
            for (const m of raw.selected_models || []) {
                if (m?.trim()) set.add(m.trim());
            }
            for (const m of raw.discovered_models || []) {
                if (m?.trim()) set.add(m.trim());
            }
        }
        for (const l of logs) {
            if (l.request_model_name?.trim()) set.add(l.request_model_name.trim());
            if (l.actual_model_name?.trim()) set.add(l.actual_model_name.trim());
        }
        return Array.from(set).sort((a, b) => a.localeCompare(b));
    }, [channelRows, logs, modelList, selectedModel]);

    const handleSelectUser = (value: string) => {
        const nextUserID = Number(value) || undefined;
        setSelectedUserID(nextUserID);
        // 换了用户但当前选中的 Key 不属于他 → 清掉 Key，避免矛盾筛选。
        if (selectedAPIKey && nextUserID && selectedAPIKey.user_id !== nextUserID) {
            setSelectedAPIKeyID(undefined);
        }
        setCurrentPage(1);
    };

    const handleSelectAPIKey = (value: string) => {
        const nextAPIKeyID = Number(value) || undefined;
        setSelectedAPIKeyID(nextAPIKeyID);
        const nextAPIKey = apiKeys.find((apiKey) => apiKey.id === nextAPIKeyID);
        if (nextAPIKey?.user_id) setSelectedUserID(nextAPIKey.user_id);
        setCurrentPage(1);
    };

    const handleSelectEndpoint = (value: string) => {
        setSelectedEndpoint(value);
        setCurrentPage(1);
    };

    const handleSelectProvider = (value: string) => {
        setSelectedProvider(value);
        setCurrentPage(1);
    };

    const handleSelectModel = (value: string) => {
        setSelectedModel(value);
        setCurrentPage(1);
    };

    const handleStartDate = (value: string) => { setStartDate(value); setCurrentPage(1); };
    const handleEndDate = (value: string) => { setEndDate(value); setCurrentPage(1); };

    const applyDateRangeShortcut = useCallback((shortcut: LogDateRangeShortcut) => {
        const nextRange = resolveDateRangeShortcut(shortcut, todayLabel);
        setStartDate(nextRange.startDate);
        setEndDate(nextRange.endDate);
        setCurrentPage(1);
    }, [todayLabel]);

    // 所有筛选一键回默认（近 7 天、不限用户/Key/端点/厂商/模型、全部状态、不限重试、清空搜索）。
    const handleResetFilters = useCallback(() => {
        setSelectedUserID(undefined);
        setSelectedAPIKeyID(undefined);
        setSelectedEndpoint('');
        setSelectedProvider('');
        setSelectedModel('');
        setSeverityFilter('all');
        setRetriedOnly(false);
        setHideModelTest(false);
        setSearchKeyword('');
        setStartDate(defaultRange.startDate);
        setEndDate(defaultRange.endDate);
        setCurrentPage(1);
    }, [defaultRange]);

    const isDefaultRange = startDate === defaultRange.startDate && endDate === defaultRange.endDate;
    // 有任何非默认筛选在生效（决定是否显示「重置」+ 空状态提示是否算「被筛掉」）。
    const hasActiveFilter =
        !!selectedEndpoint ||
        !!selectedProvider ||
        !!selectedModel ||
        !!selectedUserID ||
        !!effectiveSelectedAPIKeyID ||
        severityFilter !== 'all' ||
        retriedOnly ||
        hideModelTest ||
        !!searchKeyword.trim() ||
        !isDefaultRange;

    // 「生效筛选」药丸：把当前每一维筛选摊开成一个可一键删除的小标签，让人清楚现在到底在看什么。
    const activePills: Array<{ key: string; label: string; onClear: () => void }> = [];
    if (!startDate && !endDate) {
        activePills.push({ key: 'alldate', label: '全部日期', onClear: () => { setStartDate(defaultRange.startDate); setEndDate(defaultRange.endDate); setCurrentPage(1); } });
    } else if (!isDefaultRange) {
        const dl = startDate && endDate ? (startDate === endDate ? startDate : `${startDate} ~ ${endDate}`) : (startDate || endDate);
        activePills.push({ key: 'date', label: `日期 ${dl}`, onClear: () => { setStartDate(defaultRange.startDate); setEndDate(defaultRange.endDate); setCurrentPage(1); } });
    }
    if (searchKeyword.trim()) activePills.push({ key: 'search', label: `搜索 "${searchKeyword.trim()}"`, onClear: () => { setSearchKeyword(''); setCurrentPage(1); } });
    if (selectedEndpoint) activePills.push({ key: 'ep', label: `接口 ${selectedEndpoint}`, onClear: () => { setSelectedEndpoint(''); setCurrentPage(1); } });
    if (selectedProvider) activePills.push({ key: 'prov', label: `厂商 ${providerFilters.find((p) => p.value === selectedProvider)?.label ?? selectedProvider}`, onClear: () => { setSelectedProvider(''); setCurrentPage(1); } });
    if (selectedModel) activePills.push({ key: 'mod', label: `模型 ${selectedModel}`, onClear: () => { setSelectedModel(''); setCurrentPage(1); } });
    if (severityFilter !== 'all') activePills.push({ key: 'sev', label: `状态 ${t(`list.filters.${severityFilter}`)}`, onClear: () => { setSeverityFilter('all'); setCurrentPage(1); } });
    if (retriedOnly) activePills.push({ key: 'retry', label: t('list.retriedOnly'), onClear: () => { setRetriedOnly(false); setCurrentPage(1); } });
    if (hideModelTest) activePills.push({ key: 'hidetest', label: '隐藏渠道测试探针', onClear: () => { setHideModelTest(false); setCurrentPage(1); } });
    if (selectedUserID) activePills.push({ key: 'user', label: `用户 ${users.find((u) => u.id === selectedUserID)?.username ?? selectedUserID}`, onClear: () => { setSelectedUserID(undefined); setCurrentPage(1); } });
    if (effectiveSelectedAPIKeyID) activePills.push({ key: 'key', label: `Key ${selectedAPIKey?.name ?? effectiveSelectedAPIKeyID}`, onClear: () => { setSelectedAPIKeyID(undefined); setCurrentPage(1); } });

    const activeDateShortcut = useMemo(() => {
        return dateRangeShortcuts.find((shortcut) => {
            const shortcutRange = resolveDateRangeShortcut(shortcut.id, todayLabel);
            return shortcutRange.startDate === startDate && shortcutRange.endDate === endDate;
        })?.id;
    }, [endDate, startDate, todayLabel]);

    // 高级筛选（用户 / API Key）里有几项在生效——给折叠按钮上挂计数。
    const advancedActiveCount = (selectedUserID ? 1 : 0) + (effectiveSelectedAPIKeyID ? 1 : 0);

    // 徽章数字：全部→total，其余取对应严重程度的全量计数（后端返回，非当前页）。
    const badgeCount = useCallback((id: LogSeverityFilter): number | undefined => {
        if (!severityCounts) return undefined;
        return id === 'all' ? severityCounts.total : severityCounts[id];
    }, [severityCounts]);

    // 服务端已按 severity 过滤；这里再做一遍本地过滤仅作实时插入时的显示兜底。
    const filteredLogs = useMemo(() => {
        return logs.filter((log) => {
            if (selectedEndpoint) {
                const stored = log.request_endpoint?.trim() ?? '';
                if (stored !== selectedEndpoint && !stored.startsWith(`${selectedEndpoint}_`)) {
                    return false;
                }
            }
            if (severityFilter !== 'all' && getRelayLogSeverity(log) !== severityFilter) {
                return false;
            }
            if (retriedOnly) {
                const attemptCount = log.total_attempts ?? log.attempts?.length ?? 0;
                if (attemptCount <= 1) return false;
            }
            if (hideModelTest && (log.request_endpoint?.trim() ?? '').startsWith('model_test')) {
                return false;
            }
            return true;
        });
    }, [hideModelTest, logs, retriedOnly, selectedEndpoint, severityFilter]);


    // 当前历史日志中已包含的 log ID 集合，供 LiveActivityPanel 过滤救援列表（避免已落库日志的救援条目重复呈现）
    const historyLogIDs = useMemo(() => {
        const set = new Set<number>();
        for (const log of logs) {
            if (typeof log.id === 'number') set.add(log.id);
        }
        return set;
    }, [logs]);

    // 空状态智能提示的依据：列表真空时，查一下「不限日期」下总共有多少历史，
    // 好把「不是坏了、是被日期/筛选挡住了」说破。仅在真的空时才发这个请求。
    const listIsEmpty = !isLoading && filteredLogs.length === 0;
    const { data: allTimeCounts } = useLogSeverityCounts({
        userID: selectedUserID,
        apiKeyID: effectiveSelectedAPIKeyID,
        endpoint: selectedEndpoint || undefined,
        provider: selectedProvider || undefined,
        model: selectedModel || undefined,
        retried: retriedOnly,
        hideModelTest,
        enabled: listIsEmpty,
    });

    const canLoadMore = hasMore && !isLoading && !isLoadingMore && logs.length > 0;
    const handleReachEnd = useCallback(() => {
        if (!canLoadMore) return;
        void loadMore();
    }, [canLoadMore, loadMore]);
    const handleRefresh = useCallback(() => {
        void refresh();
    }, [refresh]);
    const handleExport = useCallback(() => {
        exportLogs.mutate(
            {
                start_time: startTime,
                end_time: endTime,
                user_id: selectedUserID,
                api_key_id: effectiveSelectedAPIKeyID,
                endpoint: selectedEndpoint || undefined,
                provider: selectedProvider || undefined,
                model: selectedModel || undefined,
                search: deferredSearch || undefined,
                severity: severityFilter === 'all' ? undefined : severityFilter,
                retried: retriedOnly,
                hide_model_test: hideModelTest,
            },
            {
                onSuccess: () => toast.success('日志已导出'),
                onError: (error) => toast.error('日志导出失败', { description: error instanceof Error ? error.message : String(error) }),
            }
        );
    }, [deferredSearch, effectiveSelectedAPIKeyID, endTime, exportLogs, hideModelTest, retriedOnly, selectedEndpoint, selectedModel, selectedProvider, selectedUserID, severityFilter, startTime]);

    const renderLogCard = useCallback((log: RelayLog) => <LogCard log={log} />, []);
    const getLogRowKey = useCallback((log: RelayLog) => log.id, []);

    const footer = useMemo(() => {
        if (isLoading || isLoadingMore) {
            return (
                <div className="flex justify-center py-4">
                    <Loader2 className="h-6 w-6 animate-spin text-muted-foreground" />
                </div>
            );
        }
        if (filteredLogs.length === 0) {
            const historyTotal = allTimeCounts?.total ?? 0;
            // 当前范围空、但不限日期时其实有货 → 说破「被日期/筛选挡住了」并给一键放开。
            const hiddenByFilter = historyTotal > 0;
            return (
                <div className="flex flex-col items-center justify-center gap-3 rounded-xl border border-dashed border-border bg-card/50 px-4 py-10 text-center">
                    <ScrollText className="size-8 text-muted-foreground/40" />
                    <div className="space-y-1">
                        <p className="text-sm font-medium text-foreground">
                            {hiddenByFilter ? '当前筛选条件下暂无日志' : '还没有任何日志'}
                        </p>
                        <p className="text-xs text-muted-foreground">
                            {hiddenByFilter
                                ? `不是坏了 —— 共有 ${historyTotal.toLocaleString()} 条记录，只是不在当前范围。`
                                : '有请求经过时会自动出现在这里。'}
                        </p>
                    </div>
                    {hiddenByFilter && (
                        <div className="flex flex-wrap items-center justify-center gap-2">
                            <Button variant="default" size="sm" className="rounded-lg" onClick={() => { setStartDate(''); setEndDate(''); setCurrentPage(1); }}>
                                查看全部
                            </Button>
                            {hasActiveFilter && (
                                <Button variant="outline" size="sm" className="rounded-lg" onClick={handleResetFilters}>
                                    <RotateCcw className="size-4" />
                                    重置筛选
                                </Button>
                            )}
                        </div>
                    )}
                    {hasMore && (
                        <Button variant="ghost" size="sm" onClick={() => void loadMore()}>
                            {t('list.loadMoreForFilter')}
                        </Button>
                    )}
                </div>
            );
        }
        // 始终分页：导航交给分页控件；实时模式下也不显示"已全部加载"（还有新日志会来）。
        return null;
    }, [allTimeCounts?.total, filteredLogs.length, hasActiveFilter, hasMore, handleResetFilters, isLoading, isLoadingMore, loadMore, t]);

    return (
        // Bottom clearance for the fixed mobile nav lives inside VirtualizedGrid's
        // scroller (not here). Outer pb on an overflow-hidden flex shell only shrinks
        // the viewport or gets clipped — last log cards still sit under the nav pill.
        <PageWrapper className="box-border flex h-full min-h-0 flex-col gap-3 overflow-hidden rounded-t-3xl [&>*]:min-h-0 [&>*:last-child]:flex [&>*:last-child]:min-h-0 [&>*:last-child]:flex-1 [&>*:last-child]:flex-col">
            {persistenceOff && (
                <div className="flex flex-none flex-col gap-2 rounded-lg border border-amber-500/40 bg-amber-500/10 px-3 py-2.5 text-sm sm:flex-row sm:items-center sm:justify-between">
                    <div className="flex items-start gap-2 text-amber-700 dark:text-amber-300">
                        <AlertTriangle className="mt-0.5 size-4 shrink-0" />
                        <span>
                            历史日志持久化<b className="font-semibold">未开启</b>：当前只显示最近内存记录（约 100 条，重启即丢），按日期查历史会是空的。
                        </span>
                    </div>
                    <Button
                        variant="outline"
                        size="sm"
                        onClick={handleEnablePersistence}
                        disabled={setSetting.isPending}
                        className="shrink-0 rounded-lg border-amber-500/50 text-amber-700 hover:bg-amber-500/10 dark:text-amber-300"
                    >
                        {setSetting.isPending ? '开启中…' : '开启持久化'}
                    </Button>
                </div>
            )}
            <div className="flex flex-none flex-col gap-2 rounded-lg border border-border bg-card px-3 py-2">
                {/* Row 1: 历史/实时切换 */}
                <div className="flex min-w-0 items-center gap-2">
                    <div className="flex min-w-0 items-center gap-1 rounded-lg bg-muted/60 p-1">
                        <button
                            type="button"
                            aria-pressed={viewMode === 'history'}
                            onClick={() => setViewMode('history')}
                            className={cn(
                                'inline-flex h-8 items-center gap-1.5 rounded-lg px-3 text-sm font-medium transition-colors',
                                viewMode === 'history'
                                    ? 'bg-background text-foreground shadow-sm'
                                    : 'text-muted-foreground hover:bg-background/60 hover:text-foreground'
                            )}
                        >
                            <ScrollText className="size-3.5" />
                            <span>历史日志</span>
                        </button>
                        <button
                            type="button"
                            aria-pressed={isLiveMode}
                            onClick={() => {
                                setViewMode('live');
                                setCurrentPage(1);
                            }}
                            className={cn(
                                'inline-flex h-8 items-center gap-1.5 rounded-lg px-3 text-sm font-medium transition-colors',
                                isLiveMode
                                    ? 'bg-background text-foreground shadow-sm'
                                    : 'text-muted-foreground hover:bg-background/60 hover:text-foreground'
                            )}
                        >
                            <span className="relative flex size-2">
                                {isLiveMode && isConnected && (
                                    <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-emerald-400 opacity-75" />
                                )}
                                <span
                                    className={cn(
                                        'relative inline-flex size-2 rounded-full',
                                        isLiveMode
                                            ? isConnected
                                                ? 'bg-emerald-500'
                                                : streamError
                                                    ? 'bg-destructive'
                                                    : 'bg-amber-500'
                                            : 'bg-muted-foreground/50'
                                    )}
                                />
                            </span>
                            <span>实时调用</span>
                        </button>
                    </div>
                </div>

                {/* Row 2: 搜索框 + 下拉框 + 高级筛选 */}
                <div className="flex min-w-0 flex-wrap items-center gap-2">
                    <label className="relative flex min-w-0 items-center">
                        <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
                        <input
                            type="text"
                            value={searchKeyword}
                            onChange={(e) => { setSearchKeyword(e.target.value); setCurrentPage(1); }}
                            placeholder="模糊搜索：用户名/Key/模型/渠道/端点/路径/会话/错误/ID…"
                            title="可搜索用户名、API Key 名、请求/实际模型名、渠道名、端点名、路径、会话 Key、错误信息及错误码；输入纯数字时额外精准匹配日志 ID 或渠道 ID"
                            className="h-9 w-52 rounded-lg border border-input bg-background pl-8 pr-7 text-xs text-foreground placeholder:text-muted-foreground focus:outline-none focus:ring-1 focus:ring-ring sm:w-64"
                        />
                        {searchKeyword && (
                            <button
                                type="button"
                                onClick={() => { setSearchKeyword(''); setCurrentPage(1); }}
                                aria-label="清空搜索"
                                className="absolute right-2 top-1/2 grid size-4 -translate-y-1/2 place-items-center rounded-full text-muted-foreground transition-colors hover:text-foreground"
                            >
                                <X className="size-3" />
                            </button>
                        )}
                    </label>

                    <label className="flex min-w-0 flex-wrap items-center gap-2">
                        <span className="text-sm font-medium text-card-foreground">端点</span>
                        <select
                            value={selectedEndpoint}
                            onChange={(event) => handleSelectEndpoint(event.target.value)}
                            className="h-9 min-w-36 rounded-lg border border-input bg-background px-3 text-sm text-foreground"
                        >
                            {endpointFilters.map((endpoint) => (
                                <option key={endpoint.value || 'all'} value={endpoint.value}>
                                    {endpoint.label}
                                </option>
                            ))}
                        </select>
                    </label>

                    <label className="flex min-w-0 flex-wrap items-center gap-2">
                        <span className="text-sm font-medium text-card-foreground">厂商</span>
                        <select
                            value={selectedProvider}
                            onChange={(event) => handleSelectProvider(event.target.value)}
                            className="h-9 min-w-36 rounded-lg border border-input bg-background px-3 text-sm text-foreground"
                        >
                            {providerFilters.map((provider) => (
                                <option key={provider.value || 'all'} value={provider.value}>
                                    {provider.label}
                                </option>
                            ))}
                        </select>
                    </label>

                    <label className="flex min-w-0 flex-wrap items-center gap-2">
                        <span className="text-sm font-medium text-card-foreground">模型</span>
                        <select
                            value={selectedModel}
                            onChange={(event) => handleSelectModel(event.target.value)}
                            className="h-9 min-w-40 max-w-56 rounded-lg border border-input bg-background px-3 text-sm text-foreground"
                        >
                            <option value="">全部模型</option>
                            {availableModelOptions.map((modelName) => (
                                <option key={modelName} value={modelName}>
                                    {modelName}
                                </option>
                            ))}
                        </select>
                    </label>

                    {isAdmin && (
                        <Button
                            variant="outline"
                            size="sm"
                            onClick={() => setAdvancedOpen((open) => !open)}
                            className="rounded-lg"
                        >
                            <SlidersHorizontal className="size-4" />
                            <span>{t('list.advancedFilters')}</span>
                            {advancedActiveCount > 0 && (
                                <Badge variant="secondary" className="h-5 min-w-5 justify-center px-1 text-[10px]">
                                    {advancedActiveCount}
                                </Badge>
                            )}
                            {advancedOpen ? <ChevronUp className="size-4" /> : <ChevronDown className="size-4" />}
                        </Button>
                    )}
                </div>

                {/* Row 3: 日期快捷键 + 状态筛选pills + checkboxes + 操作按钮 */}
                <div className="flex min-w-0 flex-wrap items-center gap-2">
                    {/* 左侧：日期快捷键pills */}
                    <div className="flex min-w-0 flex-wrap items-center gap-1 rounded-lg bg-muted/60 p-1">
                        {dateRangeShortcuts.map((shortcut) => {
                            const active = activeDateShortcut === shortcut.id;

                            return (
                                <button
                                    key={shortcut.id}
                                    type="button"
                                    onClick={() => applyDateRangeShortcut(shortcut.id)}
                                    className={cn(
                                        'inline-flex h-8 items-center rounded-lg px-2.5 text-xs font-medium transition-colors',
                                        active
                                            ? 'bg-background text-foreground shadow-sm'
                                            : 'text-muted-foreground hover:bg-background/60 hover:text-foreground'
                                    )}
                                >
                                    {shortcut.label}
                                </button>
                            );
                        })}
                    </div>

                    {/* 中间：状态筛选pills */}
                    <div className="flex min-w-0 flex-wrap items-center gap-1 rounded-lg bg-muted/60 p-1">
                        {severityFilters.map((filter) => {
                            const Icon = filter.icon;
                            const active = severityFilter === filter.id;

                            return (
                                <button
                                    key={filter.id}
                                    type="button"
                                    onClick={() => { setSeverityFilter(filter.id); setCurrentPage(1); }}
                                    className={cn(
                                        'inline-flex h-8 min-w-0 items-center gap-1.5 rounded-lg px-2 text-xs font-medium transition-colors',
                                        active
                                            ? 'bg-background text-foreground shadow-sm'
                                            : 'text-muted-foreground hover:bg-background/60 hover:text-foreground'
                                    )}
                                >
                                    <Icon className={cn('size-3.5 shrink-0', filter.className)} />
                                    <span>{t(`list.filters.${filter.id}`)}</span>
                                    <Badge variant="secondary" className="h-5 min-w-5 justify-center px-1 text-[10px]">
                                        {badgeCount(filter.id)?.toLocaleString() ?? '—'}
                                    </Badge>
                                </button>
                            );
                        })}
                    </div>

                    {/* Checkboxes：只看重试 + 隐藏测试探针 */}
                    <button
                        type="button"
                        onClick={() => { setRetriedOnly((v) => !v); setCurrentPage(1); }}
                        title="只看发生过重试 / 换渠道的请求"
                        className={cn(
                            'inline-flex h-8 items-center gap-1.5 rounded-lg border px-2.5 text-xs font-medium transition-colors',
                            retriedOnly
                                ? 'border-amber-500/50 bg-amber-500/10 text-amber-700 dark:text-amber-300'
                                : 'border-border bg-background text-muted-foreground hover:text-foreground'
                        )}
                    >
                        <RotateCw className="size-3.5" />
                        <span>{t('list.retriedOnly')}</span>
                    </button>

                    <button
                        type="button"
                        onClick={() => { setHideModelTest((v) => !v); setCurrentPage(1); }}
                        title="隐藏渠道测试探针（model_test），只看真实业务流量"
                        className={cn(
                            'inline-flex h-8 items-center gap-1.5 rounded-lg border px-2.5 text-xs font-medium transition-colors',
                            hideModelTest
                                ? 'border-primary/50 bg-primary/10 text-primary'
                                : 'border-border bg-background text-muted-foreground hover:text-foreground'
                        )}
                    >
                        <EyeOff className="size-3.5" />
                        <span>隐藏测试探针</span>
                    </button>

                    {/* 重置按钮 */}
                    {hasActiveFilter && (
                        <Button
                            variant="ghost"
                            size="sm"
                            onClick={handleResetFilters}
                            className="rounded-lg"
                        >
                            <RotateCcw className="size-4" />
                            <span>{t('list.reset')}</span>
                        </Button>
                    )}

                    {/* 右侧：操作按钮组 */}
                    <div className="ml-auto flex min-w-0 flex-wrap items-center gap-2">
                        <Button
                            variant="ghost"
                            size="icon"
                            onClick={() => setSensitiveVisible(!sensitiveVisible)}
                            title={sensitiveVisible ? t('list.hideSensitive') : t('list.showSensitive')}
                            aria-label={sensitiveVisible ? t('list.hideSensitive') : t('list.showSensitive')}
                            className="rounded-lg text-muted-foreground"
                        >
                            {sensitiveVisible ? <Eye className="size-4" /> : <EyeOff className="size-4" />}
                        </Button>
                        <Button
                            variant="ghost"
                            size="icon"
                            onClick={handleRefresh}
                            disabled={isRefreshing || isLoading}
                            title={isRefreshing ? t('list.refreshing') : t('list.refresh')}
                            aria-label={t('list.refresh')}
                            className="rounded-lg text-muted-foreground max-sm:hidden"
                        >
                            <RefreshCw className={cn('size-4', isRefreshing && 'animate-spin')} />
                        </Button>
                        <Button
                            variant="ghost"
                            size="icon"
                            onClick={handleExport}
                            disabled={exportLogs.isPending}
                            title={t('list.export')}
                            aria-label={t('list.export')}
                            className="rounded-lg text-muted-foreground"
                        >
                            {exportLogs.isPending ? <Loader2 className="size-4 animate-spin" /> : <Download className="size-4" />}
                        </Button>
                    </div>
                </div>

                {isAdmin && advancedOpen && (
                    <div className="flex min-w-0 flex-wrap items-center gap-2 border-t border-border pt-2">
                        <label className="flex min-w-0 flex-wrap items-center gap-2">
                            <span className="text-sm font-medium text-card-foreground">{t('list.userFilter')}</span>
                            <select
                                value={selectedUserID ?? ''}
                                onChange={(event) => handleSelectUser(event.target.value)}
                                className="h-9 min-w-40 rounded-lg border border-input bg-background px-3 text-sm text-foreground"
                            >
                                <option value="">{t('list.allUsers')}</option>
                                {users.map((user) => (
                                    <option key={user.id} value={user.id}>
                                        {user.username}
                                    </option>
                                ))}
                            </select>
                        </label>

                        <label className="flex min-w-0 flex-wrap items-center gap-2">
                            <span className="text-sm font-medium text-card-foreground">API Key</span>
                            <select
                                value={selectedAPIKeyID ?? ''}
                                onChange={(event) => handleSelectAPIKey(event.target.value)}
                                className="h-9 min-w-48 rounded-lg border border-input bg-background px-3 text-sm text-foreground"
                            >
                                <option value="">全部 API Key</option>
                                {apiKeysForSelectedUser.map((apiKey) => (
                                    <option key={apiKey.id} value={apiKey.id}>
                                        {apiKey.name}{apiKey.user_name ? ` · ${apiKey.user_name}` : ''}
                                    </option>
                                ))}
                            </select>
                        </label>
                    </div>
                )}

                {/* 日期选择器独立行（在高级筛选展开时显示） */}
                {advancedOpen && (
                    <div className="flex min-w-0 flex-wrap items-center gap-2 border-t border-border pt-2">
                        <label className="flex min-w-0 flex-wrap items-center gap-2">
                            <span className="text-sm font-medium text-card-foreground">日期范围</span>
                            <input
                                type="date"
                                value={startDate}
                                onChange={(event) => handleStartDate(event.target.value)}
                                max={endDate || todayLabel}
                                className="h-9 min-w-36 rounded-lg border border-input bg-background px-3 text-sm text-foreground"
                            />
                            <span className="text-xs text-muted-foreground">到</span>
                            <input
                                type="date"
                                value={endDate}
                                onChange={(event) => handleEndDate(event.target.value)}
                                min={startDate || undefined}
                                max={todayLabel}
                                className="h-9 min-w-36 rounded-lg border border-input bg-background px-3 text-sm text-foreground"
                            />
                        </label>
                    </div>
                )}

                {activePills.length > 0 && (
                    <div className="flex flex-wrap items-center gap-1.5 border-t border-border/60 pt-2">
                        <span className="text-xs text-muted-foreground">生效筛选</span>
                        {activePills.map((pill) => (
                            <FilterPill key={pill.key} label={pill.label} onClear={pill.onClear} />
                        ))}
                    </div>
                )}
                <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5 text-xs text-muted-foreground">
                    {severityCounts !== undefined
                        ? <span>共 {activeTotal.toLocaleString()} 条{severityFilter !== 'all' ? `（${t(`list.filters.${severityFilter}`)}）` : ''}</span>
                        : <span>{t('list.loadedCount', { count: logs.length })}</span>}
                    <span>时间按浏览器本地时区显示</span>
                    {isLiveMode && currentPage === 1 && <span className="text-emerald-700 dark:text-emerald-300">实时插入中</span>}
                    {totalPages > 1 && (
                        <div className="hidden items-center gap-1 sm:flex">
                            <Button
                                variant="ghost"
                                size="icon"
                                onClick={() => setCurrentPage((p) => Math.max(1, p - 1))}
                                disabled={currentPage <= 1 || isLoading}
                                className="h-6 w-6 rounded-md"
                                aria-label="上一页"
                            >
                                <ChevronLeft className="size-3" />
                            </Button>
                            <span className="tabular-nums">第 {currentPage} / {totalPages} 页</span>
                            <Button
                                variant="ghost"
                                size="icon"
                                onClick={() => setCurrentPage((p) => Math.min(totalPages, p + 1))}
                                disabled={currentPage >= totalPages || isLoading}
                                className="h-6 w-6 rounded-md"
                                aria-label="下一页"
                            >
                                <ChevronRight className="size-3" />
                            </Button>
                            <label className="flex items-center gap-1">
                                <span>跳至</span>
                                <input
                                    type="number"
                                    min={1}
                                    max={totalPages}
                                    value={pageJumpInput}
                                    onChange={(e) => setPageJumpInput(e.target.value)}
                                    onKeyDown={(e) => {
                                        if (e.key !== 'Enter') return;
                                        const p = parseInt(pageJumpInput, 10);
                                        if (Number.isFinite(p) && p >= 1 && p <= totalPages) {
                                            setCurrentPage(p);
                                        }
                                        setPageJumpInput('');
                                    }}
                                    onBlur={() => setPageJumpInput('')}
                                    placeholder={String(currentPage)}
                                    className="h-6 w-10 rounded-md border border-input bg-background px-1.5 text-center text-xs text-foreground [appearance:textfield] [&::-webkit-inner-spin-button]:appearance-none [&::-webkit-outer-spin-button]:appearance-none"
                                />
                                <span>页</span>
                            </label>
                        </div>
                    )}
                </div>
                {totalPages > 1 && (
                    <div className="flex items-center gap-2 border-t border-border/60 pt-2 sm:hidden">
                        <Button
                            variant="outline"
                            size="sm"
                            onClick={() => setCurrentPage((p) => Math.max(1, p - 1))}
                            disabled={currentPage <= 1 || isLoading}
                            className="h-9 flex-1 rounded-lg"
                        >
                            <ChevronLeft className="size-4" />
                            <span>上一页</span>
                        </Button>
                        <span className="shrink-0 text-xs tabular-nums text-muted-foreground">第 {currentPage} / {totalPages} 页</span>
                        <Button
                            variant="outline"
                            size="sm"
                            onClick={() => setCurrentPage((p) => Math.min(totalPages, p + 1))}
                            disabled={currentPage >= totalPages || isLoading}
                            className="h-9 flex-1 rounded-lg"
                        >
                            <span>下一页</span>
                            <ChevronRight className="size-4" />
                        </Button>
                    </div>
                )}
            </div>
            {/* 实时模式下的活跃动态面板 (请求中 / 救援中) */}
            <LiveActivityPanel
                isAdmin={isAdmin}
                enabled={isLiveMode}
                requestStates={requestStates}
                requestStateConnected={requestStateConnected}
                historyLogIDs={historyLogIDs}
            />
            <div className="min-h-0 flex-1">
                <TooltipProvider>
                <VirtualizedGrid
                    items={filteredLogs}
                    layout="list"
                    columns={{ default: 1 }}
                    estimateItemHeight={80}
                    overscan={8}
                    getItemKey={getLogRowKey}
                    renderItem={renderLogCard}
                    footer={footer}
                    onReachEnd={handleReachEnd}
                    reachEndEnabled={canLoadMore}
                    reachEndOffset={2}
                />
                </TooltipProvider>
            </div>
        </PageWrapper>
    );
}
