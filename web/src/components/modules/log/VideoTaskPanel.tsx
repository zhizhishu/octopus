'use client';

import { useState } from 'react';
import {
    AlertCircle,
    ChevronLeft,
    ChevronRight,
    Download,
    Loader2,
    Play,
    RefreshCw,
    Video,
} from 'lucide-react';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Dialog, DialogContent, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { Progress } from '@/components/ui/progress';
import { CopyIconButton } from '@/components/common/CopyButton';
import { cn } from '@/lib/utils';
import {
    VIDEO_TASK_STATUS_LABELS,
    useVideoTaskList,
    type VideoTaskItem,
    type VideoTaskStatus,
} from '@/api/endpoints/video-task';

/**
 * 视频任务视图：日志页的一个分类，不是独立页面。
 *
 * 形态照 new-api 的 usage-logs 任务分类（task-logs-columns / task-artifacts）：
 * 一条任务展示 task_id、模型、状态、进度、失败原因、结果链接、时间。
 * 结果本身走单独的预览弹窗（加载/失败/空三态），不把 <video> 直接塞进列表——
 * 一屏二十条任务同时拉二十个视频，等于让浏览器去下载二十个文件。
 */
const VIDEO_TASK_PAGE_SIZE = 20;

/** 状态徽标配色。success/failure 借语义色，进行中的三种用天蓝/琥珀区分「在排队」与「在生成」。 */
const STATUS_BADGE_CLASS: Record<VideoTaskStatus, string> = {
    not_start: 'border-border bg-muted text-muted-foreground',
    submitted: 'border-sky-500/40 bg-sky-500/10 text-sky-700 dark:text-sky-300',
    queued: 'border-amber-500/40 bg-amber-500/10 text-amber-700 dark:text-amber-300',
    in_progress: 'border-amber-500/40 bg-amber-500/10 text-amber-700 dark:text-amber-300',
    success: 'border-emerald-500/40 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300',
    failure: 'border-destructive/40 bg-destructive/10 text-destructive',
    unknown: 'border-border bg-muted text-muted-foreground',
};

const STATUS_FILTER_OPTIONS: Array<{ value: VideoTaskStatus | ''; label: string }> = [
    { value: '', label: '全部状态' },
    { value: 'in_progress', label: VIDEO_TASK_STATUS_LABELS.in_progress },
    { value: 'queued', label: VIDEO_TASK_STATUS_LABELS.queued },
    { value: 'submitted', label: VIDEO_TASK_STATUS_LABELS.submitted },
    { value: 'success', label: VIDEO_TASK_STATUS_LABELS.success },
    { value: 'failure', label: VIDEO_TASK_STATUS_LABELS.failure },
];

/** unix 秒 → 本地时间。0 表示上游还没给这个时间点，显示「—」而不是 1970 年。 */
function formatUnixSeconds(seconds: number): string {
    if (!seconds) return '—';
    const date = new Date(seconds * 1000);
    const pad = (n: number) => String(n).padStart(2, '0');
    return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
}

/** 完成耗时。两端都得有值才算得出来，否则就是「还不知道」，不拿当前时间假装。 */
function formatDuration(startSeconds: number, finishSeconds: number): string | undefined {
    if (!startSeconds || !finishSeconds || finishSeconds < startSeconds) return undefined;
    const seconds = finishSeconds - startSeconds;
    if (seconds < 60) return `${seconds} 秒`;
    const minutes = Math.floor(seconds / 60);
    if (minutes < 60) return `${minutes} 分 ${seconds % 60} 秒`;
    return `${Math.floor(minutes / 60)} 时 ${minutes % 60} 分`;
}

/** 进度是上游给的自由文本（"50%" / "generating" / 空）。能解析成百分比才画进度条。 */
function parseProgressPercent(progress: string): number | undefined {
    const match = progress.trim().match(/^(\d{1,3}(?:\.\d+)?)\s*%$/);
    if (!match) return undefined;
    const value = Number(match[1]);
    if (!Number.isFinite(value)) return undefined;
    return Math.min(100, Math.max(0, value));
}

function StatusBadge({ status }: { status: VideoTaskStatus }) {
    return (
        <Badge
            variant="outline"
            className={cn('shrink-0 rounded-md px-1.5 py-0.5 text-[11px] font-medium', STATUS_BADGE_CLASS[status])}
        >
            {VIDEO_TASK_STATUS_LABELS[status] ?? status}
        </Badge>
    );
}

/**
 * 结果预览：先给「正在拉取」的提示，失败给出错+重试。
 *
 * 只在 result_url 非空时才挂载（见 VideoTaskCard），所以这里不需要再判空；
 * 一个没有结果地址的任务不该出现预览按钮。
 */
