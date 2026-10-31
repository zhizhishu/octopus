// API Key 列表的状态 / 搜索 / 筛选 / 计数，和 Key 的遮挡显示。
// 后端 /apikey/list 一次给全部（而且带完整 Key，别的客户端依赖这个形状），这些都在前端做。

import { DEFAULT_EXPIRE_TIME, toExpireAt } from './expire';

export const KEY_FAMILIES = ['openai-compatible', 'anthropic', 'gemini', 'video'] as const;
export type KeyFamily = typeof KEY_FAMILIES[number];

/**
 * 跟后端鉴权中间件同一套判断，顺序也一样：
 * 停用 → 过期（expire_at > 0 且早于现在）→ 用完（上限 > 0 且累计花费超过上限）→ 能用。
 */
export type KeyStatus = 'ok' | 'off' | 'expired' | 'used_up';
export type KeyStatusTab = 'all' | KeyStatus;
export const KEY_STATUS_TABS: KeyStatusTab[] = ['all', 'ok', 'off', 'expired', 'used_up'];

export interface FilterableKey {
    id: number;
    name: string;
    api_key: string;
    user_id?: number;
    enabled: boolean;
    expire_at?: number;
    max_cost?: number;
    supported_models?: string;
    endpoint_families?: unknown;
    endpoint_scopes?: unknown;
    allowed_endpoint_families?: unknown;
    endpoint_family_scopes?: unknown;
    access_plan_ids?: number[];
    access_plans?: Array<{ id: number }>;
    default_access_plan_id?: number;
}

export interface KeyFilters {
    status: KeyStatusTab;
    search: string;
    owners: number[];
    plans: number[];
    families: KeyFamily[];
}

export const EMPTY_KEY_FILTERS: KeyFilters = { status: 'all', search: '', owners: [], plans: [], families: [] };

export function keyStatus(key: Pick<FilterableKey, 'enabled' | 'expire_at' | 'max_cost'>, spent: number, nowSec: number): KeyStatus {
    if (!key.enabled) return 'off';
    if ((key.expire_at ?? 0) > 0 && (key.expire_at as number) < nowSec) return 'expired';
    // 与后端一致：花到上限（等于也算）就拒绝。
    if ((key.max_cost ?? 0) > 0 && (key.max_cost as number) <= spent) return 'used_up';
    return 'ok';
}

/** sk-octopus-UbS2…Hm4q → sk-…Hm4q。只用于显示，复制按钮复制完整 Key。 */
export function maskKey(key: string | undefined | null): string {
    const k = String(key ?? '');
    if (k.length <= 8) return k ? `${k.slice(0, 2)}…` : '';
    const prefix = k.startsWith('sk-') ? 'sk-' : k.slice(0, 3);
    return `${prefix}…${k.slice(-4)}`;
}

/** 代码块里用：前缀多留一点，看得出是哪把，中间用 • 盖住。 */
export function maskKeyLong(key: string | undefined | null): string {
    const k = String(key ?? '');
    if (k.length <= 16) return maskKey(k);
    return `${k.slice(0, 15)}••••${k.slice(-4)}`;
}

/** 允许的协议。没配或空 = 全部。旧字段名（endpoint_scopes 等）也认，openai 当成 openai-compatible。 */
export function keyFamilies(key: Partial<FilterableKey> | undefined | null): KeyFamily[] {
    const candidates = [key?.endpoint_families, key?.endpoint_scopes, key?.allowed_endpoint_families, key?.endpoint_family_scopes];
    const configured = candidates.find((v) => (Array.isArray(v) && v.length > 0) || (typeof v === 'string' && v.trim().length > 0));
    if (configured === undefined) return [...KEY_FAMILIES];
    const items = Array.isArray(configured) ? configured : String(configured).split(',');
    const picked = new Set(items.map((i) => {
        const v = String(i).trim().toLowerCase();
        return v === 'openai' ? 'openai-compatible' : v;
    }));
    const out = KEY_FAMILIES.filter((f) => picked.has(f));
    return out.length > 0 ? out : [...KEY_FAMILIES];
}

export function isAllFamilies(families: readonly string[]) {
    return KEY_FAMILIES.every((f) => families.includes(f));
}

/** 限定的模型；空数组 = 不限（全部模型，以后新加的也算）。 */
export function keyModels(key: Pick<FilterableKey, 'supported_models'> | undefined | null): string[] {
    return String(key?.supported_models ?? '').split(',').map((m) => m.trim()).filter(Boolean);
}

export function keyPlanIDs(key: Pick<FilterableKey, 'access_plan_ids' | 'access_plans'>): number[] {
    if (key.access_plan_ids && key.access_plan_ids.length > 0) return key.access_plan_ids;
    return (key.access_plans ?? []).map((p) => p.id);
}

