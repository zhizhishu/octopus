// 用户页的时间 / 数字小工具（与界面文字无关，文字在组件里按语言拼）。

export const DAY_SECONDS = 24 * 60 * 60;

export function nowUnix() {
    return Math.floor(Date.now() / 1000);
}

export type RelativeTime =
    | { kind: 'never' }
    | { kind: 'justNow' }
    | { kind: 'minutes'; n: number }
    | { kind: 'hours'; n: number }
    | { kind: 'yesterday' }
    | { kind: 'days'; n: number }
    | { kind: 'date'; ts: number };

export function relativeTime(ts: number | undefined, nowSec: number): RelativeTime {
    if (!ts || ts <= 0) return { kind: 'never' };
    const diff = Math.max(0, nowSec - ts);
    if (diff < 60) return { kind: 'justNow' };
    if (diff < 3600) return { kind: 'minutes', n: Math.floor(diff / 60) };
    const now = new Date(nowSec * 1000);
    const startOfToday = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime() / 1000;
    if (ts >= startOfToday) return { kind: 'hours', n: Math.max(1, Math.floor(diff / 3600)) };
    if (ts >= startOfToday - DAY_SECONDS) return { kind: 'yesterday' };
    const days = Math.ceil((startOfToday - ts) / DAY_SECONDS);
    if (days < 7) return { kind: 'days', n: days };
    return { kind: 'date', ts };
}

export function unixToDateInput(ts?: number) {
    if (!ts) return '';
    const d = new Date(ts * 1000);
    return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}

/** 到期日按那天 23:59:59 算（与旧页面一致）。 */
export function dateInputToEndOfDayUnix(value: string) {
    if (!value) return 0;
    const [y, m, d] = value.split('-').map(Number);
    if (!y || !m || !d) return 0;
    return Math.floor(new Date(y, m - 1, d, 23, 59, 59).getTime() / 1000);
}

/** +7 天 / +30 天：还没到期就从原到期日往后加，已经到期或没开就从今天 23:59:59 往后加。 */
export function extendExpire(expireAt: number, days: number, nowSec: number) {
    const now = new Date(nowSec * 1000);
    const endOfToday = Math.floor(new Date(now.getFullYear(), now.getMonth(), now.getDate(), 23, 59, 59).getTime() / 1000);
    const base = expireAt > nowSec ? expireAt : endOfToday;
    return base + days * DAY_SECONDS;
}

export function daysLeft(expireAt: number, nowSec: number) {
    if (expireAt <= nowSec) return 0;
    return Math.ceil((expireAt - nowSec) / DAY_SECONDS);
}

export function formatTokens(n: number) {
    const v = Math.max(0, Number(n) || 0);
    // 卡片窄：一个单位里到 100 以上就不要小数（415K、3.6M），不然「输入 · 输出」一行放不下。
    const unit = (x: number, suffix: string) => `${x >= 100 ? Math.round(x) : Number(x.toFixed(1))}${suffix}`;
    if (v >= 1_000_000_000) return unit(v / 1_000_000_000, 'B');
    if (v >= 1_000_000) return unit(v / 1_000_000, 'M');
    if (v >= 1_000) return unit(v / 1_000, 'K');
    return String(Math.round(v));
}

export function formatCount(n: number) {
    return (Number(n) || 0).toLocaleString('en-US');
}
