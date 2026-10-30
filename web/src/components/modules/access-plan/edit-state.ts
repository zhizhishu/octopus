// 方案画布的编辑状态：冻结排序、保存排队、未保存判定（纯函数，node --test 直接测）。

// ── 1. 交互期间冻结卡片顺序 ──
//
// 优先填充按数字从小到大画、分层轮询按数字从大到小画。点一下 +/− 数字就变，卡片立刻换位，
// 手指/鼠标底下换成了另一张卡，连点第二下就点错了。做法和 React Flow 拖拽一样：
// 交互期间位置不动，松手（停手一会儿）并且保存落定之后再按新数字重排。
// 冻结期间「第 N 顺位」照常显示真实顺位，只是卡片先不挪。

export type OrderableTarget = { id?: number; channel_id: number; upstream_model: string };
export type OrderRow<T extends OrderableTarget> = { requestKey: string; targets: T[]; ranks?: number[] };
/** requestKey -> 冻结时自上而下的卡片键 */
export type FrozenOrder = Record<string, string[]>;

/**
 * 卡片键用「渠道 + 上游模型」，不用 id：路由整表保存时后端会重建这些行，回包里的 id 全是新的，
 * 按 id 冻结的话第一次保存回来就对不上了。同一条规则里 (channel_id, upstream_model) 本来就唯一
 * （后端 idx_access_route_target 唯一索引）。
 */
export function targetOrderKey(target: OrderableTarget): string {
    return `${target.channel_id}:${target.upstream_model.trim().toLowerCase()}`;
}

export function captureOrder<T extends OrderableTarget>(rows: OrderRow<T>[]): FrozenOrder {
    const out: FrozenOrder = {};
    for (const row of rows) out[row.requestKey] = row.targets.map(targetOrderKey);
    return out;
}

/**
 * 按冻结的顺序摆放每行卡片；冻结后新出现的卡片按当前排序接在后面，消失的卡片直接略过。
 * ranks[i] 是第 i 张卡在「当前真实排序」里的顺位（1 起），给「第 N 顺位」显示用。
 */
export function applyFrozenOrder<T extends OrderableTarget, R extends OrderRow<T>>(rows: R[], frozen: FrozenOrder | null): R[] {
    if (!frozen) return rows;
    return rows.map((row) => {
        const order = frozen[row.requestKey];
        if (!order) return row;
        const rank = new Map(row.targets.map((target, index) => [targetOrderKey(target), index + 1]));
        const byKey = new Map(row.targets.map((target) => [targetOrderKey(target), target]));
        const placed: T[] = [];
        const seen = new Set<string>();
        for (const key of order) {
            const target = byKey.get(key);
            if (target && !seen.has(key)) {
                placed.push(target);
                seen.add(key);
            }
        }
        for (const target of row.targets) {
            const key = targetOrderKey(target);
            if (!seen.has(key)) {
                placed.push(target);
                seen.add(key);
            }
        }
        return { ...row, targets: placed, ranks: placed.map((target) => rank.get(targetOrderKey(target)) ?? 0) };
    });
}

/** 停手多久算松手（毫秒）。 */
export const FROZEN_ORDER_IDLE_MS = 900;

/** 什么时候解除冻结、按新数字重排：保存都落定、指针不在按钮上、并且停手够久。 */
export function shouldReleaseFrozenOrder(state: { now: number; lastChangeAt: number; saving: boolean; hovering: boolean; idleMs?: number }): boolean {
    if (state.saving || state.hovering) return false;
    return state.now - state.lastChangeAt >= (state.idleMs ?? FROZEN_ORDER_IDLE_MS);
}

// ── 2. 保存排队：同一时间只发一个保存，排队的只留最新一份 ──
//
// 画布上连点 +/− 每下都会整表保存。并发发出去的话，先发的可能后到，把后面那次覆盖掉；
// 而且 react-query 的 mutate 回调只认最后一次调用，前面失败了也没人回滚。
// 这里改成串行：在飞的那一个回来之前，新的改动只记最新一份（整表，已经包含之前的改动），
// 回来之后再发。只有「最后一次」的结果才拿来覆盖本地状态。

export type SaveOutcome<P, R> =
    | { ok: true; value: R; payload: P; superseded: boolean }
    | { ok: false; error: unknown; payload: P; superseded: boolean };

export type LatestSaveQueue<P> = {
    submit: (payload: P) => void;
    busy: () => boolean;
};

export function createLatestSaveQueue<P, R>(
    send: (payload: P) => Promise<R>,
    onSettled: (outcome: SaveOutcome<P, R>) => void,
    onBusyChange?: (busy: boolean) => void,
): LatestSaveQueue<P> {
    let inFlight = false;
    let pending: { payload: P } | null = null;

    const run = (payload: P) => {
        inFlight = true;
        send(payload).then(
            (value) => finish({ ok: true, value, payload, superseded: pending !== null }),
            (error: unknown) => finish({ ok: false, error, payload, superseded: pending !== null }),
        );
    };

    const finish = (outcome: SaveOutcome<P, R>) => {
        const next = pending;
        pending = null;
        if (next) {
            // 还有更新的整表在排队：先发它，这次的结果不拿来覆盖本地。
            onSettled(outcome);
            run(next.payload);
            return;
        }
        inFlight = false;
        onSettled(outcome);
        onBusyChange?.(false);
    };

    return {
        submit(payload: P) {
            if (inFlight) {
                pending = { payload };
                return;
            }
            onBusyChange?.(true);
            run(payload);
        },
        busy: () => inFlight,
    };
}

// ── 3. 未保存判定 ──

/** 稳定的 JSON：对象键排序、undefined 当缺省，用来比较「改没改」。 */
export function stableJSON(value: unknown): string {
    return JSON.stringify(value, (_key, v: unknown) => {
        if (v && typeof v === 'object' && !Array.isArray(v)) {
            return Object.fromEntries(Object.entries(v as Record<string, unknown>)
                .filter(([, x]) => x !== undefined)
                .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0)));
        }
        return v;
    });
}

export function sameDraft(a: unknown, b: unknown): boolean {
    return stableJSON(a) === stableJSON(b);
}

/** 关闭编辑弹窗：没改直接关，改了先问一句。 */
export function decideDialogClose(dirty: boolean): 'close' | 'confirm' {
    return dirty ? 'confirm' : 'close';
}

/**
 * 外部数据（列表回刷 / 别处保存）什么时候可以覆盖弹窗/编辑器里的草稿：
 * 服务器版本变了，并且本地没有未保存的改动、也没有保存在路上。
 */
export function shouldResyncDraft(syncedVersion: string, serverVersion: string, saving: boolean, dirty: boolean): boolean {
    return !saving && !dirty && syncedVersion !== serverVersion;
}
