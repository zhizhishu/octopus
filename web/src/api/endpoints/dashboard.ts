import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { apiClient } from '../client';

/**
 * 仪表盘（管理员）用的两个按时间段接口。数据来自请求日志：
 * 日志被清理过的日子没有原料，算不出来（oldest_log_time 之前）。
 */

export type BreakdownDimension = 'model' | 'channel' | 'plan' | 'user' | 'apikey' | 'hour';

export interface StatsUsage {
    request_success: number;
    request_failed: number;
    input_token: number;
    output_token: number;
    cache_hit_token: number;
    cache_write_token: number;
    cache_input_token: number;
    total_cost: number;
    wait_time: number;
}

export interface StatsBreakdownItem extends StatsUsage {
    key: string;
    id: number;
    name: string;
    user_id?: number;
    user_name?: string;
    missing?: boolean;
    start?: number;
}

export interface StatsBreakdown {
    from: string;
    to: string;
    by: BreakdownDimension;
    days: number;
    start: number;
    end: number;
    total: StatsUsage;
    items: StatsBreakdownItem[];
    item_count: number;
    others?: StatsUsage;
    others_count: number;
    active_users?: number;
    oldest_log_time: number;
}

export interface StatsUserTrendUser {
    user_id: number;
    name: string;
    missing?: boolean;
    total: StatsUsage;
    points: StatsUsage[];
}

export interface StatsUserTrend {
    from: string;
    to: string;
    dates: string[];
    users: StatsUserTrendUser[];
    oldest_log_time: number;
}

export interface BreakdownParams {
    from: string;
    to: string;
    by: BreakdownDimension;
    limit?: number;
}

export function useStatsBreakdown(params: BreakdownParams | null) {
    return useQuery({
        queryKey: ['stats', 'breakdown', params?.from, params?.to, params?.by, params?.limit ?? 0],
        queryFn: async () => {
            const p = params!;
            const qs = new URLSearchParams({ from: p.from, to: p.to, by: p.by });
            if (p.limit) qs.set('limit', String(p.limit));
            return apiClient.get<StatsBreakdown>(`/api/v1/stats/breakdown?${qs.toString()}`);
        },
        enabled: !!params,
        placeholderData: keepPreviousData,
        staleTime: 30000,
    });
}

export function useStatsUserTrend(params: { from: string; to: string; top?: number } | null) {
    return useQuery({
        queryKey: ['stats', 'user-trend', params?.from, params?.to, params?.top ?? 5],
        queryFn: async () => {
            const p = params!;
            const qs = new URLSearchParams({ from: p.from, to: p.to, top: String(p.top ?? 5) });
            return apiClient.get<StatsUserTrend>(`/api/v1/stats/user-trend?${qs.toString()}`);
        },
        enabled: !!params,
        placeholderData: keepPreviousData,
        staleTime: 30000,
    });
}

/** /stats/daily 原始行（不做格式化）；和主页共用同一个缓存键与请求。 */
export interface StatsDailyRaw {
    date: string;
    input_token: number;
    output_token: number;
    input_cost: number;
    output_cost: number;
    wait_time: number;
    request_success: number;
    request_failed: number;
    cache_hit_token: number;
    cache_write_token: number;
    cache_input_token: number;
}

export function useStatsDailyRaw() {
    return useQuery({
        queryKey: ['stats', 'daily'],
        queryFn: async () => apiClient.get<StatsDailyRaw[]>('/api/v1/stats/daily'),
        refetchOnMount: 'always',
    });
}
