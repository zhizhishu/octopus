'use client';

import { useMemo, useState } from 'react';
import {
    AlertTriangle,
    Ban,
    CheckCircle2,
    Clock3,
    Database,
    FileSearch,
    HardDrive,
    Loader2,
    Play,
    RefreshCw,
    ShieldCheck,
    XCircle,
} from 'lucide-react';
import type { ApiError } from '@/api/types';
import {
    type NewAPIMigrationJob,
    type NewAPIMigrationSummary,
    type StartNewAPIMigrationJobRequest,
    useCancelNewAPIMigrationJob,
    useNewAPIMigrationJob,
    useNewAPIMigrationJobs,
    useStartNewAPIMigrationJob,
} from '@/api/endpoints/migration';
import { PageWrapper } from '@/components/common/PageWrapper';
import { toast } from '@/components/common/Toast';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Progress } from '@/components/ui/progress';
import {
    Table,
    TableBody,
    TableCell,
    TableHead,
    TableHeader,
    TableRow,
} from '@/components/ui/table';
import { cn } from '@/lib/utils';

const STAGES = [
    { id: 'scan_users', label: '扫描用户' },
    { id: 'scan_logs', label: '扫描日志' },
    { id: 'dry_run', label: 'dry-run 报告' },
    { id: 'import', label: '正式导入' },
    { id: 'complete', label: '完成' },
] as const;

const statusLabels: Record<NewAPIMigrationJob['status'], string> = {
    queued: '排队中',
    running: '运行中',
    succeeded: '已完成',
    failed: '失败',
    canceled: '已取消',
};

function apiErrorMessage(error: unknown): string | undefined {
    return (error as ApiError | undefined)?.message;
}

