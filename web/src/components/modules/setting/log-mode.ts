// 日志模式（普通 / 调试）的纯函数：读设置、算自动切回普通的时间。
// 与后端 internal/op/log_mode.go 同一套规则：
//   - log_mode 只认 "debug"，其它一律当普通；
//   - 自动关闭时刻 = log_debug_since + log_debug_auto_off_hours 小时；任一为 0 = 不会自动关。

export type LogMode = 'normal' | 'debug';

/** 设置页给的「自动切回普通」选项（小时）。0 = 不自动。 */
export const LOG_DEBUG_AUTO_OFF_CHOICES = [1, 6, 24, 72, 168, 0] as const;
export const DEFAULT_LOG_DEBUG_AUTO_OFF_HOURS = 24;
export const MAX_LOG_DEBUG_AUTO_OFF_HOURS = 168;

export function parseLogMode(value: string | undefined): LogMode {
    return (value ?? '').trim() === 'debug' ? 'debug' : 'normal';
}

export function parseAutoOffHours(value: string | undefined): number {
    const trimmed = (value ?? '').trim();
    if (!/^\d+$/.test(trimmed)) return DEFAULT_LOG_DEBUG_AUTO_OFF_HOURS;
    const n = Number.parseInt(trimmed, 10);
    if (n < 0 || n > MAX_LOG_DEBUG_AUTO_OFF_HOURS) return DEFAULT_LOG_DEBUG_AUTO_OFF_HOURS;
    return n;
}

/** 自动切回普通的时刻（毫秒时间戳）；不会自动关时返回 null。 */
export function logDebugExpiresAtMs(sinceValue: string | undefined, hours: number): number | null {
    const since = Number.parseInt((sinceValue ?? '').trim(), 10);
    if (!Number.isFinite(since) || since <= 0 || hours <= 0) return null;
    return (since + hours * 3600) * 1000;
}

/** 剩余时间拆成 小时 / 分钟（向上取整到分钟，最少 1 分钟）。 */
export function remainingParts(expiresAtMs: number, nowMs: number): { hours: number; minutes: number } {
    const leftMinutes = Math.max(1, Math.ceil((expiresAtMs - nowMs) / 60000));
    return { hours: Math.floor(leftMinutes / 60), minutes: leftMinutes % 60 };
}

/** 选项按钮上的短文案键：1h / 6h / 24h / 3d / 7d / never。 */
export function autoOffChoiceKey(hours: number): string {
    if (hours === 0) return 'never';
    if (hours % 24 === 0 && hours >= 72) return `${hours / 24}d`;
    return `${hours}h`;
}

/** 读当前时间（毫秒）；单独放这里，方便组件在事件里刷新「还剩多久」。 */
export function nowMs(): number {
    return Date.now();
}
