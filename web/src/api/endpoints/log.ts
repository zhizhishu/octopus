import type { InfiniteData } from '@tanstack/react-query';
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiClient, API_BASE_URL } from '../client';
import { useAuthStore } from './user';
import { logger } from '@/lib/logger';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

/**
 * 尝试状态
 */
export type AttemptStatus = 'success' | 'failed' | 'circuit_break' | 'skipped';

/**
 * 单次渠道尝试信息
 */
export interface ChannelAttempt {
    channel_id: number;
    channel_key_id?: number;
    channel_name: string;
    model_name: string;
    upstream_path?: string;
    attempt_num: number;    // 第几次尝试
    status: AttemptStatus;
    duration: number;       // 耗时(毫秒)
    sticky?: boolean;
    proxy_used?: boolean;   // 该次尝试是否经代理出站
    proxy_source?: string;  // 代理来源: channel / system
    proxy_scheme?: string;  // 代理协议: http / socks5
    proxy_target?: string;  // 代理地址(host:port), 含 SOCKS
    msg?: string;
}

/**
 * 日志数据
 */
export interface RelayLog {
    id: number;
    user_id: number;
    api_key_id: number;
    request_ip?: string;
    time: number;                // 时间戳
    request_endpoint?: string;   // chat / responses / messages / raw protocol name
    request_path?: string;       // original inbound path
    request_model_name: string;  // 请求模型名称
    request_api_key_name?: string; // 请求使用的 API Key 名称
    user_name?: string;          // 发起请求的用户名, 仅管理员日志返回
    reasoning_effort?: string;   // 生效的推理/思考强度(经 gpt-5.6 自动抬升后的最终值)
    channel: number;             // 实际使用的渠道ID
    channel_name: string;        // 渠道名称
    channel_key_remark?: string; // 实际使用的渠道 Key 的备注名
    actual_model_name: string;   // 实际使用模型名称
    input_tokens: number;        // 输入Token
    output_tokens: number;       // 输出Token
    cache_hit_tokens: number;     // 缓存命中Token
    cache_write_tokens: number;   // 缓存写入Token
    cache_input_tokens: number;   // 可参与缓存统计的输入Token
    cache_hit_rate: number;       // 缓存命中率, 0-1
    ftut: number;                // 首字时间(毫秒)
    use_time: number;            // 总用时(毫秒)
    cost: number;                // 消耗费用
    request_content?: string;    // 请求内容, 仅管理员日志详情返回
    response_content?: string;   // 响应内容, 仅管理员日志详情返回
    error?: string;              // 错误信息, 普通用户只接收 error_code/error_status
    error_code?: string;
    error_status?: number;
    error_strategy?: string;
    attempts?: ChannelAttempt[]; // 所有尝试记录
    total_attempts?: number;     // 总尝试次数
    session_key?: string;
    session_source?: string;
    route_sticky_hit?: boolean;
    is_stream?: boolean;
    usage_source?: string;
    usage_missing_reason?: string;
}

export interface RelayLogStorage {
    stored_bytes: number;
    max_bytes: number;
    max_gb: number;
}

export type RelayLogSeverity = 'success' | 'warn' | 'error';

/** 全量严重程度计数（后端 /log/count 返回），用于日志页徽章显示真实总数而非当前页。 */
export interface RelayLogSeverityCounts {
    total: number;
    success: number;
    warn: number;
    error: number;
}

export function getRelayLogSeverity(log: RelayLog): RelayLogSeverity {
    const hasError =
        !!log.error?.trim() ||
        !!log.error_code?.trim() ||
        ((log.error_status ?? 0) >= 400);

    if (hasError) return 'error';

    // Warn == the request ultimately succeeded but took more than one channel
    // attempt (a retry/failover). Mirrors the server relayLogSeverityValue rule so
    // a client-side filter never hides a row the server returned for that severity.
    const attemptCount = log.total_attempts ?? log.attempts?.length ?? 0;
    if (attemptCount > 1) return 'warn';
    return 'success';
}

