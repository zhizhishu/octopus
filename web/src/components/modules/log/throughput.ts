/**
 * 生成速度（token/s）——纯计算，不碰 React，方便单测。
 *
 * 公式：tps = 输出 tokens ÷（总耗时 − 首字）。公式没错，错在把**不可采信的
 * 生成窗口**也拿去算——实测记录（2026-10-27 起）：库里有 ftut 落在 use_time 前
 * 1ms 的行，1ms 窗口把 5874 个 token 算成 5,874,000 tok/s，3 天均值被拉到 10453。
 * 这是「窗口不可采信」的观测，本文件**不推断**那一行为什么把首字拖到最后
 * （缓冲、排队、重试、思考 token 都只是可能，单凭阈值证不出来）。
 *
 * 所以不能按 is_stream 卡（IsStream 记的是客户端意图，判不出上游是否缓冲），
 * 只能按「生成窗口是否可采信」卡。**不可采信时绝不算生成速度**——但可以另给一条
 * 明确标注为约值的「全程平均」估算（kind 'overall'，界面带 ≈ 前缀，见
 * assessOverallThroughput），口径借自 sub2api（output*1000/duration_ms + >0 与
 * Number.isFinite 闸门）、axonhub（分母 10ms 兜底）与 claude-code-hub（>5000 不展示），
 * 与生成速度分开、不冒充生成速度。
 *
 * 阈值与 internal/op/throughput.go 一一对应（astra 裁决的同一套拒绝条件），
 * 改任何一边都必须同步另一边和测试。
 *
 * 界面口径（2026-11-06）：算不出来不等于「这一行没这个指标」——文本生成行恒给
 * 一个中性徽标「TPS —」，点开说明为什么算不出来；原因走本文件的 reason code，
 * 由界面翻成人话，**不把机器码糊到界面上**，也绝不拿 0 当速度。
 */

import { classifyRelayLogSeverity } from '../../../api/endpoints/log-severity';

/** 输出太少的样本：观测窗口不足以代表生成速度（短样本里非生成开销占比不明）。 */
export const TPS_MIN_OUTPUT_TOKENS = 100;
/** 总耗时不足 1s 没有可分辨的窗口。 */
export const TPS_MIN_USE_TIME_MS = 1000;
/** 窗口不足 1s 时 1ms 的窗口误差就是千倍的速度误差。 */
export const TPS_MIN_GEN_MS = 1000;
/** 生成窗口至少要覆盖总耗时的 25%（挡住首字几乎贴着总耗时、窗口只剩零头的形态）。 */
export const TPS_MIN_GEN_SHARE = 0.25;
/** 展示上限：超过就不展示（不去区分是测量问题还是真值，也不拿它反推上游）。 */
export const TPS_MAX_PLAUSIBLE = 1000;
/** 全程平均估算：总耗时不足 10ms 时按 10ms 兜底，挡住除零和天文数字（axonhub 同款）。 */
export const TPS_OVERALL_MIN_DURATION_MS = 10;
/** 全程平均估算的展示上限：高于它的速率几乎都是测量假象（claude-code-hub 同款）。 */
export const TPS_OVERALL_MAX_PLAUSIBLE = 5000;

/**
 * 算不出来的原因码（机器码，只在前端内部流转，界面必须翻成人话）。
 * 与 internal/op/throughput.go 的拒绝顺序一一对应，多一个少一个都算口径漂移。
 */
export type ThroughputUnavailableReason =
    /** 请求失败（error / error_code / error_status 任一成立）：失败行没有「速度」可言。 */
    | 'request_failed'
    /** 根本没有输出 token（无用量、上游未报、图片/二进制等按次计费端点）。 */
    | 'output_missing'
    /** 有输出但不足 100 token：样本太短，暂不展示估算。 */
    | 'output_too_short'
    /** 总耗时缺失/非法/不足 1s。 */
    | 'duration_invalid'
    /** 首字缺失、非正、或不早于总耗时（这条行没有可用的首字时间戳）。 */
    | 'first_token_missing'
    /** 首字之后不足 1s：窗口太短，分辨不出生成速度。 */
    | 'generation_window_short'
    /** 首字后窗口占总耗时不足 25%：首字出现得太晚，窗口占比不够。 */
    | 'generation_share_low'
    /** 算出来超过当前展示上限：当前观测不足以采信。 */
    | 'implausible_speed';