function matchesSearch(key: FilterableKey, q: string, ownerName: (id?: number) => string) {
    if (!q) return true;
    const needle = q.trim().toLowerCase();
    if (!needle) return true;
    if (needle.startsWith('#') && String(key.id) === needle.slice(1)) return true;
    if (key.name.toLowerCase().includes(needle)) return true;
    if (ownerName(key.user_id).toLowerCase().includes(needle)) return true;
    // 只认 Key 的末尾（列表上看得到的就是末 4 位），免得粘一段完整 Key 进来也能当搜索用；完整 Key 也能搜到。
    const k = key.api_key.toLowerCase();
    return k.endsWith(needle) || (needle.length >= 12 && k.includes(needle));
}

function matchesFacets(key: FilterableKey, f: KeyFilters) {
    if (f.owners.length > 0 && !f.owners.includes(key.user_id ?? 0)) return false;
    if (f.plans.length > 0 && !keyPlanIDs(key).some((id) => f.plans.includes(id))) return false;
    if (f.families.length > 0) {
        const fam = keyFamilies(key);
        if (!f.families.every((x) => fam.includes(x))) return false;
    }
    return true;
}

export function filterKeys<T extends FilterableKey>(keys: T[], f: KeyFilters, spentOf: (id: number) => number, nowSec: number, ownerName: (id?: number) => string = () => ''): T[] {
    return keys.filter((k) => matchesSearch(k, f.search, ownerName)
        && matchesFacets(k, f)
        && (f.status === 'all' || keyStatus(k, spentOf(k.id), nowSec) === f.status));
}

/** 状态页签上的数字：按搜索和其他筛选算，不按当前页签算（切页签数字不跳）。 */
export function keyStatusCounts(keys: FilterableKey[], f: KeyFilters, spentOf: (id: number) => number, nowSec: number, ownerName: (id?: number) => string = () => ''): Record<KeyStatusTab, number> {
    const out: Record<KeyStatusTab, number> = { all: 0, ok: 0, off: 0, expired: 0, used_up: 0 };
    for (const k of keys) {
        if (!matchesSearch(k, f.search, ownerName) || !matchesFacets(k, f)) continue;
        out.all += 1;
        out[keyStatus(k, spentOf(k.id), nowSec)] += 1;
    }
    return out;
}

/** 已用 / 上限：0 上限 = 不限。pct 给进度条，到 80% 算快用完。 */
export function spendState(spent: number, cap: number | undefined) {
    const c = cap ?? 0;
    if (c <= 0) return { limited: false as const, pct: 0, left: 0, level: 'ok' as const };
    const pct = Math.min(100, (Math.max(0, spent) / c) * 100);
    const level = spent >= c ? ('over' as const) : pct >= 80 ? ('near' as const) : ('ok' as const);
    return { limited: true as const, pct, left: Math.max(0, c - spent), level };
}

const DAY = 24 * 60 * 60;

/** 有效期显示：永久 / 还剩 N 天（不到 1 天按 1 天）/ 过期 N 天。 */
export function expiryState(expireAt: number | undefined, nowSec: number): { kind: 'forever' } | { kind: 'left'; days: number; soon: boolean } | { kind: 'past'; days: number } {
    const e = expireAt ?? 0;
    if (e <= 0) return { kind: 'forever' };
    if (e < nowSec) return { kind: 'past', days: Math.max(1, Math.floor((nowSec - e) / DAY)) };
    const days = Math.max(1, Math.ceil((e - nowSec) / DAY));
    return { kind: 'left', days, soon: days <= 3 };
}

/** 抽屉里「+1 / +30 / +90 天」：从今天往后数 N 天，那天 23:59（本机时区）。和这一组按钮哪个对得上就亮哪个。 */
export function expireAfterDays(days: number, nowSec: number): number {
    const now = new Date(nowSec * 1000);
    return toExpireAt(new Date(now.getFullYear(), now.getMonth(), now.getDate() + days), DEFAULT_EXPIRE_TIME);
}

export function matchExpirePreset(expireAt: number | undefined, presets: number[], nowSec: number): number | null {
    const e = expireAt ?? 0;
    if (e <= 0) return 0;
    for (const d of presets) if (expireAfterDays(d, nowSec) === e) return d;
    return null;
}

/** <input type="datetime-local"> 的值（本机时区）↔ Unix 秒。 */
export function unixToLocalInput(ts: number | undefined): string {
    if (!ts) return '';
    const d = new Date(ts * 1000);
    const p = (n: number) => String(n).padStart(2, '0');
    return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}

export function localInputToUnix(value: string): number {
    const m = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})/.exec(value);
    if (!m) return 0;
    const [, y, mo, d, h, mi] = m.map(Number);
    return Math.floor(new Date(y, mo - 1, d, h, mi, 0).getTime() / 1000);
}

/** 输入框里的钱：只留数字和一个小数点，最多 6 位小数。 */
export function normalizeMoneyInput(input: string): string {
    const cleaned = input.replace(/[^\d.]/g, '');
    const [intPart, ...rest] = cleaned.split('.');
    return rest.length > 0 ? `${intPart}.${rest.join('').slice(0, 6)}` : intPart;
}
