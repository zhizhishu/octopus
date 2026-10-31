// 调试详情（RelayLog.debug_info）展示用的纯函数。
import type { RelayLogDebugInfo, RelayLogDebugRoute } from '@/api/endpoints/log';

/** 分组模式 → 中文。与路由页「优先 / 轮询」同名。 */
export function debugRouteModeLabel(mode: RelayLogDebugRoute['mode']): string {
    switch (mode) {
        case 'fill_first':
            return '优先';
        case 'spread':
            return '轮询';
        case 'spread_tiered':
            return '分层轮询';
        default:
            return mode || '未知';
    }
}

export function debugRouteStageLabel(stage: string): string {
    return stage === 'fallback' ? '回退池' : '正常选路';
}

/** 毫秒 → 短文案；-1（没测到）显示「—」。 */
export function debugMs(ms: number | undefined): string {
    if (ms === undefined || ms < 0) return '—';
    if (ms < 1000) return `${ms} ms`;
    return `${(ms / 1000).toFixed(ms < 10000 ? 2 : 1)} s`;
}

export type DebugTimingRow = { key: string; label: string; value: string };

export function debugTimingRows(info: RelayLogDebugInfo): DebugTimingRow[] {
    const t = info.timing;
    return [
        { key: 'total', label: '总耗时', value: debugMs(t?.total_ms) },
        { key: 'before', label: '发出前准备', value: debugMs(t?.before_send_ms) },
        { key: 'header', label: '等响应头', value: debugMs(t?.header_wait_ms) },
        { key: 'first', label: '首字', value: debugMs(t?.first_token_ms) },
        { key: 'gen', label: '生成', value: debugMs(t?.generation_ms) },
        { key: 'calls', label: '上游请求', value: `${t?.upstream_calls ?? info.calls?.length ?? 0} 次` },
    ];
}

/** 一次上游调用的结果标签与色调。 */
export function debugCallTone(status: number | undefined, error: string | undefined): 'ok' | 'warn' | 'error' {
    if (error && !status) return 'error';
    if (!status) return 'warn';
    if (status >= 200 && status < 300) return 'ok';
    if (status === 429 || status >= 500) return 'warn';
    return 'error';
}

/** 负载均衡器的 trace 用 " | " 分段；拆成一行一个候选，手机上好读。 */
export function splitRouteTrace(trace: string | undefined): string[] {
    if (!trace) return [];
    return trace.split(' | ').map((s) => s.trim()).filter(Boolean);
}

export function hasDebugInfo(info: RelayLogDebugInfo | undefined | null): info is RelayLogDebugInfo {
    return !!info && ((info.calls?.length ?? 0) > 0 || (info.routes?.length ?? 0) > 0 || (info.inbound_headers?.length ?? 0) > 0);
}