export type ThroughputInput = {
    output_tokens?: number;
    use_time?: number;
    ftut?: number;
    /** 非空（trim 后）表示这次请求失败。判定统一交给 classifyRelayLogSeverity。 */
    error?: string | null;
    /** 失败原因码。与 error 同权：只有分桶为 error 才拒算（warn=重试成功照常可算）。 */
    error_code?: string | null;
    /** 上游/网关 HTTP 状态；≥400 视为失败。 */
    error_status?: number | null;
    total_attempts?: number | null;
    attempts?: readonly unknown[] | null;
    /** 用量来源；local_estimate 表示输出 token 是本地估算的。 */
    usage_source?: string | null;
};

export type ThroughputReading = {
    /** 已格式化好的数字（含 '<0.01' 这种下限写法） */
    text: string;
};

export type ThroughputAssessment =
    /** kind 'generation' = 首字后窗口的生成速度；kind 'overall' = 全程平均约值（界面带 ≈ 前缀）。 */
    | { available: true; text: string; tps: number; kind: 'generation' | 'overall'; /** 仅 kind='overall'：精确生成速度为什么算不出来（界面在弹层里解释）。 */ preciseReason?: ThroughputUnavailableReason }
    | { available: false; reason: ThroughputUnavailableReason };

/** 已格式化好的速度数字（含 '<0.01' 下限写法）。精确路径与全程平均共用一份口径。 */
function formatTpsText(tps: number): string {
    // 别让 0.04 t/s 被舍成 "0.0"（看着像没数据）；三位数以上不要小数，免得占宽。
    return tps < 0.01 ? '<0.01' : tps < 10 ? tps.toFixed(2) : tps >= 100 ? tps.toFixed(0) : tps.toFixed(1);
}

/**
 * 全程平均估算（fallback，不是生成速度）：输出 tokens ÷ 总耗时（含首字等待）。
 * 失败行与无输出行没有速度可言，不退；总耗时不足 10ms 按 10ms 兜底；算出来超过
 * 5000 tok/s 视为测量假象不展示。闸门与精确路径各自独立，这里不改任何精确闸门。
 */
export function assessOverallThroughput(log: ThroughputInput): ThroughputAssessment {
    if (classifyRelayLogSeverity(log) === 'error') return { available: false, reason: 'request_failed' };

    const out = log.output_tokens ?? 0;
    const total = log.use_time ?? 0;
    if (!Number.isFinite(out) || out <= 0) return { available: false, reason: 'output_missing' };
    if (!Number.isFinite(total) || total <= 0) return { available: false, reason: 'duration_invalid' };

    const tps = out / (Math.max(total, TPS_OVERALL_MIN_DURATION_MS) / 1000);
    if (!Number.isFinite(tps) || tps <= 0 || tps > TPS_OVERALL_MAX_PLAUSIBLE) {
        return { available: false, reason: 'implausible_speed' };
    }
    return { available: true, text: formatTpsText(tps), tps, kind: 'overall' };
}

/**
 * 逐条判定：可采信 → 数字；不可采信 → 原因码。
 *
 * 拒绝顺序（与 internal/op/throughput.go 的 relayLogTPSSample 严格一致）：
 * 失败 → 无输出 → 输出过短 → 总耗时非法 → 首字非法 → 窗口过短 → 占比过低 → 速度离谱。
 * 精确路径算不出来时（失败与无输出除外）退回「全程平均」约值：sub2api / axonhub /
 * claude-code-hub 同款口径，见 assessOverallThroughput。整体退不出来时保留精确
 * 路径的原因码。失败与无输出永远不退（没有速度概念）；闸门常量与顺序一个都不动。
 */
export function assessThroughput(log: ThroughputInput): ThroughputAssessment {
    // 失败行不显示速度。判据用全局唯一的分桶 classifyRelayLogSeverity：
    // error / error_code / error_status≥400 才算失败，warn（重试后成功）照常可算。
    // 这样管理员载荷（带 error）与用户摘要载荷（只有 error_code/error_status）结论一致。
    if (classifyRelayLogSeverity(log) === 'error') return { available: false, reason: 'request_failed' };

    // 精确路径拒绝时给 fallback 一个统一的出口：可退的原因先试全程平均，
    // 退出来带上精确路径的原因码（界面用它解释为什么是约值）；退不出来保留原原因。
    const refuse = (reason: ThroughputUnavailableReason): ThroughputAssessment => {
        if (reason === 'request_failed' || reason === 'output_missing') return { available: false, reason };
        const overall = assessOverallThroughput(log);
        if (overall.available) return { ...overall, preciseReason: reason };
        return { available: false, reason };
    };

    const out = log.output_tokens ?? 0;
    const total = log.use_time ?? 0;
    const first = log.ftut ?? 0;

    if (!Number.isFinite(out) || out <= 0) return { available: false, reason: 'output_missing' };
    if (out < TPS_MIN_OUTPUT_TOKENS) return refuse('output_too_short');
    if (!Number.isFinite(total) || total < TPS_MIN_USE_TIME_MS) {
        return refuse('duration_invalid');
    }
    if (!Number.isFinite(first) || first <= 0 || first >= total) {
        return refuse('first_token_missing');
    }

    const genMs = total - first;
    if (genMs < TPS_MIN_GEN_MS) return refuse('generation_window_short');
    if (genMs < total * TPS_MIN_GEN_SHARE) return refuse('generation_share_low');

    const tps = out / (genMs / 1000);
    if (!Number.isFinite(tps) || tps <= 0 || tps > TPS_MAX_PLAUSIBLE) {
        return refuse('implausible_speed');
    }

    return { available: true, text: formatTpsText(tps), tps, kind: 'generation' as const };
}

