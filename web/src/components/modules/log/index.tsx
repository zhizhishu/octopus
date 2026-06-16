'use client';

import { useCallback, useMemo, useState } from 'react';
import { getRelayLogSeverity, type RelayLogSeverity, useExportLogs, useLogs } from '@/api/endpoints/log';
import { LogCard } from './Item';
import { AlertCircle, CheckCircle2, Circle, Download, Loader2, RefreshCw, Wifi, WifiOff } from 'lucide-react';
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

type LogSeverityFilter = RelayLogSeverity | 'all';

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

function localDateInput(date: Date) {
    const pad = (n: number) => String(n).padStart(2, '0');
    return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
}

function startOfLocalDayUnix(value: string) {
    const [year, month, day] = value.split('-').map(Number);
    return Math.floor(new Date(year, month - 1, day, 0, 0, 0, 0).getTime() / 1000);
}

function endOfLocalDayUnix(value: string) {
    const [year, month, day] = value.split('-').map(Number);
    return Math.floor(new Date(year, month - 1, day, 23, 59, 59, 999).getTime() / 1000);
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
    const [selectedUserID, setSelectedUserID] = useState<number | undefined>();
    const [selectedAPIKeyID, setSelectedAPIKeyID] = useState<number | undefined>();
    const [selectedEndpoint, setSelectedEndpoint] = useState('');
    const [severityFilter, setSeverityFilter] = useState<LogSeverityFilter>('all');
    const [autoRefresh, setAutoRefresh] = useState(false);
    const today = useMemo(() => localDateInput(new Date()), []);
    const [startDate, setStartDate] = useState(today);
    const [endDate, setEndDate] = useState(today);
    const { data: users = [] } = useUserList({ enabled: isAdmin });
    const { data: apiKeys = [] } = useAPIKeyList();
    const exportLogs = useExportLogs();

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
    const startTime = startOfLocalDayUnix(startDate);
    const endTime = endOfLocalDayUnix(endDate);

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
        pageSize: 10,
        userID: selectedUserID,
        apiKeyID: effectiveSelectedAPIKeyID,
        endpoint: selectedEndpoint || undefined,
        startTime,
        endTime,
        live: autoRefresh,
    });

    const updateSelectedUser = (value: string) => {
        const nextUserID = Number(value) || undefined;
        setSelectedUserID(nextUserID);
        if (selectedAPIKey && nextUserID && selectedAPIKey.user_id !== nextUserID) {
            setSelectedAPIKeyID(undefined);
        }
    };

    const updateSelectedAPIKey = (value: string) => {
        const nextAPIKeyID = Number(value) || undefined;
        setSelectedAPIKeyID(nextAPIKeyID);
        const nextAPIKey = apiKeys.find((apiKey) => apiKey.id === nextAPIKeyID);
        if (nextAPIKey?.user_id) {
            setSelectedUserID(nextAPIKey.user_id);
        }
    };

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
            <div className="flex flex-none flex-col gap-2 rounded-lg border border-border bg-card px-3 py-2">
                <div className="flex min-w-0 flex-wrap items-center gap-2">
                    {isAdmin && (
                        <label className="flex min-w-0 flex-wrap items-center gap-2">
                            <span className="text-sm font-medium text-card-foreground">{t('list.userFilter')}</span>
                            <select
                                value={selectedUserID ?? ''}
                                onChange={(event) => updateSelectedUser(event.target.value)}
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
                    )}

                    {isAdmin && (
                        <label className="flex min-w-0 flex-wrap items-center gap-2">
                            <span className="text-sm font-medium text-card-foreground">API Key</span>
                            <select
                                value={effectiveSelectedAPIKeyID ?? ''}
                                onChange={(event) => updateSelectedAPIKey(event.target.value)}
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
                    )}

                    <label className="flex min-w-0 flex-wrap items-center gap-2">
                        <span className="text-sm font-medium text-card-foreground">Endpoint</span>
                        <select
                            value={selectedEndpoint}
                            onChange={(event) => setSelectedEndpoint(event.target.value)}
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
                            onChange={(event) => setStartDate(event.target.value || today)}
                            className="h-9 min-w-36 rounded-lg border border-input bg-background px-3 text-sm text-foreground"
                        />
                        <span className="text-xs text-muted-foreground">到</span>
                        <input
                            type="date"
                            value={endDate}
                            onChange={(event) => setEndDate(event.target.value || today)}
                            className="h-9 min-w-36 rounded-lg border border-input bg-background px-3 text-sm text-foreground"
                        />
                    </label>

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
                            variant="outline"
                            size="sm"
                            onClick={handleRefresh}
                            disabled={isRefreshing || isLoading}
                            className="rounded-lg"
                        >
                            <RefreshCw className={cn('size-4', isRefreshing && 'animate-spin')} />
                            {isRefreshing ? t('list.refreshing') : t('list.refresh')}
                        </Button>
                        <Button
                            variant="outline"
                            size="sm"
                            onClick={handleExport}
                            disabled={exportLogs.isPending}
                            className="rounded-lg"
                        >
                            {exportLogs.isPending ? <Loader2 className="size-4 animate-spin" /> : <Download className="size-4" />}
                            导出
                        </Button>
                        <label className="inline-flex h-9 items-center gap-2 rounded-lg border border-border bg-background px-2 text-xs text-muted-foreground">
                            <Switch checked={autoRefresh} onCheckedChange={setAutoRefresh} />
                            <span>{t('list.autoRefresh')}</span>
                        </label>
                        <Badge
                            variant="outline"
                            className={cn(
                                'min-w-0 gap-1.5 rounded-lg px-2 py-1 text-xs',
                                autoRefresh && isConnected
                                    ? 'border-emerald-500/40 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300'
                                    : autoRefresh && streamError
                                        ? 'border-destructive/40 bg-destructive/10 text-destructive'
                                        : 'border-border bg-muted/40 text-muted-foreground'
                            )}
                            title={streamError?.message}
                        >
                            {autoRefresh && isConnected ? <Wifi className="size-3.5" /> : <WifiOff className="size-3.5" />}
                            <span>
                                {autoRefresh
                                    ? isConnected
                                        ? t('list.liveOn')
                                        : streamError
                                            ? t('list.streamError')
                                            : t('list.liveConnecting')
                                    : t('list.liveOff')}
                            </span>
                        </Badge>
                    </div>
                </div>
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
