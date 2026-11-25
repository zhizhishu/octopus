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
    Trash2,
    XCircle,
} from 'lucide-react';
import { Button } from '@/components/ui/button';
import { toast } from '@/components/common/Toast';
import { cn } from '@/lib/utils';
import {
    useAuditJobs,
    useAuditJobDetail,
    useCancelAuditJob,
    useClearFinishedAuditJobs,
    useDeleteAuditJob,
    useDeleteAuditJobs,
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

/** 任务是否还在花钱：排队中、进行中。这样的任务不能勾选删除。 */
function jobIsActive(job: AuditJob): boolean {
    return job.status === 'queued' || job.status === 'running';
}

/** 「有问题」= 可疑或没测成（对照任务只有真失败才算）。 */
function jobHasProblem(job: AuditJob): boolean {
    return jobIsSuspect(job) || jobIsFailed(job);
}

function triggerText(trigger: string): string {
    if (trigger === 'schedule') return '定时';
    if (trigger === 'manual' || trigger === '') return '手动';
    if (trigger === 'echo_mismatch') return '日志跟查';
    return trigger;
}

function spentText(job: AuditJob): string {
    if (isHlwyJob(job)) {
        const started = job.hlwy_summary?.counts?.started_slots;
        return typeof started === 'number' ? `${started} 次` : '';
    }
    return typeof job.sent_requests === 'number' && job.sent_requests > 0 ? `约 ${job.sent_requests} 次` : '';
}

function errorText(error: unknown): string {
    return error instanceof Error ? error.message : String(error);
}

export function ModelAuditQueue({
    onOpenReport,
    hideActive = false,
    focusJobId = null,
}: {
    onOpenReport?: (result: AuditResult) => void;
    /** 任务页上方已经有「进行中」卡片时，记录表只列已经停下的。 */
    hideActive?: boolean;
    /** 从「最近检测」点进来时自动展开那一条（换一条时调用方用 key 让它重新挂载）。 */
    focusJobId?: number | null;
} = {}) {
    const t = useTranslations('modelAudit');
    const { data: jobs } = useAuditJobs(50);
    const cancelJob = useCancelAuditJob();
    const deleteJob = useDeleteAuditJob();
    const deleteJobs = useDeleteAuditJobs();
    const clearFinished = useClearFinishedAuditJobs();

    const [openJobID, setOpenJobID] = useState<number | null>(focusJobId);
    const [filter, setFilter] = useState<'all' | 'problem' | 'schedule'>('all');
    const [query, setQuery] = useState('');
    const [checked, setChecked] = useState<number[]>([]);
    // 二次确认：批量删 / 清空 / 单条删 都先停一下，删了就找不回来。
    const [confirm, setConfirm] = useState<null | 'batch' | 'clear' | number>(null);

    const all = (jobs ?? []).filter((job) => !hideActive || !jobIsActive(job));
    const problemN = all.filter((j) => jobHasProblem(j)).length;
    const scheduleN = all.filter((j) => j.trigger === 'schedule').length;

    const filtered = all.filter((job) => {
        const q = query.trim().toLowerCase();
        if (q) {
            const hay = `${job.channel_name} ${job.single_model ?? ''}`.toLowerCase();
            if (!hay.includes(q)) return false;
        }
        if (filter === 'all') return true;
        if (filter === 'problem') return jobHasProblem(job);
        return job.trigger === 'schedule';
    });
    const deletable = filtered.filter((job) => !jobIsActive(job));
    const checkedVisible = checked.filter((id) => deletable.some((job) => job.id === id));
    const allChecked = deletable.length > 0 && checkedVisible.length === deletable.length;

    const toggleChecked = (id: number, on: boolean) =>
        setChecked((current) => (on ? [...new Set([...current, id])] : current.filter((item) => item !== id)));

    const reportSkipped = (skipped: number[]) => {
        if (skipped.length > 0) toast.warning(`${skipped.length} 条还在跑，没删；等它停下再删`);
    };

    const runBatchDelete = () => {
        const ids = checkedVisible;
        setConfirm(null);
        if (ids.length === 0) return;
        deleteJobs.mutate(ids, {
            onSuccess: (res) => {
                toast.success(`已删除 ${res.deleted} 条记录`);
                reportSkipped(res.skipped_busy ?? []);
                setChecked([]);
            },
            onError: (error) => toast.error('删除失败', { description: errorText(error) }),
        });
    };

    const runClear = () => {
        setConfirm(null);
        clearFinished.mutate(undefined, {
            onSuccess: (res) => {
                toast.success(res.deleted > 0 ? `已清空 ${res.deleted} 条已完成的记录` : '没有可以清的记录');
                setChecked([]);
            },
            onError: (error) => toast.error('清空失败', { description: errorText(error) }),
        });
    };

    const runDeleteOne = (id: number) => {
        setConfirm(null);
        deleteJob.mutate(id, {
            onSuccess: () => {
                toast.success('已删除这条记录');
                setChecked((current) => current.filter((item) => item !== id));
                if (openJobID === id) setOpenJobID(null);
            },
            onError: (error) => toast.error('删除失败', { description: errorText(error) }),
        });
    };

    const busy = deleteJob.isPending || deleteJobs.isPending || clearFinished.isPending;

    return (
        <div className="min-w-0 space-y-4">
            <div className="rounded-3xl border border-border bg-card p-4 sm:p-5">
                <div className="flex flex-wrap items-center gap-2">
                    <h3 className="mr-1 text-sm font-semibold">记录</h3>
                    {([
                        ['all', `全部`],
                        ['problem', `有问题 ${problemN}`],
                        ['schedule', `定时 ${scheduleN}`],
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
                        className="h-8 min-w-[140px] flex-1 rounded-lg border border-border bg-background px-2.5 text-xs sm:ml-auto sm:flex-none"
                    />
                </div>

                <div className="mt-3 flex flex-wrap items-center gap-2 text-xs">
                    <label className="flex items-center gap-1.5 text-muted-foreground">
                        <input
                            type="checkbox"
                            aria-label="全选这一页能删的记录"
                            checked={allChecked}
                            disabled={deletable.length === 0}
                            onChange={(e) => setChecked(e.target.checked ? deletable.map((job) => job.id) : [])}
                        />
                        全选
                    </label>
                    <Button
                        variant="ghost"
                        size="sm"
                        className="h-7 rounded-lg text-[11px]"
                        disabled={checkedVisible.length === 0 || busy}
                        onClick={() => setConfirm('batch')}
                    >
                        <Trash2 className="size-3" />
                        删除选中的 {checkedVisible.length} 条
                    </Button>
                    <Button
                        variant="ghost"
                        size="sm"
                        className="ml-auto h-7 rounded-lg text-[11px] text-muted-foreground hover:text-destructive"
                        disabled={busy || all.every((job) => jobIsActive(job))}
                        onClick={() => setConfirm('clear')}
                    >
                        清空已完成记录
                    </Button>
                </div>

                {(confirm === 'batch' || confirm === 'clear') && (
                    <div role="alertdialog" className="mt-2 flex flex-wrap items-center gap-2 rounded-xl bg-destructive/10 px-3 py-2 text-xs text-destructive">
                        <span className="min-w-0 flex-1">
                            {confirm === 'batch'
                                ? `确定删掉选中的 ${checkedVisible.length} 条记录？报告会一起删掉，找不回来。`
                                : '确定清空所有已完成的检测记录？报告会一起删掉，找不回来；进行中、排队中的不受影响。'}
                        </span>
                        <Button size="sm" variant="destructive" className="h-7 rounded-lg text-[11px]" onClick={confirm === 'batch' ? runBatchDelete : runClear}>
                            {confirm === 'batch' ? '确定删除' : '确定清空'}
                        </Button>
                        <Button size="sm" variant="ghost" className="h-7 rounded-lg text-[11px]" onClick={() => setConfirm(null)}>
                            算了
                        </Button>
                    </div>
                )}

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
                                checked={checked.includes(job.id)}
                                onCheck={(on) => toggleChecked(job.id, on)}
                                confirmingDelete={confirm === job.id}
                                onAskDelete={() => setConfirm(job.id)}
                                onConfirmDelete={() => runDeleteOne(job.id)}
                                onCancelDelete={() => setConfirm(null)}
                                deleting={busy}
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
    checked = false,
    onCheck,
    confirmingDelete = false,
    onAskDelete,
    onConfirmDelete,
    onCancelDelete,
    deleting = false,
}: {
    job: AuditJob;
    open: boolean;
    onToggle: () => void;
    onCancel: () => void;
    canceling: boolean;
    onOpenReport?: (result: AuditResult) => void;
    checked?: boolean;
    onCheck?: (on: boolean) => void;
    confirmingDelete?: boolean;
    onAskDelete?: () => void;
    onConfirmDelete?: () => void;
    onCancelDelete?: () => void;
    deleting?: boolean;
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
            <div className="flex min-w-0 items-center">
            {onCheck && (
                <input
                    type="checkbox"
                    className="ml-3 shrink-0"
                    aria-label={`选中 ${job.channel_name} #${job.id}`}
                    checked={checked}
                    // 还在跑的不能删：勾不上，免得「删选中的」悄悄跳过它。
                    disabled={active}
                    onChange={(e) => onCheck(e.target.checked)}
                />
            )}
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
                                数字习惯对照
                            </span>
                        )}
                        {active && <Loader2 className="size-3 animate-spin text-primary" />}
                        {!hlwy && job.preset && (
                            <span className="rounded bg-background px-1.5 py-0.5 text-[10px] text-muted-foreground">{job.preset}</span>
                        )}
                        <span className="text-[10px] text-muted-foreground">{triggerText(job.trigger ?? '')}</span>
                        {spentText(job) && <span className="text-[10px] text-muted-foreground">· 花了{spentText(job)}</span>}
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
            {onAskDelete && !active && (
                <button
                    type="button"
                    onClick={onAskDelete}
                    disabled={deleting}
                    aria-label={`删除 ${job.channel_name} #${job.id}`}
                    title="删除这条记录"
                    className="mr-2 shrink-0 rounded-lg p-1.5 text-muted-foreground hover:bg-destructive/10 hover:text-destructive disabled:opacity-40"
                >
                    <Trash2 className="size-3.5" />
                </button>
            )}
            </div>

            {confirmingDelete && (
                <div role="alertdialog" className="mx-3 mb-2 flex flex-wrap items-center gap-2 rounded-xl bg-destructive/10 px-3 py-2 text-xs text-destructive">
                    <span className="min-w-0 flex-1">删掉这条记录？报告会一起删掉，找不回来。</span>
                    <Button size="sm" variant="destructive" className="h-7 rounded-lg text-[11px]" onClick={onConfirmDelete}>
                        删除
                    </Button>
                    <Button size="sm" variant="ghost" className="h-7 rounded-lg text-[11px]" onClick={onCancelDelete}>
                        算了
                    </Button>
                </div>
            )}

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
