/**
 * 生成速度（token/s）——纯计算，不碰 React，方便单测。
 *
 * 日志里只有「总耗时」和「首字」，后端没存单独的生成时长，所以：
 *   流式 + 首字有效 → 生成窗口 = 总耗时 − 首字；
 *   非流式 / 首字缺失 / 首字 ≥ 总耗时 → 退回全程平均。
 *
 * 🔴 这是**估算**，不是精确测量：首字可能一次吐好几个 token，总耗时也可能含收尾开销，
 * 输出 token 按上游口径统计（可能含推理 token）。两种窗口必须分开称呼，
 * 别把"全程平均"说成"纯生成速度"。
 */

export type ThroughputInput = {
    output_tokens?: number;
    use_time?: number;
    ftut?: number;
    is_stream?: boolean;
};

export type ThroughputReading = {
    /** 已格式化好的数字（含 '<0.01' 这种下限写法） */
    text: string;
    /** 用的哪个时间窗，调用方据此选提示文案 */
    window: 'gen' | 'total';
};

export function describeThroughput(log: ThroughputInput): ThroughputReading | null {
    const out = log.output_tokens ?? 0;
    const total = log.use_time ?? 0;
    const first = log.ftut ?? 0;

    // 没有输出、或总耗时不成数 → 无从计算，整块不显示（不要 0、不要 Infinity）。
    if (!Number.isFinite(out) || out <= 0) return null;
    if (!Number.isFinite(total) || total <= 0) return null;

    // 只有流式请求的「总耗时 − 首字」才真的是生成阶段；非流式或没有流式标记（老日志）一律全程平均，
    // 宁可读数偏低，也不假装那是纯生成速度。
    const firstTokenValid =
        log.is_stream === true && Number.isFinite(first) && first > 0 && first < total;
    const genMs = firstTokenValid ? total - first : total;
    const sec = genMs / 1000;
    if (!Number.isFinite(sec) || sec <= 0) return null;

    const tps = out / sec;
    if (!Number.isFinite(tps) || tps <= 0) return null;

    // 别让 0.04 t/s 被舍成 "0.0"（看着像没数据）；三位数以上不要小数，免得占宽。
    const text =
        tps < 0.01 ? '<0.01' : tps < 10 ? tps.toFixed(2) : tps >= 100 ? tps.toFixed(0) : tps.toFixed(1);

    return { text, window: firstTokenValid ? 'gen' : 'total' };
}
