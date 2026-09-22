'use client';

import { useEffect, useMemo, useState } from 'react';
import {
    AlertTriangle,
    Binary,
    ChevronRight,
    Fingerprint,
    Loader2,
    MessageSquareQuote,
    Radar,
    Repeat2,
    Shield,
    Signature,
    SlidersHorizontal,
    TextQuote,
    Wifi,
} from 'lucide-react';
import type { LucideIcon } from 'lucide-react';
import type { ApiError } from '@/api/types';
import { ChannelType, useChannelList, type Channel } from '@/api/endpoints/channel';
import {
    AUDIT_PROBE_IDS,
    useLogAnomalies,
    useRunModelAudit,
    type AuditFinding,
    type AuditProbeId,
    type AuditProbeResult,
    type AuditVerdict,
    type ModelAuditResponse,
} from '@/api/endpoints/model-audit';
import { PageWrapper } from '@/components/common/PageWrapper';
import { toast } from '@/components/common/Toast';
import { Button } from '@/components/ui/button';
import {
    Dialog,
    DialogContent,
    DialogDescription,
    DialogHeader,
    DialogTitle,
} from '@/components/ui/dialog';
import { cn } from '@/lib/utils';
import { getSelectedChannelModels } from '@/components/modules/channel/channel-utils';
import { sanitizeChannelTestError } from '@/lib/channel-test';

type PlanId = 'quick' | 'standard';
type RowTone = 'pass' | 'warn' | 'info' | 'fail' | 'skip';
type PageState = 'idle' | 'running' | 'report' | 'error';

type ProbeMeta = {
    id: AuditProbeId;
    name: string;
    hint: string;
    desc: string;
    icon: LucideIcon;
};

const PROBES: ProbeMeta[] = [
    { id: 'liveness', name: '连通回显', hint: '基础', desc: '确认渠道能正常回显请求', icon: Wifi },
    { id: 'identity', name: '身份自述', hint: '仅线索', desc: '读取模型自述的供应商标识', icon: Fingerprint },
    { id: 'glitch', name: '词元指纹', hint: '仅线索', desc: '比对分词边界的已知特征', icon: Binary },
    { id: 'token_delta', name: '用量偏差', hint: '仅线索', desc: '核对返回用量与请求估算', icon: Radar },
    { id: 'echo_rewrite', name: '输出改写', hint: '基础', desc: '检查指定文本是否被原样回传', icon: Repeat2 },
    { id: 'context_canary', name: '上下文完整性', hint: '基础', desc: '确认上下文标记未被截断', icon: TextQuote },
    { id: 'signature', name: '思考签名', hint: '仅Claude', desc: '检查思考块是否附带签名字段', icon: Signature },
];

const QUICK_IDS: AuditProbeId[] = ['liveness', 'echo_rewrite', 'context_canary'];

type DisplayRow = {
    meta: ProbeMeta;
    tone: RowTone;
    status: string;
    result?: AuditProbeResult;
    findings: AuditFinding[];
};

function apiErrorMessage(error: unknown): string | undefined {
    return (error as ApiError | undefined)?.message;
}

function isApplicable(channel: Channel | undefined, id: AuditProbeId): boolean {
    if (id !== 'signature') return true;
    return channel?.type === ChannelType.Anthropic;
}

function planIds(plan: PlanId, channel: Channel | undefined): AuditProbeId[] {
    const base = plan === 'quick' ? QUICK_IDS : [...AUDIT_PROBE_IDS];
    return base.filter((id) => isApplicable(channel, id));
}

function asRecord(value: unknown): Record<string, unknown> {
    if (value && typeof value === 'object' && !Array.isArray(value)) {
        return value as Record<string, unknown>;
    }
    return {};
}

