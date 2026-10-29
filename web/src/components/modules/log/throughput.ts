/**
 * 生成速度（token/s）——纯计算，不碰 React，方便单测。
 *
 * 公式：tps = 输出 tokens ÷（总耗时 − 首字）。公式没错，错在把**不可采信的
 * 生成窗口**也拿去算——生产实证（2026-10-27）：非流式上游的完整响应在最后一刻
 * 才合成下推，ftut 落在 use_time−1ms，1ms 窗口把 5874 个 token 显示成
 * 5,874,000 tok/s；3 天均值被污染到 10453 tok/s。
 *
 * 所以不能按 is_stream 卡（IsStream 记的是客户端意图，判不出上游是否缓冲），
 * 只能按「生成窗口是否可采信」卡。**不可采信的正确处理是不显示**（返回 null，
 * 调用处整块不渲染），不是退回全程平均冒充生成速度——那是更隐蔽的骗法。
 *
 * 🔴 本文件的阈值与后端 internal/op/throughput.go 一一对应（astra 裁决的同一套
 * 拒绝条件），改任何一边都必须同步另一边和测试。
 */

/** 短回复的窗口量的是网络冲刷不是生成速度。 */
export const TPS_MIN_OUTPUT_TOKENS = 100;
/** 总耗时不足 1s 没有可分辨的窗口。 */
export const TPS_MIN_USE_TIME_MS = 1000;
/** 窗口不足 1s 时 1ms 的窗口误差就是千倍的速度误差。 */
export const TPS_MIN_GEN_MS = 1000;
/** 生成窗口至少要覆盖总耗时的 25%（防 ftut 落在 98% 处的爆表形态）。 */
export const TPS_MIN_GEN_SHARE = 0.25;
/** 超出现实上限说明窗口测量坏了，不是上游真快。 */
export const TPS_MAX_PLAUSIBLE = 1000;

export type ThroughputInput = {
    output_tokens?: number;
    use_time?: number;
    ftut?: number;
    /** 非空（trim 后）表示这次请求失败。与后端 relayLogTPSSample 一样不采样。 */
    error?: string;
};

export type ThroughputReading = {
    /** 已格式化好的数字（含 '<0.01' 这种下限写法） */
    text: string;
};

export function describeThroughput(log: ThroughputInput): ThroughputReading | null {
    // 与 internal/op/throughput.go 的 relayLogTPSSample 一致：有错误就不显示速度。
    if ((log.error ?? '').trim() !== '') return null;

    const out = log.output_tokens ?? 0;
    const total = log.use_time ?? 0;
    const first = log.ftut ?? 0;

    if (!Number.isFinite(out) || out < TPS_MIN_OUTPUT_TOKENS) return null;
    if (!Number.isFinite(total) || total < TPS_MIN_USE_TIME_MS) return null;
    if (!Number.isFinite(first) || first <= 0 || first >= total) return null;

    const genMs = total - first;
    if (genMs < TPS_MIN_GEN_MS) return null;
    if (genMs < total * TPS_MIN_GEN_SHARE) return null;

    const tps = out / (genMs / 1000);
    if (!Number.isFinite(tps) || tps <= 0 || tps > TPS_MAX_PLAUSIBLE) return null;

    // 别让 0.04 t/s 被舍成 "0.0"（看着像没数据）；三位数以上不要小数，免得占宽。
    const text =
        tps < 0.01 ? '<0.01' : tps < 10 ? tps.toFixed(2) : tps >= 100 ? tps.toFixed(0) : tps.toFixed(1);

    return { text };
}
