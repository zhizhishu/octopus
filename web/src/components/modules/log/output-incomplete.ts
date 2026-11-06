/**
 * 「输出不完整」展示口径 —— 纯函数，不碰 React，方便直接单测。
 *
 * 后端把上游「没有可识别结束标记」记成 ChannelAttempt.output_incomplete_reason（只挂在被
 * 选中的那次成功尝试上）与 RelayLog.output_incomplete（后端计算，完整/摘要 JSON 都带）。
 * 竞速里落选/失败的尝试不算数，这里也绝不参与 severity 判定。
 */
import type { ChannelAttempt } from '@/api/endpoints/log';

const UPSTREAM_MISSING_TERMINAL = 'upstream_missing_terminal';

type AttemptLike = Pick<ChannelAttempt, 'status' | 'output_incomplete_reason'>;

/** 后端视野里的"最终那次尝试"：最后一次成功，否则最后一次。没有尝试返回 -1。 */
function finalSelectedIndex(attempts: readonly AttemptLike[]): number {
    for (let i = attempts.length - 1; i >= 0; i--) {
        if (attempts[i].status === 'success') return i;
    }
    return attempts.length - 1;
}

/** 第 index 次尝试是否该标注输出不完整：最终那次 + 成功 + 已知原因码，缺一不可。 */
export function attemptShowsOutputIncomplete(attempts: readonly AttemptLike[], index: number): boolean {
    if (index < 0 || index !== finalSelectedIndex(attempts)) return false;
    const attempt = attempts[index];
    if (!attempt || attempt.status !== 'success') return false;
    return (attempt.output_incomplete_reason ?? '').trim() === UPSTREAM_MISSING_TERMINAL;
}

/**
 * 整条日志的展示口径：顶层布尔权威（在就照它），缺失时才回落到最终成功尝试上的原因码。
 * 两个字段都没有的老记录返回 false —— 不显示徽标。
 */
export function logOutputIncomplete(log: {
    output_incomplete?: boolean;
    attempts?: readonly AttemptLike[] | null;
}): boolean {
    if (typeof log.output_incomplete === 'boolean') return log.output_incomplete;
    const attempts = log.attempts ?? [];
    return attemptShowsOutputIncomplete(attempts, finalSelectedIndex(attempts));
}