function ResultPreviewDialog({
    task,
    open,
    onOpenChange,
}: {
    task: VideoTaskItem;
    open: boolean;
    onOpenChange: (open: boolean) => void;
}) {
    const [failed, setFailed] = useState(false);
    const [revision, setRevision] = useState(0);

    return (
        <Dialog open={open} onOpenChange={onOpenChange}>
            <DialogContent className="sm:max-w-3xl">
                <DialogHeader>
                    <DialogTitle className="truncate font-mono text-sm">{task.task_id}</DialogTitle>
                </DialogHeader>
                {failed ? (
                    <div className="flex flex-col items-start gap-2 rounded-lg border border-destructive/40 bg-destructive/5 p-3 text-sm">
                        <span className="flex items-center gap-1.5 text-destructive">
                            <AlertCircle className="size-4" /> 结果加载失败
                        </span>
                        <span className="text-xs text-muted-foreground">
                            结果地址可能已过期，或者上游拒绝了这个来源的访问。
                        </span>
                        <Button
                            variant="outline"
                            size="sm"
                            onClick={() => {
                                setFailed(false);
                                setRevision((value) => value + 1);
                            }}
                        >
                            <RefreshCw className="size-3.5" />
                            重试
                        </Button>
                    </div>
                ) : (
                    <video
                        key={revision}
                        src={task.result_url}
                        controls
                        preload="metadata"
                        className="max-h-[60vh] w-full rounded-md bg-black"
                        onError={() => setFailed(true)}
                    />
                )}
            </DialogContent>
        </Dialog>
    );
}

function VideoTaskCard({ task }: { task: VideoTaskItem }) {
    const [previewOpen, setPreviewOpen] = useState(false);
    const percent = parseProgressPercent(task.progress);
    const duration = formatDuration(task.start_time, task.finish_time);

    return (
        <div className="flex flex-col gap-2 rounded-xl border border-border bg-card p-3 shadow-sm">
            <div className="flex min-w-0 flex-wrap items-start gap-x-2 gap-y-1">
                <Video className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
                <span className="min-w-0 flex-1 truncate font-mono text-xs text-foreground" title={task.task_id}>
                    {task.task_id || '—'}
                </span>
                {task.task_id && <CopyIconButton text={task.task_id} />}
                <StatusBadge status={task.status} />
            </div>

            <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1 text-[11px] text-muted-foreground">
                {task.model && (
                    <span className="truncate font-mono" title={task.model}>
                        {task.model}
                    </span>
                )}
                <span className="tabular-nums" title="提交时间">
                    提交 {formatUnixSeconds(task.submit_time || task.created_at)}
                </span>
                <span className="tabular-nums" title="完成时间">
                    完成 {formatUnixSeconds(task.finish_time)}
                </span>
                {duration && <span className="tabular-nums">耗时 {duration}</span>}
            </div>

            {(percent !== undefined || task.progress) && (
                <div className="flex items-center gap-2">
                    {percent !== undefined ? (
                        <>
                            <Progress value={percent} className="h-1.5 flex-1" />
                            <span className="shrink-0 text-[11px] tabular-nums text-muted-foreground">{percent}%</span>
                        </>
                    ) : (
                        <span className="min-w-0 truncate text-[11px] text-muted-foreground" title={task.progress}>
                            进度：{task.progress}
                        </span>
                    )}
                </div>
            )}

            {task.fail_reason && (
                <p className="flex items-start gap-1.5 rounded-md bg-destructive/5 px-2 py-1.5 text-xs text-destructive">
                    <AlertCircle className="mt-0.5 size-3.5 shrink-0" />
                    <span className="min-w-0 break-words whitespace-pre-wrap">{task.fail_reason}</span>
                </p>
            )}

            {task.result_url && (
                <div className="flex flex-wrap items-center gap-1.5">
                    <Button
                        variant="outline"
                        size="sm"
                        className="h-7 rounded-md px-2 text-xs"
                        onClick={() => setPreviewOpen(true)}
                    >
                        <Play className="size-3.5" />
                        预览
                    </Button>
                    <Button variant="outline" size="sm" className="h-7 rounded-md px-2 text-xs" asChild>
                        <a href={task.result_url} download target="_blank" rel="noopener noreferrer">
                            <Download className="size-3.5" />
                            下载
                        </a>
                    </Button>
                    <ResultPreviewDialog task={task} open={previewOpen} onOpenChange={setPreviewOpen} />
                </div>
            )}
        </div>
    );
}

