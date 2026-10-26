/**
 * 生成速度（token/s）——纯计算，不碰 React，方便单测。
 *
 * 日志里只有「总耗时」和「首字」，后端没存单独的生成时长，所以：
 *   首字有效（> 0 且 < 总耗时）→ 生成窗口 = 总耗时 − 首字；
 *   否则（上游没报首字 / 首字 ≥ 总耗时）→ 退回全程平均。
 *
 * 🔴 这个口径不是这里发明的，是**复用后端既有口径**，三处实现完全一致：
 *   internal/op/model_health.go:193-196、internal/op/model_rank.go:146-152、
 *   internal/relay/balancer/telemetry.go:285-296 —— 都是 `Ftut > 0 && UseTime > Ftut` 才相减。
 *
 * 🔴 为什么不看 `is_stream`：`ftut` 只在流式读取路径上写入
 *   （internal/relay/relay.go:3151 handleStreamResponse、:3555 handleStreamResponseAsNonStream、
 *    :3673 handleNonStreamResponseAsStream），非流式的 handleResponse 从不写它。
 *   所以「首字有效」本身就等价于「内容是一点点到的」，多判一次 is_stream 只会
 *   让卡片上的数字和首页「平均吞吐」对不上——那是更难受的错。
 *
 * 🔴 这是**估算**，不是精确测量：首字可能一次吐好几个 token，总耗时也可能含重试/竞速与收尾开销，
 * 输出 token 按上游口径统计（可能含推理 token）。两种窗口分开称呼，别把"全程平均"说成"生成速度"。
 */

export type ThroughputInput = {
    output_tokens?: number;
    use_time?: number;
    ftut?: number;
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

    const firstTokenValid = Number.isFinite(first) && first > 0 && first < total;
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
