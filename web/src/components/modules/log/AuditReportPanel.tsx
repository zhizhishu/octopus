import { useMemo, useState } from 'react';
import { ChevronDown } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { cn } from '@/lib/utils';

import {
    parseAuditReport,
    type AuditReport,
    type AuditReportFinding,
    type AuditReportProbeError,
    type AuditReportPlain,
    type AuditReportResult,
} from './audit-report';

// 这几个类型在本文件里被用作渲染签名，re-export 只为让 diff 读起来不用跳文件。
export type { AuditReport, AuditReportFinding, AuditReportProbeError, AuditReportPlain, AuditReportResult };

/**
 * 详情弹窗里的「模型检测报告明细」。
 *
 * 数据来源：`model_audit_report`（RelayLog 上的一列），内容是 Go 侧
 * `behavior.ViewReport` 序列化后的 JSON（见 internal/modelverify/behavior/view.go）。
 * 字段名一律以那边的 struct tag 为准：
 *   requested_model / requested_families / score / verdict / findings / results /
 *   errors / disclaimer / plain{level,headline,meaning,caveat,next_step} / family
 * findings 每项 {probe,severity,score,title,evidence,recommendation}；
 * results 每项 {probe_id,ok,data,error}（注意是 probe_id，不是 probe）；
 * errors 每项 {probe,error}（注意不是 probe_id/message）。
 *
 * findings 与 results 不是一回事：findings 是判定出的「问题」，results 是每个探针的
 * 「原始观测」（跑通没跑的原始数字都记）。用户要能查「到底测出了什么数」，
 * 所以 results 必须能展开看 data 的每个键值。
 *
 * 语义边界（与后端措辞一致）：这套检测只测「行为像不像官方」，不证明模型真伪。
 * 所以 caveat（"这不能说明什么"）和 disclaimer 必须留在面板里，哪怕它们让人不快——
 * 藏掉边界的检测结论会被当成验真证书用。
 *
 * 空字符串 / 解析失败 = 这行根本没被检测过，整栏不渲染，不摆空壳。
 */

/** verdict 文案复用卡片上已有的档位 key，不新造一套说法。 */
function verdictLabel(verdict: string | undefined, t: (key: string) => string): string {
    switch (verdict) {
        case 'none':
            return t('auditClean');
        case 'unknown':
            return t('auditInsufficient');
        case 'low':
            return t('auditLow');
        case 'medium':
            return t('auditMedium');
        case 'high':
            return t('auditHigh');
        default:
            return '';
    }
}

function findingStyle(severity: string | undefined): string {
    switch (severity) {
        case 'high':
            return 'border-destructive/40 bg-destructive/5';
        case 'medium':
            return 'border-amber-500/40 bg-amber-500/5';
        default:
            // low 与未知档都用中性色：低危项是观察，不是判决。
            return 'border-border bg-background/60';
    }
}

function severityTextStyle(severity: string | undefined): string {
    switch (severity) {
        case 'high':
            return 'text-destructive';
        case 'medium':
            return 'text-amber-600 dark:text-amber-400';
        default:
            return 'text-muted-foreground';
    }
}

/** 单条原始观测：probe_id + 成功/失败标记，data 可展开逐键看。 */
function ResultRow({ result }: { result: AuditReportResult }) {
    const t = useTranslations('log.card');
    const [showData, setShowData] = useState(false);
    const dataRows = useMemo(
        () => Object.entries(result.data ?? {}).filter(([, value]) => value !== undefined),
        [result.data]
    );
    const ok = result.ok === true;

    return (
        <li className="min-w-0">
            <div className="flex min-w-0 flex-wrap items-baseline gap-x-2 gap-y-0.5 text-[11px]">
                <span className="min-w-0 break-words font-mono text-foreground [overflow-wrap:anywhere]">
                    {result.probe_id ?? ''}
                </span>
                <span
                    className={cn(
                        'shrink-0 font-medium',
                        ok ? 'text-emerald-600 dark:text-emerald-400' : 'text-amber-600 dark:text-amber-400'
                    )}
                >
                    {ok ? t('auditReportResultOk') : t('auditReportResultFail')}
                </span>
                {dataRows.length > 0 && (
                    <button
                        type="button"
                        onClick={() => setShowData((v) => !v)}
                        aria-expanded={showData}
                        className="shrink-0 text-[11px] text-muted-foreground transition-colors hover:text-foreground"
                    >
                        {showData ? t('auditReportResultDataHide') : t('auditReportResultDataShow')}
                    </button>
                )}
            </div>
            {/* 探针自身报错用琥珀色：与下面的 errors 块同色系，不读成高危。 */}
            {result.error && (
                <p className="mt-0.5 break-words text-[11px] leading-5 text-amber-700 dark:text-amber-300 [overflow-wrap:anywhere]">
                    {result.error}
                </p>
            )}
            {showData && dataRows.length > 0 && (
                <div className="mt-1 space-y-0.5 pl-2">
                    {dataRows.map(([key, value]) => (
                        <div key={key} className="flex flex-wrap items-baseline gap-x-1.5 text-[11px]">
                            <span className="shrink-0 text-muted-foreground">{key}</span>
                            <span className="min-w-0 break-words font-mono tabular-nums text-foreground [overflow-wrap:anywhere]">
                                {formatEvidenceValue(value)}
                            </span>
                        </div>
                    ))}
                </div>
            )}
        </li>
    );
}