/**
 * 旧接口：只要数字，算不出来给 null（历史调用方与既有测试用它）。
 * 闸门只在 assessThroughput 里实现一次，这里只做取值，不再复制一遍条件。
 */
export function describeThroughput(log: ThroughputInput): ThroughputReading | null {
    const assessed = assessThroughput(log);
    return assessed.available ? { text: assessed.text } : null;
}

/** 原因码 → `log.card.*` 文案键（界面翻人话，机器码不外露）。 */
export function throughputReasonLabelKey(reason: ThroughputUnavailableReason): string {
    switch (reason) {
        case 'request_failed':
            return 'tpsReasonFailed';
        case 'output_missing':
            return 'tpsReasonNoOutput';
        case 'output_too_short':
            return 'tpsReasonTooShort';
        case 'duration_invalid':
            return 'tpsReasonNoWindow';
        case 'first_token_missing':
            return 'tpsReasonNoFirstToken';
        case 'generation_window_short':
            return 'tpsReasonWindowShort';
        case 'generation_share_low':
            return 'tpsReasonShareLow';
        case 'implausible_speed':
            return 'tpsReasonImplausible';
    }
}

/**
 * 输出 token 是不是本地估算出来的（上游没报用量，metrics 自己数了一遍）。
 * 速度仍然是「tokens ÷ 实测窗口」，但分子是估算值，详情里必须点明。
 */
export function throughputOutputIsEstimated(log: ThroughputInput): boolean {
    return (log.usage_source ?? '').trim().toLowerCase() === 'local_estimate';
}

/**
 * 不产生「生成 token」的端点族：图/视频/音频/向量/审核/重排按张、按秒、按条计费，
 * 「token/s」在其中没有意义（images.go 的 usage 也是图片张数换算出来的），整块不显示。
 *
 * 取值来自后端实际落库的 request_endpoint（不是归一化显示名），逐个核对过：
 *   - internal/relay/images.go imageEndpointName() → images / images_generations /
 *     images_edits（异步再带 `_async`；midjourney 落 images_midjourney）
 *   - videos 日志 → videos / videos_poll
 *   - internal/server/handlers/raw_protocol.go:114-197 → audio_speech / audio_tts /
 *     audio_stt / audio_transcriptions / audio_translations / moderations / rerank / edits
 *   - internal/relay/relay.go:4851 → embeddings（audio_realtime 落在 audio 族下）
 *
 * 后端没有现成的「是否文本生成端点」分类函数可复用（internal/op/log_passive.go:113-118
 * 的「范围外端点」是同口径的行内判断，未导出），所以这里留一份小清单；
 * 匹配口径同 relayLogEndpointMatches：族名本身或 `<族名>_<变体>`。
 */
const TPS_NON_TEXT_ENDPOINT_FAMILIES = [
    'images',
    'edits',
    'videos',
    'audio',
    'embeddings',
    'moderations',
    'rerank',
] as const;

/** 与 internal/op/log.go relayLogEndpointMatches 同口径（族名 or 族名_变体）。 */
function matchesEndpointFamily(endpoint: string, family: string): boolean {
    return endpoint === family || endpoint.startsWith(`${family}_`);
}

/**
 * 这一行该不该出现 TPS 徽标。
 *
 * 只看落库的 request_endpoint（后端已清洗成 endpoint 名，不是裸 URL；也不做模型名
 * 猜测、不拿请求路径反推）。
 * 认不出的端点（空值、历史行、systemone/completions 之类）沿用旧行为：照常给
 * 徽标，算不出来就显示原因——宁可显示「没测准」，也不假装这一行没有速度概念。
 * 连通性测试行（model_test*）由调用方用 isModelTestEndpoint 另行排除。
 */
export function throughputEndpointEligible(endpoint: string | null | undefined): boolean {
    const value = (endpoint ?? '').trim().toLowerCase();
    if (!value) return true;
    return !TPS_NON_TEXT_ENDPOINT_FAMILIES.some((family) => matchesEndpointFamily(value, family));
}
