/**
 * 仪表盘的纯计算：时间段、上一段、合计、涨跌、趋势点。没有 React，方便单测。
 *
 * 日期一律是 YYYY-MM-DD 字符串，按统计时区的「今天」（/stats/today 的 date）推算，
 * 不读浏览器时区，免得服务器和浏览器不在一个时区时「今天」对不上。
 */

export type RangePreset = 'today' | 'yesterday' | '7d' | '30d' | 'month' | 'custom';
export type Granularity = 'hour' | 'day';
export type TrendMetric = 'cost' | 'count' | 'tokens' | 'success';

export interface DateRange {
    from: string;
    to: string;
}

export const MAX_RANGE_DAYS = 90;
export const RANGE_PRESETS: readonly Exclude<RangePreset, 'custom'>[] = ['today', 'yesterday', '7d', '30d', 'month'];

const ISO_RE = /^(\d{4})-(\d{2})-(\d{2})$/;

function toUTC(iso: string): number {
    const m = ISO_RE.exec(iso);
    if (!m) return NaN;
    return Date.UTC(Number(m[1]), Number(m[2]) - 1, Number(m[3]));
}

function fromUTC(ms: number): string {
    const d = new Date(ms);
    const y = d.getUTCFullYear();
    const mo = String(d.getUTCMonth() + 1).padStart(2, '0');
    const da = String(d.getUTCDate()).padStart(2, '0');
    return `${y}-${mo}-${da}`;
}

export function isISODate(value: string): boolean {
    return ISO_RE.test(value) && fromUTC(toUTC(value)) === value;
}

/** 20261030 -> 2026-10-30；已经是 ISO 的原样返回。 */
export function compactToISO(value: string | undefined | null): string {
    if (!value) return '';
    if (/^\d{8}$/.test(value)) return `${value.slice(0, 4)}-${value.slice(4, 6)}-${value.slice(6, 8)}`;
    return value;
}

export function isoToCompact(value: string): string {
    return value.replaceAll('-', '');
}

export function addDays(iso: string, days: number): string {
    return fromUTC(toUTC(iso) + days * 86400000);
}

/** 含首尾的天数。 */
export function rangeDays(range: DateRange): number {
    return Math.round((toUTC(range.to) - toUTC(range.from)) / 86400000) + 1;
}

export function listDates(range: DateRange): string[] {
    const n = rangeDays(range);
    const out: string[] = [];
    for (let i = 0; i < n; i++) out.push(addDays(range.from, i));
    return out;
}

export function presetRange(preset: Exclude<RangePreset, 'custom'>, today: string): DateRange {
    switch (preset) {
        case 'today':
            return { from: today, to: today };
        case 'yesterday': {
            const y = addDays(today, -1);
            return { from: y, to: y };
        }
        case '7d':
            return { from: addDays(today, -6), to: today };
        case '30d':
            return { from: addDays(today, -29), to: today };
        case 'month':
            return { from: `${today.slice(0, 8)}01`, to: today };
    }
}

/** 紧挨在前面、一样长的一段。 */
export function previousRange(range: DateRange): DateRange {
    const n = rangeDays(range);
    return { from: addDays(range.from, -n), to: addDays(range.from, -1) };
}

export function isSingleDay(range: DateRange): boolean {
    return range.from === range.to;
}

/** 一天的时间段默认按小时，多天按天；按小时只支持一天（后端同样限制）。 */
export function defaultGranularity(range: DateRange): Granularity {
    return isSingleDay(range) ? 'hour' : 'day';
}

export function effectiveGranularity(range: DateRange, wanted: Granularity): Granularity {
    return isSingleDay(range) ? wanted : 'day';
}

/** 快捷段（自定义面板里）：都不超过 90 天、都不晚于今天。 */
export function quickRanges(today: string): Record<'last14' | 'lastMonth' | 'last30' | 'last90', DateRange> {
    const firstOfMonth = `${today.slice(0, 8)}01`;
    const lastMonthEnd = addDays(firstOfMonth, -1);
    return {
        last14: { from: addDays(today, -13), to: today },
        lastMonth: { from: `${lastMonthEnd.slice(0, 8)}01`, to: lastMonthEnd },
        last30: { from: addDays(today, -29), to: today },
        last90: { from: addDays(today, -(MAX_RANGE_DAYS - 1)), to: today },
    };
}