function formatEvidenceValue(value: unknown): string {
    if (value === null || value === undefined) return '—';
    if (typeof value === 'string') return value;
    if (typeof value === 'number' || typeof value === 'boolean') return String(value);
    try {
        return JSON.stringify(value);
    } catch {
        return String(value);
    }
}

/** 单条风险项：标题 + 档位/得分 + 建议 + 证据（折叠）。 */
function FindingRow({ finding }: { finding: AuditReportFinding }) {
    const t = useTranslations('log.card');
    const [showEvidence, setShowEvidence] = useState(false);
    const evidenceRows = useMemo(
        () => Object.entries(finding.evidence ?? {}).filter(([, value]) => value !== undefined),
        [finding.evidence]
    );
    const severity = finding.severity?.trim().toLowerCase();

    return (
        <li className={cn('rounded-lg border px-2.5 py-2', findingStyle(severity))}>
            <div className="flex min-w-0 flex-wrap items-baseline gap-x-2 gap-y-0.5">
                <span className="min-w-0 text-xs leading-5 text-foreground">{finding.title ?? finding.probe ?? ''}</span>
                {severity && (
                    <span className={cn('shrink-0 text-[11px] font-medium uppercase', severityTextStyle(severity))}>
                        {severity}
                    </span>
                )}
                {typeof finding.score === 'number' && (
                    <span className="shrink-0 font-mono text-[11px] tabular-nums text-muted-foreground">
                        {t('auditScore')} {finding.score}
                    </span>
                )}
            </div>
            {finding.recommendation && (
                <p className="mt-1 text-[11px] leading-5 text-muted-foreground">{finding.recommendation}</p>
            )}
            {evidenceRows.length > 0 && (
                <div className="mt-1.5">
                    <button
                        type="button"
                        onClick={() => setShowEvidence((v) => !v)}
                        aria-expanded={showEvidence}
                        className="inline-flex items-center gap-1 text-[11px] text-muted-foreground transition-colors hover:text-foreground"
                    >
                        <ChevronDown className={cn('size-3 transition-transform', showEvidence && 'rotate-180')} />
                        {showEvidence ? t('auditReportEvidenceHide') : t('auditReportEvidenceShow')}
                    </button>
                    {showEvidence && (
                        <div className="mt-1 space-y-0.5">
                            {evidenceRows.map(([key, value]) => (
                                <div key={key} className="flex flex-wrap items-baseline gap-x-1.5 text-[11px]">
                                    <span className="shrink-0 text-muted-foreground">{key}</span>
                                    <span className="min-w-0 break-words font-mono tabular-nums text-foreground [overflow-wrap:anywhere]">
                                        {formatEvidenceValue(value)}
                                    </span>
                                </div>
                            ))}
                        </div>
                    )}
                </div>
            )}
        </li>
    );
}

/**
 * - `defaultOpen`：默认折叠。标题行给「检测明细 + 发现数」，点开才铺内容——
 *   明细很长，默认展开会把下面的请求/响应面板挤走。
 */
