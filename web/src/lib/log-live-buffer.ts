// 日志列表「详情打开期间冻结实时插入」的纯状态机 + 判定函数。
// 不依赖 React / 浏览器 / React Query：node --test 直接跑这份真模块
// （tests/log-detail-stability.test.cjs），所以测到的就是线上跑的那套决定。
//
// 为什么需要这一层：
//   1) 详情打开时靠 DOM 观察（MutationObserver）+ useEffect 去暂停插入是**异步**的——
//      打开的那一帧 liveFlushMs=0（来一条插一条）下一条 SSE 就已经 flush 了；
//   2) 关掉弹窗后「暂停原因消失」，同一条路径又会把积压一次性顶到刚回来的列表上；
//   3) 定时器（setInterval）可能抢在冻结那次渲染/effect 清理之前跑一拍。
// 所以真正需要的是一个 freeze() 同步立起、只能由一次**成功的恢复 GET**解开的闩，
// 并且每个会插入行的入口（SSE / timer / resume）都走同一个纯函数再判一次。
//
// log.ts 只负责执行这里的输出（setQueryData / refetch / cancelQueries），
// 决定本身全部留在这里，测试因此可以直接压并发用例。

import { bufferLog, LOG_AUTO_REFRESH_BUFFER_MAX } from './log-auto-refresh';

interface WithID {
    id: number;
}

/** 冻结闩 + 单飞恢复 + 溢出标记。全部是值，不做修改，每步返回新对象。 */
export interface LiveBufferState {
    /** 详情打开时同步立起；只有一次成功的恢复 GET 才会解开（关闭详情不解）。 */
    frozen: boolean;
    /** 正在跑恢复 GET（单飞：重复点直接忽略）。 */
    resuming: boolean;
    /** 本次恢复属于哪个列表签名（queryKey 派生）；签名一变这次恢复就作废。 */
    resumeKey: string | null;
    /** 缓冲曾经满过（最旧的一条被挤掉）→ UI 只能显示「200+」。 */
    overflowed: boolean;
}

export const INITIAL_LIVE_BUFFER_STATE: LiveBufferState = Object.freeze({
    frozen: false,
    resuming: false,
    resumeKey: null,
    overflowed: false,
});

export interface LiveBufferStep<T> {
    state: LiveBufferState;
    /** 这一步之后的缓冲区（已经去重、按 id 新的在前、封顶 200）。 */
    buffer: T[];
    /** 这一步应当立刻插进列表的日志（已过冻结/暂停/节奏判定）；空数组＝这次只进缓冲。 */
    insert: T[];
}

export type LiveFlushDecision = 'flush' | 'buffer';

export interface LiveFlushInput {
    /** 实时（SSE）模式；关着的时候什么都不插。 */
    live: boolean;
    /** 合并节奏：0=来一条插一条。 */
    flushMs: number;
    /** livePaused：后台 / 详情弹窗 / 选字 / 已往下翻（这些是「观察得到」的暂停）。 */
    paused: boolean;
    /** 详情打开时同步立起的闩；只有成功的恢复 GET 才会变回 false。 */
    frozen: boolean;
}

/**
 * 这一条实时日志现在能不能插进列表。
 * 冻结（frozen）优先于一切：闩没解开就只能进缓冲，关掉详情也不例外——
 * 否则「关弹窗 → 暂停原由消失 → 下一条 SSE」会把积压直接顶到用户刚回到的列表上。
 */
export function liveFlushDecision(input: LiveFlushInput): LiveFlushDecision {
    if (!input.live) return 'buffer';
    if (input.paused || input.frozen) return 'buffer';
    return input.flushMs <= 0 ? 'flush' : 'buffer';
}

/**
 * 这条新日志进来会不会把缓冲里最旧的一条挤掉（缓冲已满且不是重复 id）。
 * 挤掉就意味着「积压里少了一条」：UI 只能老实说 200+，恢复时统一 GET 回查兜底。
 */
export function pendingWouldDropOldest<T extends WithID>(
    buffer: readonly T[],
    incoming: WithID,
    max = LOG_AUTO_REFRESH_BUFFER_MAX
): boolean {
    if (buffer.length < max) return false;
    return !buffer.some((item) => item.id === incoming.id);
}

/** 未读徽标文案：溢出过就只说「200+」，不假装知道确切总数。 */
export function pendingUnreadLabel(count: number, overflowed: boolean): string {
    if (count <= 0) return '0';
    return overflowed ? `${count}+` : String(count);
}

/**
 * 详情快照取哪一份。
 * - 同一条日志（id 相同）绝不覆盖：SSE 插入/重排/后端回写都会给出「同一 id 的新对象」，
 *   正在读的那一份不能因此换内容。
 * - 只有翻到别的 id 才换成那一条的快照。
 * - 关掉（id=null）保留上一份快照：关闭动画与焦点返回还用得上，下次打开会整份换掉。
 */
export function nextDetailSnapshot<T extends WithID>(
    current: T | null,
    id: number | null,
    candidates: readonly T[]
): T | null {
    if (id === null) return current;
    if (current && current.id === id) return current;
    return candidates.find((log) => log.id === id) ?? current;
}

/**
 * 打开详情：同步冻结（页面在点击回调里第一个调它，不经过渲染/effect/DOM 观察）。
 *
 * 如果这时正好有一次恢复 GET 在飞（用户点完「N 条新日志」又打开了一条），闩必须压过它：
 * 把 resumeKey 清空，于是那次慢 GET 落地后 settleResume 只收掉 busy，不会解冻、
 * 更不会把积压插到用户正在读的列表背后。resuming 保持 true —— GET 确实还在飞，
 * 单飞与「恢复中」的显示都得照旧。
 */
