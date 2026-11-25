'use client';

import { Loader2, Square } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { cn } from '@/lib/utils';
import { useAuditJobs, useCancelAuditJob, type AuditJob, type AuditResult } from '@/api/endpoints/model-audit';
import { ModelAuditQueue } from './Queue';
import { isHlwyJob } from './hlwy';
import { queueOrder, taskBlocks, type BlockState } from './v6';

/**
 * 「任务」页：上面是进行中的任务卡片（总进度、每一项一个小方块、取消），
 * 下面是记录表（筛选、看报告、单条删、勾选批量删、清空已完成）。
 *
 * 进度只读后端：多探针任务的小方块来自 worker 在内存里记的实时进度（job.live），
 * 对照任务看采样账目（hlwy_summary）。读不到就写「等待」，不自己编百分比。
 */

const BLOCK_STYLE: Record<BlockState, string> = {
    done: 'bg-primary/10 text-foreground',
    running: 'bg-sky-500/10 ring-1 ring-sky-500/40 text-foreground',
    waiting: 'bg-muted/60 text-muted-foreground',
    failed: 'bg-destructive/10 text-destructive',
};

function clock(iso?: string): string {
    if (!iso) return '';
    const d = new Date(iso);
    if (Number.isNaN(d.getTime())) return '';
    return d.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit', hour12: false });
}

function jobTitle(job: AuditJob): string {
    const model = job.live?.model || job.single_model || (job.total > 1 ? `${job.total} 个模型` : '');
    return [job.channel_name || '渠道', model].filter(Boolean).join(' · ');
}

function jobRequests(job: AuditJob): { sent: number; planned: number } | null {
    if (isHlwyJob(job)) {
        const counts = job.hlwy_summary?.counts;
        return counts ? { sent: counts.started_slots, planned: counts.planned_calls } : null;
    }
    if (typeof job.planned_requests === 'number' && job.planned_requests > 0) {
        return { sent: job.sent_requests ?? 0, planned: job.planned_requests };
    }
    return null;
}

function RunningCard({
    job,
    position,
    onCancel,
    canceling,
}: {
    job: AuditJob;
    position: number;
    onCancel: () => void;
    canceling: boolean;
}) {
    const blocks = taskBlocks(job);
    const req = jobRequests(job);
    const pct = req && req.planned > 0
        ? Math.min(100, Math.round((req.sent / req.planned) * 100))
        : job.total > 0 ? Math.round((job.finished / job.total) * 100) : 0;
    const queued = position > 0;
    const preset = isHlwyJob(job) ? '数字习惯' : job.preset;
    return (
        <section className="rounded-3xl border border-border bg-card p-4 sm:p-5">
            <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                {!queued && <Loader2 className="size-4 animate-spin text-primary" />}
                <h4 className="min-w-0 truncate text-sm font-semibold">{jobTitle(job)}</h4>
                <span className="text-xs text-muted-foreground">
                    {[preset, queued ? `${clock(job.created_at)} 排队` : `${clock(job.started_at || job.created_at)} 开始`, job.trigger === 'schedule' ? '定时' : '手动']
                        .filter(Boolean)
                        .join(' · ')}
                </span>
                <span className="ml-auto text-xs text-muted-foreground">
                    {queued
                        ? `排第 ${position}，前面的跑完就开始`
                        : [
                              req ? `${req.sent} / ${req.planned} 次` : '',
                              job.total > 1 ? `第 ${Math.min(job.finished + 1, job.total)} / ${job.total} 个模型` : '',
                          ]
                              .filter(Boolean)
                              .join(' · ')}
                </span>
                <Button
                    variant="outline"
                    size="sm"
                    className="h-7 rounded-lg text-xs"
                    onClick={onCancel}
                    disabled={canceling}
                >
                    <Square className="size-3" />
                    取消
                </Button>
            </div>
            <div className="mt-3 h-1.5 overflow-hidden rounded-full bg-muted" role="progressbar" aria-valuenow={pct} aria-valuemin={0} aria-valuemax={100}>
                <div className="h-full rounded-full bg-primary transition-all" style={{ width: `${pct}%` }} />
            </div>
            {blocks.length > 0 && (
                <div className="mt-3 grid grid-cols-2 gap-2 sm:grid-cols-3 lg:grid-cols-4">
                    {blocks.map((block) => (
                        <div key={block.id} className={cn('rounded-xl px-3 py-2', BLOCK_STYLE[block.state])}>
                            <p className="truncate text-xs font-semibold">{block.label}</p>
                            <p className="mt-0.5 truncate text-[11px] tabular-nums opacity-80">{block.detail}</p>
                        </div>
                    ))}
                </div>
            )}
        </section>
    );
}

export function TasksView({
    onOpenReport,
    focusJobId = null,
}: {
    onOpenReport?: (result: AuditResult) => void;
    focusJobId?: number | null;
}) {
    const { data: jobs } = useAuditJobs(50);
    const cancelJob = useCancelAuditJob();
    const running = queueOrder(jobs ?? []);
    return (
        <div className="min-w-0 space-y-4">
            <div className="flex flex-wrap items-baseline justify-between gap-2 px-1">
                <h3 className="text-base font-semibold">进行中 · {running.length}</h3>
                <p className="text-xs text-muted-foreground">后台在跑，关掉页面不影响；跑完到下面「记录」里看报告。</p>
            </div>
            {running.length === 0 ? (
                <div className="rounded-3xl border border-dashed border-border/70 bg-muted/20 p-6 text-center text-sm text-muted-foreground">
                    现在没有在跑的检测。去「检测」选好模型点开始，进度会出现在这里。
                </div>
            ) : (
                running.map(({ job, position }) => (
                    <RunningCard
                        key={job.id}
                        job={job}
                        position={position}
                        onCancel={() => cancelJob.mutate(job.id)}
                        canceling={cancelJob.isPending && cancelJob.variables === job.id}
                    />
                ))
            )}
            <ModelAuditQueue key={focusJobId ?? 'all'} onOpenReport={onOpenReport} hideActive focusJobId={focusJobId} />
        </div>
    );
}
