'use client';

import { useEffect, useState } from 'react';
import {
    AlertCircle,
    ChevronLeft,
    ChevronRight,
    Download,
    Image as ImageIcon,
    Loader2,
    Play,
    RefreshCw,
    Video,
} from 'lucide-react';
import { useTranslations } from 'next-intl';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Dialog, DialogContent, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { Progress } from '@/components/ui/progress';
import { CopyIconButton } from '@/components/common/CopyButton';
import { cn } from '@/lib/utils';
import {
    useMediaTaskList,
    type MediaTaskItem,
    type MediaTaskKind,
} from '@/api/endpoints/media-task';

/**
 * 「图片/视频任务」合并视图（fix-ui1127a）：日志页的第三个标签。
 *
 * 形态照 VideoTaskPanel（本身照 new-api usage-logs 的 task 分类）：一条任务展示
 * task_id、模型、状态、进度、失败原因、结果链接、时间，词表不新造。类型筛选
 * （全部/图片/视频）+ 状态 + 模型 + 时间都在后端 SQL 层过滤，分页对合并后的整列成立。
 */
const MEDIA_TASK_PAGE_SIZE = 20;

/** 状态徽标配色：视频词表 + 图片词表共用语义色，不造新词。 */
const STATUS_BADGE_CLASS: Record<string, string> = {
    not_start: 'border-border bg-muted text-muted-foreground',
    submitted: 'border-sky-500/40 bg-sky-500/10 text-sky-700 dark:text-sky-300',
    queued: 'border-amber-500/40 bg-amber-500/10 text-amber-700 dark:text-amber-300',
    in_progress: 'border-amber-500/40 bg-amber-500/10 text-amber-700 dark:text-amber-300',
    processing: 'border-amber-500/40 bg-amber-500/10 text-amber-700 dark:text-amber-300',
    success: 'border-emerald-500/40 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300',
    completed: 'border-emerald-500/40 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300',
    failure: 'border-destructive/40 bg-destructive/10 text-destructive',
    failed: 'border-destructive/40 bg-destructive/10 text-destructive',
    unknown: 'border-border bg-muted text-muted-foreground',
};

const STATUS_LABEL_KEYS: Record<string, string> = {
    not_start: 'statusNotStart',
    submitted: 'statusSubmitted',
    queued: 'statusQueued',
    in_progress: 'statusInProgress',
    processing: 'statusProcessing',
    success: 'statusSuccess',
    completed: 'statusCompleted',
    failure: 'statusFailure',
    failed: 'statusFailed',
    unknown: 'statusUnknown',
};

function formatUnixSeconds(seconds: number): string {
    if (!seconds) return '—';
    const date = new Date(seconds * 1000);
    const pad = (n: number) => String(n).padStart(2, '0');
    return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
}

function formatDuration(startSeconds: number, finishSeconds: number): string | undefined {
    if (!startSeconds || !finishSeconds || finishSeconds < startSeconds) return undefined;
    const seconds = finishSeconds - startSeconds;
    if (seconds < 60) return `${seconds} 秒`;
    const minutes = Math.floor(seconds / 60);
    if (minutes < 60) return `${minutes} 分 ${seconds % 60} 秒`;
    return `${Math.floor(minutes / 60)} 时 ${minutes % 60} 分`;
}

function parseProgressPercent(progress: string): number | undefined {
    const match = progress.trim().match(/^(\d{1,3}(?:\.\d+)?)\s*%$/);
    if (!match) return undefined;
    const value = Number(match[1]);
    if (!Number.isFinite(value)) return undefined;
    return Math.min(100, Math.max(0, value));
}

function StatusBadge({ status }: { status: string }) {
    const t = useTranslations('log.mediaTasks');
    const key = STATUS_LABEL_KEYS[status];
    return (
        <Badge
            variant="outline"
            className={cn('shrink-0 rounded-md px-1.5 py-0.5 text-[11px] font-medium', STATUS_BADGE_CLASS[status] ?? STATUS_BADGE_CLASS.unknown)}
        >
            {key ? t(key as never) : status}
        </Badge>
    );
}

