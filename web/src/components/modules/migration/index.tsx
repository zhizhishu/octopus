'use client';

import { useState } from 'react';
import { AlertTriangle, Ban, CheckCircle2, Clock3, Database, Loader2, RefreshCw, XCircle } from 'lucide-react';
import { type MigrationJob, type MigrationSummary, useMigrationJob, useMigrationJobs } from '@/api/endpoints/migration';
import { PageWrapper } from '@/components/common/PageWrapper';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Progress } from '@/components/ui/progress';
import { cn } from '@/lib/utils';

const STAGES = [
    { id: 'scan_users', label: '扫描用户' },
    { id: 'scan_logs', label: '扫描日志' },
    { id: 'dry_run', label: '试运行报告' },
    { id: 'import', label: '正式导入' },
    { id: 'complete', label: '完成' },
] as const;

const statusLabels: Record<MigrationJob['status'], string> = {
    queued: '排队中',
    running: '运行中',
    succeeded: '已完成',
    failed: '失败',
    canceled: '已取消',
};

function isActive(job?: MigrationJob | null) {
    return job?.status === 'queued' || job?.status === 'running';
}

function formatDate(value?: string) {
    if (!value) return '-';
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return value;
    return date.toLocaleString();
}

function formatNumber(value?: number) {
    return new Intl.NumberFormat().format(value ?? 0);
}

function formatCost(value?: number) {
    return (value ?? 0).toFixed(6).replace(/\.?0+$/, '') || '0';
}

function SummaryMetric({ label, value, tone = 'default' }: { label: string; value: string | number; tone?: 'default' | 'good' | 'warn' }) {
    return (
        <div className={cn(
            'rounded-2xl border border-border bg-background px-4 py-3',
            tone === 'good' && 'border-emerald-500/30 bg-emerald-500/5',
            tone === 'warn' && 'border-amber-500/30 bg-amber-500/5'
        )}>
            <div className="text-xs text-muted-foreground">{label}</div>
            <div className="mt-1 text-xl font-semibold tabular-nums">{value}</div>
        </div>
    );
}

function StatusBadge({ job }: { job?: MigrationJob | null }) {
    if (!job) return <Badge variant="outline">未开始</Badge>;
    if (job.status === 'queued') return <Badge variant="outline"><Clock3 className="size-3" />排队中</Badge>;
    if (job.status === 'succeeded') {
        return <Badge variant="secondary" className="bg-emerald-500/15 text-emerald-700 dark:text-emerald-300"><CheckCircle2 className="size-3" />已完成</Badge>;
    }
    if (job.status === 'failed') {
        return <Badge variant="destructive"><XCircle className="size-3" />失败</Badge>;
    }
    if (job.status === 'canceled') {
        return <Badge variant="outline"><Ban className="size-3" />已取消</Badge>;
    }
    return <Badge variant="outline"><Loader2 className="size-3 animate-spin" />运行中</Badge>;
}

function StageTimeline({ job }: { job?: MigrationJob | null }) {
    const currentIndex = Math.max(0, STAGES.findIndex((stage) => stage.id === job?.stage));
    return (
        <div className="grid gap-3 sm:grid-cols-5">
            {STAGES.map((stage, index) => {
                const done = Boolean(job) && (job?.status === 'succeeded' || (isActive(job) && index < currentIndex));
                const current = job?.stage === stage.id && isActive(job);
                return (
                    <div key={stage.id} className="min-w-0">
                        <div className={cn(
                            'flex h-10 items-center gap-2 rounded-2xl border px-3 text-sm',
                            done && 'border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300',
                            current && 'border-primary/40 bg-primary/10 text-primary',
                            !done && !current && 'border-border bg-background text-muted-foreground'
                        )}>
                            {done ? <CheckCircle2 className="size-4 shrink-0" /> : current ? <Loader2 className="size-4 shrink-0 animate-spin" /> : <Clock3 className="size-4 shrink-0" />}
                            <span className="truncate">{stage.label}</span>
                        </div>
                    </div>
                );
            })}
        </div>
    );
}