function isActive(job?: NewAPIMigrationJob | null) {
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

function StatusBadge({ job }: { job?: NewAPIMigrationJob | null }) {
    if (!job) return <Badge variant="outline">未开始</Badge>;
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

function StageTimeline({ job }: { job?: NewAPIMigrationJob | null }) {
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

function ReportSummary({ summary }: { summary?: NewAPIMigrationSummary }) {
    if (!summary) {
        return (
            <div className="rounded-2xl border border-dashed border-border px-4 py-10 text-center text-sm text-muted-foreground">
                先运行 dry-run，完成后这里会出现可导入用户、余额和用量摘要。
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
    const [sourceType, setSourceType] = useState('sqlite');
    const [sourceDSN, setSourceDSN] = useState('/app/data/new-api.db');
    const [sourceLogType, setSourceLogType] = useState('');
    const [sourceLogDSN, setSourceLogDSN] = useState('');
    const [preserveAdminRole, setPreserveAdminRole] = useState(false);
    const [batchSize, setBatchSize] = useState(500);
    const [quotaPerUnit, setQuotaPerUnit] = useState('');
    const [conflictStrategy, setConflictStrategy] = useState<'skip' | 'merge' | 'rename'>('skip');
    const [passwordMode, setPasswordMode] = useState<'preserve' | 'random' | 'disabled'>('preserve');
    const [confirmText, setConfirmText] = useState('');
    const [activeJobID, setActiveJobID] = useState<string>();

    const jobsQuery = useNewAPIMigrationJobs();
    const activeFromList = jobsQuery.data?.find((job) => job.id === activeJobID);
    const activePolling = isActive(activeFromList);
    const activeJobQuery = useNewAPIMigrationJob(activeJobID, activePolling);
    const startJob = useStartNewAPIMigrationJob();
    const cancelJob = useCancelNewAPIMigrationJob();

    const activeJob = activeJobQuery.data ?? activeFromList ?? null;
    const running = isActive(activeJob) || startJob.isPending;
    const dryRunForApply = activeJob?.status === 'succeeded' && activeJob.summary?.dry_run ? activeJob : null;
    const canApply = Boolean(dryRunForApply?.can_apply && !running && confirmText.trim() === 'IMPORT');

    const payloadBase = useMemo<StartNewAPIMigrationJobRequest>(() => ({
        source_type: sourceType,
        source_dsn: sourceDSN.trim(),
        source_log_type: sourceLogType || undefined,
        source_log_dsn: sourceLogDSN.trim() || undefined,
        include_logs: false,
        include_api_keys: false,
        quota_per_unit: quotaPerUnit.trim() ? Number(quotaPerUnit) : undefined,
        batch_size: batchSize,
        conflict_strategy: conflictStrategy,
        password_mode: passwordMode,
        api_key_prefix: '',
        preserve_admin_role: preserveAdminRole,
    }), [
        batchSize,
        conflictStrategy,
        passwordMode,
        preserveAdminRole,
        quotaPerUnit,
        sourceDSN,
        sourceLogDSN,
        sourceLogType,
        sourceType,
    ]);

    const start = (apply: boolean) => {
        if (!payloadBase.source_dsn) {
            toast.error('请填写 New API 数据库路径或 DSN');
            return;
        }
        if (quotaPerUnit.trim()) {
            const parsedQuota = Number(quotaPerUnit);
            if (!Number.isFinite(parsedQuota) || parsedQuota <= 0) {
                toast.error('额度单位需要是大于 0 的数字');
                return;
            }
        }
        const payload: StartNewAPIMigrationJobRequest = apply
            ? {
                ...payloadBase,
                apply: true,
                confirm_apply: true,
                dry_run_job_id: dryRunForApply?.id,
            }
            : { ...payloadBase, apply: false };

        startJob.mutate(payload, {
            onSuccess: (job) => {
                setActiveJobID(job.id);
                if (!apply) {
                    setConfirmText('');
                }
                toast.success(apply ? '正式导入已进入后台任务' : 'dry-run 已进入后台任务');
            },
            onError: (error) => {
                toast.error(apply ? '正式导入启动失败' : 'dry-run 启动失败', {
                    description: apiErrorMessage(error),
                });
            },
        });
    };

    const cancelActiveJob = () => {
        if (!activeJob?.id) return;
        cancelJob.mutate(activeJob.id, {
            onSuccess: (job) => {
                setActiveJobID(job.id);
                toast.success('迁移任务已取消');
            },
            onError: (error) => {
                toast.error('取消失败', { description: apiErrorMessage(error) });
            },
        });
    };

    return (
        <PageWrapper className="h-full min-h-0 overflow-y-auto overscroll-contain space-y-5 pb-24 md:pb-4 rounded-t-3xl">
            <div className="grid min-w-0 gap-5 xl:grid-cols-[390px_minmax(0,1fr)]">
                <div className="min-w-0 space-y-5">
                    <div className="rounded-3xl border border-border bg-card p-5">
                        <div className="mb-4 flex items-center justify-between gap-3">
                            <h3 className="flex items-center gap-2 text-base font-semibold">
                                <Database className="size-5" />
                                New API 源库
                            </h3>
                            <Badge variant="outline">服务器路径</Badge>
                        </div>

                        <div className="grid gap-4">
                            <label className="grid gap-2 text-sm">
                                <span className="font-medium">数据库类型</span>
                                <select
                                    value={sourceType}
                                    onChange={(event) => setSourceType(event.target.value)}
                                    className="h-9 rounded-xl border border-input bg-background px-3 text-sm text-foreground"
                                >
                                    <option value="sqlite">SQLite</option>
                                    <option value="mysql">MySQL</option>
                                    <option value="postgres">Postgres</option>
                                </select>
                            </label>

                            <label className="grid gap-2 text-sm">
                                <span className="font-medium">主库路径 / DSN</span>
                                <Input
                                    value={sourceDSN}
                                    onChange={(event) => setSourceDSN(event.target.value)}
                                    placeholder="/app/data/new-api.db"
                                    className="rounded-xl font-mono text-xs"
                                />
                            </label>

                            <div className="rounded-2xl border border-border bg-background p-3">
                                <div className="mb-3 flex items-center gap-2 text-sm font-medium">
                                    <HardDrive className="size-4" />
                                    独立日志库
                                </div>
                                <div className="grid gap-3">
                                    <select
                                        value={sourceLogType}
                                        onChange={(event) => setSourceLogType(event.target.value)}
                                        className="h-9 rounded-xl border border-input bg-background px-3 text-sm text-foreground"
                                    >
                                        <option value="">跟随主库</option>
                                        <option value="sqlite">SQLite</option>
                                        <option value="mysql">MySQL</option>
                                        <option value="postgres">Postgres</option>
                                    </select>
                                    <Input
                                        value={sourceLogDSN}
                                        onChange={(event) => setSourceLogDSN(event.target.value)}
                                        placeholder="留空则读取主库 logs"
                                        className="rounded-xl font-mono text-xs"
                                    />
                                </div>
                            </div>
                        </div>
                    </div>

                    <div className="rounded-3xl border border-border bg-card p-5">
                        <div className="mb-4 flex items-center gap-2 text-base font-semibold">
                            <FileSearch className="size-5" />
                            筛选与导入
                        </div>
                        <div className="grid gap-4">
                            <div className="grid grid-cols-2 gap-3">
                                <label className="grid gap-2 text-sm">
                                    <span className="font-medium">批大小</span>
                                    <Input
                                        type="number"
                                        min={50}
                                        max={5000}
                                        value={batchSize}
                                        onChange={(event) => setBatchSize(Math.min(5000, Math.max(50, Number(event.target.value) || 500)))}
                                        className="rounded-xl"
                                    />
                                </label>
                                <label className="grid gap-2 text-sm">
                                    <span className="font-medium">额度单位</span>
                                    <Input
                                        value={quotaPerUnit}
                                        onChange={(event) => setQuotaPerUnit(event.target.value)}
                                        placeholder="自动读取"
                                        className="rounded-xl"
                                    />
                                </label>
                            </div>

                            <div className="rounded-2xl border border-border bg-background p-3 text-sm">
                                <div className="font-medium">摘要迁移策略</div>
                                <div className="mt-2 grid gap-1 text-xs leading-relaxed text-muted-foreground">
                                    <span>导入活跃用户、余额和用量摘要。</span>
                                    <span>不导入 New API 旧密钥, 用户迁移后重新创建 Octopus API Key。</span>
                                    <span>不导入详细历史日志, 避免大日志库冲击实时统计和 SQLite 写入。</span>
                                </div>
                            </div>

                            <label className="flex items-center justify-between gap-3 rounded-2xl border border-border bg-background px-3 py-2 text-sm">
                                <span className="font-medium">保留管理员角色</span>
                                <input
                                    type="checkbox"
                                    checked={preserveAdminRole}
                                    onChange={(event) => setPreserveAdminRole(event.target.checked)}
                                    className="size-4"
                                />
                            </label>

                            <label className="grid gap-2 text-sm">
                                <span className="font-medium">用户名冲突</span>
                                <select
                                    value={conflictStrategy}
                                    onChange={(event) => setConflictStrategy(event.target.value as typeof conflictStrategy)}
                                    className="h-9 rounded-xl border border-input bg-background px-3 text-sm text-foreground"
                                >
                                    <option value="skip">跳过冲突用户</option>
                                    <option value="merge">合并到现有用户</option>
                                    <option value="rename">自动重命名</option>
                                </select>
                            </label>

                            <label className="grid gap-2 text-sm">
                                <span className="font-medium">密码策略</span>
                                <select
                                    value={passwordMode}
                                    onChange={(event) => setPasswordMode(event.target.value as typeof passwordMode)}
                                    className="h-9 rounded-xl border border-input bg-background px-3 text-sm text-foreground"
                                >
                                    <option value="preserve">保留 bcrypt 哈希</option>
                                    <option value="random">随机重置密码</option>
                                    <option value="disabled">导入后禁用登录</option>
                                </select>
                            </label>

                        </div>
                    </div>

                    <div className="rounded-3xl border border-border bg-card p-5">
                        <div className="mb-4 flex items-center gap-2 text-base font-semibold">
                            <ShieldCheck className="size-5" />
                            执行
                        </div>
                        <div className="grid gap-3">
                            <Button
                                type="button"
                                className="rounded-xl"
                                disabled={running}
                                onClick={() => start(false)}
                            >
                                {startJob.isPending ? <Loader2 className="size-4 animate-spin" /> : <Play className="size-4" />}
                                运行 dry-run
                            </Button>

                            <label className="grid gap-2 text-sm">
                                <span className="font-medium">正式导入确认</span>
                                <Input
                                    value={confirmText}
                                    onChange={(event) => setConfirmText(event.target.value)}
                                    placeholder="输入 IMPORT"
                                    className="rounded-xl"
                                />
                            </label>

                            <Button
                                type="button"
                                variant="destructive"
                                className="rounded-xl"
                                disabled={!canApply}
                                onClick={() => start(true)}
                            >
                                {startJob.isPending ? <Loader2 className="size-4 animate-spin" /> : <ShieldCheck className="size-4" />}
                                按 dry-run 结果正式导入
                            </Button>

                            <div className="rounded-2xl border border-amber-500/30 bg-amber-500/5 p-3 text-xs leading-5 text-amber-800 dark:text-amber-200">
                                SQLite 目标库正式导入时可能短暂影响正常请求；百万级日志建议在维护窗口运行。
                            </div>
                        </div>
                    </div>
                </div>

                <div className="min-w-0 space-y-5">
                    <div className="rounded-3xl border border-border bg-card p-5">
                        <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
                            <div>
                                <h3 className="flex items-center gap-2 text-base font-semibold">
                                    <RefreshCw className="size-5" />
                                    后台任务
                                </h3>
                                <div className="mt-1 text-xs text-muted-foreground">
                                    {activeJob ? `${activeJob.id} · ${activeJob.apply ? '正式导入' : 'dry-run'}` : '暂无任务'}
                                </div>
                            </div>
                            <div className="flex items-center gap-2">
                                <StatusBadge job={activeJob} />
                                {isActive(activeJob) ? (
                                    <Button type="button" variant="outline" size="sm" className="rounded-xl" onClick={cancelActiveJob}>
                                        取消
                                    </Button>
                                ) : null}
                            </div>
                        </div>

                        <div className="grid gap-4">
                            <Progress value={activeJob?.percent ?? 0} className="h-2.5" />
                            <div className="flex flex-wrap items-center justify-between gap-3 text-xs text-muted-foreground">
                                <span>{activeJob?.message || '等待 dry-run'}</span>
                                <span className="tabular-nums">{activeJob?.percent ?? 0}%</span>
                            </div>
                            <StageTimeline job={activeJob} />
                            {activeJob?.error ? (
                                <div className="rounded-2xl border border-destructive/30 bg-destructive/5 p-4 text-sm text-destructive">
                                    {activeJob.error}
                                </div>
                            ) : null}
                        </div>
                    </div>

                    <div className="rounded-3xl border border-border bg-card p-5">
                        <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
                            <h3 className="text-base font-semibold">迁移报告</h3>
                            {activeJob ? (
                                <div className="flex flex-wrap gap-2">
                                    <Badge variant="outline">摘要</Badge>
                                    <Badge variant="outline">不导入日志</Badge>
                                    <Badge variant="outline">不导入旧密钥</Badge>
                                    <Badge variant="outline">{activeJob.request.conflict_strategy}</Badge>
                                </div>
                            ) : null}
                        </div>
                        <ReportSummary summary={activeJob?.summary} />
                    </div>

                    <div className="rounded-3xl border border-border bg-card p-5">
                        <div className="mb-4 flex items-center justify-between gap-3">
                            <h3 className="text-base font-semibold">最近任务</h3>
                            {jobsQuery.isFetching ? <Loader2 className="size-4 animate-spin text-muted-foreground" /> : null}
                        </div>
                        <Table className="min-w-[760px]">
                                <TableHeader>
                                    <TableRow>
                                        <TableHead>模式</TableHead>
                                        <TableHead>状态</TableHead>
                                        <TableHead>源库</TableHead>
                                        <TableHead>活跃用户</TableHead>
                                        <TableHead>策略</TableHead>
                                        <TableHead>更新时间</TableHead>
                                    </TableRow>
                                </TableHeader>
                                <TableBody>
                                    {(jobsQuery.data ?? []).length === 0 ? (
                                        <TableRow>
                                            <TableCell colSpan={6} className="py-10 text-center text-sm text-muted-foreground">
                                                暂无迁移任务
                                            </TableCell>
                                        </TableRow>
                                    ) : (
                                        (jobsQuery.data ?? []).map((job) => (
                                            <TableRow
                                                key={job.id}
                                                className="cursor-pointer"
                                                onClick={() => setActiveJobID(job.id)}
                                            >
                                                <TableCell>
                                                    <Badge variant={job.apply ? 'secondary' : 'outline'}>
                                                        {job.apply ? 'apply' : 'dry-run'}
                                                    </Badge>
                                                </TableCell>
                                                <TableCell>{statusLabels[job.status]}</TableCell>
                                                <TableCell className="max-w-64 font-mono text-xs">
                                                    <span className="block max-w-64 truncate" title={job.request.source_dsn}>
                                                        {job.request.source_dsn}
                                                    </span>
                                                </TableCell>
                                                <TableCell className="tabular-nums">{formatNumber(job.summary?.active_users)}</TableCell>
                                                <TableCell>摘要</TableCell>
                                                <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
                                                    {formatDate(job.updated_at)}
                                                </TableCell>
                                            </TableRow>
                                        ))
                                    )}
                                </TableBody>
                        </Table>
                    </div>
                </div>
            </div>
        </PageWrapper>
    );
}
