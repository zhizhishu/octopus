'use client';

import { useState } from 'react';
import { useTranslations } from 'next-intl';
import {
    CheckCircle2,
    ChevronDown,
    ChevronRight,
    CircleDashed,
    HelpCircle,
    Hash,
    Loader2,
    Square,
    XCircle,
} from 'lucide-react';
import { Button } from '@/components/ui/button';
import { cn } from '@/lib/utils';
import { historyBucket, verdictHeadline, formatReportTime } from '@/lib/model-audit-verdict';
import {
    useAuditJobs,
    useAuditJobDetail,
    useCancelAuditJob,
    type AuditJob,
    type AuditResult,
    type AuditVerdict,
} from '@/api/endpoints/model-audit';
import {
    hlwyComparabilityLabel,
    hlwyExecutionLabel,
    hlwyInFlightNote,
    hlwyJobBucket,
    hlwyJobHeadline,
    hlwyJobLedger,
    hlwyProgressPercent,
    hlwySkippedText,
    isHlwyJob,
    readHlwyComparison,
    readStoredReport,
} from './hlwy';

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

/**
 * 「可疑」只看多探针结论。基准对照不参与风险计分，把它算进可疑等于凭空造了一个结论。
 */
function jobIsSuspect(job: AuditJob): boolean {
    if (isHlwyJob(job)) return false;
    return (
        job.single_verdict === 'medium' ||
        job.single_verdict === 'high' ||
        (job.verdict_counts?.medium ?? 0) + (job.verdict_counts?.high ?? 0) > 0
    );
}

/**
 * 「没测成」对基准对照的另一条口径：只有真失败算。
 *
 * 旧的判据是 `single_verdict === 'unknown'`——对照结论本来就不是风险档位，
 * 套上去会把一次跑满 100 个样本、报告齐全的对照显示成「没测成」。
 * 取消/超时/中断也不算没测成：它们保留了已有结果，只是提前停了。
 */
