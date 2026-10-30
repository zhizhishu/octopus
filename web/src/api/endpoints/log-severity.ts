export type RelayLogSeverity = 'success' | 'warn' | 'error';

/** 徽章计数的轮询间隔。筛选变化和手动刷新不走这个间隔。 */
export const LOG_SEVERITY_COUNT_POLL_MS = 60_000;

/**
 * 与后端 model.ClassifyRelayLogSeverity / Go strings.TrimSpace 对齐的分桶。
 * String.prototype.trim 吃掉空格、\\t \\n \\r \\f \\v 以及 NBSP，和 Go 的 TrimSpace
 * 在这些字符上一致。不要改成只剥空格：那是 SQL TRIM() 的语义，和落库的 severity 列不一致。
 */
export function classifyRelayLogSeverity(log: {
    error?: string | null;
    error_code?: string | null;
    error_status?: number | null;
    total_attempts?: number | null;
    attempts?: readonly unknown[] | null;
}): RelayLogSeverity {
    const hasError =
        !!log.error?.trim() ||
        !!log.error_code?.trim() ||
        ((log.error_status ?? 0) >= 400);

    if (hasError) return 'error';

    // Warn == 最终成功但真实尝试次数 > 1（重试/故障转移）。total_attempts 为 0
    // 时不能退回 attempts.length：0 是「没有尝试」，不是「字段缺失」。
    const attemptCount = log.total_attempts ?? log.attempts?.length ?? 0;
    if (attemptCount > 1) return 'warn';
    return 'success';
}

/**
 * 计数查询的刷新策略。
 * poll=false 时不轮询、不在聚焦/重连时重取：给「空列表的全时段总数」用，
 * 那条查询没有时间范围，反复打就是全表扫。
 */
export function logSeverityCountQueryOptions(poll: boolean): {
    staleTime: number;
    refetchOnMount: boolean | 'always';
    refetchOnWindowFocus: boolean;
    refetchOnReconnect: boolean;
    refetchInterval: number | false;
    refetchIntervalInBackground: boolean;
} {
    if (!poll) {
        return {
            staleTime: Number.POSITIVE_INFINITY,
            refetchOnMount: false,
            refetchOnWindowFocus: false,
            refetchOnReconnect: false,
            refetchInterval: false,
            refetchIntervalInBackground: false,
        };
    }
    return {
        staleTime: 0,
        refetchOnMount: 'always',
        refetchOnWindowFocus: true,
        refetchOnReconnect: true,
        refetchInterval: LOG_SEVERITY_COUNT_POLL_MS,
        refetchIntervalInBackground: false,
    };
}
