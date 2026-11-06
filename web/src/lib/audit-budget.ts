/**
 * 后台检测的额度逻辑（纯函数，不引 React、不引 api 类型）。
 *
 * 组件和 node 测试共用同一份读法，否则迟早出现「卡片说一套、额度算法说另一套」。
 * 三件事只在这里做：
 *
 *   1. 读「今天已用多少请求」。只认日期是今天的那份 JSON；跨天、坏 JSON、负数
 *      一律当 0 —— 读错已用量会让界面谎报额度，宁可按 0 说，也不编一个数字出来。
 *   2. 把设置里的字符串收窄成合法取值（每渠道每天轮数 1-4、全站每天上限正整数）。
 *      放宽任何一条都等于替用户多花钱，所以越界一律按默认值。
 *   3. 布尔设置只认 "true"/"false"：别的写法（"1"、"on"、空串）都算没写，用调用方的 fallback。
 */

/** 全站每天请求上限的默认值（后端同期实现，默认 200）。 */
export const AUDIT_BUDGET_DEFAULT_LIMIT = 200;
/** 上限的天花板：再大就不是「少量 token 抽检」了。 */
export const AUDIT_BUDGET_MAX_LIMIT = 100000;

/** 每渠道每天的抽检轮数。 */
export type AuditRounds = 1 | 2 | 3 | 4;

function pad2(value: number): string {
    return String(value).padStart(2, '0');
}

/**
 * 本地时区的 YYYY-MM-DD。
 *
 * 不能用 toISOString().slice(0,10)：那是 UTC 日期，东八区晚上八点以后会算成第二天，
 * 于是「今日已用」和后台按本地日期滚动的额度对不上。
 */
export function todayLocal(date: Date = new Date()): string {
    return `${date.getFullYear()}-${pad2(date.getMonth() + 1)}-${pad2(date.getDate())}`;
}

/**
 * 读 `model_audit_budget_usage`（`{"date":"YYYY-MM-DD","requests":N}`）。
 *
 * 日期不是 today → 0（昨天的用量今天不算）；解析失败 / 不是对象 / requests
 * 不是非负数 → 0。只有「确实是今天的、确实是数」才返回那个数。
 */
export function parseBudgetUsage(raw: string | undefined, today: string): number {
    if (!raw) return 0;
    let parsed: unknown;
    try {
        parsed = JSON.parse(raw);
    } catch {
        return 0;
    }
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return 0;
    const record = parsed as { date?: unknown; requests?: unknown };
    if (record.date !== today) return 0;
    const requests = record.requests;
    if (typeof requests !== 'number' || !Number.isFinite(requests) || requests < 0) return 0;
    return Math.floor(requests);
}

/** 每渠道每天轮数：只有 1-4 的整数算数，其余（含 0、5、"abc"、空）按 1。 */
export function clampRounds(raw: string | undefined): AuditRounds {
    const value = (raw ?? '').trim();
    if (!/^[0-9]+$/.test(value)) return 1;
    const n = Number(value);
    return n === 2 ? 2 : n === 3 ? 3 : n === 4 ? 4 : 1;
}

/** 全站每天上限：非正整数 → 200；超过 100000 按 100000。 */
export function clampLimit(raw: string | undefined): number {
    const value = (raw ?? '').trim();
    if (!/^[0-9]+$/.test(value)) return AUDIT_BUDGET_DEFAULT_LIMIT;
    const n = Number(value);
    if (!Number.isFinite(n) || n <= 0) return AUDIT_BUDGET_DEFAULT_LIMIT;
    return Math.min(Math.floor(n), AUDIT_BUDGET_MAX_LIMIT);
}

/**
 * 布尔设置读法：只有 "true" / "false" 两种写法算数。
 * 认不出的值（空、旧值、"1"）用 fallback，不由前端猜用户想说什么。
 */
export function settingBool(raw: string | undefined, fallback: boolean): boolean {
    if (raw === 'true') return true;
    if (raw === 'false') return false;
    return fallback;
}
