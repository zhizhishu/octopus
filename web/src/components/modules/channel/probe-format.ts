/**
 * 主动探测（P3）的纯展示逻辑。
 *
 * 单独成模块（而不是塞进组件里）的理由和 log/audit-report.ts 一样：**它要被测试直接加载**。
 *
 * 三条底线，改这里之前先读：
 *
 *   1. 后端给的 state / result 是开放字符串。认识的翻成人话，**不认识的原样露出**——
 *      把认不出的状态吞成「未知」和「没事」是两回事，运维会据此判断要不要动手。
 *   2. 秒数只来自后端（next_in_seconds / remaining_seconds），前端不自己倒计时猜：
 *      猜错了界面会显示一个比真实时间更乐观的数字。
 *   3. 时间是「时间戳」不是「现在 - 时间戳」：探测历史是已经发生的事，不该随时间飘。
 */

/**
 * 一条探测安排的归一结果。seconds 只在 kind = scheduled / waitUpstream 时有意义。
 *
 * kind 与后端 internal/relay/balancer/circuit_probe.go 的 CircuitProbeState* 逐一对应：
 *   scheduled / running / wait_upstream / skipped_traffic / not_probed_media / disabled / stopped
 */
export type ProbeBadgeKind =
    | 'scheduled'
    | 'probing'
    | 'waitUpstream'
    | 'skippedTraffic'
    | 'notProbed'
    | 'off'
    | 'stopped'
    | 'unknown';

export interface ProbeStateView {
    kind: ProbeBadgeKind;
    seconds: number;
    /** 后端原文，未识别时用来露出，绝不丢。 */
    raw: string;
}

type ProbeLike = {
    state?: string;
    next_in_seconds?: number;
    remaining_seconds?: number;
};

const SCHEDULED_STATES = new Set(['scheduled']);
const PROBING_STATES = new Set(['running']);
// wait_upstream = 上游给过 Retry-After，到点前不探。
const WAIT_UPSTREAM_STATES = new Set(['wait_upstream']);
// 最近有真实流量：这时候探测只会和用户请求抢同一个上游配额。
const SKIPPED_TRAFFIC_STATES = new Set(['skipped_traffic']);
// 媒体作用域 / 认不出的作用域 / 没 opt-in 的嵌入端点：没有可探的题目形状。
const NOT_PROBED_STATES = new Set(['not_probed_media']);
// 开关关掉或当日额度用尽（后端把两者归同一个 disabled）。
const OFF_STATES = new Set(['disabled']);
// 连续未恢复达上限，或剩余冷却已塞不进一次探测。
const STOPPED_STATES = new Set(['stopped']);

function nonNegativeInt(value: unknown): number {
    const n = Number(value);
    return Number.isFinite(n) && n > 0 ? Math.trunc(n) : 0;
}

/**
 * 把 `probe: {state, next_in_seconds}` 归一成界面用的「哪种徽标 + 多少秒」。
 * 没有状态（旧后端不给 probe、或 state 为空）时返回 null —— 界面就不渲染探测徽标，
 * 不拿「探测中」冒充「没有探测信息」。
 */
export function probeStateView(probe?: ProbeLike | null): ProbeStateView | null {
    const raw = (probe?.state ?? '').trim();
    if (!raw) return null;
    const key = raw.toLowerCase();
    const next = nonNegativeInt(probe?.next_in_seconds);
    const remaining = nonNegativeInt(probe?.remaining_seconds);
    if (WAIT_UPSTREAM_STATES.has(key)) {
        // 上游要求的等待时间才是真的；手动探测被拒时后端给 remaining_seconds，
        // 熔断条目的 probe 只给 next_in_seconds（调度器已把等待折进去）。
        return { kind: 'waitUpstream', seconds: remaining || next, raw };
    }
    if (SCHEDULED_STATES.has(key)) return { kind: 'scheduled', seconds: next, raw };
    if (PROBING_STATES.has(key)) return { kind: 'probing', seconds: 0, raw };
    if (SKIPPED_TRAFFIC_STATES.has(key)) return { kind: 'skippedTraffic', seconds: 0, raw };
    if (NOT_PROBED_STATES.has(key)) return { kind: 'notProbed', seconds: 0, raw };
    if (OFF_STATES.has(key)) return { kind: 'off', seconds: 0, raw };
    if (STOPPED_STATES.has(key)) return { kind: 'stopped', seconds: 0, raw };
    return { kind: 'unknown', seconds: next, raw };
}

/** 探测历史的结论归一：恢复 / 仍失败 / 跳过 / 不计结论。 */
export type ProbeResultKind = 'recovered' | 'failed' | 'skipped' | 'noVerdict' | 'unknown';

/** 归一结果：档位 + 尾巴里的原因 token（没有尾巴就是空串）。 */
export type ProbeResultView = { kind: ProbeResultKind; reason: string };

