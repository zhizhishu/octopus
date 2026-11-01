// 日志页「自动刷新」的纯逻辑：选项、本地记忆、暂停判定、新日志缓冲合并、新行识别。
// 不依赖 React / 浏览器全局，方便 node --test 直接测（tests/log-auto-refresh.test.cjs）。
//
// 传输层复用已有的日志 SSE（/api/v1/log/stream）：新日志先进缓冲，按所选节奏、且在「没被
// 打扰」时才插进列表顶部。SSE 断开时才退回按同一节奏轮询第 1 页。

export const LOG_AUTO_REFRESH_STORAGE_KEY = 'octopus.log.autoRefresh';
export const LIVE_ACTIVITY_COLLAPSED_STORAGE_KEY = 'octopus.log.liveActivityCollapsed';

/** off=不自动刷新；live=来一条插一条（原「实时动态」）；其余=每 N 秒合并插入一次。 */
export const LOG_AUTO_REFRESH_CHOICES = ['off', 'live', '5s', '10s', '30s'] as const;
export type LogAutoRefresh = (typeof LOG_AUTO_REFRESH_CHOICES)[number];

export function parseLogAutoRefresh(raw: unknown): LogAutoRefresh {
    const value = typeof raw === 'string' ? raw.trim() : '';
    return (LOG_AUTO_REFRESH_CHOICES as readonly string[]).includes(value) ? (value as LogAutoRefresh) : 'off';
}

/** 合并插入的节奏（毫秒）。live=0 表示来了就插；off=null 表示不连流。 */
export function logAutoRefreshFlushMs(mode: LogAutoRefresh): number | null {
    switch (mode) {
        case 'live': return 0;
        case '5s': return 5_000;
        case '10s': return 10_000;
        case '30s': return 30_000;
        default: return null;
    }
}

/** SSE 断开时的兜底轮询间隔：同所选节奏，「实时」按 5 秒，避免断线时每秒扫库。 */
export function logAutoRefreshPollMs(mode: LogAutoRefresh): number | null {
    const flush = logAutoRefreshFlushMs(mode);
    if (flush === null) return null;
    return Math.max(flush, 5_000);
}

export type LogAutoRefreshPauseReason = 'hidden' | 'detail' | 'selecting' | 'scrolled';

export interface LogAutoRefreshPauseInput {
    /** document.visibilityState === 'hidden' */
    hidden: boolean;
    /** 详情抽屉或任何弹窗开着 */
    dialogOpen: boolean;
    /** 页面里有非空的文字选区 */
    selecting: boolean;
    /** 列表滚动容器的 scrollTop */
    scrollTop: number;
}

/** 列表离顶多少像素以内仍算「在顶部」：往顶部插行不会把正在看的内容顶走。 */
export const LOG_AUTO_REFRESH_TOP_SLACK_PX = 8;

/** 返回暂停原因（优先级：后台 > 弹窗 > 选字 > 已下滚），null 表示可以插入。 */
export function logAutoRefreshPauseReason(input: LogAutoRefreshPauseInput): LogAutoRefreshPauseReason | null {
    if (input.hidden) return 'hidden';
    if (input.dialogOpen) return 'detail';
    if (input.selecting) return 'selecting';
    if (input.scrollTop > LOG_AUTO_REFRESH_TOP_SLACK_PX) return 'scrolled';
    return null;
}

/** 缓冲里最多攒多少条；再多就只留最新的（反正插进来也只看第 1 页）。 */
export const LOG_AUTO_REFRESH_BUFFER_MAX = 200;

interface WithID { id: number }

/** 把新到的一条并进缓冲：按 id 去重、新的在前、超上限丢最旧的。返回新数组，不改入参。 */
export function bufferLog<T extends WithID>(buffer: readonly T[], incoming: T, max = LOG_AUTO_REFRESH_BUFFER_MAX): T[] {
    if (buffer.some((item) => item.id === incoming.id)) return buffer.slice();
    const next = [incoming, ...buffer];
    next.sort((a, b) => b.id - a.id);
    return next.length > max ? next.slice(0, max) : next;
}

/**
 * 把一批缓冲日志插到第 1 页顶部（无限查询的 pages 结构），已在列表里的跳过。
 * 返回 { pages, inserted }；一条都没插时 pages 原样返回（引用不变，React Query 不触发重渲染）。
 */
export function prependLogBatch<T extends WithID>(pages: readonly (readonly T[] | null | undefined)[], batch: readonly T[]): { pages: T[][]; inserted: number[] } {
    const existing = new Set<number>();
    for (const page of pages) for (const item of page ?? []) existing.add(item.id);
    const fresh = batch.filter((item) => !existing.has(item.id)).slice().sort((a, b) => b.id - a.id);
    const normalized = pages.map((page) => (page ?? []).slice());
    if (fresh.length === 0) return { pages: normalized, inserted: [] };
    const first = normalized[0] ?? [];
    return { pages: [[...fresh, ...first], ...normalized.slice(1)], inserted: fresh.map((item) => item.id) };
}