function jobIsFailed(job: AuditJob): boolean {
    if (isHlwyJob(job)) return hlwyJobBucket(job) === 'failed';
    return (
        job.single_status === 'failed' ||
        job.single_verdict === 'unknown' ||
        (job.verdict_counts?.failed ?? 0) > 0
    );
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
    const [filter, setFilter] = useState<'all' | 'suspect' | 'failed'>('all');
    const [query, setQuery] = useState('');

    const suspectN = (jobs ?? []).filter((j) => jobIsSuspect(j)).length;
    const failedN = (jobs ?? []).filter((j) => jobIsFailed(j)).length;

    const filtered = (jobs ?? []).filter((job) => {
        const q = query.trim().toLowerCase();
        if (q) {
            const hay = `${job.channel_name} ${job.single_model ?? ''}`.toLowerCase();
            if (!hay.includes(q)) return false;
        }
        if (filter === 'all') return true;
        if (filter === 'suspect') return jobIsSuspect(job);
        return jobIsFailed(job);
    });

    return (
        <div className="min-w-0 space-y-4">
            <div className="rounded-3xl border border-border bg-card p-4 sm:p-5">
                <div className="flex flex-wrap items-center gap-2">
                    {([
                        ['all', `全部`],
                        ['suspect', `可疑 ${suspectN}`],
                        ['failed', `没测成 ${failedN}`],
                    ] as const).map(([key, label]) => (
                        <button
                            key={key}
                            type="button"
                            onClick={() => setFilter(key)}
                            className={cn(
                                'rounded-full px-3 py-1 text-xs',
                                filter === key ? 'bg-primary text-primary-foreground' : 'bg-muted text-muted-foreground',
                            )}
                        >
                            {label}
                        </button>
                    ))}
                    <input
                        value={query}
                        onChange={(e) => setQuery(e.target.value)}
                        placeholder="搜模型或渠道"
                        className="ml-auto h-8 min-w-[140px] rounded-lg border border-border bg-background px-2.5 text-xs"
                    />
                </div>

                {filtered.length === 0 ? (
                    <div className="mt-3 rounded-2xl border border-dashed border-border/70 bg-muted/20 p-6 text-center text-sm text-muted-foreground">
                        {t('queue.empty')}
                    </div>
                ) : (
                    <div className="mt-3 space-y-2">
                        {filtered.map((job) => (
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

function batchSummary(job: AuditJob): string {
    const c = job.verdict_counts ?? {};
    const ok = (c.none ?? 0) + (c.low ?? 0);
    const warn = (c.medium ?? 0) + (c.high ?? 0);
    const unknown = c.unknown ?? 0;
    const failed = c.failed ?? 0;
    const parts: string[] = [];
    if (ok) parts.push(`${ok} 个没问题`);
    if (warn) parts.push(`${warn} 个可疑`);
    if (unknown) parts.push(`${unknown} 个数据不够`);
    if (failed) parts.push(`${failed} 个没测成`);
    if (parts.length === 0) return `${job.channel_name} · ${job.total} 个模型`;
    return `${job.channel_name} · ${job.total} 个模型 · ${parts.join('，')}`;
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
    const hlwy = isHlwyJob(job);
    // 对照任务的进度是「落定槽位 / 计划上限」，不是「已发起 / 计划上限」，也不是
    // 「几个模型跑完了」：一个模型跑完不等于 100 个样本到齐。用已发起当分子，
    // 最后一个样本还在飞的时候进度条就停在 100%，看起来像跑完了却没有结果。
    const hlwyPct = hlwy ? hlwyProgressPercent(job) : null;
    const pct = hlwy
        ? (hlwyPct ?? 0)
        : job.total > 0
          ? Math.round((job.finished / job.total) * 100)
          : 0;
    const ledger = hlwy ? hlwyJobLedger(job) : '';
    // 还有样本没回：说「正在收尾」，不把它当成 100%。
    const inFlight = hlwy ? hlwyInFlightNote(job) : '';
    const bucket = hlwy ? hlwyJobBucket(job) : null;

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
                        {hlwy && (
                            <span className="rounded-full bg-primary/10 px-1.5 py-0.5 text-[10px] font-medium text-primary">
                                基准对照
                            </span>
                        )}
                        {active && <Loader2 className="size-3 animate-spin text-primary" />}
                    </div>
                    <div className="mt-0.5 truncate text-[11px] text-muted-foreground">
                        {hlwy
                            ? `${job.single_model || '本轮模型'} · ${hlwyJobHeadline(job)}${inFlight ? ` · ${inFlight}` : ''}${ledger ? ` · ${ledger}` : ''}`
                            : active
                              ? jobProgressText(job, t)
                              : job.single_model
                                ? `${job.single_model} · ${job.plain_headline || ''}`
                                : job.total > 1
                                  ? batchSummary(job)
                                  : jobProgressText(job, t)}
                        {' · '}
                        {formatTime(job.report_at || job.created_at)}
                    </div>
                </div>
                {/* 已取消的任务不画进度条：0% 的空轨在卡片上就是一个来历不明的灰块，
                    看起来像没渲染完的占位符。状态文案（已取消）已经在副标题里。 */}
                {job.status !== 'canceled' && (
                    <div className="w-24 shrink-0">
                        <div className="h-1.5 overflow-hidden rounded-full bg-background">
                            <div
                                className={cn(
                                    'h-full rounded-full transition-all',
                                    bucket === 'failed'
                                        ? 'bg-destructive/70'
                                        : bucket === 'stopped' || bucket === 'insufficient'
                                          ? 'bg-amber-500'
                                          : active
                                            ? 'bg-primary'
                                            : 'bg-emerald-500',
                                )}
                                style={{ width: `${pct}%` }}
                            />
                        </div>
                    </div>
                )}
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

            {open && <JobResults jobID={job.id} hlwy={hlwy} jobStatus={job.status} onOpenReport={onOpenReport} />}
        </div>
    );
}

function JobResults({
    jobID,
    hlwy,
    jobStatus,
    onOpenReport,
}: {
    jobID: number;
    hlwy?: boolean;
    jobStatus?: string;
    onOpenReport?: (result: AuditResult) => void;
}) {
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
                <ResultRow key={r.id} result={r} hlwy={hlwy} jobStatus={jobStatus} onOpenReport={onOpenReport} />
            ))}
        </div>
    );
}

function ResultRow({
    result,
    hlwy,
    jobStatus,
    onOpenReport,
}: {
    result: AuditResult;
    hlwy?: boolean;
    jobStatus?: string;
    onOpenReport?: (result: AuditResult) => void;
}) {
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
                          : // 任务被取消但这一次已经发出去了：说清楚它还在收尾，
                            // 否则用户会以为取消以后还会再扣一次钱。
                            hlwy && jobStatus === 'canceled'
                            ? '任务已停，这一次在收尾'
                            : t('queue.resultRunning')}
                </span>
            </div>
        );
    }

    if (result.status === 'failed' || result.status === 'skipped') {
        // 失败行也可能带着报告：探针真的打出去了、也真的计过费，只是全部没拿到
        // 有效响应。后端现在会把这份"每条探针为什么失败"的结构化结果一起存下来，
        // 所以这里要留一个入口——只显示一句"失败"等于把排错线索扔掉。
        const hasReport = (result.report ?? '').trim() !== '';
        return (
            <div className="rounded-xl bg-background/50 px-2.5 py-2 text-xs">
                <div className="flex items-center gap-2">
                    <XCircle className="size-3 shrink-0 text-muted-foreground" />
                    <span className="min-w-0 flex-1 truncate font-mono">{result.model}</span>
                    <span className="shrink-0 text-muted-foreground">
                        {hlwy
                            ? result.status === 'skipped'
                                ? // 被跳过的行不一定没开始：取消时已经发出去的那一次
                                  // 也标成 skipped，但它花过钱、也有部分结果。
                                  hlwySkippedText(result.report)
                                : '失败'
                            : result.status === 'skipped'
                              ? t('queue.resultSkipped')
                              : t('queue.resultFailed')}
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
                        {hlwy ? '看已采到的部分' : t('queue.failureDetail')}
                    </button>
                )}
            </div>
        );
    }

    // 基准对照的完成行：没有风险档位，也不该套用「证据不足」那套标题。
    // 状态、相似度、采样账目从报告里读，读不出来就说读不出来。
    if (hlwy) {
        const read = readStoredReport(result.report || '');
        const view = read.ok ? readHlwyComparison(read.view) : null;
        const headline = view
            ? hlwyExecutionLabel(view.execution)
            : result.status === 'done'
              ? '对照完成（细节读不出）'
              : '结果未完成';
        // 计划数是上限：写「最多 N 次调用」，不把它读成「一定有 100 个有效样本」。
        const ledger = view?.counts
            ? `最多 ${view.counts.planned_calls} 次调用 · 有效 ${view.counts.valid} · 未确认 ${view.counts.unresolved}`
            : '';
        const scoreText = view && view.hasScore && view.chosen?.percent ? view.chosen.percent : '';
        return (
            <div className="rounded-xl bg-background/50 px-2.5 py-2">
                <div className="flex w-full items-center gap-2">
                    <button
                        type="button"
                        onClick={() => setExpanded(!expanded)}
                        className="flex min-w-0 flex-1 items-center gap-2 text-left"
                    >
                        <span className="flex size-5 shrink-0 items-center justify-center rounded-full bg-muted">
                            <Hash className="size-3 text-muted-foreground" />
                        </span>
                        <span className="min-w-0 flex-1 truncate font-mono text-xs text-card-foreground">{result.model}</span>
                        <span className="shrink-0 text-[11px] font-medium text-muted-foreground">{headline}</span>
                        {expanded ? <ChevronDown className="size-3 shrink-0" /> : <ChevronRight className="size-3 shrink-0" />}
                    </button>
                    {onOpenReport && (
                        <button
                            type="button"
                            onClick={() => onOpenReport(result)}
                            className="shrink-0 text-[11px] text-muted-foreground underline-offset-2 hover:underline"
                            title="查看完整对照报告"
                        >
                            {t('queue.openReport')}
                        </button>
                    )}
                </div>
                {expanded && (
                    <div className="mt-2 space-y-1 border-t border-border/40 pt-2 pl-7 text-[11px] leading-5">
                        {scoreText && <p className="text-card-foreground">与主对照的数字分布相似度 {scoreText}</p>}
                        {ledger && <p className="text-muted-foreground">采样账目：{ledger}</p>}
                        {view && <p className="text-muted-foreground">{hlwyComparabilityLabel(view.comparability)}</p>}
                        {!read.ok && <p className="text-muted-foreground">{read.error}</p>}
                        {view?.problem && <p className="text-muted-foreground">报告只能部分读取：{view.problem}</p>}
                        <p className="text-muted-foreground/70">时长 {formatDuration(result.duration_ms) || '-'}</p>
                    </div>
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
