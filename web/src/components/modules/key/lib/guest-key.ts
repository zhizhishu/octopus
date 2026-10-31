// 访客 Key 的纯函数（不碰界面文字）。和后端 op/guest_key.go 的规则一一对应：
// 必须有模型、必须有到期时间、最长 365 天、上限不能为负；作废是终态。

export const GUEST_MAX_DAYS = 365;

/** 有效期预设：秒数。custom = 自己选日期时间。 */
export const GUEST_EXPIRE_PRESETS = [
    { id: '1h', sec: 3600 },
    { id: '1d', sec: 86400 },
    { id: '3d', sec: 3 * 86400 },
    { id: '7d', sec: 7 * 86400 },
    { id: '30d', sec: 30 * 86400 },
] as const;
export type GuestExpirePreset = typeof GUEST_EXPIRE_PRESETS[number]['id'] | 'custom';
export const DEFAULT_GUEST_PRESET: GuestExpirePreset = '1d';

export type GuestStatus = 'ok' | 'off' | 'expired' | 'no_cap' | 'used_up' | 'revoked';

export interface GuestKeyLike {
    id: number;
    name: string;
    note?: string;
    api_key: string;
    enabled: boolean;
    expire_at?: number;
    max_cost?: number;
    supported_models?: string;
    revoked_at?: number;
}

/** 状态和后端鉴权顺序一致：作废 > 停用 > 过期 > 没设上限（旧 Key，后端拒绝）> 用完（花到上限就拒绝）> 能用。 */
export function guestStatus(key: Pick<GuestKeyLike, 'enabled' | 'expire_at' | 'max_cost' | 'revoked_at'>, spent: number, nowSec: number): GuestStatus {
    if ((key.revoked_at ?? 0) > 0) return 'revoked';
    if (!key.enabled) return 'off';
    if ((key.expire_at ?? 0) > 0 && (key.expire_at as number) < nowSec) return 'expired';
    if (!((key.max_cost ?? 0) > 0)) return 'no_cap';
    if ((key.max_cost as number) <= spent) return 'used_up';
    return 'ok';
}

export function presetExpireAt(preset: GuestExpirePreset, nowSec: number): number | null {
    const p = GUEST_EXPIRE_PRESETS.find((x) => x.id === preset);
    return p ? nowSec + p.sec : null;
}

/** 剩余时长拆成「天 / 小时 / 分钟」里最大的那个单位，给「还剩 3 小时」这种显示用。 */
export function timeLeft(expireAt: number | undefined, nowSec: number): { unit: 'days' | 'hours' | 'minutes'; n: number; past: boolean } {
    const diff = (expireAt ?? 0) - nowSec;
    const abs = Math.abs(diff);
    const past = diff < 0;
    if (abs >= 86400) return { unit: 'days', n: Math.floor(abs / 86400), past };
    if (abs >= 3600) return { unit: 'hours', n: Math.floor(abs / 3600), past };
    return { unit: 'minutes', n: Math.max(1, Math.floor(abs / 60)), past };
}

export function guestModels(key: Pick<GuestKeyLike, 'supported_models'>): string[] {
    return String(key.supported_models ?? '').split(',').map((s) => s.trim()).filter(Boolean);
}

export interface GuestDraft {
    name: string;
    note: string;
    models: string[];
    expireAt: number;
    cap: string;
    enabled: boolean;
}

export type GuestDraftError = 'needName' | 'needModel' | 'needExpire' | 'pastExpire' | 'tooFar' | 'needCap' | 'badCap';

/** 保存前的检查，返回第一条错误（与后端同口径）；没问题返回 null。expireChanged=false 时不查过期（改别的字段时已过期的 Key 也能保存）。 */
export function validateGuestDraft(d: GuestDraft, nowSec: number, expireChanged = true): GuestDraftError | null {
    if (!d.name.trim()) return 'needName';
    if (d.models.filter((m) => m.trim()).length === 0) return 'needModel';
    if (expireChanged) {
        if (!d.expireAt) return 'needExpire';
        if (d.expireAt <= nowSec) return 'pastExpire';
        if (d.expireAt > nowSec + GUEST_MAX_DAYS * 86400) return 'tooFar';
    }
    // 花费上限必填且 > 0（访客 Key 不扣余额，上限是唯一的闸；后端同样拒绝 0 / 空）。
    if (d.cap.trim() === '') return 'needCap';
    if (!(Number(d.cap) > 0)) return 'badCap';
    return null;
}

export function guestSearch<T extends GuestKeyLike>(keys: T[], q: string): T[] {
    const s = q.trim().toLowerCase();
    if (!s) return keys;
    return keys.filter((k) =>
        k.name.toLowerCase().includes(s) ||
        (k.note ?? '').toLowerCase().includes(s) ||
        k.api_key.toLowerCase().endsWith(s) ||
        guestModels(k).some((m) => m.toLowerCase().includes(s))
    );
}