/**
 * 日志列表查询参数
 */

/** Admin detail dialog: fetch one log including request/response bodies. */
export async function fetchLogById(id: number): Promise<RelayLog> {
    return apiClient.get<RelayLog>(`/api/v1/log/${id}`);
}

export interface LogListParams {
    page?: number;
    page_size?: number;
    start_time?: number;
    end_time?: number;
    user_id?: number;
    api_key_id?: number;
    endpoint?: string;
    severity?: string;
    retried?: boolean;
    hide_model_test?: boolean;
}

export interface LogExportParams {
    start_time?: number;
    end_time?: number;
    user_id?: number;
    api_key_id?: number;
    endpoint?: string;
}

function getAuthHeader(): string {
    const token = useAuthStore.getState().token;
    if (!token) throw new Error('Not authenticated');
    return `Bearer ${token}`;
}

function exportFilename(startTime?: number, endTime?: number) {
    const pad = (n: number) => String(n).padStart(2, '0');
    const fmt = (ts?: number) => {
        if (!ts) return '';
        const d = new Date(ts * 1000);
        return `${d.getFullYear()}${pad(d.getMonth() + 1)}${pad(d.getDate())}`;
    };
    const start = fmt(startTime);
    const end = fmt(endTime);
    if (start && end) return `octopus-log-summary-${start}-${end}.json`;
    return `octopus-log-summary-${fmt(Math.floor(Date.now() / 1000))}.json`;
}

function appendOptionalParam(query: URLSearchParams, key: string, value?: number | string) {
    if (value === undefined || value === null || value === '') return;
    query.set(key, String(value));
}

function logMatchesTimeRange(log: RelayLog, startTime?: number, endTime?: number) {
    if (startTime === undefined || endTime === undefined) return true;
    return log.time >= startTime && log.time <= endTime;
}

function compareRelayLogsDesc(a: RelayLog, b: RelayLog) {
    if (a.time !== b.time) return b.time - a.time;
    return b.id - a.id;
}

async function downloadBlob(blob: Blob, filename: string) {
    const url = URL.createObjectURL(blob);
    try {
        const a = document.createElement('a');
        a.href = url;
        a.download = filename;
        document.body.appendChild(a);
        a.click();
        a.remove();
    } finally {
        URL.revokeObjectURL(url);
    }
}

export function useExportLogs() {
    return useMutation({
        mutationFn: async (params: LogExportParams = {}) => {
            const query = new URLSearchParams();
            appendOptionalParam(query, 'start_time', params.start_time);
            appendOptionalParam(query, 'end_time', params.end_time);
            appendOptionalParam(query, 'user_id', params.user_id);
            appendOptionalParam(query, 'api_key_id', params.api_key_id);
            appendOptionalParam(query, 'endpoint', params.endpoint);
            const res = await fetch(`${API_BASE_URL}/api/v1/log/export?${query.toString()}`, {
                method: 'GET',
                headers: { Authorization: getAuthHeader() },
            });
            if (!res.ok) {
                const text = await res.text();
                throw new Error(text || res.statusText);
            }
            await downloadBlob(await res.blob(), exportFilename(params.start_time, params.end_time));
        },
        onError: (error) => {
            logger.error('export logs failed:', error);
        },
    });
}

/**
 * 清空日志 Hook
 * 
 * @example
 * const clearLogs = useClearLogs();
 * 
 * clearLogs.mutate();
 */
export function useClearLogs() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: async () => {
            return apiClient.delete<null>('/api/v1/log/clear');
        },
        onSuccess: () => {
            logger.log('日志清空成功');
            queryClient.invalidateQueries({ queryKey: ['logs'] });
        },
        onError: (error) => {
            logger.error('日志清空失败:', error);
        },
    });
}

export function useLogStorage() {
    return useQuery({
        queryKey: ['logs', 'storage'],
        queryFn: async () => {
            return apiClient.get<RelayLogStorage>('/api/v1/log/storage');
        },
        refetchInterval: 30000,
        refetchOnMount: 'always',
    });
}