function ReportSummary({ summary }: { summary?: MigrationSummary }) {
    if (!summary) {
        return (
            <div className="rounded-2xl border border-dashed border-border px-4 py-10 text-center text-sm text-muted-foreground">
                后台尚未生成报告。任务完成后，这里会显示后台记录的用户、余额和用量。
            </div>
        );
    }

    return (
        <div className="grid gap-4">
            <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
                <SummaryMetric label="源用户" value={formatNumber(summary.source_users)} />
                <SummaryMetric label="活跃用户" value={formatNumber(summary.active_users)} tone="good" />
                <SummaryMetric label="跳过注册机" value={formatNumber(summary.inactive_users_skipped)} tone="warn" />
                <SummaryMetric label="额度单位" value={`${formatNumber(summary.quota_per_unit)} (${summary.source_reference})`} />
                <SummaryMetric label="将创建用户" value={formatNumber(summary.users_created)} />
                <SummaryMetric label="合并用户" value={formatNumber(summary.users_merged)} />
                <SummaryMetric label="冲突跳过" value={formatNumber(summary.users_skipped_conflict)} />
                <SummaryMetric label="重命名用户" value={formatNumber(summary.users_renamed)} />
                <SummaryMetric label="旧密钥导入" value={summary.included_api_keys ? formatNumber(summary.api_keys_created) : '关闭'} />
                <SummaryMetric label="详细日志导入" value={summary.included_logs ? formatNumber(summary.logs_created) : '关闭'} />
            </div>

            <div className="grid gap-3 md:grid-cols-3">
                <SummaryMetric label="导入余额" value={formatCost(summary.imported_balance)} />
                <SummaryMetric label="历史已用额度" value={formatCost(summary.imported_used_quota)} />
                <SummaryMetric label="实时统计回灌" value={summary.stats_updated ? '已回灌' : '不回灌'} />
            </div>

            {summary.warnings?.length ? (
                <div className="rounded-2xl border border-amber-500/30 bg-amber-500/5 p-4 text-sm text-amber-800 dark:text-amber-200">
                    <div className="mb-2 flex items-center gap-2 font-medium">
                        <AlertTriangle className="size-4" />
                        迁移提示
                    </div>
                    <ul className="grid gap-1">
                        {summary.warnings.map((warning) => <li key={warning}>- {warning}</li>)}
                    </ul>
                </div>
            ) : null}
        </div>
    );
}

