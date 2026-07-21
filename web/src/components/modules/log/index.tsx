'use client';

import { useCallback, useMemo, useState } from 'react';
import { getRelayLogSeverity, type RelayLogSeverity, useExportLogs, useLogSeverityCounts, useLogs } from '@/api/endpoints/log';
import { LogCard, useSensitiveStore } from './Item';
import { AlertCircle, AlertTriangle, CheckCircle2, ChevronDown, ChevronLeft, ChevronRight, ChevronUp, Circle, Download, Eye, EyeOff, Loader2, RefreshCw, RotateCcw, RotateCw, ScrollText, SlidersHorizontal, Wifi, WifiOff, X } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { VirtualizedGrid } from '@/components/common/VirtualizedGrid';
import { PageWrapper } from '@/components/common/PageWrapper';
import { useAPIKeyList } from '@/api/endpoints/apikey';
import { useAuthStore, useUserList } from '@/api/endpoints/user';
import { Button } from '@/components/ui/button';
import { Badge } from '@/components/ui/badge';
import { Switch } from '@/components/ui/switch';
import { cn } from '@/lib/utils';
import { toast } from '@/components/common/Toast';
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
    // 所有筛选都「即选即生效」——改了立刻查，不再需要点「搜索」。这正是过去让人觉得「筛选无效」的
    // 另一半原因：改了接口下拉/用户/Key 却要再点搜索才生效，看起来像没反应。
    const [selectedUserID, setSelectedUserID] = useState<number | undefined>();
    const [selectedAPIKeyID, setSelectedAPIKeyID] = useState<number | undefined>();
    const [selectedEndpoint, setSelectedEndpoint] = useState('');
    const [startDate, setStartDate] = useState(defaultRange.startDate);
    const [endDate, setEndDate] = useState(defaultRange.endDate);
    // 严重程度 + 「只看有重试」都是服务端过滤，翻页/总数都对得上。
    const [severityFilter, setSeverityFilter] = useState<LogSeverityFilter>('all');
    const [retriedOnly, setRetriedOnly] = useState(false);
    const [hideModelTest, setHideModelTest] = useState(false);
    const [autoRefresh, setAutoRefresh] = useState(false);
    const [advancedOpen, setAdvancedOpen] = useState(false);
    // 分页状态：当前页（从 1 开始）+ 跳页输入框草稿值。自动刷新时禁用分页，回退无限滚动。
    const [currentPage, setCurrentPage] = useState(1);
    const [pageJumpInput, setPageJumpInput] = useState('');
    const sensitiveVisible = useSensitiveStore((state) => state.sensitiveVisible);
    const setSensitiveVisible = useSensitiveStore((state) => state.setSensitiveVisible);
    const { data: users = [] } = useUserList({ enabled: isAdmin });
    const { data: apiKeys = [] } = useAPIKeyList();
    const exportLogs = useExportLogs();
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
        startTime,
        endTime,
        retried: retriedOnly,
        hideModelTest,
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
        startTime,
        endTime,
        retried: retriedOnly,
        hideModelTest,
        // 实时刷新只在第 1 页（最新）生效；翻到历史页自然暂停，回第 1 页恢复。
        live: autoRefresh && currentPage === 1,
    });

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

    const handleStartDate = (value: string) => { setStartDate(value); setCurrentPage(1); };
    const handleEndDate = (value: string) => { setEndDate(value); setCurrentPage(1); };

    const applyDateRangeShortcut = useCallback((shortcut: LogDateRangeShortcut) => {
        const nextRange = resolveDateRangeShortcut(shortcut, todayLabel);
        setStartDate(nextRange.startDate);
        setEndDate(nextRange.endDate);
        setCurrentPage(1);
    }, [todayLabel]);

    // 所有筛选一键回默认（近 7 天、不限用户/Key/端点、全部状态、不限重试）。
    const handleResetFilters = useCallback(() => {
        setSelectedUserID(undefined);
        setSelectedAPIKeyID(undefined);
        setSelectedEndpoint('');
        setSeverityFilter('all');
        setRetriedOnly(false);
        setHideModelTest(false);
        setStartDate(defaultRange.startDate);
        setEndDate(defaultRange.endDate);
        setCurrentPage(1);
    }, [defaultRange]);

    const isDefaultRange = startDate === defaultRange.startDate && endDate === defaultRange.endDate;
    // 有任何非默认筛选在生效（决定是否显示「重置」+ 空状态提示是否算「被筛掉」）。
    const hasActiveFilter =
        !!selectedEndpoint ||
        !!selectedUserID ||
        !!effectiveSelectedAPIKeyID ||
        severityFilter !== 'all' ||
        retriedOnly ||
        hideModelTest ||
        !isDefaultRange;

    // 「生效筛选」药丸：把当前每一维筛选摊开成一个可一键删除的小标签，让人清楚现在到底在看什么。
    const activePills: Array<{ key: string; label: string; onClear: () => void }> = [];
    if (!startDate && !endDate) {
        activePills.push({ key: 'alldate', label: '全部日期', onClear: () => { setStartDate(defaultRange.startDate); setEndDate(defaultRange.endDate); setCurrentPage(1); } });
    } else if (!isDefaultRange) {
        const dl = startDate && endDate ? (startDate === endDate ? startDate : `${startDate} ~ ${endDate}`) : (startDate || endDate);
        activePills.push({ key: 'date', label: `日期 ${dl}`, onClear: () => { setStartDate(defaultRange.startDate); setEndDate(defaultRange.endDate); setCurrentPage(1); } });
    }
    if (selectedEndpoint) activePills.push({ key: 'ep', label: `接口 ${selectedEndpoint}`, onClear: () => { setSelectedEndpoint(''); setCurrentPage(1); } });
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
        if (severityFilter === 'all') return logs;
        return logs.filter((log) => getRelayLogSeverity(log) === severityFilter);
    }, [logs, severityFilter]);

    // 空状态智能提示的依据：列表真空时，查一下「不限日期」下总共有多少历史，
    // 好把「不是坏了、是被日期/筛选挡住了」说破。仅在真的空时才发这个请求。
    const listIsEmpty = !isLoading && filteredLogs.length === 0;
    const { data: allTimeCounts } = useLogSeverityCounts({
        userID: selectedUserID,
        apiKeyID: effectiveSelectedAPIKeyID,
        endpoint: selectedEndpoint || undefined,
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
            },
            {
                onSuccess: () => toast.success('日志已导出'),
                onError: (error) => toast.error('日志导出失败', { description: error instanceof Error ? error.message : String(error) }),
            }
        );
    }, [effectiveSelectedAPIKeyID, endTime, exportLogs, selectedEndpoint, selectedUserID, startTime]);

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
        <PageWrapper className="box-border flex h-full min-h-0 flex-col gap-3 overflow-hidden rounded-t-3xl pb-[calc(6rem+env(safe-area-inset-bottom))] md:pb-4 [&>*]:min-h-0 [&>*:last-child]:flex [&>*:last-child]:flex-1">
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
                <div className="flex min-w-0 flex-wrap items-center gap-2">
                    <label className="flex min-w-0 flex-wrap items-center gap-2">
                        <span className="text-sm font-medium text-card-foreground">Endpoint</span>
                        <select
                            value={selectedEndpoint}
                            onChange={(event) => handleSelectEndpoint(event.target.value)}
                            className="h-9 min-w-44 rounded-lg border border-input bg-background px-3 text-sm text-foreground"
                        >
                            {endpointFilters.map((endpoint) => (
                                <option key={endpoint.value || 'all'} value={endpoint.value}>
                                    {endpoint.label}
                                </option>
                            ))}
                        </select>
                    </label>

                    <label className="flex min-w-0 flex-wrap items-center gap-2">
                        <span className="text-sm font-medium text-card-foreground">日期</span>
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

                    {/* 排障向快捷筛选：只看发生过重试 / 换渠道的请求（抖动渠道一眼揪出）。 */}
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

                    {/* 隐藏渠道测试探针（model_test）：容量坏窗口 / 压测时探针失败会刷屏，藏掉只看真实业务。 */}
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
                            className="rounded-lg text-muted-foreground"
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
                        {/* 自动刷新开关 + 连接状态合并成一个控件：label 文案/图标随连接态变，省掉独立徽章。 */}
                        <label
                            className="inline-flex h-9 items-center gap-2 rounded-lg border border-border bg-background px-2 text-xs text-muted-foreground"
                            title={streamError?.message}
                        >
                            <Switch checked={autoRefresh} onCheckedChange={(v) => { setAutoRefresh(v); if (v) setCurrentPage(1); }} />
                            <span
                                className={cn(
                                    'inline-flex items-center gap-1.5',
                                    autoRefresh && isConnected
                                        ? 'text-emerald-700 dark:text-emerald-300'
                                        : autoRefresh && streamError
                                            ? 'text-destructive'
                                            : 'text-muted-foreground'
                                )}
                            >
                                {autoRefresh && isConnected ? <Wifi className="size-3.5" /> : <WifiOff className="size-3.5" />}
                                {autoRefresh
                                    ? isConnected
                                        ? t('list.liveOn')
                                        : streamError
                                            ? t('list.streamError')
                                            : t('list.liveConnecting')
                                    : t('list.autoRefresh')}
                            </span>
                        </label>
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
                    {autoRefresh && currentPage === 1 && <span className="text-emerald-700 dark:text-emerald-300">实时插入中</span>}
                    {totalPages > 1 && (
                        <div className="flex items-center gap-1">
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
            </div>
            <div className="min-h-0 flex-1">
                <VirtualizedGrid
                    items={filteredLogs}
                    layout="list"
                    columns={{ default: 1 }}
                    estimateItemHeight={80}
                    overscan={8}
                    getItemKey={(log) => `log-${log.id}`}
                    renderItem={(log) => <LogCard log={log} />}
                    footer={footer}
                    onReachEnd={handleReachEnd}
                    reachEndEnabled={canLoadMore}
                    reachEndOffset={2}
                />
            </div>
        </PageWrapper>
    );
}
