'use client';

import { useCallback, useMemo, useState } from 'react';
import { getRelayLogSeverity, type RelayLogSeverity, useExportLogs, useLogs } from '@/api/endpoints/log';
import { LogCard, useSensitiveStore } from './Item';
import { AlertCircle, AlertTriangle, CheckCircle2, ChevronDown, ChevronUp, Circle, Download, Eye, EyeOff, Loader2, RefreshCw, RotateCcw, Search, SlidersHorizontal, Wifi, WifiOff } from 'lucide-react';
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

const endpointFilters = [
    { value: '', label: '全部端点' },
    { value: 'chat', label: 'chat' },
    { value: 'responses', label: 'responses' },
    { value: 'messages', label: 'messages' },
    { value: 'gemini', label: 'gemini' },
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
    // 已生效（真正喂给查询）的筛选条件。日期默认按浏览器本地时区取今天，界面更清爽。
    const [selectedUserID, setSelectedUserID] = useState<number | undefined>();
    const [selectedAPIKeyID, setSelectedAPIKeyID] = useState<number | undefined>();
    const [selectedEndpoint, setSelectedEndpoint] = useState('');
    const [startDate, setStartDate] = useState(todayLabel);
    const [endDate, setEndDate] = useState(todayLabel);
    // 草稿（界面上正在改、还没点「搜索」）的筛选条件——改多项不会每改一次都打接口。
    const [draftUserID, setDraftUserID] = useState<number | undefined>();
    const [draftAPIKeyID, setDraftAPIKeyID] = useState<number | undefined>();
    const [draftEndpoint, setDraftEndpoint] = useState('');
    const [draftStartDate, setDraftStartDate] = useState(todayLabel);
    const [draftEndDate, setDraftEndDate] = useState(todayLabel);
    // 严重程度是对「已加载日志」的本地过滤，不是查询参数，所以保持即时生效。
    const [severityFilter, setSeverityFilter] = useState<LogSeverityFilter>('all');
    const [autoRefresh, setAutoRefresh] = useState(false);
    const [advancedOpen, setAdvancedOpen] = useState(false);
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
            .filter((apiKey) => !draftUserID || apiKey.user_id === draftUserID)
            .sort((a, b) => a.name.localeCompare(b.name));
    }, [apiKeys, draftUserID]);

    const draftAPIKey = useMemo(() => {
        if (!draftAPIKeyID) return undefined;
        return apiKeys.find((apiKey) => apiKey.id === draftAPIKeyID);
    }, [apiKeys, draftAPIKeyID]);

    const selectedAPIKey = useMemo(() => {
        if (!selectedAPIKeyID) return undefined;
        return apiKeys.find((apiKey) => apiKey.id === selectedAPIKeyID);
    }, [apiKeys, selectedAPIKeyID]);

    const effectiveSelectedAPIKeyID = selectedAPIKey && (!selectedUserID || selectedAPIKey.user_id === selectedUserID)
        ? selectedAPIKeyID
        : undefined;
    const { startTime, endTime } = resolveLogTimeRange(startDate, endDate);

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
        pageSize: 20,
        userID: selectedUserID,
        apiKeyID: effectiveSelectedAPIKeyID,
        endpoint: selectedEndpoint || undefined,
        startTime,
        endTime,
        live: autoRefresh,
    });

    const updateDraftUser = (value: string) => {
        const nextUserID = Number(value) || undefined;
        setDraftUserID(nextUserID);
        if (draftAPIKey && nextUserID && draftAPIKey.user_id !== nextUserID) {
            setDraftAPIKeyID(undefined);
        }
    };

    const updateDraftAPIKey = (value: string) => {
        const nextAPIKeyID = Number(value) || undefined;
        setDraftAPIKeyID(nextAPIKeyID);
        const nextAPIKey = apiKeys.find((apiKey) => apiKey.id === nextAPIKeyID);
        if (nextAPIKey?.user_id) {
            setDraftUserID(nextAPIKey.user_id);
        }
    };

    const applyDateRangeShortcut = useCallback((shortcut: LogDateRangeShortcut) => {
        const nextRange = resolveDateRangeShortcut(shortcut, todayLabel);
        setDraftStartDate(nextRange.startDate);
        setDraftEndDate(nextRange.endDate);
        setStartDate(nextRange.startDate);
        setEndDate(nextRange.endDate);
    }, [todayLabel]);

    // 把草稿条件一次性提交为生效条件（点「搜索」或在输入框回车时调用）。
    const handleApply = useCallback(() => {
        setSelectedUserID(draftUserID);
        setSelectedAPIKeyID(draftAPIKeyID);
        setSelectedEndpoint(draftEndpoint);
        setStartDate(draftStartDate);
        setEndDate(draftEndDate);
    }, [draftUserID, draftAPIKeyID, draftEndpoint, draftStartDate, draftEndDate]);

    // 草稿和生效条件都回到默认（今天、不限用户/Key/端点）。
    const handleResetFilters = useCallback(() => {
        setDraftUserID(undefined);
        setDraftAPIKeyID(undefined);
        setDraftEndpoint('');
        setDraftStartDate(todayLabel);
        setDraftEndDate(todayLabel);
        setSelectedUserID(undefined);
        setSelectedAPIKeyID(undefined);
        setSelectedEndpoint('');
        setStartDate(todayLabel);
        setEndDate(todayLabel);
    }, [todayLabel]);

    // 草稿是否被改过（决定「搜索」是否高亮 + 是否显示「重置」）。
    const draftDirty =
        draftUserID !== selectedUserID ||
        draftAPIKeyID !== selectedAPIKeyID ||
        draftEndpoint !== selectedEndpoint ||
        draftStartDate !== startDate ||
        draftEndDate !== endDate;

    const activeDateShortcut = useMemo(() => {
        return dateRangeShortcuts.find((shortcut) => {
            const shortcutRange = resolveDateRangeShortcut(shortcut.id, todayLabel);
            return shortcutRange.startDate === startDate && shortcutRange.endDate === endDate;
        })?.id;
    }, [endDate, startDate, todayLabel]);

    // 高级筛选（用户 / API Key）里有几项在生效——给折叠按钮上挂计数。
    const advancedActiveCount = (selectedUserID ? 1 : 0) + (effectiveSelectedAPIKeyID ? 1 : 0);

    const severityCounts = useMemo(() => {
        const counts: Record<LogSeverityFilter, number> = {
            all: logs.length,
            success: 0,
            warn: 0,
            error: 0,
        };

        for (const log of logs) {
            counts[getRelayLogSeverity(log)] += 1;
        }

        return counts;
    }, [logs]);

    const filteredLogs = useMemo(() => {
        if (severityFilter === 'all') return logs;
        return logs.filter((log) => getRelayLogSeverity(log) === severityFilter);
    }, [logs, severityFilter]);

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
            const message = logs.length === 0 ? t('list.empty') : t('list.emptyFiltered');
            return (
                <div className="flex flex-col items-center justify-center gap-3 rounded-lg border border-dashed border-border bg-card/50 px-4 py-8 text-center">
                    <span className="text-sm text-muted-foreground">{message}</span>
                    {hasMore && (
                        <Button variant="outline" size="sm" onClick={() => void loadMore()}>
                            {t('list.loadMoreForFilter')}
                        </Button>
                    )}
                </div>
            );
        }
        if (!hasMore && logs.length > 0) {
            return (
                <div className="flex justify-center py-4">
                    <span className="text-sm text-muted-foreground">{t('list.noMore')}</span>
                </div>
            );
        }
        return null;
    }, [filteredLogs.length, hasMore, isLoading, isLoadingMore, loadMore, logs.length, t]);

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
                            value={draftEndpoint}
                            onChange={(event) => setDraftEndpoint(event.target.value)}
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
                            value={draftStartDate}
                            onChange={(event) => setDraftStartDate(event.target.value)}
                            max={draftEndDate || todayLabel}
                            className="h-9 min-w-36 rounded-lg border border-input bg-background px-3 text-sm text-foreground"
                        />
                        <span className="text-xs text-muted-foreground">到</span>
                        <input
                            type="date"
                            value={draftEndDate}
                            onChange={(event) => setDraftEndDate(event.target.value)}
                            min={draftStartDate || undefined}
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

                    <Button
                        variant="default"
                        size="sm"
                        onClick={handleApply}
                        disabled={!draftDirty}
                        className="rounded-lg"
                    >
                        <Search className="size-4" />
                        <span>{t('list.search')}</span>
                    </Button>
                    {draftDirty && (
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
                                    onClick={() => setSeverityFilter(filter.id)}
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
                                        {severityCounts[filter.id]}
                                    </Badge>
                                </button>
                            );
                        })}
                    </div>

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
                            <Switch checked={autoRefresh} onCheckedChange={setAutoRefresh} />
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
                                value={draftUserID ?? ''}
                                onChange={(event) => updateDraftUser(event.target.value)}
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
                                value={draftAPIKeyID ?? ''}
                                onChange={(event) => updateDraftAPIKey(event.target.value)}
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
                <div className="text-xs text-muted-foreground">
                    {t('list.loadedCount', { count: logs.length })} · 时间按浏览器本地时区显示
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