export function Migration() {
    const [activeJobID, setActiveJobID] = useState<string>();
    const jobsQuery = useMigrationJobs();
    const selectedID = activeJobID ?? jobsQuery.data?.[0]?.id;
    const activeFromList = jobsQuery.data?.find((job) => job.id === selectedID);
    const activeJobQuery = useMigrationJob(selectedID, isActive(activeFromList));
    const activeJob = activeJobQuery.data ?? activeFromList ?? null;
    const refreshing = jobsQuery.isFetching || activeJobQuery.isFetching;
    const failedToRead = jobsQuery.isError || activeJobQuery.isError;
    const refresh = () => {
        void jobsQuery.refetch();
        if (selectedID) void activeJobQuery.refetch();
    };

    return (
        <PageWrapper className="h-full min-h-0 overflow-y-auto overscroll-contain space-y-5 pb-24 md:pb-4 rounded-t-3xl">
            <section className="flex min-w-0 flex-wrap items-start justify-between gap-3 rounded-3xl border border-border bg-card p-5">
                <div className="min-w-0 flex-1">
                    <h3 className="flex items-center gap-2 text-base font-semibold"><Database className="size-5 shrink-0" />迁移结果</h3>
                    <p className="mt-2 text-sm leading-relaxed text-muted-foreground">
                        迁移任务由后台运维流程创建和执行。本页只显示后台结果与状态，不接收源库凭据，不启动、取消或重试迁移。
                    </p>
                </div>
                <Button type="button" variant="outline" onClick={refresh} disabled={refreshing}
                    aria-busy={refreshing} className="h-11 min-w-11 shrink-0 rounded-xl">
                    <RefreshCw className={cn('size-4', refreshing && 'animate-spin')} />刷新结果
                </Button>
            </section>
            {failedToRead && (
                <div role="alert" className="rounded-2xl border border-destructive/30 bg-destructive/5 p-4 text-sm text-destructive">
                    读取后台迁移结果失败。已加载的记录保留显示，可通过「刷新结果」重新读取；不会重新执行任务。
                </div>
            )}
            <section className="min-w-0 space-y-4 rounded-3xl border border-border bg-card p-5" aria-label="后台迁移任务状态">
                <div className="flex min-w-0 flex-wrap items-center justify-between gap-3">
                    <div className="min-w-0">
                        <h3 className="text-base font-semibold">后台任务</h3>
                        <p className="mt-1 break-all text-xs text-muted-foreground">
                            {activeJob ? `${activeJob.id} · ${activeJob.apply ? '正式导入' : '试运行'}` : jobsQuery.isLoading ? '读取中…' : '暂无任务'}
                        </p>
                    </div>
                    <StatusBadge job={activeJob} />
                </div>
                <Progress value={activeJob?.percent ?? 0} className="h-2.5" />
                <div className="flex min-w-0 flex-wrap items-start justify-between gap-3 text-xs text-muted-foreground" aria-live="polite">
                    <span className="min-w-0 flex-1 break-words [overflow-wrap:anywhere]">{activeJob?.message || '等待后台任务结果'}</span>
                    <span className="shrink-0 tabular-nums">{activeJob?.percent ?? 0}%</span>
                </div>
                <StageTimeline job={activeJob} />
                {activeJob?.error && (
                    <div role="status" className="break-words rounded-2xl border border-destructive/30 bg-destructive/5 p-4 text-sm text-destructive [overflow-wrap:anywhere]">
                        {activeJob.error}
                    </div>
                )}
            </section>
            <section className="min-w-0 space-y-4 rounded-3xl border border-border bg-card p-5">
                <div className="flex flex-wrap items-center justify-between gap-3">
                    <h3 className="text-base font-semibold">迁移报告</h3>
                    {activeJob && (
                        <div className="flex min-w-0 flex-wrap gap-2">
                            <Badge variant="outline">{activeJob.apply ? '执行结果' : '试运行结果'}</Badge>
                            <Badge variant="outline">{activeJob.request.include_logs ? '含日志' : '不含日志'}</Badge>
                            <Badge variant="outline">{activeJob.request.include_api_keys ? '含密钥' : '不含密钥'}</Badge>
                        </div>
                    )}
                </div>
                <ReportSummary summary={activeJob?.summary} />
            </section>
            <section className="min-w-0 space-y-3 rounded-3xl border border-border bg-card p-5" aria-label="最近迁移任务">
                <h3 className="text-base font-semibold">最近任务</h3>
                {(jobsQuery.data ?? []).length === 0 ? (
                    <p className="py-6 text-sm text-muted-foreground">{jobsQuery.isLoading ? '正在读取后台任务…' : '暂无迁移任务'}</p>
                ) : (
                    <div className="grid min-w-0 gap-2">
                        {(jobsQuery.data ?? []).map((job) => (
                            <button key={job.id} type="button" onClick={() => setActiveJobID(job.id)}
                                aria-pressed={job.id === selectedID}
                                className={cn('grid min-h-11 min-w-0 gap-2 rounded-2xl border border-border p-3 text-left text-sm transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring sm:grid-cols-[minmax(0,1fr)_auto]',
                                    job.id === selectedID ? 'bg-primary/5 border-primary/30' : 'bg-background hover:bg-muted/60')}>
                                <span className="min-w-0 break-all font-mono text-xs">{job.id}</span>
                                <span className="min-w-0 break-words">{job.apply ? '正式导入' : '试运行'} · {statusLabels[job.status]}</span>
                                <span className="min-w-0 break-words text-xs text-muted-foreground">活跃用户 {formatNumber(job.summary?.active_users)}</span>
                                <span className="min-w-0 break-words text-xs text-muted-foreground">{formatDate(job.updated_at)}</span>
                            </button>
                        ))}
                    </div>
                )}
            </section>
        </PageWrapper>
    );
}