function rowFromResult(meta: ProbeMeta, report: ModelAuditResponse): DisplayRow {
    const result = report.report.results.find((item) => item.probe_id === meta.id);
    const findings = (report.report.findings ?? []).filter((item) => item.probe === meta.id);
    const probeError = (report.report.errors ?? []).find((item) => item.probe === meta.id);
    const data = asRecord(result?.data);

    if (data.applicable === false) {
        return { meta, tone: 'skip', status: '不适用', result, findings: [] };
    }
    if (probeError || result?.error) {
        return {
            meta,
            tone: 'fail',
            status: '执行未完成',
            result,
            findings,
        };
    }
    if (findings.some((item) => item.severity === 'medium' || item.severity === 'high')) {
        return { meta, tone: 'warn', status: '待复核', result, findings };
    }
    if (findings.length > 0) {
        return { meta, tone: 'warn', status: '待复核', result, findings };
    }
    if (meta.id === 'glitch' && result?.ok) {
        return { meta, tone: 'info', status: '结构可解析', result, findings };
    }
    if (result?.ok) {
        return { meta, tone: 'pass', status: '未见异常', result, findings };
    }
    return { meta, tone: 'fail', status: '证据不足', result, findings };
}

function orderedRows(rows: DisplayRow[]): DisplayRow[] {
    const rank: Record<RowTone, number> = { warn: 0, fail: 1, info: 2, pass: 3, skip: 4 };
    return [...rows].sort((a, b) => rank[a.tone] - rank[b.tone]);
}

function formatDuration(ms: number): string {
    if (ms < 1000) return `${ms} ms`;
    return `${(ms / 1000).toFixed(1)}s`;
}

function headline(verdict: AuditVerdict, warnCount: number, failCount: number): { title: string; sub: string } {
    if (failCount > 0 && warnCount === 0) {
        return { title: '证据不足，部分检查未完成', sub: '未完成的检查不记为异常，只说明这次没查清。' };
    }
    if (verdict === 'unknown') {
        return { title: '证据不足', sub: '跑通的检查太少，不能当成未见异常。' };
    }
    if (warnCount > 0) {
        return {
            title: `发现 ${warnCount} 项需要复核`,
            sub: '其余检查未见异常或仅为结构线索。',
        };
    }
    return {
        title: '未见需要复核的项',
        sub: '这不是原厂证明，只说明这次检查没有待复核线索。',
    };
}

function evidencePairs(row: DisplayRow): Array<[string, string]> {
    const pairs: Array<[string, string]> = [];
    for (const finding of row.findings) {
        pairs.push(['判定', finding.title]);
        if (finding.recommendation) pairs.push(['建议', finding.recommendation]);
        for (const [key, value] of Object.entries(finding.evidence ?? {})) {
            pairs.push([key, typeof value === 'string' ? value : JSON.stringify(value)]);
        }
    }
    const data = asRecord(row.result?.data);
    for (const [key, value] of Object.entries(data)) {
        if (key === 'applicable') continue;
        pairs.push([key, typeof value === 'string' ? value : JSON.stringify(value)]);
    }
    if (row.result?.error) pairs.push(['错误', sanitizeChannelTestError(row.result.error)]);
    if (pairs.length === 0) pairs.push(['备注', '本次没有可展开的证据字段。']);
    return pairs.slice(0, 16);
}