export type RangeProblem = 'format' | 'order' | 'future' | 'tooLong';

/** 自定义段的校验，和后端 ParseStatsRange 同一套规则。 */
export function validateRange(range: DateRange, today: string): RangeProblem | null {
    if (!isISODate(range.from) || !isISODate(range.to)) return 'format';
    if (range.to < range.from) return 'order';
    if (range.to > today) return 'future';
    if (rangeDays(range) > MAX_RANGE_DAYS) return 'tooLong';
    return null;
}

/** 当前选中的是哪个预设（自定义选出来的段正好等于某个预设时，也亮那个预设）。 */
export function matchPreset(range: DateRange, today: string): RangePreset {
    for (const preset of RANGE_PRESETS) {
        const r = presetRange(preset, today);
        if (r.from === range.from && r.to === range.to) return preset;
    }
    return 'custom';
}

/* ---------------- 合计 ---------------- */

/** /stats/daily、/stats/today 的一行（StatsMetrics）。 */
export interface DailyMetrics {
    date?: string;
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

/** 日志接口的一行（StatsUsage）。 */
export interface LogUsage {
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

export interface Usage {
    success: number;
    failed: number;
    input: number;
    output: number;
    cacheHit: number;
    cacheWrite: number;
    cacheInput: number;
    cost: number;
    wait: number;
}

export const EMPTY_USAGE: Usage = { success: 0, failed: 0, input: 0, output: 0, cacheHit: 0, cacheWrite: 0, cacheInput: 0, cost: 0, wait: 0 };

export function fromDaily(row: DailyMetrics | undefined | null): Usage {
    if (!row) return { ...EMPTY_USAGE };
    return {
        success: row.request_success || 0,
        failed: row.request_failed || 0,
        input: row.input_token || 0,
        output: row.output_token || 0,
        cacheHit: row.cache_hit_token || 0,
        cacheWrite: row.cache_write_token || 0,
        cacheInput: row.cache_input_token || 0,
        cost: (row.input_cost || 0) + (row.output_cost || 0),
        wait: row.wait_time || 0,
    };
}

export function fromLog(row: LogUsage | undefined | null): Usage {
    if (!row) return { ...EMPTY_USAGE };
    return {
        success: row.request_success || 0,
        failed: row.request_failed || 0,
        input: row.input_token || 0,
        output: row.output_token || 0,
        cacheHit: row.cache_hit_token || 0,
        cacheWrite: row.cache_write_token || 0,
        cacheInput: row.cache_input_token || 0,
        cost: row.total_cost || 0,
        wait: row.wait_time || 0,
    };
}

/** 缓存率分母：和主页（stats.ts getCacheRateBase）同一口径。 */
export function cacheRateBase(u: Usage): number {
    const reported = Math.max(0, u.cacheInput);
    const sum = Math.max(0, u.cacheHit) + Math.max(0, u.cacheWrite);
    if (reported > 0) return Math.max(reported, sum);
    if (sum > 0) return Math.max(Math.max(0, u.input), sum);
    return Math.max(0, u.input);
}

/** Tokens 合计 = 有效输入 + 输出，和主页的 total_token 同一口径。 */
export function totalTokens(u: Usage): number {
    return Math.max(Math.max(0, u.input), cacheRateBase(u)) + u.output;
}

export interface Summary {
    requests: number;
    success: number;
    successRate: number | null;
    cost: number;
    tokens: number;
    output: number;
    cacheHit: number;
    cacheRate: number | null;
    avgMs: number | null;
}

/** 多行合计。Tokens 逐行算再相加、缓存率用合计的命中 / 合计的分母——都和主页趋势卡一样。 */
export function summarize(rows: Usage[]): Summary {
    let success = 0, failed = 0, cost = 0, tokens = 0, output = 0, cacheHit = 0, base = 0, wait = 0;
    for (const u of rows) {
        success += u.success;
        failed += u.failed;
        cost += u.cost;
        tokens += totalTokens(u);
        output += u.output;
        cacheHit += u.cacheHit;
        base += cacheRateBase(u);
        wait += u.wait;
    }
    const requests = success + failed;
    return {
        requests,
        success,
        successRate: requests > 0 ? success / requests : null,
        cost,
        tokens,
        output,
        cacheHit,
        cacheRate: base > 0 ? cacheHit / base : null,
        avgMs: requests > 0 ? wait / requests : null,
    };
}

/** 涨跌：上一段是 0 或没有数据时没法比，返回 null（页面显示「—」）。 */
export function pctChange(current: number, previous: number): number | null {
    if (!Number.isFinite(current) || !Number.isFinite(previous) || previous <= 0) return null;
    return (current - previous) / previous;
}

/**
 * 按天取一段时间的每天用量。今天那一行用 /stats/today（内存里的实时值），
 * 库里 stats_daily 的「今天」是定时落库的、会慢一拍。
 */
export function dailySeries(range: DateRange, daily: DailyMetrics[] | undefined, today: string, todayRow?: DailyMetrics | null): Usage[] {
    const byDate = new Map<string, DailyMetrics>();
    for (const row of daily ?? []) {
        if (row.date) byDate.set(compactToISO(row.date), row);
    }
    return listDates(range).map((date) => {
        if (date === today && todayRow) return fromDaily(todayRow);
        return fromDaily(byDate.get(date));
    });
}

export function metricValue(u: Usage | undefined, metric: TrendMetric): number | null {
    if (!u) return null;
    switch (metric) {
        case 'cost':
            return u.cost;
        case 'count':
            return u.success + u.failed;
        case 'tokens':
            return totalTokens(u);
        case 'success': {
            const n = u.success + u.failed;
            return n > 0 ? (u.success / n) * 100 : null;
        }
    }
}

export interface TrendPoint {
    label: string;
    current: number | null;
    previous: number | null;
}

/**
 * 趋势点：current 是这一段，previous 是上一段同一位置（虚线）。
 * currentCut = 这一段只画到第几格（今天按小时时，还没到的小时不画）。
 */
export function buildTrend(labels: string[], current: Usage[], previous: Usage[], metric: TrendMetric, currentCut?: number): TrendPoint[] {
    return labels.map((label, i) => ({
        label,
        current: currentCut !== undefined && i > currentCut ? null : metricValue(current[i], metric),
        previous: metricValue(previous[i], metric),
    }));
}

/** 「花在哪」：前几项 + 「其他 N 个」，占比按花费算。 */
export interface ShareSlice {
    key: string;
    name: string;
    cost: number;
    share: number;
    isOthers?: boolean;
    othersCount?: number;
}

export function shareSlices(items: { key: string; name: string; total_cost: number }[], others: { total_cost: number } | undefined, othersCount: number, total: number): ShareSlice[] {
    const slices: ShareSlice[] = items.map((item) => ({
        key: item.key,
        name: item.name,
        cost: item.total_cost,
        share: total > 0 ? item.total_cost / total : 0,
    }));
    if (others && othersCount > 0) {
        slices.push({
            key: '__others__',
            name: '',
            cost: others.total_cost,
            share: total > 0 ? others.total_cost / total : 0,
            isOthers: true,
            othersCount,
        });
    }
    return slices;
}

/**
 * 日志清理提示：这段的开头早于库里最早一条日志 → 那几天的日志没了，按日志算的部分（花在哪、
 * 排行、活跃用户、前 5 名）算不进来。返回最早日志所在的日期（统计时区里由调用方格式化）。
 */
export function logsCleanedBefore(rangeStartUnix: number | undefined, oldestLogUnix: number | undefined): boolean {
    if (!rangeStartUnix || !oldestLogUnix) return false;
    return oldestLogUnix > rangeStartUnix;
}

/** 当前小时格（今天按小时时，上一段只比到同一个钟点）。 */
export function currentHourIndex(hourStarts: number[], nowUnix: number): number {
    let idx = -1;
    for (let i = 0; i < hourStarts.length; i++) {
        if (hourStarts[i] <= nowUnix) idx = i;
    }
    return idx;
}
