'use client';

import { useState } from 'react';
import { useTranslations } from 'next-intl';
import {
    CheckCircle2,
    ChevronDown,
    ChevronRight,
    CircleDashed,
    HelpCircle,
    Loader2,
    Square,
    XCircle,
} from 'lucide-react';
import { Button } from '@/components/ui/button';
import { cn } from '@/lib/utils';
import {
    useAuditJobs,
    useAuditJobDetail,
    useCancelAuditJob,
    type AuditJob,
    type AuditResult,
    type AuditVerdict,
} from '@/api/endpoints/model-audit';

/**
 * 检测记录页：只看历史，不再承担提交。
 *
 * 这里以前顶上还挂着一块「批量检测」提交框（选渠道、粘模型、开始排队），
 * 跟「发起检测」那一页是同一个动作的第二个入口——同一个动作给两个入口，
 * 用户只会疑惑"到底点哪个"，两边校验规则还得各维护一份。
 * 现在提交统一归「发起检测」，这一页只留记录：查询、展开、看报告、取消。
 *
 * 为什么记录要单独存一份：检测是花钱买来的结论。以前结论写回被检测的那行
 * 日志，日志一清理就再也找不回来。
 */

/** 档位 → 视觉与图标。颜色只做辅助，文字始终是主要载体（色盲可用）。 */
const VERDICT_STYLE: Record<
    AuditVerdict,
    { icon: typeof CheckCircle2; tone: string; ring: string }
> = {
    none: { icon: CheckCircle2, tone: 'text-emerald-600 dark:text-emerald-400', ring: 'bg-emerald-500/10' },
    low: { icon: CircleDashed, tone: 'text-amber-600 dark:text-amber-400', ring: 'bg-amber-500/10' },
    medium: { icon: CircleDashed, tone: 'text-orange-600 dark:text-orange-400', ring: 'bg-orange-500/10' },
    high: { icon: XCircle, tone: 'text-rose-600 dark:text-rose-400', ring: 'bg-rose-500/10' },
    unknown: { icon: HelpCircle, tone: 'text-muted-foreground', ring: 'bg-muted' },
};

function verdictStyle(v: string) {
    return VERDICT_STYLE[(v as AuditVerdict) ?? 'unknown'] ?? VERDICT_STYLE.unknown;
}

function formatTime(iso?: string): string {
    if (!iso) return '';
    const d = new Date(iso);
    if (Number.isNaN(d.getTime())) return '';
    return d.toLocaleString();
}

function formatDuration(ms: number): string {
    if (!ms || ms < 0) return '';
    if (ms < 1000) return `${ms}ms`;
    const s = Math.round(ms / 1000);
    if (s < 60) return `${s}s`;
    return `${Math.floor(s / 60)}m${s % 60}s`;
}

/** 任务状态 → 进度条文案。 */
function jobProgressText(job: AuditJob, t: (k: string, v?: Record<string, string | number>) => string): string {
    switch (job.status) {
        case 'queued':
            return t('queue.statusQueued');
        case 'running':
            return t('queue.statusRunning', { done: job.finished, total: job.total });
        case 'canceled':
            return t('queue.statusCanceled');
        default:
            return t('queue.statusDone', { done: job.finished, total: job.total });
    }
}

export function ModelAuditQueue({ onOpenReport }: { onOpenReport?: (result: AuditResult) => void } = {}) {
    const t = useTranslations('modelAudit');
    const { data: jobs } = useAuditJobs(50);
    const cancelJob = useCancelAuditJob();

    const [openJobID, setOpenJobID] = useState<number | null>(null);

    return (
        <div className="min-w-0 space-y-4">
            {/* 任务列表 */}
            <div className="rounded-3xl border border-border bg-card p-4 sm:p-5">
                <h2 className="text-base font-semibold text-card-foreground">{t('queue.historyTitle')}</h2>
                <p className="mt-1 text-xs leading-5 text-muted-foreground">{t('queue.historyDescription')}</p>

                {(jobs ?? []).length === 0 ? (
                    <div className="mt-3 rounded-2xl border border-dashed border-border/70 bg-muted/20 p-6 text-center text-sm text-muted-foreground">
                        {t('queue.empty')}
                    </div>
                ) : (
                    <div className="mt-3 space-y-2">
                        {(jobs ?? []).map((job) => (
                            <JobRow
                                key={job.id}
                                job={job}
                                open={openJobID === job.id}
                                onToggle={() => setOpenJobID(openJobID === job.id ? null : job.id)}
                                onCancel={() => cancelJob.mutate(job.id)}
                                canceling={cancelJob.isPending && cancelJob.variables === job.id}
                                onOpenReport={onOpenReport}
                            />
                        ))}
                    </div>
                )}
            </div>
        </div>
    );
}

