import { useQuery } from '@tanstack/react-query';
import { apiClient } from '../client';
import { formatCount, formatMoney, formatPercent, formatTime } from '@/lib/utils';

/**
 * 统计数据
 */
export interface StatsMetrics {
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

export interface StatsMetricsFormatted {
    input_token: ReturnType<typeof formatCount>;
    output_token: ReturnType<typeof formatCount>;
    input_cost: ReturnType<typeof formatMoney>;
    output_cost: ReturnType<typeof formatMoney>;
    wait_time: ReturnType<typeof formatTime>;
    request_success: ReturnType<typeof formatCount>;
    request_failed: ReturnType<typeof formatCount>;
    cache_hit_token: ReturnType<typeof formatCount>;
    cache_write_token: ReturnType<typeof formatCount>;
    cache_input_token: ReturnType<typeof formatCount>;
    cache_rate_base_token: ReturnType<typeof formatCount>;
    effective_input_token: ReturnType<typeof formatCount>;
    cache_hit_rate: ReturnType<typeof formatPercent>;

    request_count: ReturnType<typeof formatCount>;
    total_token: ReturnType<typeof formatCount>;
    total_cost: ReturnType<typeof formatMoney>;
}

export interface StatsChannel extends StatsMetrics {
    channel_id: number;
}

export interface StatsDaily extends StatsMetrics {
    date: string;
}
export interface StatsDailyFormatted extends StatsMetricsFormatted {
    date: string;
}

export interface StatsTotal extends StatsMetrics {
    id: number;
}
export type StatsTotalFormatted = StatsMetricsFormatted;

export interface StatsHourly extends StatsMetrics {
    hour: number;
    date: string;
}
export interface StatsHourlyFormatted extends StatsMetricsFormatted {
    hour: number;
    date: string;
}

export interface ModelHealthSummary {
    request_success: number;
    request_failed: number;
    first_token_p90_ms: number;
    avg_throughput: number;
    cache_hit_rate: number;
}

export interface ModelHealthHour extends ModelHealthSummary {
    hour: number;
}

export interface ModelHealthModel {
    model: string;
    hours: ModelHealthHour[];
    summary: ModelHealthSummary;
}

export interface ModelHealthProvider {
    provider: 'OpenAI' | 'Gemini' | 'Anthropic' | string;
    models: ModelHealthModel[];
}

export interface ModelHealthResponse {
    date: string;
    providers: ModelHealthProvider[];
}

export interface UserUsageSummary {
    user_id: number;
    username: string;
    role: 'admin' | 'user';
    request_success: number;
    request_failed: number;
    input_token: number;
    output_token: number;
    total_cost: number;
}

export interface StatsModelRank {
    model?: string;
    name?: string;
    request_model_name?: string;
    request_count?: number;
    request_success?: number;
    request_failed?: number;
    input_token?: number;
    output_token?: number;
    total_token?: number;
    total_cost?: number;
    cache_hit_token?: number;
    cache_input_token?: number;
    cache_write_token?: number;
    cache_hit_rate?: number;
    first_token_p90_ms?: number;
    avg_throughput?: number;
    recent_actual_models?: string[];
}

export interface StatsModelRankFormatted {
    model: string;
    request_count: ReturnType<typeof formatCount>;
    request_success: ReturnType<typeof formatCount>;
    request_failed: ReturnType<typeof formatCount>;
    input_token: ReturnType<typeof formatCount>;
    output_token: ReturnType<typeof formatCount>;
    total_token: ReturnType<typeof formatCount>;
    total_cost: ReturnType<typeof formatMoney>;
    cache_hit_token: ReturnType<typeof formatCount>;
    cache_input_token: ReturnType<typeof formatCount>;
    cache_write_token: ReturnType<typeof formatCount>;
    cache_hit_rate: ReturnType<typeof formatPercent>;
    first_token_p90_ms: ReturnType<typeof formatTime>;
    avg_throughput: ReturnType<typeof formatCount>;
    recent_actual_models: string[];
}

function getCacheTokenSum(item: StatsMetrics): number {
    return Math.max(0, item.cache_hit_token || 0) + Math.max(0, item.cache_write_token || 0);
}

function getCacheRateBase(item: StatsMetrics): number {
    const reportedBase = Math.max(0, item.cache_input_token || 0);
    const cacheTokenSum = getCacheTokenSum(item);
    if (reportedBase > 0) return Math.max(reportedBase, cacheTokenSum);
    if (cacheTokenSum > 0) return Math.max(Math.max(0, item.input_token || 0), cacheTokenSum);
    return Math.max(0, item.input_token || 0);
}

function getEffectiveInputToken(item: StatsMetrics): number {
    return Math.max(Math.max(0, item.input_token || 0), getCacheRateBase(item));
}

function getCacheHitRate(item: StatsMetrics): number {
    const denominator = getCacheRateBase(item);
    if (!denominator || denominator <= 0 || item.cache_hit_token <= 0) return 0;
    return item.cache_hit_token / denominator;
}

export function formatStatsMetrics(item: StatsMetrics): StatsMetricsFormatted {
    const cacheRateBase = getCacheRateBase(item);
    const effectiveInputToken = getEffectiveInputToken(item);

    return {
        input_token: formatCount(item.input_token),
        output_token: formatCount(item.output_token),
        total_token: formatCount(effectiveInputToken + item.output_token),
        input_cost: formatMoney(item.input_cost),
        output_cost: formatMoney(item.output_cost),
        total_cost: formatMoney(item.input_cost + item.output_cost),
        wait_time: formatTime(item.wait_time),
        request_success: formatCount(item.request_success),
        request_failed: formatCount(item.request_failed),
        request_count: formatCount(item.request_success + item.request_failed),
        cache_hit_token: formatCount(item.cache_hit_token),
        cache_write_token: formatCount(item.cache_write_token),
        cache_input_token: formatCount(item.cache_input_token),
        cache_rate_base_token: formatCount(cacheRateBase),
        effective_input_token: formatCount(effectiveInputToken),
        cache_hit_rate: formatPercent(getCacheHitRate(item)),
    };
}
/**
 * API Key 统计数据
 */
export interface StatsAPIKey extends StatsMetrics {
    api_key_id: number;
}

export interface StatsAPIKeyFormatted extends StatsMetricsFormatted {
    api_key_id: number;
}

export interface StatsAPIKeyUsage {
    api_key_id: number;
    input_token: number;
    output_token: number;
    input_cost: number;
    output_cost: number;
    wait_time: number;
    request_success: number;
    request_failed: number;
}

export interface StatsAPIKeyUsageFormatted {
    api_key_id: number;
    input_token: ReturnType<typeof formatCount>;
    output_token: ReturnType<typeof formatCount>;
    input_cost: ReturnType<typeof formatMoney>;
    output_cost: ReturnType<typeof formatMoney>;
    wait_time: ReturnType<typeof formatTime>;
    request_success: ReturnType<typeof formatCount>;
    request_failed: ReturnType<typeof formatCount>;
    request_count: ReturnType<typeof formatCount>;
    total_token: ReturnType<typeof formatCount>;
    total_cost: ReturnType<typeof formatMoney>;
}

export function formatStatsAPIKeyUsage(item: StatsAPIKeyUsage): StatsAPIKeyUsageFormatted {
    return {
        api_key_id: item.api_key_id,
        input_token: formatCount(item.input_token),
        output_token: formatCount(item.output_token),
        total_token: formatCount(item.input_token + item.output_token),
        input_cost: formatMoney(item.input_cost),
        output_cost: formatMoney(item.output_cost),
        total_cost: formatMoney(item.input_cost + item.output_cost),
        wait_time: formatTime(item.wait_time),
        request_success: formatCount(item.request_success),
        request_failed: formatCount(item.request_failed),
        request_count: formatCount(item.request_success + item.request_failed),
    };
}
/**
 * 获取今日统计数据 Hook
 */
export function useStatsToday() {
    return useQuery({
        queryKey: ['stats', 'today'],
        queryFn: async () => {
            return apiClient.get<StatsDaily>('/api/v1/stats/today');
        },
        refetchInterval: 30000,
        refetchOnMount: 'always',
    });
}

/**
 * 获取每日统计数据 Hook
 */
export function useStatsDaily() {
    return useQuery({
        queryKey: ['stats', 'daily'],
        queryFn: async () => {
            return apiClient.get<StatsDaily[]>('/api/v1/stats/daily');
        },
        select: (data) => data.map((item): StatsDailyFormatted => ({
            ...formatStatsMetrics(item),
            date: item.date,
        })),
        refetchInterval: 3600000, // 1 小时
        refetchOnMount: 'always',
    });
}
/**
 * 获取总统计数据 Hook
 */
export function useStatsHourly() {
    return useQuery({
        queryKey: ['stats', 'hourly'],
        queryFn: async () => {
            return apiClient.get<StatsHourly[]>('/api/v1/stats/hourly');
        },
        select: (data) => data.map((item): StatsHourlyFormatted => ({
            ...formatStatsMetrics(item),
            hour: item.hour,
            date: item.date,
        })),
        refetchInterval: 10000,// 10 秒
        refetchOnMount: 'always',
    });
}

export function useStatsTotal() {
    return useQuery({
        queryKey: ['stats', 'total'],
        queryFn: async () => {
            return apiClient.get<StatsTotal>('/api/v1/stats/total');
        },
        select: formatStatsMetrics,
        refetchInterval: 10000,// 10 秒
        refetchOnMount: 'always',
    });
}

export function useModelHealth() {
    return useQuery({
        queryKey: ['stats', 'model-health'],
        queryFn: async () => {
            return apiClient.get<ModelHealthResponse>('/api/v1/stats/model-health');
        },
        refetchInterval: 10000,
        refetchOnMount: 'always',
    });
}

export function useStatsModelRank() {
    return useQuery({
        queryKey: ['stats', 'model-rank'],
        queryFn: async () => {
            return apiClient.get<StatsModelRank[]>('/api/v1/stats/model-rank');
        },
        select: (data) => data.map((item): StatsModelRankFormatted => {
            const requestSuccess = item.request_success ?? 0;
            const requestFailed = item.request_failed ?? 0;
            const inputToken = item.input_token ?? 0;
            const outputToken = item.output_token ?? 0;

            return {
                model: item.request_model_name?.trim() || item.model?.trim() || item.name?.trim() || 'unknown',
                request_count: formatCount(item.request_count ?? requestSuccess + requestFailed),
                request_success: formatCount(requestSuccess),
                request_failed: formatCount(requestFailed),
                input_token: formatCount(inputToken),
                output_token: formatCount(outputToken),
                total_token: formatCount(item.total_token ?? inputToken + outputToken),
                total_cost: formatMoney(item.total_cost),
                cache_hit_token: formatCount(item.cache_hit_token),
                cache_input_token: formatCount(item.cache_input_token),
                cache_write_token: formatCount(item.cache_write_token),
                cache_hit_rate: formatPercent(item.cache_hit_rate),
                first_token_p90_ms: formatTime(item.first_token_p90_ms),
                avg_throughput: formatCount(item.avg_throughput),
                recent_actual_models: item.recent_actual_models ?? [],
            };
        }),
        refetchInterval: 30000,
        refetchOnMount: 'always',
    });
}



/**
 * 获取 API Key 统计数据列表 Hook
 */
export function useStatsAPIKey() {
    return useQuery({
        queryKey: ['stats', 'apikey'],
        queryFn: async () => {
            return apiClient.get<StatsAPIKeyUsage[]>('/api/v1/stats/apikey');
        },
        select: (data) => data.map(formatStatsAPIKeyUsage),
        refetchInterval: 30000,
        refetchOnMount: 'always',
    });
}

export function useUserUsageRank() {
    return useQuery({
        queryKey: ['stats', 'user-rank'],
        queryFn: async () => {
            return apiClient.get<UserUsageSummary[]>('/api/v1/stats/user-rank');
        },
        refetchInterval: 30000,
        refetchOnMount: 'always',
    });
}