/**
 * B 侧（internal/relay/balancer/circuit_probe_scheduler.go）真正写进历史的取值：
 *
 *   recovered
 *   stale                        —— 条目在结算前变了，或置换没抢到
 *   failed / failed:<outcome>    —— 可归因失败（probeFailedResult）
 *   skipped / skipped:<reason>   —— 不归因（probeSkippedResult / recordSkipLocked），
 *                                   reason 可能是 skipped_traffic / wait_upstream /
 *                                   capped / streak，也可能是 A 的 outcome（canceled…）
 *
 * 只看**冒号前那一段**定档，尾巴保留下来给界面显示原因（不丢信息）。
 */
const RECOVERED_RESULTS = new Set(['ok', 'recovered', 'mixed_scope', 'embedding_ok', 'success']);
// stale = 探测答对了，但被探的那条熔断记录在结算前已经变了：这次不是我们修好的。
// 它不是渠道的错也不是它好，所以单列一档，不冒充「已恢复」。
const NO_VERDICT_RESULTS = new Set(['stale']);
/**
 * 🔴 ambiguous 永远不进 failed：A 侧对「答不出一个确定整数」强制 Attributable=false
 * （见 internal/modeltest/circuit_probe.go 的 circuitProbeFinish），它不是渠道的故障证据。
 * 万一某条行把它写在 failed: 后面，这里也按「不计结论」显示，不冤枉渠道。
 */
const AMBIGUOUS_RESULTS = new Set(['ambiguous']);
// 历史行里可能出现的裸 token（老行 / 直写）：按最接近的一档显示。
const LEGACY_FAILED_RESULTS = new Set([
    'wrong_answer',
    'gibberish',
    'no_answer',
    'timeout',
    'upstream_error',
    'transport_error',
    'error',
    'failed',
]);

/** 拆出「档位 + 原因 token」；前缀不认识就是 unknown，不猜。 */
export function probeResultView(result: string | undefined): ProbeResultView {
    const raw = (result ?? '').trim().toLowerCase();
    if (!raw) return { kind: 'unknown', reason: '' };
    const separator = raw.indexOf(':');
    const head = separator < 0 ? raw : raw.slice(0, separator).trim();
    const reason = separator < 0 ? '' : raw.slice(separator + 1).trim();
    if (head === 'failed') {
        return { kind: reason && AMBIGUOUS_RESULTS.has(reason) ? 'noVerdict' : 'failed', reason };
    }
    if (head === 'skipped') return { kind: 'skipped', reason };
    if (AMBIGUOUS_RESULTS.has(head)) return { kind: 'noVerdict', reason };
    if (RECOVERED_RESULTS.has(head)) return { kind: 'recovered', reason };
    // stale 行本身不带尾巴：把后端这个词原样当原因，界面才能说清“不计结论”到底是
    // 条目已变还是判不出来（两者对运维的含义完全不同）。
    if (NO_VERDICT_RESULTS.has(head)) return { kind: 'noVerdict', reason: reason || head };
    if (LEGACY_FAILED_RESULTS.has(head)) return { kind: 'failed', reason };
    return { kind: 'unknown', reason: '' };
}

export function probeResultKind(result: string | undefined): ProbeResultKind {
    return probeResultView(result).kind;
}

/** 时间戳可能是 Unix 秒、毫秒，或 RFC3339 / 已排好的时间串。三种都接受。 */
export function formatProbeClock(timestamp: number | string | undefined | null): string {
    if (timestamp === null || timestamp === undefined) return '';
    if (typeof timestamp === 'string') {
        const text = timestamp.trim();
        if (!text) return '';
        if (/^\d+$/.test(text)) return formatProbeClock(Number(text));
        // 后端历史行的 timestamp 是 RFC3339（见 circuit_probe_scheduler.go 的 recordHistory），
        // 这里排成 HH:MM:SS；解析不了的格式原样露出，不把它吞掉。
        const parsed = Date.parse(text);
        return Number.isFinite(parsed) ? clockFromMs(parsed) : text;
    }
    if (!Number.isFinite(timestamp) || timestamp <= 0) return '';
    const ms = timestamp > 1e12 ? timestamp : timestamp * 1000;
    return clockFromMs(ms);
}