function ResultPreviewDialog({
    task,
    open,
    onOpenChange,
}: {
    task: MediaTaskItem;
    open: boolean;
    onOpenChange: (open: boolean) => void;
}) {
    const t = useTranslations('log.mediaTasks');
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
                            <AlertCircle className="size-4" /> {t('resultLoadFailed')}
                        </span>
                        <span className="text-xs text-muted-foreground">{t('resultLoadFailedHint')}</span>
                        <Button
                            variant="outline"
                            size="sm"
                            onClick={() => {
                                setFailed(false);
                                setRevision((value) => value + 1);
                            }}
                        >
                            <RefreshCw className="size-3.5" />
                            {t('retry')}
                        </Button>
                    </div>
                ) : task.kind === 'image' ? (
                    // eslint-disable-next-line @next/next/no-img-element
                    <img
                        key={revision}
                        src={task.result_url}
                        alt={task.task_id}
                        className="max-h-[60vh] w-full rounded-md object-contain bg-black"
                        onError={() => setFailed(true)}
                    />
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

function MediaTaskCard({ task }: { task: MediaTaskItem }) {
    const t = useTranslations('log.mediaTasks');
    const [previewOpen, setPreviewOpen] = useState(false);
    const percent = parseProgressPercent(task.progress);
    const duration = formatDuration(task.start_time, task.finish_time);
    const KindIcon = task.kind === 'image' ? ImageIcon : Video;

    return (
        <div className="flex flex-col gap-2 rounded-xl border border-border bg-card p-3 shadow-sm">
            <div className="flex min-w-0 flex-wrap items-start gap-x-2 gap-y-1">
                <KindIcon className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
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
                <span className="tabular-nums" title={t('submitTime')}>
                    {t('submit')} {formatUnixSeconds(task.submit_time || task.created_at)}
                </span>
                <span className="tabular-nums" title={t('finishTime')}>
                    {t('finish')} {formatUnixSeconds(task.finish_time)}
                </span>
                {duration && <span className="tabular-nums">{t('duration')} {duration}</span>}
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
                            {t('progress')}：{task.progress}
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
                        {t('preview')}
                    </Button>
                    <Button variant="outline" size="sm" className="h-7 rounded-md px-2 text-xs" asChild>
                        <a href={task.result_url} download target="_blank" rel="noopener noreferrer">
                            <Download className="size-3.5" />
                            {t('download')}
                        </a>
                    </Button>
                    <ResultPreviewDialog task={task} open={previewOpen} onOpenChange={setPreviewOpen} />
                </div>
            )}
        </div>
    );
}