export function ModelAudit() {
    const { data: channelsData, isLoading } = useChannelList();
    const { data: logAnomalies } = useLogAnomalies();
    const runAudit = useRunModelAudit();
    const channels = useMemo(
        () => (channelsData ?? []).map((item) => item.raw).filter((item) => item.enabled),
        [channelsData],
    );

    const [channelId, setChannelId] = useState<number>(0);
    const [modelName, setModelName] = useState('');
    const [plan, setPlan] = useState<PlanId>('standard');
    const [selected, setSelected] = useState<AuditProbeId[]>([...AUDIT_PROBE_IDS]);
    const [showAdvanced, setShowAdvanced] = useState(false);
    const [showParams, setShowParams] = useState(false);
    const [pageState, setPageState] = useState<PageState>('idle');
    const [report, setReport] = useState<ModelAuditResponse | null>(null);
    const [errorText, setErrorText] = useState('');
    const [openRow, setOpenRow] = useState<DisplayRow | null>(null);

    const channel = channels.find((item) => item.id === channelId) ?? channels[0];
    const models = useMemo(
        () => (channel ? getSelectedChannelModels(channel) : []),
        [channel],
    );
    const activeModel = models.includes(modelName) ? modelName : (models[0] ?? '');

    useEffect(() => {
        if (!channel) return;
        if (channelId !== channel.id) setChannelId(channel.id);
        const nextModel = models[0] ?? '';
        if (!models.includes(modelName) && modelName !== nextModel) setModelName(nextModel);
    }, [channel, channelId, modelName, models]);
    const applicableSelected = selected.filter((id) => isApplicable(channel, id));
    const runCount = plan === 'quick' ? planIds('quick', channel).length : applicableSelected.length;

    const rows = useMemo(() => {
        if (!report) return [];
        const ran = new Set((report.report.results ?? []).map((item) => item.probe_id));
        return orderedRows(
            PROBES.filter((meta) => ran.has(meta.id)).map((meta) => rowFromResult(meta, report)),
        );
    }, [report]);
    const warnCount = rows.filter((row) => row.tone === 'warn').length;
    const failCount = rows.filter((row) => row.tone === 'fail').length;
    const copy = report ? headline(report.report.verdict, warnCount, failCount) : null;

    const chooseChannel = (id: number) => {
        setChannelId(id);
        const next = channels.find((item) => item.id === id);
        const nextModels = next ? getSelectedChannelModels(next) : [];
        setModelName(nextModels[0] ?? '');
        if (plan === 'standard') {
            setSelected(AUDIT_PROBE_IDS.filter((probeId) => isApplicable(next, probeId)));
        }
    };

    const choosePlan = (next: PlanId) => {
        setPlan(next);
        if (next === 'quick') setSelected(planIds('quick', channel));
        else setSelected(AUDIT_PROBE_IDS.filter((id) => isApplicable(channel, id)));
    };

    const toggleProbe = (id: AuditProbeId, checked: boolean) => {
        setPlan('standard');
        setSelected((current) => {
            if (checked) return AUDIT_PROBE_IDS.filter((item) => item === id || current.includes(item));
            return current.filter((item) => item !== id);
        });
    };

    const run = async () => {
        if (!channel || !activeModel) {
            toast.warning('先选渠道和模型');
            return;
        }
        const probes = plan === 'quick' ? planIds('quick', channel) : applicableSelected;
        if (probes.length === 0) {
            toast.warning('至少勾一项检查');
            return;
        }
        setPageState('running');
        setErrorText('');
        try {
            const data = await runAudit.mutateAsync({
                channel_id: channel.id,
                model: activeModel,
                probes,
            });
            setReport(data);
            setPageState('report');
        } catch (error) {
            const message = sanitizeChannelTestError(apiErrorMessage(error) || '审计未完成');
            setErrorText(message);
            setPageState('error');
            toast.error(message);
        }
    };

    const exportJson = () => {
        if (!report) return;
        const blob = new Blob([JSON.stringify(report, null, 2)], { type: 'application/json' });
        const url = URL.createObjectURL(blob);
        const link = document.createElement('a');
        link.href = url;
        link.download = `model-audit-${report.channel_id}.json`;
        link.click();
        URL.revokeObjectURL(url);
    };

    const renderParams = () => (
        <aside className="flex min-h-0 flex-col gap-5 rounded-3xl border border-border bg-card p-5 md:w-[290px] md:shrink-0">
            <section>
                <p className="mb-3 text-[11px] font-medium tracking-[0.16em] text-muted-foreground">01 目标</p>
                <label className="mb-1 block text-xs text-muted-foreground">渠道</label>
                <select
                    value={channel?.id ?? ''}
                    onChange={(event) => chooseChannel(Number(event.target.value))}
                    className="h-10 w-full rounded-xl border border-input bg-background px-3 text-sm"
                >
                    {channels.length === 0 && <option value="">暂无启用渠道</option>}
                    {channels.map((item) => (
                        <option key={item.id} value={item.id}>{item.name}</option>
                    ))}
                </select>
                <label className="mb-1 mt-3 block text-xs text-muted-foreground">模型</label>
                <select
                    value={activeModel}
                    onChange={(event) => setModelName(event.target.value)}
                    className="h-10 w-full rounded-xl border border-input bg-background px-3 text-sm"
                >
                    {models.length === 0 && <option value="">该渠道没有可选模型</option>}
                    {models.map((item) => (
                        <option key={item} value={item}>{item}</option>
                    ))}
                </select>
            </section>

            <section className="border-t border-border pt-4">
                <p className="mb-3 text-[11px] font-medium tracking-[0.16em] text-muted-foreground">02 检查</p>
                <div className="grid gap-1">
                    {PROBES.map((probe) => {
                        const disabled = !isApplicable(channel, probe.id);
                        const checked = selected.includes(probe.id) && !disabled;
                        return (
                            <label key={probe.id} className={cn('flex items-center gap-2 rounded-xl px-1 py-1.5 text-sm', disabled && 'opacity-40')}>
                                <input
                                    type="checkbox"
                                    checked={checked}
                                    disabled={disabled || pageState === 'running'}
                                    onChange={(event) => toggleProbe(probe.id, event.target.checked)}
                                />
                                <span className="min-w-0 flex-1 truncate">{probe.name}</span>
                                <span className="text-[11px] text-muted-foreground">{probe.hint}</span>
                            </label>
                        );
                    })}
                </div>
            </section>

            <section className="border-t border-border pt-4">
                <p className="mb-3 text-[11px] font-medium tracking-[0.16em] text-muted-foreground">03 执行</p>
                <div className="grid grid-cols-2 gap-1 rounded-xl bg-muted/40 p-1">
                    {(['quick', 'standard'] as const).map((item) => (
                        <button
                            key={item}
                            type="button"
                            onClick={() => choosePlan(item)}
                            className={cn(
                                'h-8 rounded-lg text-xs font-medium',
                                plan === item ? 'bg-background shadow-xs' : 'text-muted-foreground',
                            )}
                        >
                            {item === 'quick' ? '快速' : '标准'}
                        </button>
                    ))}
                </div>
                <button
                    type="button"
                    className="mt-3 text-xs text-muted-foreground"
                    onClick={() => setShowAdvanced((value) => !value)}
                >
                    {showAdvanced ? '收起高级参数' : '高级参数'}
                </button>
                {showAdvanced && (
                    <p className="mt-2 text-xs leading-5 text-muted-foreground">
                        超时跟随渠道检测上限。签名、身份、词元只作线索，不作为原厂证明。
                    </p>
                )}
                <Button type="button" className="mt-4 h-11 w-full rounded-xl" disabled={pageState === 'running' || runCount === 0} onClick={() => void run()}>
                    {pageState === 'running' ? <Loader2 className="size-4 animate-spin" /> : null}
                    {pageState === 'running' ? '正在检查' : '开始检查'}
                </Button>
                <p className="mt-2 text-center text-[11px] text-muted-foreground">预计执行 {runCount} 项检查 · 串行</p>
            </section>
        </aside>
    );

    return (
        <PageWrapper className="h-full min-h-0 space-y-4 overflow-y-auto overscroll-contain rounded-t-3xl pb-24 md:pb-4">
            <div className="flex flex-wrap items-end justify-between gap-3 px-1">
                <div>
                    <p className="text-[11px] font-medium tracking-[0.18em] text-muted-foreground">OCTOPUS</p>
                    <h2 className="text-2xl font-bold tracking-tight">模型审计</h2>
                    <p className="mt-1 text-sm text-muted-foreground">对渠道模型做一组轻量能力检查，结果仅作参考线索。</p>
                </div>
            </div>

            <div className="md:hidden">
                <div className="flex items-center justify-between rounded-2xl border border-border bg-card px-4 py-3">
                    <p className="text-sm">已选 {runCount} 项 · {channel?.name ?? '未选渠道'}</p>
                    <Button type="button" variant="outline" size="sm" className="rounded-xl" onClick={() => setShowParams(true)}>
                        <SlidersHorizontal className="size-4" />
                        调整参数
                    </Button>
                </div>
            </div>

            <div className="flex min-h-0 flex-col gap-4 md:flex-row">
                <div className="hidden md:block">{renderParams()}</div>

                <section className="min-w-0 flex-1 rounded-3xl border border-border bg-card p-5">
                    {pageState === 'idle' && (
                        <div className="flex min-h-80 flex-col items-center justify-center text-center">
                            <Shield className="mb-3 size-8 text-muted-foreground" />
                            <h3 className="text-lg font-semibold">未开始检测</h3>
                            <p className="mt-1 max-w-sm text-sm text-muted-foreground">选好渠道和模型后开始。结果只作线索，不证明真假。</p>
                            {logAnomalies && (
                                <div className="mt-6 w-full max-w-lg rounded-2xl border border-border bg-muted/30 px-4 py-3 text-left">
                                    <p className="text-xs text-muted-foreground">近 {logAnomalies.window_hours} 小时真实调用扫描 · {logAnomalies.sample_count} 条成功样本</p>
                                    {(logAnomalies.findings ?? []).length === 0 ? (
                                        <p className="mt-2 text-sm">近期日志未见需要复核的统计异常。</p>
                                    ) : (
                                        <ul className="mt-2 space-y-2">
                                            {(logAnomalies.findings ?? []).slice(0, 4).map((item) => (
                                                <li key={`${item.channel_id}-${item.code}-${item.model}`} className="text-sm">
                                                    <span className="font-medium">{item.title}</span>
                                                    <span className="mt-0.5 block text-xs text-muted-foreground">{item.channel_name || `渠道 ${item.channel_id}`} · {item.model}</span>
                                                </li>
                                            ))}
                                        </ul>
                                    )}
                                    <p className="mt-2 text-[11px] leading-5 text-muted-foreground">{logAnomalies.disclaimer}</p>
                                </div>
                            )}
                        </div>
                    )}

                    {pageState === 'running' && (
                        <div className="flex min-h-80 flex-col items-center justify-center text-center">
                            <Loader2 className="mb-3 size-8 animate-spin text-primary" />
                            <h3 className="text-lg font-semibold">正在检查</h3>
                            <p className="mt-1 text-sm text-muted-foreground">沿用渠道检测的出站链路，不改指纹。</p>
                        </div>
                    )}

                    {pageState === 'error' && (
                        <div className="flex min-h-80 flex-col items-center justify-center text-center">
                            <AlertTriangle className="mb-3 size-8 text-amber-600" />
                            <h3 className="text-lg font-semibold">执行未完成</h3>
                            <p className="mt-1 max-w-md text-sm text-muted-foreground">{errorText || '这次没有拿到完整报告。网络或额度问题不记为造假。'}</p>
                        </div>
                    )}

                    {pageState === 'report' && report && copy && (
                        <div className="space-y-5">
                            <div className="flex flex-wrap items-start justify-between gap-3">
                                <p className="text-xs text-muted-foreground">本次报告 {new Date().toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' })}</p>
                                <Button type="button" variant="outline" size="sm" className="rounded-full" onClick={exportJson}>导出 JSON</Button>
                            </div>
                            <div>
                                <h3 className="flex items-center gap-2 text-2xl font-semibold tracking-tight">
                                    <AlertTriangle className={cn('size-5', warnCount > 0 ? 'text-amber-600' : 'text-muted-foreground')} />
                                    {copy.title}
                                </h3>
                                <p className="mt-1 text-sm text-muted-foreground">{copy.sub}</p>
                            </div>
                            <div className="grid grid-cols-3 gap-2">
                                <div className="rounded-2xl bg-muted/40 px-3 py-3">
                                    <p className="text-[11px] text-muted-foreground">已完成检查</p>
                                    <p className="mt-1 text-2xl font-semibold">{report.report.results.length}</p>
                                </div>
                                <div className="rounded-2xl bg-muted/40 px-3 py-3">
                                    <p className="text-[11px] text-muted-foreground">需要复核</p>
                                    <p className={cn('mt-1 text-2xl font-semibold', warnCount > 0 && 'text-amber-700 dark:text-amber-400')}>{warnCount}</p>
                                </div>
                                <div className="rounded-2xl bg-muted/40 px-3 py-3">
                                    <p className="text-[11px] text-muted-foreground">耗时</p>
                                    <p className="mt-1 text-2xl font-semibold">{formatDuration(report.duration_ms)}</p>
                                </div>
                            </div>
                            <div className="flex h-1.5 overflow-hidden rounded-full bg-muted">
                                {rows.map((row) => (
                                    <span
                                        key={row.meta.id}
                                        className={cn(
                                            'flex-1',
                                            row.tone === 'warn' && 'bg-amber-500',
                                            row.tone === 'info' && 'bg-muted-foreground/40',
                                            row.tone === 'pass' && 'bg-primary',
                                            row.tone === 'fail' && 'bg-destructive/70',
                                            row.tone === 'skip' && 'bg-border',
                                        )}
                                    />
                                ))}
                            </div>
                            <div className="flex items-center justify-between text-xs text-muted-foreground">
                                <span>检查结果</span>
                                <span>点按行查看证据</span>
                            </div>
                            <div className="divide-y divide-border overflow-hidden rounded-2xl border border-border">
                                {rows.map((row) => {
                                    const Icon = row.meta.icon;
                                    return (
                                        <button
                                            key={row.meta.id}
                                            type="button"
                                            onClick={() => setOpenRow(row)}
                                            className={cn(
                                                'flex w-full items-center gap-3 px-3 py-3 text-left hover:bg-muted/40',
                                                row.tone === 'warn' && 'bg-amber-500/8',
                                            )}
                                        >
                                            <Icon className="size-4 shrink-0 text-muted-foreground" />
                                            <span className="min-w-0 flex-1">
                                                <span className="block text-sm font-medium">{row.meta.name}</span>
                                                <span className="block truncate text-xs text-muted-foreground">{row.meta.desc}</span>
                                            </span>
                                            <span className={cn(
                                                'rounded-full px-2 py-0.5 text-[11px]',
                                                row.tone === 'warn' && 'bg-amber-500/15 text-amber-800 dark:text-amber-300',
                                                row.tone === 'pass' && 'bg-primary/15 text-primary',
                                                row.tone === 'info' && 'bg-muted text-muted-foreground',
                                                row.tone === 'fail' && 'bg-destructive/10 text-destructive',
                                                row.tone === 'skip' && 'bg-muted text-muted-foreground',
                                            )}>
                                                {row.status}
                                            </span>
                                            <ChevronRight className="size-4 text-muted-foreground" />
                                        </button>
                                    );
                                })}
                            </div>
                            <p className="text-xs leading-5 text-muted-foreground">
                                {report.report.disclaimer || '检查结果提供参考，不等同模型真实性证明；身份、词元、签名仅为线索。'}
                            </p>
                        </div>
                    )}
                </section>
            </div>

            {isLoading && (
                <p className="px-1 text-xs text-muted-foreground">正在读取渠道列表…</p>
            )}

            <Dialog open={showParams} onOpenChange={setShowParams}>
                <DialogContent className="max-w-md p-0 sm:max-w-md">
                    <DialogHeader className="px-5 pt-5">
                        <DialogTitle>检查参数</DialogTitle>
                        <DialogDescription>选渠道、模型和要跑的检查。</DialogDescription>
                    </DialogHeader>
                    <div className="px-2 pb-4">{renderParams()}</div>
                </DialogContent>
            </Dialog>

            <Dialog open={!!openRow} onOpenChange={(open) => { if (!open) setOpenRow(null); }}>
                <DialogContent className="max-w-lg">
                    <DialogHeader>
                        <DialogTitle className="flex items-center gap-2">
                            <MessageSquareQuote className="size-4" />
                            {openRow?.meta.name}
                        </DialogTitle>
                        <DialogDescription>{openRow?.meta.desc} · {openRow?.meta.hint}</DialogDescription>
                    </DialogHeader>
                    <dl className="grid gap-2 text-sm">
                        {(openRow ? evidencePairs(openRow) : []).map(([key, value], index) => (
                            <div key={`${key}-${index}`} className="grid grid-cols-[96px_1fr] gap-3 rounded-xl bg-muted/40 px-3 py-2">
                                <dt className="text-xs text-muted-foreground">{key}</dt>
                                <dd className="break-all">{value}</dd>
                            </div>
                        ))}
                    </dl>
                </DialogContent>
            </Dialog>
        </PageWrapper>
    );
}
