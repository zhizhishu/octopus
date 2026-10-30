// API Key 过期时间：日期和时分都按浏览器本地时区理解，存成 Unix 秒。
// 以前日期取本地、时分却按 UTC 拼（Date.UTC），西半球会显示错、还会提前过期。

/** 取消「永不过期」时的默认有效期：从今天起第 30 天的本地 23:59。 */
export const DEFAULT_EXPIRE_DAYS = 30;
export const DEFAULT_EXPIRE_TIME = '23:59';

/** 本地日期 + 本地 HH:mm → Unix 秒。时间格式不对按 00:00。 */
export function toExpireAt(date: Date, time: string): number {
    const t = /^\d{2}:\d{2}$/.test(time) ? time : '00:00';
    const [hh, mm] = t.split(':').map(Number);
    const d = new Date(date.getFullYear(), date.getMonth(), date.getDate(), hh, mm, 0, 0);
    return Math.floor(d.getTime() / 1000);
}

/** Unix 秒 → 本地 HH:mm；没有或非法时返回 fallback。 */
export function expireTimeOf(expireAt: number | undefined, fallback = DEFAULT_EXPIRE_TIME): string {
    if (!expireAt) return fallback;
    const d = new Date(expireAt * 1000);
    if (isNaN(d.getTime())) return fallback;
    return `${d.getHours().toString().padStart(2, '0')}:${d.getMinutes().toString().padStart(2, '0')}`;
}

/** 默认有效期：now 所在本地日期 + 30 天的 23:59。 */
export function defaultExpireAt(now: Date = new Date()): number {
    const day = new Date(now.getFullYear(), now.getMonth(), now.getDate() + DEFAULT_EXPIRE_DAYS);
    return toExpireAt(day, DEFAULT_EXPIRE_TIME);
}

/** 有过期时间且不晚于 now。 */
export function isPastExpire(expireAt: number | undefined, nowMs: number = Date.now()): boolean {
    return !!expireAt && expireAt * 1000 <= nowMs;
}

/** 本地今天 00:00，用于日历禁选今天之前的日子。 */
export function startOfLocalDay(now: Date = new Date()): Date {
    return new Date(now.getFullYear(), now.getMonth(), now.getDate());
}
