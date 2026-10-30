// 调整余额弹窗里那一行预览的算法。与后端 /api/v1/user/balance 的规则一一对应：
//   - 加 / 减：金额必须 > 0；减的金额不能超过当前余额（后端会拒绝，一分不动）。
//   - 直接改成：金额必须 >= 0。
// 结果按后端 roundBalance 一样保留 6 位小数，避免预览和落库差一个浮点尾巴。

export type BalanceMode = 'add' | 'subtract' | 'set';

export type BalancePreviewError = 'empty' | 'invalid' | 'not_positive' | 'negative' | 'not_enough';

export interface BalancePreview {
    ok: boolean;
    amount: number;
    next: number;
    delta: number;
    error?: BalancePreviewError;
}

export function roundBalance(value: number): number {
    return Math.round(value * 1_000_000) / 1_000_000;
}

export function parseAmount(raw: string): number | null {
    const text = raw.trim().replace(/^\$/, '').replace(/,/g, '');
    if (text === '') return null;
    if (!/^[+-]?(\d+(\.\d*)?|\.\d+)$/.test(text)) return Number.NaN;
    return Number(text);
}

export function computeBalancePreview(current: number, mode: BalanceMode, raw: string): BalancePreview {
    const base = roundBalance(Number.isFinite(current) ? current : 0);
    const parsed = parseAmount(raw);
    const fail = (error: BalancePreviewError, amount = 0): BalancePreview => ({ ok: false, amount, next: base, delta: 0, error });
    if (parsed === null) return fail('empty');
    if (!Number.isFinite(parsed)) return fail('invalid');
    const amount = roundBalance(parsed);
    if (mode === 'set') {
        if (amount < 0) return fail('negative', amount);
        return { ok: true, amount, next: amount, delta: roundBalance(amount - base) };
    }
    if (amount <= 0) return fail('not_positive', amount);
    if (mode === 'subtract') {
        if (amount > base) return fail('not_enough', amount);
        return { ok: true, amount, next: roundBalance(base - amount), delta: -amount };
    }
    return { ok: true, amount, next: roundBalance(base + amount), delta: amount };
}

/** $42.18；不到 1 美元的零头多给两位，免得 $0.0042 显示成 $0.00。 */
export function formatUSD(value: number | undefined | null): string {
    const v = Number(value ?? 0);
    if (!Number.isFinite(v)) return '$0.00';
    const abs = Math.abs(v);
    const sign = v < 0 ? '-' : '';
    if (abs === 0) return '$0.00';
    if (abs < 0.01) return `${sign}$${abs.toFixed(4)}`;
    return `${sign}$${abs.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`;
}