export function VideoTaskPanel() {
    const [page, setPage] = useState(1);
    const [status, setStatus] = useState<VideoTaskStatus | ''>('');
    const query = useVideoTaskList({ page, pageSize: VIDEO_TASK_PAGE_SIZE, status });
    const items = query.data?.items ?? [];
    const total = query.data?.total ?? 0;
    const totalPages = Math.max(1, Math.ceil(total / VIDEO_TASK_PAGE_SIZE));

    return (
        <div className="flex min-h-0 flex-1 flex-col gap-3">
            <div className="flex flex-none flex-wrap items-center gap-2 rounded-2xl border border-border bg-card p-3 shadow-sm sm:px-4">
                <select
                    aria-label="任务状态"
                    value={status}
                    onChange={(event) => {
                        setStatus(event.target.value as VideoTaskStatus | '');
                        setPage(1);
                    }}
                    className="h-8 min-w-0 rounded-lg border border-input bg-background px-2 text-xs text-foreground outline-none sm:px-3"
                >
                    {STATUS_FILTER_OPTIONS.map((option) => (
                        <option key={option.value || 'all'} value={option.value}>
                            {option.label}
                        </option>
                    ))}
                </select>
                <span className="text-xs text-muted-foreground">共 {total.toLocaleString()} 条</span>
                <span role="status" aria-live="polite" className="min-w-0 text-xs text-muted-foreground">
                    {query.isLoading ? '加载中…' : query.isFetching ? '更新中…' : ''}
                </span>
                <div className="ml-auto flex shrink-0 items-center gap-1.5">
                    <Button
                        variant="ghost"
                        size="icon"
                        onClick={() => void query.refetch()}
                        disabled={query.isFetching}
                        title="刷新"
                        aria-label="刷新视频任务"
                        className="h-8 w-8 rounded-lg border border-border bg-background text-muted-foreground hover:text-foreground dark:bg-card"
                    >
                        <RefreshCw className={cn('size-4', query.isFetching && 'animate-spin')} />
                    </Button>
                </div>
            </div>

            {query.isError && (
                <div
                    role="alert"
                    className="flex flex-none flex-wrap items-center justify-between gap-2 rounded-lg border border-destructive/40 bg-destructive/5 px-3 py-2 text-sm"
                >
                    <span>视频任务加载失败，请重试。</span>
                    <Button variant="outline" size="sm" onClick={() => void query.refetch()} disabled={query.isFetching}>
                        <RefreshCw className={cn('size-3.5', query.isFetching && 'animate-spin')} />
                        重试
                    </Button>
                </div>
            )}

            <div className="min-h-0 flex-1 overflow-y-auto pr-0.5" aria-busy={query.isFetching}>
                {query.isLoading ? (
                    <div className="flex flex-col gap-2" aria-label="加载中">
                        {[0, 1, 2].map((row) => (
                            <div key={row} className="h-20 animate-pulse rounded-xl border border-border bg-muted/40" />
                        ))}
                    </div>
                ) : items.length === 0 ? (
                    <div className="flex h-full min-h-40 flex-col items-center justify-center gap-1 rounded-xl border border-dashed border-border p-6 text-center">
                        <Video className="size-6 text-muted-foreground" />
                        <p className="text-sm font-medium text-foreground">
                            {status ? '该状态下没有视频任务' : '还没有视频任务'}
                        </p>
                        <p className="text-xs text-muted-foreground">
                            用视频生成接口（POST /v1/videos）发起任务后，这里会按时间倒序列出自己的任务和结果。
                        </p>
                    </div>
                ) : (
                    <div className="flex flex-col gap-2">
                        {items.map((task) => (
                            <VideoTaskCard key={task.task_id} task={task} />
                        ))}
                    </div>
                )}
            </div>

            {totalPages > 1 && (
                <div className="flex flex-none items-center gap-2 border-t border-border/60 pt-2">
                    <Button
                        variant="outline"
                        size="sm"
                        onClick={() => setPage((value) => Math.max(1, value - 1))}
                        disabled={page <= 1 || query.isFetching}
                        className="h-8 rounded-lg px-2"
                        aria-label="上一页"
                    >
                        <ChevronLeft className="size-3.5" />
                        上一页
                    </Button>
                    <span className="text-xs tabular-nums text-muted-foreground">
                        第 {page} / {totalPages} 页
                    </span>
                    <Button
                        variant="outline"
                        size="sm"
                        onClick={() => setPage((value) => Math.min(totalPages, value + 1))}
                        disabled={page >= totalPages || query.isFetching}
                        className="h-8 rounded-lg px-2"
                        aria-label="下一页"
                    >
                        下一页
                        <ChevronRight className="size-3.5" />
                    </Button>
                    {query.isFetching && <Loader2 className="size-3.5 animate-spin text-muted-foreground" />}
                </div>
            )}
        </div>
    );
}