export function freezeLiveBuffer<T extends WithID>(input: { state: LiveBufferState; buffer: readonly T[] }): LiveBufferStep<T> {
    const { state } = input;
    const supersedesResume = state.resuming && state.resumeKey !== null;
    const nextState: LiveBufferState = state.frozen && !supersedesResume
        ? state
        : { ...state, frozen: true, resumeKey: supersedesResume ? null : state.resumeKey };
    return {
        state: nextState,
        buffer: input.buffer.slice(),
        insert: [],
    };
}

/**
 * 收到一条实时日志：先并入缓冲（去重/新的在前/封顶 200），
 * 只有「实时开着 + 没被观察到的原因暂停 + 没冻结 + 节奏允许」时才立刻整批插出去。
 */
export function receiveLiveLog<T extends WithID>(input: {
    state: LiveBufferState;
    buffer: readonly T[];
    log: T;
    live: boolean;
    flushMs: number;
    paused: boolean;
}): LiveBufferStep<T> {
    const overflowedNow =
        input.state.overflowed || pendingWouldDropOldest(input.buffer, input.log, LOG_AUTO_REFRESH_BUFFER_MAX);
    const buffered = bufferLog(input.buffer, input.log, LOG_AUTO_REFRESH_BUFFER_MAX);
    const state: LiveBufferState = overflowedNow === input.state.overflowed ? input.state : { ...input.state, overflowed: overflowedNow };
    const decision = liveFlushDecision({
        live: input.live,
        flushMs: input.flushMs,
        paused: input.paused,
        frozen: state.frozen,
    });
    if (decision === 'flush') return { state, buffer: [], insert: buffered };
    return { state, buffer: buffered, insert: [] };
}

/**
 * 定时器（或暂停解除后的第一次补插）的一拍。
 * ⚠ 这里必须自己再判一次闩：setInterval 的回调可能抢在「冻结」那次渲染 / effect 清理之前执行，
 * 只靠 effect 的清理函数挡不住那一拍。
 */
export function tickLiveBuffer<T extends WithID>(input: {
    state: LiveBufferState;
    buffer: readonly T[];
    live: boolean;
    paused: boolean;
}): LiveBufferStep<T> {
    if (!input.live || input.paused || input.state.frozen || input.buffer.length === 0) {
        return { state: input.state, buffer: input.buffer.slice(), insert: [] };
    }
    return { state: input.state, buffer: [], insert: input.buffer.slice() };
}

/**
 * 开始一次恢复（点「N 条新日志」/手动刷新）。
 * 返回 null 表示已有恢复在飞：重复点直接忽略（单飞），不会叠出第二次 GET。
 * 冻结在整段 GET 期间保持，新 SSE 继续进缓冲。
 *
 * ⚠ 自己也要立闩，不能假定调用方已经冻过：从「已往下翻」的暂停点「N 条新日志」时，
 * 列表并没有冻结，滚动回顶后暂停原因消失，GET 还没回来 SSE 就会插到列表上，
 * 而 refetch 落地会整份替换这份缓存——刚插进去、还没落库的行因此消失。
 */
export function beginResume<T extends WithID>(input: {
    state: LiveBufferState;
    buffer: readonly T[];
    key: string;
}): LiveBufferStep<T> | null {
    if (input.state.resuming) return null;
    return {
        state: { ...input.state, frozen: true, resuming: true, resumeKey: input.key },
        buffer: input.buffer.slice(),
        insert: [],
    };
}

/**
 * 恢复 GET 落地。
 * - 列表签名变了（筛选/页码/实时开关切换）：这次恢复作废——不把旧列表的积压插进新列表，
 *   状态回到初始（新列表的缓冲与闩由调用方的 reset 清）。
 * - 恢复期间用户又打开了详情（freeze 已清掉 resumeKey）：只收掉 resuming，**继续冻结**，
 *   不插积压——否则慢 GET 落地会把行插到用户正在读的列表背后。
 * - 失败：**继续冻结**，缓冲与溢出标记原样留着，用户可以再点一次恢复（不清空、不丢）。
 * - 成功：把缓冲整批交出去，由调用方与刚 GET 到的数据在同一批次里合并去重（prependLogBatch），
 *   然后解冻、清溢出标记。GET 期间新到的 SSE 也在这一批里，所以不依赖
 *   「GET 一定包含最近 1 秒内还没落库的日志」。
 */
export function settleResume<T extends WithID>(input: {
    state: LiveBufferState;
    buffer: readonly T[];
    ok: boolean;
    currentKey: string;
}): LiveBufferStep<T> {
    const { state, buffer, ok, currentKey } = input;
    // 没有恢复在飞（例如 reset 已经跑过）：什么都不动。
    if (!state.resuming) return { state, buffer: buffer.slice(), insert: [] };
    // 这次恢复被「恢复期间又打开了详情」作废了（freeze 清掉了 resumeKey）：
    // 只收掉 busy，**继续冻结**，缓冲与溢出标记原样留着等下一次显式恢复。
    if (state.resumeKey === null) {
        return { state: { ...state, resuming: false }, buffer: buffer.slice(), insert: [] };
    }
    // 旧恢复作废：列表签名换了，积压不属于新列表。
    if (state.resumeKey !== null && currentKey !== state.resumeKey) {
        return { state: INITIAL_LIVE_BUFFER_STATE, buffer: [], insert: [] };
    }
    if (!ok) {
        return { state: { ...state, resuming: false, resumeKey: null }, buffer: buffer.slice(), insert: [] };
    }
    return { state: INITIAL_LIVE_BUFFER_STATE, buffer: [], insert: buffer.slice() };
}