function JobRow({
    job,
    open,
    onToggle,
    onCancel,
    canceling,
    onOpenReport,
}: {
    job: AuditJob;
    open: boolean;
    onToggle: () => void;
    onCancel: () => void;
    canceling: boolean;
    onOpenReport?: (result: AuditResult) => void;
}) {
    const t = useTranslations('modelAudit');
    const active = job.status === 'queued' || job.status === 'running';
    const pct = job.total > 0 ? Math.round((job.finished / job.total) * 100) : 0;

    return (
        <div className="rounded-2xl border border-border/70 bg-muted/20">
            <button
                type="button"
                onClick={onToggle}
                className="flex w-full min-w-0 items-center gap-2 p-3 text-left"
            >
                {open ? <ChevronDown className="size-4 shrink-0" /> : <ChevronRight className="size-4 shrink-0" />}
                <div className="min-w-0 flex-1">
                    <div className="flex flex-wrap items-center gap-1.5">
                        <span className="truncate text-sm font-medium text-card-foreground">{job.channel_name}</span>
                        <span className="rounded bg-background px-1.5 py-0.5 text-[10px] text-muted-foreground">
                            #{job.id}
                        </span>
                        {active && <Loader2 className="size-3 animate-spin text-primary" />}
                    </div>
                    <div className="mt-0.5 truncate text-[11px] text-muted-foreground">
                        {jobProgressText(job, t)} · {formatTime(job.created_at)}
                    </div>
                </div>
                <div className="w-24 shrink-0">
                    <div className="h-1.5 overflow-hidden rounded-full bg-background">
                        <div
                            className={cn('h-full rounded-full transition-all', active ? 'bg-primary' : 'bg-emerald-500')}
                            style={{ width: `${pct}%` }}
                        />
                    </div>
                </div>
            </button>

            {active && (
                <div className="flex justify-end px-3 pb-2">
                    <Button
                        variant="ghost"
                        size="sm"
                        onClick={onCancel}
                        disabled={canceling}
                        className="h-7 rounded-lg text-[11px] text-muted-foreground hover:text-destructive"
                    >
                        <Square className="size-3" />
                        {t('queue.cancel')}
                    </Button>
                </div>
            )}

            {open && <JobResults jobID={job.id} onOpenReport={onOpenReport} />}
        </div>
    );
}

function JobResults({ jobID, onOpenReport }: { jobID: number; onOpenReport?: (result: AuditResult) => void }) {
    const t = useTranslations('modelAudit');
    const { data, isLoading } = useAuditJobDetail(jobID);

    if (isLoading) {
        return (
            <div className="flex items-center gap-2 border-t border-border/60 px-3 py-3 text-xs text-muted-foreground">
                <Loader2 className="size-3 animate-spin" />
                {t('queue.loading')}
            </div>
        );
    }
    const results = data?.results ?? [];
    if (results.length === 0) {
        return <div className="border-t border-border/60 px-3 py-3 text-xs text-muted-foreground">{t('queue.noResults')}</div>;
    }

    return (
        <div className="space-y-1.5 border-t border-border/60 p-3">
            {results.map((r) => (
                <ResultRow key={r.id} result={r} onOpenReport={onOpenReport} />
            ))}
        </div>
    );
}