const logsInfiniteQueryKey = (
    pageSize: number,
    userID?: number,
    apiKeyID?: number,
    endpoint?: string,
    startTime?: number,
    endTime?: number,
    page?: number,
    severity?: string,
    retried?: boolean,
    hideModelTest?: boolean
) => ['logs', 'infinite', pageSize, userID ?? 0, apiKeyID ?? 0, endpoint ?? '', startTime ?? 0, endTime ?? 0, page ?? -1, severity ?? '', retried ? 1 : 0, hideModelTest ? 1 : 0] as const;

const logCountQueryKey = (
    userID?: number,
    apiKeyID?: number,
    endpoint?: string,
    startTime?: number,
    endTime?: number,
    retried?: boolean,
    hideModelTest?: boolean
) => ['logs', 'count', userID ?? 0, apiKeyID ?? 0, endpoint ?? '', startTime ?? 0, endTime ?? 0, retried ? 1 : 0, hideModelTest ? 1 : 0] as const;
const LOG_STREAM_RECONNECT_BASE_MS = 1000;
const LOG_STREAM_RECONNECT_MAX_MS = 15000;

/**
 * 日志严重程度计数 Hook
 * 接受与 useLogs 相同的过滤参数（不含 page/page_size/severity），返回符合条件的
 * 全量总数 + 成功/警告/错误各自的总数，供日志页徽章与分页页数使用（不受当前页限制）。
 */
export function useLogSeverityCounts(options: {
    userID?: number;
    apiKeyID?: number;
    endpoint?: string;
    startTime?: number;
    endTime?: number;
    retried?: boolean;
    hideModelTest?: boolean;
    enabled?: boolean;
} = {}) {
    const { userID, apiKeyID, endpoint, startTime, endTime, retried, hideModelTest, enabled = true } = options;
    return useQuery({
        queryKey: logCountQueryKey(userID, apiKeyID, endpoint, startTime, endTime, retried, hideModelTest),
        queryFn: async () => {
            const params = new URLSearchParams();
            appendOptionalParam(params, 'user_id', userID);
            appendOptionalParam(params, 'api_key_id', apiKeyID);
            appendOptionalParam(params, 'endpoint', endpoint);
            appendOptionalParam(params, 'start_time', startTime);
            appendOptionalParam(params, 'end_time', endTime);
            if (retried) params.set('retried', '1');
            if (hideModelTest) params.set('hide_model_test', '1');
            return apiClient.get<RelayLogSeverityCounts>(`/api/v1/log/count?${params.toString()}`);
        },
        enabled,
        staleTime: 0,
        refetchOnMount: 'always',
    });
}

/**
 * 日志管理 Hook
 * 整合初始加载、SSE 实时推送、滚动加载更多
 *
 * @param options.page - 指定后进入分页模式：只拉该页数据，不自动加载下一页。
 *                       未指定（或 live=true）时保持无限滚动行为。
 * @example
 * const { logs, isConnected, hasMore, isLoadingMore, loadMore, clear } = useLogs();
 *
 * // logs 自动包含历史日志和实时日志，按时间倒序
 * logs.forEach(log => console.log(log.request_model_name));
 *
 * // 滚动到底部时加载更多
 * if (hasMore && !isLoadingMore) loadMore();
 */
