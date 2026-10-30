/** 仪表盘的数字格式：卡片上的大数字和表格里的小数字用同一套。 */

export function formatMoneyText(value: number | null | undefined): string {
    const v = Number(value ?? 0);
    if (!Number.isFinite(v) || v === 0) return '$0';
    const abs = Math.abs(v);
    if (abs >= 1e6) return `$${(v / 1e6).toFixed(2)}M`;
    if (abs < 0.01) return `$${v.toFixed(4)}`;
    return `$${v.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`;
}

/** 坐标轴上用的短金额：$240 / $1.2K / $0.05。 */
export function formatMoneyAxis(value: number): string {
    const abs = Math.abs(value);
    if (abs >= 1e6) return `$${(value / 1e6).toFixed(1)}M`;
    if (abs >= 1e3) return `$${(value / 1e3).toFixed(1)}K`;
    if (abs >= 10 || Number.isInteger(value)) return `$${Math.round(value)}`;
    if (abs >= 1) return `$${value.toFixed(1)}`;
    return `$${value.toFixed(2)}`;
}

export function formatCountText(value: number | null | undefined): string {
    const v = Number(value ?? 0);
    if (!Number.isFinite(v)) return '0';
    const abs = Math.abs(v);
    const short = (n: number, unit: string) => `${n >= 100 ? n.toFixed(0) : n.toFixed(1).replace(/\.0$/, '')}${unit}`;
    if (abs >= 1e9) return short(v / 1e9, 'B');
    if (abs >= 1e6) return short(v / 1e6, 'M');
    if (abs >= 1e4) return short(v / 1e3, 'K');
    return Math.round(v).toLocaleString('en-US');
}

export function formatPercentText(value: number | null | undefined, digits = 1): string {
    if (value === null || value === undefined || !Number.isFinite(value)) return '—';
    return `${(value * 100).toFixed(digits)}%`;
}

export function formatDurationText(ms: number | null | undefined): string {
    if (ms === null || ms === undefined || !Number.isFinite(ms)) return '—';
    if (ms < 1000) return `${Math.round(ms)}ms`;
    if (ms < 60000) return `${(ms / 1000).toFixed(1)}s`;
    return `${(ms / 60000).toFixed(1)}m`;
}

/** 2026-10-24 -> 10/24 */
export function shortDate(iso: string): string {
    return iso.length >= 10 ? `${iso.slice(5, 7)}/${iso.slice(8, 10)}` : iso;
}
