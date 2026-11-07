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
 * 只能按「生成窗口是否可采信」卡。**不可采信时绝不算数**（没有返回值、不退回
 * 全程平均冒充生成速度——那是更隐蔽的骗法）。
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
    | { available: true; text: string; tps: number }
    | { available: false; reason: ThroughputUnavailableReason };

/**
 * 逐条判定：可采信 → 数字；不可采信 → 原因码。
 *
 * 拒绝顺序（与 internal/op/throughput.go 的 relayLogTPSSample 严格一致）：
 * 失败 → 无输出 → 输出过短 → 总耗时非法 → 首字非法 → 窗口过短 → 占比过低 → 速度离谱。
 */
export function assessThroughput(log: ThroughputInput): ThroughputAssessment {
    // 失败行不显示速度。判据用全局唯一的分桶 classifyRelayLogSeverity：
    // error / error_code / error_status≥400 才算失败，warn（重试后成功）照常可算。
    // 这样管理员载荷（带 error）与用户摘要载荷（只有 error_code/error_status）结论一致。
    if (classifyRelayLogSeverity(log) === 'error') return { available: false, reason: 'request_failed' };

    const out = log.output_tokens ?? 0;
    const total = log.use_time ?? 0;
    const first = log.ftut ?? 0;

    if (!Number.isFinite(out) || out <= 0) return { available: false, reason: 'output_missing' };
    if (out < TPS_MIN_OUTPUT_TOKENS) return { available: false, reason: 'output_too_short' };
    if (!Number.isFinite(total) || total < TPS_MIN_USE_TIME_MS) {
        return { available: false, reason: 'duration_invalid' };
    }
    if (!Number.isFinite(first) || first <= 0 || first >= total) {
        return { available: false, reason: 'first_token_missing' };
    }

    const genMs = total - first;
    if (genMs < TPS_MIN_GEN_MS) return { available: false, reason: 'generation_window_short' };
    if (genMs < total * TPS_MIN_GEN_SHARE) return { available: false, reason: 'generation_share_low' };

    const tps = out / (genMs / 1000);
    if (!Number.isFinite(tps) || tps <= 0 || tps > TPS_MAX_PLAUSIBLE) {
        return { available: false, reason: 'implausible_speed' };
    }

    // 别让 0.04 t/s 被舍成 "0.0"（看着像没数据）；三位数以上不要小数，免得占宽。
    const text =
        tps < 0.01 ? '<0.01' : tps < 10 ? tps.toFixed(2) : tps >= 100 ? tps.toFixed(0) : tps.toFixed(1);

    return { available: true, text, tps };
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