export function useLogs(options: { pageSize?: number; userID?: number; apiKeyID?: number; endpoint?: string; startTime?: number; endTime?: number; live?: boolean; page?: number; severity?: string; retried?: boolean; hideModelTest?: boolean } = {}) {
    const { pageSize = 20, userID, apiKeyID, endpoint, startTime, endTime, live = false, page, severity, retried, hideModelTest } = options;

    const [isConnected, setIsConnected] = useState(false);
    const [error, setError] = useState<Error | null>(null);
    const eventSourceRef = useRef<EventSource | null>(null);
    const reconnectTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
    const reconnectAttemptRef = useRef(0);
    const latestLogIDRef = useRef(0);

    const queryClient = useQueryClient();
    const queryKey = useMemo(
        () => logsInfiniteQueryKey(pageSize, userID, apiKeyID, endpoint, startTime, endTime, page, severity, retried, hideModelTest),
        [apiKeyID, endpoint, endTime, hideModelTest, page, pageSize, retried, severity, startTime, userID]
    );

    const logsQuery = useInfiniteQuery({
        queryKey,
        initialPageParam: page ?? 1,
        queryFn: async ({ pageParam }) => {
            const params = new URLSearchParams();
            params.set('page', String(pageParam));
            params.set('page_size', String(pageSize));
            appendOptionalParam(params, 'user_id', userID);
            appendOptionalParam(params, 'api_key_id', apiKeyID);
            appendOptionalParam(params, 'endpoint', endpoint);
            appendOptionalParam(params, 'start_time', startTime);
            appendOptionalParam(params, 'end_time', endTime);
            appendOptionalParam(params, 'severity', severity);
            if (retried) params.set('retried', '1');
            if (hideModelTest) params.set('hide_model_test', '1');
            const result = await apiClient.get<RelayLog[] | null>(`/api/v1/log/list?${params.toString()}`);
            return result ?? [];
        },
        getNextPageParam: (lastPage, allPages) => {
            // In paginated mode (explicit page), never auto-fetch additional pages.
            if (page !== undefined) return undefined;
            if (!lastPage || lastPage.length < pageSize) return undefined;
            return allPages.length + 1;
        },
        staleTime: 0,
        refetchOnMount: 'always',
    });

    const logs = useMemo(() => {
        const pages = logsQuery.data?.pages ?? [];
        const seen = new Set<number>();
        const merged: RelayLog[] = [];

        for (const page of pages) {
            for (const log of page) {
                if (seen.has(log.id)) continue;
                seen.add(log.id);
                merged.push(log);
            }
        }

        merged.sort(compareRelayLogsDesc);
        return merged;
    }, [logsQuery.data]);

    useEffect(() => {
        latestLogIDRef.current = logs.reduce((maxID, log) => Math.max(maxID, log.id), 0);
    }, [logs]);

    const loadMore = useCallback(async () => {
        if (!logsQuery.hasNextPage) return;
        if (logsQuery.isFetchingNextPage) return;

        try {
            await logsQuery.fetchNextPage();
        } catch (e) {
            logger.error('加载更多日志失败:', e);
        }
    }, [logsQuery]);

    const refresh = useCallback(async () => {
        try {
            // Keep already loaded pages visible while the server is rechecked.
            // resetQueries drops the infinite-query page set back to page 1, which
            // makes older loaded logs look as if they disappeared after refresh.
            await logsQuery.refetch();
        } catch (e) {
            logger.error('刷新日志失败:', e);
        }
    }, [logsQuery]);

    useEffect(() => {
        let cancelled = false;

        const clearReconnectTimer = () => {
            if (!reconnectTimerRef.current) return;
            clearTimeout(reconnectTimerRef.current);
            reconnectTimerRef.current = null;
        };

        const closeEventSource = () => {
            eventSourceRef.current?.close();
            eventSourceRef.current = null;
        };

        if (!live) {
            clearReconnectTimer();
            closeEventSource();
            reconnectAttemptRef.current = 0;
            return () => {
                cancelled = true;
                clearReconnectTimer();
            };
        }

        const scheduleReconnect = (nextConnect: () => Promise<void>) => {
            if (cancelled) return;

            const attempt = reconnectAttemptRef.current;
            reconnectAttemptRef.current += 1;
            const delay = Math.min(
                LOG_STREAM_RECONNECT_BASE_MS * (2 ** Math.min(attempt, 5)),
                LOG_STREAM_RECONNECT_MAX_MS
            );

            clearReconnectTimer();
            reconnectTimerRef.current = setTimeout(() => {
                reconnectTimerRef.current = null;
                void nextConnect();
            }, delay);
        };

        const connect = async () => {
            clearReconnectTimer();
            closeEventSource();

            try {
                const params = new URLSearchParams();
                appendOptionalParam(params, 'user_id', userID);
                appendOptionalParam(params, 'api_key_id', apiKeyID);
                appendOptionalParam(params, 'endpoint', endpoint);
                appendOptionalParam(params, 'start_time', startTime);
                appendOptionalParam(params, 'end_time', endTime);
                const suffix = params.toString() ? `?${params.toString()}` : '';
                const { token } = await apiClient.get<{ token: string }>(`/api/v1/log/stream-token${suffix}`);
                if (cancelled) return;

                const streamParams = new URLSearchParams({ token });
                if (latestLogIDRef.current > 0) {
                    streamParams.set('since_id', String(latestLogIDRef.current));
                }
                const eventSource = new EventSource(`${API_BASE_URL}/api/v1/log/stream?${streamParams.toString()}`);
                eventSourceRef.current = eventSource;

                eventSource.onopen = () => {
                    const wasReconnecting = reconnectAttemptRef.current > 0;
                    reconnectAttemptRef.current = 0;
                    setIsConnected(true);
                    setError(null);
                    if (wasReconnecting) {
                        void queryClient.invalidateQueries({ queryKey, exact: true });
                    }
                };

                eventSource.onmessage = (event) => {
                    try {
                        // Live tail only makes sense on the newest page. When the user has
                        // paged into history (page > 1), never inject a brand-new log into
                        // that page's cache — it would jam the newest record onto an old page.
                        if (page !== undefined && page !== 1) return;
                        const log: RelayLog = JSON.parse(event.data);
                        if (!logMatchesTimeRange(log, startTime, endTime)) return;
                        queryClient.setQueryData(
                            queryKey,
                            (old: InfiniteData<RelayLog[], number> | undefined) => {
                                if (!old) {
                                    return { pages: [[log]], pageParams: [1] };
                                }

                                const exists = old.pages.some((p) => p?.some((x) => x.id === log.id));
                                if (exists) return old;

                                const pages = old.pages.length > 0
                                    ? [[log, ...(old.pages[0] ?? [])], ...old.pages.slice(1)]
                                    : [[log]];
                                return {
                                    ...old,
                                    pages,
                                    pageParams: old.pageParams.length === pages.length ? old.pageParams : [1],
                                };
                            }
                        );
                    } catch (e) {
                        logger.error('解析日志数据失败:', e);
                    }
                };

                eventSource.onerror = () => {
                    if (cancelled) return;
                    setIsConnected(false);
                    eventSource.close();
                    eventSourceRef.current = null;
                    setError(new Error('SSE disconnected, reconnecting'));
                    scheduleReconnect(connect);
                };
            } catch (e) {
                if (cancelled) return;
                setIsConnected(false);
                scheduleReconnect(connect);
                setError(e instanceof Error ? e : new Error('获取 stream token 失败'));
                logger.error('获取 stream token 失败:', e);
            }
        };

        void connect();

        return () => {
            cancelled = true;
            clearReconnectTimer();
            closeEventSource();
            reconnectAttemptRef.current = 0;
            setIsConnected(false);
        };
    }, [apiKeyID, endpoint, live, page, pageSize, queryClient, queryKey, startTime, endTime, userID]);

    const clear = useCallback(() => {
        queryClient.removeQueries({ queryKey, exact: true });
    }, [queryClient, queryKey]);

    return {
        logs,
        isConnected,
        error,
        hasMore: !!logsQuery.hasNextPage,
        isLoading: logsQuery.isLoading,
        isLoadingMore: logsQuery.isFetchingNextPage,
        isRefreshing: logsQuery.isRefetching && !logsQuery.isFetchingNextPage,
        loadMore,
        refresh,
        clear,
    };
}
