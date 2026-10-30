import type { AuditFinding, AuditPlainVerdict, AuditVerdict } from '@/api/endpoints/model-audit';
import {
    confidenceLabel,
    countReviewFindings,
    verdictHeadline,
} from '@/lib/model-audit-verdict';

export { countReviewFindings, verdictHeadline, confidenceLabel };

export function adviceText(plain?: AuditPlainVerdict | null): string {
    if (plain?.next_step) return plain.next_step;
    return '可以正常用，隔段时间再抽查一次。这只说明没查出矛盾，不是原厂证明。';
}

export function meaningText(plain?: AuditPlainVerdict | null, reviewCount = 0): string {
    if (plain?.meaning) return plain.meaning;
    if (plain?.summary) return plain.summary;
    if (reviewCount > 0) return `${reviewCount} 项对不上。`;
    return '';
}

/** 结果页大标题旁的把握 chip。 */
export function graspChip(plain?: AuditPlainVerdict | null): string {
    const c = confidenceLabel(plain);
    return c ? `把握：${c === '高' ? '较高' : c === '中' ? '中等' : '较低'}` : '';
}

export function isReferenceFinding(f: AuditFinding): boolean {
    return (f.score ?? 0) <= 0;
}

export function shortHistoryVerdict(verdict: string, plain?: AuditPlainVerdict | null, status?: string): string {
    if (status === 'failed' || status === 'skipped') return '没测成';
    const h = verdictHeadline(verdict, plain);
    if (h === '有可疑点，建议复核') return '有可疑点';
    if (h === '数据不够，没法判断') return '数据不够';
    return h;
}

export type { AuditVerdict, AuditPlainVerdict };