function ResultRow({ result, onOpenReport }: { result: AuditResult; onOpenReport?: (result: AuditResult) => void }) {
    const t = useTranslations('modelAudit');
    const [expanded, setExpanded] = useState(false);

    if (result.status === 'pending' || result.status === 'running') {
        // 重试中的行仍是 pending，但它和"排在别人后面等着"是两回事：
        // 前者已经花过钱、失败过、正在退避等待。不区分的话，用户看到进度条
        // 好几分钟不动，只会以为卡死了，甚至重复提交一遍（等于再花一次钱）。
        const retrying = result.status === 'pending' && !!result.next_retry_at;
        return (
            <div className="flex items-center gap-2 rounded-xl bg-background/50 px-2.5 py-2 text-xs text-muted-foreground">
                <Loader2 className="size-3 shrink-0 animate-spin" />
                <span className="min-w-0 flex-1 truncate font-mono">{result.model}</span>
                <span className="shrink-0">
                    {retrying
                        ? t('queue.resultRetrying', { attempt: result.attempt ?? 1 })
                        : result.status === 'pending'
                          ? t('queue.resultPending')
                          : t('queue.resultRunning')}
                </span>
            </div>
        );
    }

    if (result.status === 'failed' || result.status === 'skipped') {
        // 失败行也可能带着报告：探针真的打出去了、也真的计过费，只是全部没拿到
        // 有效响应。后端现在会把这份"每条探针为什么失败"的结构化结果一起存下来，
        // 所以这里要留一个入口——只显示一句"没跑成"等于把排错线索扔掉。
        const hasReport = (result.report ?? '').trim() !== '';
        return (
            <div className="rounded-xl bg-background/50 px-2.5 py-2 text-xs">
                <div className="flex items-center gap-2">
                    <XCircle className="size-3 shrink-0 text-muted-foreground" />
                    <span className="min-w-0 flex-1 truncate font-mono">{result.model}</span>
                    <span className="shrink-0 text-muted-foreground">
                        {result.status === 'skipped' ? t('queue.resultSkipped') : t('queue.resultFailed')}
                    </span>
                </div>
                {result.error_msg && (
                    <p className="mt-1 pl-5 text-[11px] leading-4 text-muted-foreground">{result.error_msg}</p>
                )}
                {hasReport && onOpenReport && (
                    <button
                        type="button"
                        onClick={() => onOpenReport(result)}
                        className="mt-1 pl-5 text-[11px] font-medium text-primary underline-offset-2 hover:underline"
                    >
                        {t('queue.failureDetail')}
                    </button>
                )}
            </div>
        );
    }

    // 已完成：解析报告拿人话结论。报告是后端存的 JSON 原文。
    let plain: { headline: string; meaning: string; caveat: string; next_step: string } | undefined;
    let family = '';
    try {
        const report = JSON.parse(result.report || '{}') as {
            plain?: { headline: string; meaning: string; caveat: string; next_step: string };
            family?: string;
        };
        plain = report.plain;
        family = report.family ?? '';
    } catch {
        plain = undefined;
    }

    const style = verdictStyle(result.verdict);
    const Icon = style.icon;

    return (
        <div className="rounded-xl bg-background/50 px-2.5 py-2">
            <div className="flex w-full items-center gap-2">
                <button
                    type="button"
                    onClick={() => setExpanded(!expanded)}
                    className="flex min-w-0 flex-1 items-center gap-2 text-left"
                >
                    <span className={cn('flex size-5 shrink-0 items-center justify-center rounded-full', style.ring)}>
                        <Icon className={cn('size-3', style.tone)} />
                    </span>
                    <span className="min-w-0 flex-1 truncate font-mono text-xs text-card-foreground">{result.model}</span>
                    {family && (
                        <span className="shrink-0 rounded bg-muted px-1.5 py-0.5 text-[10px] text-muted-foreground">
                            {family}
                        </span>
                    )}
                    <span className={cn('shrink-0 text-[11px] font-medium', style.tone)}>
                        {plain?.headline ?? result.verdict}
                    </span>
                    {expanded ? <ChevronDown className="size-3 shrink-0" /> : <ChevronRight className="size-3 shrink-0" />}
                </button>
                {/* 展开只是摘要；要看完整报告（每条探针的明细、可导出 JSON）走这里。
                    报告存在结果行的 report 字段里，不受日志保留期影响。 */}
                {onOpenReport && (
                    <button
                        type="button"
                        onClick={() => onOpenReport(result)}
                        className="shrink-0 text-[11px] text-muted-foreground underline-offset-2 hover:underline"
                        title="查看完整报告"
                    >
                        {t('queue.openReport')}
                    </button>
                )}
            </div>

            {expanded && plain && (
                <div className="mt-2 space-y-1.5 border-t border-border/40 pt-2 pl-7 text-[11px] leading-5">
                    <p className="text-card-foreground">{plain.meaning}</p>
                    {/* caveat 必须原样展示：它说明这个结论不能证明什么，家族差异就在这里。 */}
                    <p className="rounded-lg bg-muted/50 px-2 py-1 text-muted-foreground">
                        <span className="font-medium">{t('queue.caveatLabel')}</span>
                        {plain.caveat}
                    </p>
                    <p className="text-muted-foreground">
                        <span className="font-medium">{t('queue.nextStepLabel')}</span>
                        {plain.next_step}
                    </p>
                    <p className="text-muted-foreground/70">
                        {t('queue.meta', {
                            score: result.score,
                            duration: formatDuration(result.duration_ms) || '-',
                        })}
                    </p>
                </div>
            )}
        </div>
    );
}