function clockFromMs(ms: number): string {
    const d = new Date(ms);
    if (Number.isNaN(d.getTime())) return '';
    const pad = (n: number) => String(n).padStart(2, '0');
    return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

/** 耗时：0/缺失就是没测到，显示破折号，不写 0 秒。 */
export function formatProbeDuration(ms: number | undefined | null): string {
    const value = Number(ms);
    if (!Number.isFinite(value) || value <= 0) return '';
    if (value < 1000) return `${Math.round(value)}ms`;
    return `${(value / 1000).toFixed(1)}s`;
}

/** 等待秒数归一成「现在 / N 秒 / N 分」三档，单位选择由词条管，不在代码里拼「秒」字。 */
type WaitUnit = 'now' | 'seconds' | 'minutes';

export function probeWaitUnit(seconds: number | undefined): WaitUnit {
    const value = nonNegativeInt(seconds);
    if (value <= 0) return 'now';
    if (value < 120) return 'seconds';
    return 'minutes';
}

/** 显示用的等待数值（分种已四舍五入到分钟）。 */
export function probeWaitValue(seconds: number | undefined): number {
    const value = nonNegativeInt(seconds);
    if (probeWaitUnit(value) === 'minutes') return Math.round(value / 60);
    return value;
}

export type Translate = (key: string, values?: Record<string, string | number | Date>) => string;

const PROBE_STATE_KEYS: Record<Exclude<ProbeBadgeKind, 'scheduled' | 'waitUpstream' | 'unknown'>, string> = {
    probing: 'probe.state.running',
    skippedTraffic: 'probe.state.skippedTraffic',
    notProbed: 'probe.state.notProbed',
    off: 'probe.state.off',
    stopped: 'probe.state.stopped',
};

/** 带秒数的两类：按 now / 秒 / 分 三档选词条后缀，让中英文各自说通顺。 */
function waitLabelKey(base: 'scheduled' | 'waitUpstream', unit: WaitUnit): string {
    const suffix = unit === 'now' ? 'Now' : unit === 'minutes' ? 'Minutes' : '';
    return `probe.state.${base}${suffix}`;
}

/**
 * 探测安排 → 一句人话。`t` 用 `channel.circuits` 这一层。
 * 认不出的状态把原文塞进 {state}，让人能去查，而不是显示「未知状态」后消失。
 */
export function probeStateLabel(view: ProbeStateView, t: Translate): string {
    if (view.kind === 'scheduled' || view.kind === 'waitUpstream') {
        const unit = probeWaitUnit(view.seconds);
        const values: Record<string, number> | undefined = unit === 'now'
            ? undefined
            : unit === 'minutes'
                ? { minutes: probeWaitValue(view.seconds) }
                : { seconds: probeWaitValue(view.seconds) };
        return t(waitLabelKey(view.kind, unit), values);
    }
    if (view.kind === 'unknown') {
        return t('probe.state.unknown', { state: view.raw.replace(/[_-]+/g, ' ') });
    }
    return t(PROBE_STATE_KEYS[view.kind]);
}

const PROBE_RESULT_KEYS: Record<ProbeResultKind, string> = {
    recovered: 'probe.result.recovered',
    failed: 'probe.result.failed',
    skipped: 'probe.result.skipped',
    noVerdict: 'probe.result.noVerdict',
    unknown: 'probe.result.unknown',
};

/**
 * 尾巴原因的已知取值：能翻就翻，翻不出来就原文露出（下划线拆成空格）。
 * 两个来源：B 的调度侧 skip 理由，以及 A 的 outcome（见 CircuitProbeOutcome*）。
 */
const KNOWN_RESULT_REASONS = new Set([
    // B 侧调度理由（circuit_probe_scheduler.go 的 probeSkip*）
    'skipped_traffic',
    'wait_upstream',
    'capped',
    'streak',
    // A 侧 outcome（modeltest/circuit_probe.go 的 CircuitProbeOutcome*）
    'skipped',
    'canceled',
    'ambiguous',
    'stale',
    'timeout',
    'wrong_answer',
    'gibberish',
    'no_answer',
    'upstream_error',
    'transport_error',
]);

/** 原因 token → 人话；认不出的 token 不丢，只把下划线拆成空格露出。 */
export function probeResultReasonLabel(reason: string | undefined, t: Translate): string {
    const token = (reason ?? '').trim().toLowerCase();
    if (!token) return '';
    if (KNOWN_RESULT_REASONS.has(token)) return t(`probe.result.reason.${token}`);
    return token.replace(/[_-]+/g, ' ');
}

/**
 * 探测结论 → 人话。有原因尾巴时拼成「结论 · 原因」（probe.result.withReason），
 * 原因本身也走词条，认不出的 token 原样露出。顶层的 unknown 仍然露出整串原文。
 */
export function probeResultLabel(result: string | undefined, t: Translate): string {
    const view = probeResultView(result);
    if (view.kind === 'unknown') {
        const raw = (result ?? '').trim();
        return raw ? t(PROBE_RESULT_KEYS.unknown, { result: raw.replace(/[_-]+/g, ' ') }) : t('probe.result.none');
    }
    const status = t(PROBE_RESULT_KEYS[view.kind]);
    const reason = probeResultReasonLabel(view.reason, t);
    return reason ? t('probe.result.withReason', { status, reason }) : status;
}

/**
 * 折叠状态的「最近探测」摘要：最新一条的时间 + 结论。
 * 没有历史 = 一句话说明没探过；不显示「— · —」这种两截破折号的空壳。
 */
export function probeHistorySummary(
    history: Array<{ timestamp?: number | string; result?: string }> | undefined,
    t: Translate
): string {
    const latest = (history ?? [])[0];
    if (!latest) return t('probe.history.empty');
    const clock = formatProbeClock(latest.timestamp);
    const result = probeResultLabel(latest.result, t);
    return clock ? `${clock} · ${result}` : result;
}

/** 「今日已用」百分比：cap 为 0（没设上限/后端没给）时不画进度，不拿 0% 冒充。 */
export function probeUsagePercent(used: number | undefined, cap: number | undefined): number {
    const total = Number(cap);
    const mine = Number(used);
    if (!Number.isFinite(total) || total <= 0 || !Number.isFinite(mine) || mine <= 0) return 0;
    return Math.min(100, Math.round((mine / total) * 1000) / 10);
}
