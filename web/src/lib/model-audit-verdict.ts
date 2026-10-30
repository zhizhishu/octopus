/**
 * 检测结论的唯一前端口径。报告页、历史页、渠道芯片 A/B/C 都从这里读，
 * 不许各写一套。档位与后端 behavior.PlainVerdict / ChipForResult 对齐。
 */

export type AuditVerdict = 'none' | 'low' | 'medium' | 'high' | 'unknown';
export type ChipKind = 'ok' | 'warn' | 'fail' | 'none';

export type PlainVerdict = {
    level?: string;
    headline?: string;
    meaning?: string;
    caveat?: string;
    next_step?: string;
    confidence?: string;
    summary?: string;
};

/** 大标题：none→没发现问题；low→有小疑点；medium/high→有可疑点，建议复核；unknown→数据不够，没法判断 */
export function verdictHeadline(verdict: string | undefined, plain?: PlainVerdict | null): string {
    if (plain?.headline) return plain.headline;
    switch (verdict) {
        case 'none':
            return '没发现问题';
        case 'low':
            return '有小疑点';
        case 'medium':
        case 'high':
            return '有可疑点，建议复核';
        case 'unknown':
            return '数据不够，没法判断';
        default:
            return '数据不够，没法判断';
    }
}

export function confidenceLabel(plain?: PlainVerdict | null): string {
    const c = plain?.confidence;
    if (c === '高' || c === '中' || c === '低') return c;
    return '';
}

/** 只计 Score>0；tokenizer / 参考项永不进复核数。 */
export function countReviewFindings(findings: Array<{ score?: number }> | undefined | null): number {
    if (!findings) return 0;
    return findings.filter((f) => (f.score ?? 0) > 0).length;
}

/** 渠道页芯片：与后端 ChipForResult 同口径。 */
export function chipForResult(status: string | undefined, verdict: string | undefined): ChipKind {
    switch (status) {
        case 'failed':
        case 'skipped':
            return 'fail';
        case 'done':
            switch (verdict) {
                case 'none':
                case 'low':
                    return 'ok';
                case 'medium':
                case 'high':
                    return 'warn';
                case 'unknown':
                    return 'fail';
                default:
                    return 'none';
            }
        default:
            return 'none';
    }
}

export function chipLabel(kind: ChipKind): string {
    switch (kind) {
        case 'ok':
            return '正常';
        case 'warn':
            return '待复核';
        case 'fail':
            return '检测失败';
        default:
            return '没检测过';
    }
}

export function chipDotClass(kind: ChipKind): string {
    switch (kind) {
        case 'ok':
            return 'bg-emerald-500';
        case 'warn':
            return 'bg-amber-500';
        case 'fail':
            return 'bg-rose-500';
        default:
            return 'border border-muted-foreground/50 bg-transparent';
    }
}

export function chipBadgeClass(kind: ChipKind): string {
    switch (kind) {
        case 'ok':
            return 'bg-emerald-500/15 text-emerald-700 dark:text-emerald-400';
        case 'warn':
            return 'bg-amber-500/15 text-amber-700 dark:text-amber-400';
        case 'fail':
            return 'bg-rose-500/15 text-rose-700 dark:text-rose-400';
        default:
            return 'bg-muted text-muted-foreground';
    }
}

/** 历史筛选：可疑 = medium/high；没测成 = failed/skipped/unknown。 */
export function historyBucket(status: string, verdict: string): 'all' | 'suspect' | 'failed' {
    if (status === 'failed' || status === 'skipped' || verdict === 'unknown') return 'failed';
    if (verdict === 'medium' || verdict === 'high') return 'suspect';
    return 'all';
}

/** 报告时间：用结果自己的 finished_at，不许用 new Date()。 */
export function formatReportTime(iso?: string | null): string {
    if (!iso) return '';
    const d = new Date(iso);
    if (Number.isNaN(d.getTime())) return '';
    const now = new Date();
    const sameDay = d.toDateString() === now.toDateString();
    const hh = d.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit', hour12: false });
    if (sameDay) return `今天 ${hh}`;
    const md = `${d.getMonth() + 1}-${String(d.getDate()).padStart(2, '0')}`;
    return `${md} ${hh}`;
}

export function formatChipTime(iso?: string | null): string {
    if (!iso) return '';
    const d = new Date(iso);
    if (Number.isNaN(d.getTime())) return '';
    const md = `${d.getMonth() + 1}-${String(d.getDate()).padStart(2, '0')}`;
    const hh = d.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit', hour12: false });
    return `${md} ${hh}`;
}
