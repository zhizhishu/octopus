/**
 * 方案路由只有两种分配方式（界面上）：
 * - 优先 = 后端 mode 3（fill_first）：从上往下依次用，数字是顺位，越小越先。
 * - 轮询 = 后端 mode 6（分层轮询）/ mode 1（纯轮询）：每个渠道一个「优先级」数字，
 *   数字大的先用，一样的轮流，这一级都不能用才换下一级（数字更小的）。
 *   mode 1 就是「数字全一样」的轮询：选路不看数字（后端同步也会把它们重置成 1），所以画成全 1。
 *   第一次改数字时升级成 mode 6（数字才真正起作用）。
 */
export const MODE_SPREAD = 1;
export const MODE_FILL_FIRST = 3;
export const MODE_SPREAD_TIERED = 6;

export type SimpleRouteMode = 'first' | 'rotate';

export type ModeTarget = {
    channel_id: number;
    priority?: number;
    mode?: number;
    priority_overridden?: boolean;
    enabled?: boolean;
};

export function clampPriority(value: number | undefined | null): number {
    const n = Number(value);
    if (!Number.isFinite(n)) return 1;
    return Math.min(Number.MAX_SAFE_INTEGER, Math.max(1, Math.trunc(n)));
}

/** 规则级 mode：组内第一个写了 mode 的为准；缺省按后端默认 fill_first。 */
export function ruleModeOf(targets: ReadonlyArray<ModeTarget>): 1 | 3 | 6 {
    const raw = Number(targets.find((target) => target.mode !== undefined)?.mode);
    if (raw === MODE_SPREAD) return MODE_SPREAD;
    if (raw === MODE_SPREAD_TIERED) return MODE_SPREAD_TIERED;
    return MODE_FILL_FIRST;
}

export function simpleModeOf(mode: number | undefined | null): SimpleRouteMode {
    const n = Number(mode);
    return n === MODE_SPREAD || n === MODE_SPREAD_TIERED ? 'rotate' : 'first';
}

/** 卡片上显示的数字：轮询(1) 选路不看数字，一律显示 1。 */
export function displayPriority(target: ModeTarget, ruleMode: number): number {
    return Number(ruleMode) === MODE_SPREAD ? 1 : clampPriority(target.priority);
}

/** 按「谁先被尝试」排好：优先 = 数字小的在前；轮询 = 数字大的在前。同数字按渠道 ID。 */
export function sortByTryOrder<T extends ModeTarget>(targets: ReadonlyArray<T>, ruleMode: number = ruleModeOf(targets)): T[] {
    const rotate = simpleModeOf(ruleMode) === 'rotate';
    return [...targets].sort((a, b) => {
        const pa = displayPriority(a, ruleMode);
        const pb = displayPriority(b, ruleMode);
        const byPriority = rotate ? pb - pa : pa - pb;
        return byPriority !== 0 ? byPriority : (a.channel_id || 0) - (b.channel_id || 0);
    });
}

/**
 * 实际尝试顺序，一级一级列出来（只算开着的）：
 * 优先 = 每个渠道单独一级；轮询 = 同一个数字是一级，级内轮流。
 */
export function tryLevels<T extends ModeTarget>(targets: ReadonlyArray<T>, ruleMode: number = ruleModeOf(targets)): T[][] {
    const on = sortByTryOrder(targets.filter((target) => target.enabled !== false), ruleMode);
    if (simpleModeOf(ruleMode) === 'first') return on.map((target) => [target]);
    const levels: T[][] = [];
    on.forEach((target) => {
        const last = levels[levels.length - 1];
        if (last && displayPriority(last[0], ruleMode) === displayPriority(target, ruleMode)) last.push(target);
        else levels.push([target]);
    });
    return levels;
}

/** 和它数字一样、会和它轮流的其他渠道（只在轮询里有）。 */
export function rotatePeers<T extends ModeTarget>(target: T, targets: ReadonlyArray<T>, ruleMode: number = ruleModeOf(targets)): T[] {
    if (simpleModeOf(ruleMode) !== 'rotate') return [];
    const own = displayPriority(target, ruleMode);
    return targets.filter((peer) => peer !== target && peer.enabled !== false && displayPriority(peer, ruleMode) === own);
}

/**
 * 切到「优先」：按现在的尝试顺序从上往下编成 1..N。
 * （原版直接改 mode 不动数字：轮询里 A=3、B=C=1 切过去会变成 B、C 先用，顺序整个反过来。）
 */
export function switchToFirst<T extends ModeTarget>(targets: ReadonlyArray<T>): T[] {
    return sortByTryOrder(targets).map((target, index) => ({
        ...target,
        mode: MODE_FILL_FIRST,
        priority: index + 1,
        priority_overridden: true,
    }));
}

/** 切到「轮询」：先全部一样（都 1），全部一起轮流；之后改数字才分先后。 */
export function switchToRotate<T extends ModeTarget>(targets: ReadonlyArray<T>): T[] {
    return targets.map((target) => ({ ...target, mode: MODE_SPREAD, priority: 1, priority_overridden: false }));
}

/** 优先：拖完的新顺序编成 1..N。 */
export function renumberFirst<T extends ModeTarget>(ordered: ReadonlyArray<T>): T[] {
    return ordered.map((target, index) => ({ ...target, mode: MODE_FILL_FIRST, priority: index + 1, priority_overridden: true }));
}

/** 把列表里 from 位置的挪到 to 位置。 */
export function moveItem<T>(list: ReadonlyArray<T>, from: number, to: number): T[] {
    const next = [...list];
    if (from < 0 || from >= next.length) return next;
    const [item] = next.splice(from, 1);
    next.splice(Math.max(0, Math.min(to, next.length)), 0, item);
    return next;
}

/**
 * 轮询：改某个渠道的优先级数字。整条规则升级成 mode 6（数字真正起作用）；
 * 原来是 mode 1 的，其他渠道的数字先按显示的那样归 1，免得旧库里残留的数字突然生效。
 */
export function setRotatePriority<T extends ModeTarget>(targets: ReadonlyArray<T>, index: number, priority: number): T[] {
    const fromPlain = ruleModeOf(targets) === MODE_SPREAD;
    return targets.map((target, i) => ({
        ...target,
        mode: MODE_SPREAD_TIERED,
        priority: i === index ? clampPriority(priority) : (fromPlain ? 1 : clampPriority(target.priority)),
        priority_overridden: true,
    }));
}
