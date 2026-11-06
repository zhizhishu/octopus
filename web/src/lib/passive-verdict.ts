/**
 * 「被动」结论：把 passive_summary / passive_source.hints / passive_probes 三处被动证据
 * 收敛成一句人话——没发现问题（clean）还是有可疑线索（suspicious）。
 *
 * 纯逻辑、零依赖（不引 React、不引 api 类型）：组件和 node 测试共用同一份判定，
 * 否则迟早出现「徽章说一套、弹层说一套」。入参只读结论用得到的那几个字段，
 * 所以 RelayLog 整条记录能直接传进来，测试也不用造完整日志。
 *
 * 🔴 只会发现问题，绝不认证：没有可疑线索 ≠ 已验真。
 */

/** 一条被动特征线索（后端 passive_source.hints[] 的元素）。 */
export type PassiveHint = {
    /** 线索指向哪家上游（后端自报，只用于展示/排查，不参与结论）。 */
    platform: string;
    /** strong/medium 才够格出结论；weak 只是线索，放在弹层里备查。 */
    strength: 'strong' | 'medium' | 'weak';
    /** 机器码，翻译映射见 PASSIVE_REASON_LABEL_KEY。 */
    code: string;
    /** 命中的原始证据（可读串），给人自己复算用。 */
    evidence?: string;
};

/**
 * 判定只读这几个字段。故意收窄（而不是直接引用 RelayLog）：
 * 这是纯逻辑，测试不该为了跑一次判定去满足整条日志的类型。
 */
export type PassiveVerdictInput = {
    error?: string | null;
    passive_summary?: { status?: string; reason?: string } | null;
    passive_source?: {
        hints?: PassiveHint[] | null;
        header_families?: string[] | null;
    } | null;
    passive_probes?: readonly unknown[] | null;
};

/**
 * null      = 不出徽章（失败请求 / 没开被动检测 / 压根判断不了）
 * clean     = 三处证据都看过了，没发现问题
 * suspicious= 有 strong/medium 线索或探针命中；reasons 按强度排好（strong 在前、去重），
 *             只有探针命中时 reasons 为空、由 probeCount 说话。
 */
export type PassiveVerdict =
    | null
    | { kind: 'clean' }
    | { kind: 'suspicious'; reasons: string[]; probeCount: number };

/** not_applicable 里这几个原因说明「这条请求压根没法判断」，一律不出结论。 */
const NO_VERDICT_REASONS = new Set(['error', 'canceled', 'out_of_range']);

/**
 * 被动线索码 → 翻译 key。未知码不丢，落到 passiveReasonUnknown——
 * 「有异常线索」比空字符串诚实。
 */
export const PASSIVE_REASON_LABEL_KEY: Record<string, string> = {
    model_prefix_kiro: 'passiveReasonKiro',
    model_prefix_bedrock: 'passiveReasonBedrock',
    tool_id_bedrock: 'passiveReasonAws',
    tool_id_bedrock_messages: 'passiveReasonBedrock',
    response_id_bedrock: 'passiveReasonBedrock',
    header_aws: 'passiveReasonAws',
    response_id_vertex: 'passiveReasonVertex',
    tool_id_vertex: 'passiveReasonVertex',
    signature_vertex: 'passiveReasonVertex',
    header_openai_cross_vendor: 'passiveReasonOpenAICross',
    header_anthropic_cross_vendor: 'passiveReasonAnthropicCross',
    model_switch_mid_stream: 'passiveReasonModelSwitch',
};

/** 只有探针命中（没有任何 hint 码）时，徽章上那句原因。 */
export const PASSIVE_REASON_PROBE_KEY = 'passiveReasonProbe';
/** 认不出的线索码 / 没有码时的兜底。 */
export const PASSIVE_REASON_UNKNOWN_KEY = 'passiveReasonUnknown';

/** 线索码 → 翻译 key，认不出就 passiveReasonUnknown（绝不返回空串）。 */
export function passiveReasonLabelKey(code: string | null | undefined): string {
    if (!code) return PASSIVE_REASON_UNKNOWN_KEY;
    const key = PASSIVE_REASON_LABEL_KEY[code];
    return typeof key === 'string' && key ? key : PASSIVE_REASON_UNKNOWN_KEY;
}

/** 强度 → 排序名次：strong 排前、medium 其后、weak 最后（徽章只显示第一条）。 */
function strengthRank(strength: string | null | undefined): number {
    if (strength === 'strong') return 0;
    if (strength === 'medium') return 1;
    return 2;
}

/** 按强度排序的副本（稳定：同强度保持后端给的顺序）。弹层按这个顺序列线索。 */
export function sortPassiveHints(hints: readonly PassiveHint[] | null | undefined): PassiveHint[] {
    if (!Array.isArray(hints)) return [];
    return hints
        .map((hint, index) => ({ hint, index }))
        .sort((left, right) => {
            const byStrength = strengthRank(left.hint && left.hint.strength)
                - strengthRank(right.hint && right.hint.strength);
            return byStrength === 0 ? left.index - right.index : byStrength;
        })
        .map((entry) => entry.hint);
}

/**
 * 判定顺序是硬约定（测试逐条盯）：
 *   1. error 非空      → null（失败请求判断不了）
 *   2. 三处证据都没有  → null（旧记录 / 没开被动检测）
 *   3. not_applicable 且原因属于「本来就不适用」→ null
 *   4. strong/medium 线索或探针命中 → suspicious
 *   5. 其余 → clean
 */
export function passiveVerdict(log: PassiveVerdictInput): PassiveVerdict {
    // 1. 失败的请求没有可判断的回复：不出任何结论（也不许出「正常」）。
    const errorText = typeof log.error === 'string' ? log.error.trim() : '';
    if (errorText) return null;

    const summary = log.passive_summary ?? null;
    const source = log.passive_source ?? null;
    const rawProbes = log.passive_probes;
    const probes: readonly unknown[] = Array.isArray(rawProbes) ? rawProbes : [];

    // 2. 三处被动证据一个都没有：旧记录或没开被动检测，没东西可说。
    if (!summary && !source && probes.length === 0) return null;

    // 3. 明确「这条请求不适用被动检测」的原因（报错/被取消/越界）不参与结论。
    if (summary && summary.status === 'not_applicable' && NO_VERDICT_REASONS.has(summary.reason ?? '')) {
        return null;
    }

    // 4. 够格出结论的线索（weak 不够格），或硬事实探针命中。
    const rawHints = source ? source.hints : null;
    const hints: PassiveHint[] = Array.isArray(rawHints) ? rawHints : [];
    const reasons: string[] = [];
    for (const hint of sortPassiveHints(hints)) {
        if (!hint) continue;
        const strength = hint.strength;
        if (strength !== 'strong' && strength !== 'medium') continue;
        if (typeof hint.code !== 'string' || !hint.code) continue;
        if (!reasons.includes(hint.code)) reasons.push(hint.code);
    }
    if (reasons.length > 0 || probes.length > 0) {
        return { kind: 'suspicious', reasons, probeCount: probes.length };
    }

    // 5. 看过了，没发现问题。注意这只是「没发现」，不是「验真」。
    return { kind: 'clean' };
}
