// 用户表的搜索 / 筛选 / 人数 / 分页。后端 /user/list 一次给全部，这些都在前端做。

export type UserRoleValue = 'admin' | 'user';
export type StatusFilter = 'all' | 'active' | 'disabled';
/** 月卡状态：none=没开 / active=生效中 / soon=3 天内到期 / exhausted=今天用完 / expired=已过期 */
export type CardState = 'none' | 'active' | 'soon' | 'exhausted' | 'expired';

export const CARD_SOON_DAYS = 3;
const DAY = 24 * 60 * 60;

export interface FilterableUser {
    id: number;
    username: string;
    email?: string;
    note?: string;
    role: UserRoleValue;
    status: 'active' | 'disabled';
    monthly_limit: number;
    monthly_used: number;
    monthly_expire_at: number;
    access_plan_ids?: number[];
    access_plans?: { id: number }[];
}

export interface UserFilters {
    status: StatusFilter;
    search: string;
    roles: UserRoleValue[];
    planIDs: number[];
    cards: CardState[];
}

export const EMPTY_FILTERS: UserFilters = { status: 'all', search: '', roles: [], planIDs: [], cards: [] };

/** 管理员不扣费，月卡对他没意义，一律算「没开」。 */
export function cardState(user: FilterableUser, nowSec: number): CardState {
    if (user.role === 'admin') return 'none';
    const limit = Math.max(0, Number(user.monthly_limit) || 0);
    const expireAt = Number(user.monthly_expire_at) || 0;
    if (limit <= 0) return 'none';
    if (expireAt <= nowSec) return 'expired';
    if ((Number(user.monthly_used) || 0) >= limit) return 'exhausted';
    if (expireAt - nowSec <= CARD_SOON_DAYS * DAY) return 'soon';
    return 'active';
}

export function userPlanIDs(user: FilterableUser): number[] {
    if (user.access_plan_ids && user.access_plan_ids.length > 0) return user.access_plan_ids;
    return user.access_plans?.map((p) => p.id) ?? [];
}

/** 搜用户名、邮箱、备注（不分大小写）；"#12" 按编号找。 */
export function matchesSearch(user: FilterableUser, query: string): boolean {
    const q = query.trim().toLowerCase();
    if (!q) return true;
    if (/^#\d+$/.test(q)) return user.id === Number(q.slice(1));
    return [user.username, user.email, user.note].some((v) => (v ?? '').toLowerCase().includes(q));
}

function matchesFacets(user: FilterableUser, f: UserFilters, nowSec: number): boolean {
    if (!matchesSearch(user, f.search)) return false;
    if (f.roles.length > 0 && !f.roles.includes(user.role)) return false;
    if (f.planIDs.length > 0) {
        const ids = userPlanIDs(user);
        if (!f.planIDs.some((id) => ids.includes(id))) return false;
    }
    if (f.cards.length > 0 && !f.cards.includes(cardState(user, nowSec))) return false;
    return true;
}

export function filterUsers<T extends FilterableUser>(users: T[], f: UserFilters, nowSec: number): T[] {
    return users.filter((u) => {
        if (f.status !== 'all' && u.status !== f.status) return false;
        return matchesFacets(u, f, nowSec);
    });
}

/** 全部 / 启用 / 停用 的人数：按其余筛选（搜索、角色、方案、月卡）算，不看状态这一项。 */
export function statusCounts(users: FilterableUser[], f: UserFilters, nowSec: number) {
    const counts = { all: 0, active: 0, disabled: 0 };
    for (const u of users) {
        if (!matchesFacets(u, f, nowSec)) continue;
        counts.all++;
        if (u.status === 'active') counts.active++;
        else counts.disabled++;
    }
    return counts;
}

export function paginate<T>(items: T[], page: number, pageSize: number) {
    const size = Math.max(1, Math.floor(pageSize));
    const pages = Math.max(1, Math.ceil(items.length / size));
    const current = Math.min(Math.max(1, Math.floor(page) || 1), pages);
    return { items: items.slice((current - 1) * size, current * size), page: current, pages, total: items.length };
}

/** 页码按钮：页数多时只留首尾和当前页附近，中间用 0 表示省略号。 */
export function pageButtons(page: number, pages: number): number[] {
    if (pages <= 7) return Array.from({ length: pages }, (_, i) => i + 1);
    const set = new Set([1, pages, page - 1, page, page + 1]);
    const out: number[] = [];
    let prev = 0;
    for (const n of [...set].filter((n) => n >= 1 && n <= pages).sort((a, b) => a - b)) {
        if (n - prev > 1) out.push(0);
        out.push(n);
        prev = n;
    }
    return out;
}