export function MediaTaskPanel() {
    const t = useTranslations('log.mediaTasks');
    const [page, setPage] = useState(1);
    const [kind, setKind] = useState<MediaTaskKind | ''>('');
    const [status, setStatus] = useState('');
    const [model, setModel] = useState('');
    const [date, setDate] = useState('');
    const startTime = date ? Math.floor(new Date(`${date}T00:00:00`).getTime() / 1000) : 0;
    const endTime = date ? Math.floor(new Date(`${date}T23:59:59`).getTime() / 1000) : 0;
    const query = useMediaTaskList({ page, pageSize: MEDIA_TASK_PAGE_SIZE, kind, status, model, startTime, endTime });
    const items = query.data?.items ?? [];
    const total = query.data?.total ?? 0;
    const totalPages = Math.max(1, Math.ceil(total / MEDIA_TASK_PAGE_SIZE));

    // 筛选/轮询让总条数缩到本页之外时，把页码收回最后一页（口径同 VideoTaskPanel）。
    useEffect(() => {
        if (!query.data || query.isPlaceholderData) return;
        if (page > totalPages) setPage(totalPages);
    }, [page, totalPages, query.data, query.isPlaceholderData]);

    const resetPage = () => setPage(1);

    return (
        <div className="flex min-h-0 flex-1 flex-col gap-3">
            <div className="flex flex-none flex-wrap items-center gap-2 rounded-2xl border border-border bg-card p-3 shadow-sm sm:px-4">
                <select
                    aria-label={t('typeFilter')}
                    value={kind}
                    onChange={(event) => {
                        setKind(event.target.value as MediaTaskKind | '');
                        resetPage();
                    }}
                    className="h-8 min-w-0 rounded-lg border border-input bg-background px-2 text-xs text-foreground outline-none sm:px-3"
                >
                    <option value="">{t('typeAll')}</option>
                    <option value="image">{t('typeImage')}</option>
                    <option value="video">{t('typeVideo')}</option>
                </select>
                <select
                    aria-label={t('statusFilter')}
                    value={status}
                    onChange={(event) => {
                        setStatus(event.target.value);
                        resetPage();
                    }}
                    className="h-8 min-w-0 rounded-lg border border-input bg-background px-2 text-xs text-foreground outline-none sm:px-3"
                >
                    <option value="">{t('statusAll')}</option>
                    {(['processing', 'in_progress', 'queued', 'submitted', 'completed', 'success', 'failed', 'failure', 'unknown'] as const).map((value) => (
                        <option key={value} value={value}>
                            {t(STATUS_LABEL_KEYS[value] as never)}
                        </option>
                    ))}
                </select>
                <input
                    type="text"
                    value={model}
                    onChange={(event) => {
                        setModel(event.target.value);
                        resetPage();
                    }}
                    placeholder={t('modelFilter')}
                    aria-label={t('modelFilter')}
                    className="h-8 w-32 min-w-0 rounded-lg border border-input bg-background px-2 text-xs text-foreground outline-none placeholder:text-muted-foreground"
                />
                <input
                    type="date"
                    value={date}
                    onChange={(event) => {
                        setDate(event.target.value);
                        resetPage();
                    }}
                    aria-label={t('dateFilter')}
                    className="h-8 min-w-0 rounded-lg border border-input bg-background px-2 text-xs text-foreground outline-none"
                />
                <span className="text-xs text-muted-foreground">{t('total', { count: total })}</span>
                <span role="status" aria-live="polite" className="min-w-0 text-xs text-muted-foreground">
                    {query.isLoading ? t('loading') : query.isFetching ? t('updating') : ''}
                </span>
                <div className="ml-auto flex shrink-0 items-center gap-1.5">
                    <Button
                        variant="ghost"
                        size="icon"
                        onClick={() => void query.refetch()}
                        disabled={query.isFetching}
                        title={t('refresh')}
                        aria-label={t('refresh')}
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
                    <span>{t('loadFailed')}</span>
                    <Button variant="outline" size="sm" onClick={() => void query.refetch()} disabled={query.isFetching}>
                        <RefreshCw className={cn('size-3.5', query.isFetching && 'animate-spin')} />
                        {t('retry')}
                    </Button>
                </div>
            )}

            <div className="min-h-0 flex-1 overflow-y-auto pr-0.5" aria-busy={query.isFetching}>
                {query.isLoading ? (
                    <div className="flex flex-col gap-2" aria-label={t('loading')}>
                        {[0, 1, 2].map((row) => (
                            <div key={row} className="h-20 animate-pulse rounded-xl border border-border bg-muted/40" />
                        ))}
                    </div>
                ) : items.length === 0 ? (
                    <div className="flex h-full min-h-40 flex-col items-center justify-center gap-1 rounded-xl border border-dashed border-border p-6 text-center">
                        <ImageIcon className="size-6 text-muted-foreground" />
                        <p className="text-sm font-medium text-foreground">
                            {status || kind || model || date ? t('emptyFiltered') : t('empty')}
                        </p>
                        <p className="text-xs text-muted-foreground">{t('emptyHint')}</p>
                    </div>
                ) : (
                    <div className="flex flex-col gap-2">
                        {items.map((task) => (
                            <MediaTaskCard key={`${task.kind}-${task.task_id}`} task={task} />
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
                        aria-label={t('prevPage')}
                    >
                        <ChevronLeft className="size-3.5" />
                        {t('prevPage')}
                    </Button>
                    <span className="text-xs tabular-nums text-muted-foreground">
                        {t('pageOf', { page, totalPages })}
                    </span>
                    <Button
                        variant="outline"
                        size="sm"
                        onClick={() => setPage((value) => Math.min(totalPages, value + 1))}
                        disabled={page >= totalPages || query.isFetching}
                        className="h-8 rounded-lg px-2"
                        aria-label={t('nextPage')}
                    >
                        {t('nextPage')}
                        <ChevronRight className="size-3.5" />
                    </Button>
                    {query.isFetching && <Loader2 className="size-3.5 animate-spin text-muted-foreground" />}
                </div>
            )}
        </div>
    );
}