export function AuditReportPanel({ report, defaultOpen = false }: { report?: string; defaultOpen?: boolean }) {
    const t = useTranslations('log.card');
    const [expanded, setExpanded] = useState(defaultOpen);
    // 原始观测是证据，条数多、键多，默认折起来，别把请求/响应面板挤走。
    const [showResults, setShowResults] = useState(false);
    const parsed = useMemo(() => parseAuditReport(report), [report]);

    // 没被检测过（空串）或报告损坏 → 整栏不渲染。摆一个空壳会被读成"检测过但没问题"。
    if (!parsed) return null;

    const plain = parsed.plain ?? {};
    const findings = parsed.findings ?? [];
    const results = parsed.results ?? [];
    const errors = parsed.errors ?? [];
    const verdictText = verdictLabel(parsed.verdict, t);
    const metaParts = [
        parsed.requested_model?.trim() ? `${t('auditModel')} ${parsed.requested_model.trim()}` : '',
        typeof parsed.score === 'number' ? `${t('auditScore')} ${parsed.score}` : '',
    ].filter(Boolean);

    return (
        <div className="min-w-0 rounded-xl border border-border bg-muted/20 px-3.5 py-2.5">
            <button
                type="button"
                onClick={() => setExpanded((v) => !v)}
                aria-expanded={expanded}
                className="flex w-full items-baseline justify-between gap-2 text-left"
            >
                <span className="flex min-w-0 flex-wrap items-baseline gap-x-2">
                    <span className="text-sm font-medium text-foreground">{t('auditReportTitle')}</span>
                    {verdictText && (
                        <span className="text-[11px] text-muted-foreground">{verdictText}</span>
                    )}
                </span>
                <span className="flex shrink-0 items-center gap-1 text-[11px] text-muted-foreground">
                    {t('auditReportFindingCount', { count: findings.length })}
                    <ChevronDown className={cn('size-3 transition-transform', expanded && 'rotate-180')} />
                </span>
            </button>

            {expanded && (
                <div className="mt-2 space-y-2">
                    {plain.headline && (
                        <p className="text-sm font-semibold leading-5 text-foreground">{plain.headline}</p>
                    )}
                    {plain.meaning && (
                        <p className="text-xs leading-5 text-foreground/90">{plain.meaning}</p>
                    )}
                    {/* caveat 弱化但不藏：它是这套检测的诚实边界。 */}
                    {plain.caveat && (
                        <p className="text-[11px] leading-5 text-muted-foreground">
                            <span className="mr-1 text-muted-foreground/70">{t('auditReportCaveat')}</span>
                            {plain.caveat}
                        </p>
                    )}
                    {plain.next_step && (
                        <p className="text-xs leading-5 text-foreground/80">
                            <span className="mr-1 text-muted-foreground">{t('auditReportNextStep')}</span>
                            {plain.next_step}
                        </p>
                    )}
                    {(metaParts.length > 0 || parsed.family) && (
                        <p className="text-[11px] text-muted-foreground">
                            {[...metaParts, parsed.family?.trim() ?? ''].filter(Boolean).join(' · ')}
                        </p>
                    )}

                    {findings.length > 0 && (
                        <ul className="space-y-2">
                            {findings.map((finding, index) => (
                                <FindingRow key={`${finding.probe ?? 'finding'}-${index}`} finding={finding} />
                            ))}
                        </ul>
                    )}

                    {/* 原始观测：每个探针测出的原始数据，不管有没有问题都列（证据，不是结论）。 */}
                    {results.length > 0 && (
                        <div className="rounded-lg border border-border bg-background/60 px-2.5 py-2">
                            <button
                                type="button"
                                onClick={() => setShowResults((v) => !v)}
                                aria-expanded={showResults}
                                className="flex w-full items-baseline justify-between gap-2 text-left"
                            >
                                <span className="text-[11px] font-medium text-foreground">
                                    {t('auditReportResultsTitle')}
                                </span>
                                <span className="flex shrink-0 items-center gap-1 text-[11px] text-muted-foreground">
                                    {t('auditReportResultCount', { count: results.length })}
                                    <ChevronDown className={cn('size-3 transition-transform', showResults && 'rotate-180')} />
                                </span>
                            </button>
                            {showResults && (
                                <ul className="mt-1.5 space-y-1.5">
                                    {results.map((result, index) => (
                                        <ResultRow key={`${result.probe_id ?? 'result'}-${index}`} result={result} />
                                    ))}
                                </ul>
                            )}
                        </div>
                    )}

                    {/* 探针没跑成不是"对方有问题"，用琥珀色当信息提示，不用红。 */}
                    {errors.length > 0 && (
                        <div className="rounded-lg border border-amber-500/40 bg-amber-500/5 px-2.5 py-2">
                            <p className="text-[11px] font-medium text-amber-700 dark:text-amber-300">
                                {t('auditReportErrorsTitle')}
                            </p>
                            <ul className="mt-1 space-y-0.5">
                                {errors.map((err, index) => (
                                    <li key={`${err.probe ?? 'error'}-${index}`} className="flex flex-wrap items-baseline gap-x-1.5 text-[11px]">
                                        <span className="shrink-0 font-mono text-muted-foreground">{err.probe ?? ''}</span>
                                        <span className="min-w-0 break-words text-foreground/80 [overflow-wrap:anywhere]">
                                            {err.error ?? ''}
                                        </span>
                                    </li>
                                ))}
                            </ul>
                        </div>
                    )}

                    {parsed.disclaimer && (
                        <p className="text-[10px] leading-4 text-muted-foreground/70">{parsed.disclaimer}</p>
                    )}
                </div>
            )}
        </div>
    );
}
