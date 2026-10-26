'use client';

import { useEffect, useMemo, useRef, useState } from 'react';
import {
    AlertTriangle,
    Binary,
    ChevronRight,
    Dices,
    Fingerprint,
    Loader2,
    Play,
    MessageSquareQuote,
    Radar,
    Repeat2,
    Shield,
    Signature,
    SlidersHorizontal,
    TextQuote,
    Wifi,
    XCircle,
} from 'lucide-react';
import type { LucideIcon } from 'lucide-react';
import { useTranslations } from 'next-intl';
import type { ApiError } from '@/api/types';
import { ChannelType, useChannelList, type Channel } from '@/api/endpoints/channel';
import {
    AUDIT_PROBE_IDS,
    useLogAnomalies,
    useRunModelAudit,
    useScheduledAudit,
    type AuditFinding,
    type AuditProbeId,
    type AuditProbeResult,
    type AuditVerdict,
    type AuditResult,
    type AuditViewReport,
    type ModelAuditResponse,
    useEnqueueAudit,
} from '@/api/endpoints/model-audit';
import { PageWrapper } from '@/components/common/PageWrapper';
import { SearchableSelect } from '@/components/ui/searchable-select';
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
import { ModelAuditQueue } from './Queue';
import { Switch } from '@/components/ui/switch';
import { useSettingList, useSetSetting, SettingKey } from '@/api/endpoints/setting';

type PlanId = 'quick' | 'standard';
type RowTone = 'pass' | 'warn' | 'info' | 'fail' | 'skip';
type PageState = 'idle' | 'running' | 'report' | 'error' | 'cancelling' | 'cancelled';

// 队列项状态。故意只有四态，且 done/failed 是终态——队列跑完不会自动重排，
// 想再来一次由用户重新加入。skipped 表示轮到它之前队列就被用户停了。
type QueueState = 'pending' | 'running' | 'done' | 'failed' | 'skipped';
type AuditQueueItem = {
    // key 用 渠道ID::模型名，同一个模型在同一渠道不会重复入队（避免误点两次花两份钱）。
    key: string;
    channelId: number;
    channelName: string;
    model: string;
    state: QueueState;
    // error 只在 state=failed 时有值，给用户看失败原因（已脱敏）。
    error?: string;
};

type ProbeMeta = {
    id: AuditProbeId;
    name: string;
    hint: string;
    desc: string;
    icon: LucideIcon;
};

import { readFingerprint, describeDecision, formatProbability, type FingerprintSummary } from './fingerprint';

const PROBES: ProbeMeta[] = [
    { id: 'liveness', name: '连通回显', hint: '基础', desc: '确认渠道能正常回显请求', icon: Wifi },
    { id: 'identity', name: '身份自述', hint: '仅线索', desc: '读取模型自述的供应商标识', icon: Fingerprint },
    { id: 'glitch', name: '词元指纹', hint: '仅线索', desc: '比对分词边界的已知特征', icon: Binary },
    { id: 'token_delta', name: '用量偏差', hint: '仅线索', desc: '核对返回用量与请求估算', icon: Radar },
    { id: 'echo_rewrite', name: '输出改写', hint: '基础', desc: '检查指定文本是否被原样回传', icon: Repeat2 },
    { id: 'context_canary', name: '上下文完整性', hint: '基础', desc: '确认上下文标记未被截断', icon: TextQuote },
    { id: 'signature', name: '思考签名', hint: '仅Claude', desc: '检查思考块是否附带签名字段', icon: Signature },
    // 随机数指纹：要发三道题、每题几百个整数，是整套检测里最贵的一条，
    // 所以单独标"高开销"，让用户在勾选前就知道代价。
    { id: 'fingerprint', name: '随机数指纹', hint: '高开销', desc: '按取值习惯比对参考库，识别冒充', icon: Dices },
];

const QUICK_IDS: AuditProbeId[] = ['liveness', 'echo_rewrite', 'context_canary'];

type DisplayRow = {
    meta: ProbeMeta;
    tone: RowTone;
    status: string;
    result?: AuditProbeResult;
    findings: AuditFinding[];
    /** 不适用/证据不足等状态下后端给出的原因（用于行内展示）。 */
    reason?: string;
    /** 证据不足（未认证）标记：headline 据此不再谎称“未见需要复核项”。 */
    insufficient?: boolean;
};

function apiErrorMessage(error: unknown): string | undefined {
    return (error as ApiError | undefined)?.message;
}

/**
 * 型号是否明确属于 Claude 家族。用「边界匹配」：'claude' 必须作为独立一段出现
 * （名字开头/结尾或被非字母数字分隔），避免 'claudexxx'、'notclaude' 之类含子串的
 * 假名被泛化成 Claude。只作前端预筛，后端仍是权威 gate。
 */
function isClaudeModelFamily(model: string): boolean {
    const name = model.trim().toLowerCase();
    if (!name) return false;
    return /(^|[^a-z0-9])claude([^a-z0-9]|$)/.test(name);
}

/**
 * 探针适用性（前端预筛，后端会再权威 gate）：
 * - signature 需 Anthropic 渠道 **且** 型号明确是 Claude（Grok 经 Anthropic 兼容协议也不等于 Claude）；
 * - 其余探针默认适用，是否真有基准由后端返回 not_applicable/insufficient_evidence。
 */
function isApplicable(channel: Channel | undefined, id: AuditProbeId, model = ''): boolean {
    if (id !== 'signature') return true;
    return channel?.type === ChannelType.Anthropic && isClaudeModelFamily(model);
}

function planIds(plan: PlanId, channel: Channel | undefined, model = ''): AuditProbeId[] {
    const base = plan === 'quick' ? QUICK_IDS : [...AUDIT_PROBE_IDS];
    return base.filter((id) => isApplicable(channel, id, model));
}

/** 主动检测来源 → 人话（三语键）；未知来源如实标注。 */
function triggerLabel(trigger: string, t: (key: string) => string): string {
    const value = trigger.trim();
    if (value === 'manual') return t('sourceManual');
    if (value === 'schedule') return t('sourceSchedule');
    if (value === 'echo_mismatch') return t('sourceEcho');
    return t('sourceUnknown');
}

/** 后端不适用/不足原因：读 not_applicable_reason，兼容旧字段名 reason。 */
function rowReason(data: Record<string, unknown>): string | undefined {
    const value = data.not_applicable_reason ?? data.reason;
    return typeof value === 'string' ? value : undefined;
}

function asRecord(value: unknown): Record<string, unknown> {
    if (value && typeof value === 'object' && !Array.isArray(value)) {
        return value as Record<string, unknown>;
    }
    return {};
}

function rowFromResult(meta: ProbeMeta, report: ModelAuditResponse, t: (key: string) => string): DisplayRow {
    const result = report.report.results.find((item) => item.probe_id === meta.id);
    const findings = (report.report.findings ?? []).filter((item) => item.probe === meta.id);
    const probeError = (report.report.errors ?? []).find((item) => item.probe === meta.id);
    const data = asRecord(result?.data);

    if (data.applicable === false) {
        return { meta, tone: 'skip', status: t('statusNotApplicable'), result, findings: [], reason: rowReason(data) };
    }
    // 适用性未知（如身份/签名拿不到证据）：后端标 insufficient_evidence → 不进 pass/warn，只报证据不足。
    if (data.insufficient_evidence === true) {
        return { meta, tone: 'info', status: t('statusInsufficient'), result, findings: [], reason: rowReason(data), insufficient: true };
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

function headline(verdict: AuditVerdict, warnCount: number, failCount: number, insufficientCount: number, t: (key: string, values?: Record<string, string | number>) => string): { title: string; sub: string } {
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
    // 有证据不足项时，不得谎称“未见需要复核项”；不认证真伪。
    if (insufficientCount > 0) {
        return {
            title: t('headlineInsufficientTitle'),
            sub: t('headlineInsufficientSub', { count: insufficientCount }),
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

/**
 * 指纹结论卡：把「随机数指纹」探针真正算出来的东西摆到页面上。
 *
 * 为什么必须有这一块：这项探针是唯一能**认出模型**的检查，后端早就算好了最像的
 * 模型、家族、前五名候选和库内置信度。但页面以前只给它一行「未见异常/证据不足」，
 * 内容全埋在证据抽屉的原始 JSON 里——用户跑完一次检测，看到的是状态，不是结论，
 * 观感就是「检测不出东西」。
 *
 * 边界照抄后端措辞，一个字都不放宽：这是**参考库内的相对排名**，库外模型也会得到
 * 一个「最像」的答案，所以它不能当「确认就是某模型」用。
 */
function FingerprintCard({ summary }: { summary: FingerprintSummary }) {
    const probability = formatProbability(summary.probability);
    return (
        <div className="rounded-2xl border border-border bg-card px-4 py-3">
            <div className="flex flex-wrap items-start justify-between gap-3">
                <div className="min-w-0">
                    <p className="text-[11px] font-medium tracking-[0.16em] text-muted-foreground">随机数指纹 · 参考库比对</p>
                    <p className="mt-1 flex flex-wrap items-baseline gap-2">
                        <span className="text-lg font-semibold">{summary.predictionName || '参考库内无匹配'}</span>
                        {summary.familyName && (
                            <span className="rounded bg-muted px-1.5 py-0.5 text-[11px] text-muted-foreground">{summary.familyName}</span>
                        )}
                    </p>
                    <p className="mt-0.5 text-xs leading-5 text-muted-foreground">{describeDecision(summary)}</p>
                </div>
                {probability && (
                    <div className="shrink-0 rounded-xl bg-muted/50 px-3 py-2 text-center">
                        <p className="text-[11px] text-muted-foreground">库内置信度</p>
                        <p className="mt-0.5 text-lg font-semibold">{probability}</p>
                    </div>
                )}
            </div>
            {summary.candidates.length > 0 && (
                <ol className="mt-3 space-y-1 border-t border-border pt-2">
                    {summary.candidates.map((candidate, index) => (
                        <li key={candidate.model} className="flex items-center gap-2 text-xs">
                            <span className="w-4 shrink-0 text-muted-foreground">{index + 1}</span>
                            <span className="min-w-0 flex-1 truncate">{candidate.displayName}</span>
                            {candidate.familyName && (
                                <span className="shrink-0 text-[11px] text-muted-foreground">{candidate.familyName}</span>
                            )}
                            <span className="w-14 shrink-0 text-right font-mono text-[11px] text-muted-foreground">
                                {candidate.score.toFixed(3)}
                            </span>
                            <span className="w-12 shrink-0 text-right text-[11px] text-muted-foreground">
                                {formatProbability(candidate.probability)}
                            </span>
                        </li>
                    ))}
                </ol>
            )}
            <p className="mt-2 border-t border-border pt-2 text-[11px] leading-5 text-muted-foreground">
                这是参考库内的相对排名，不是「确认就是它」——库外的模型也会得到一个最像的答案。
                {summary.usedOutputs > 0 && `本次用了 ${summary.usedOutputs} 条有效回答。`}
                {summary.referenceBuiltAt && `参考库构建于 ${summary.referenceBuiltAt.slice(0, 10)}。`}
            </p>
        </div>
    );
}

/**
 * 被动检测开关卡。
 *
 * 摆在检测页而不是设置页，是因为它和主动检测是同一件事的两面，用户需要放在
 * 一起看才能理解差别：主动检测要发请求、烧 token；被动检测只读已经在流的日志，
 * 一分钱不花。参考项目里那些检测工具多多少少都要花钱，这里明确区分开。
 */
function PassiveProbeCard() {
    const { data: settings } = useSettingList();
    const setSetting = useSetSetting();
    const enabled = settings?.find((s) => s.key === SettingKey.LogPassiveProbeEnabled)?.value !== 'false';

    const toggle = (next: boolean) =>
        setSetting.mutate(
            { key: SettingKey.LogPassiveProbeEnabled, value: next ? 'true' : 'false' },
            {
                onSuccess: () => toast.success(next ? '被动检测已开启' : '被动检测已关闭'),
                onError: (e) =>
                    toast.error('保存失败', {
                        description: e instanceof Error ? e.message : String(e),
                    }),
            }
        );

    return (
        <div className="rounded-2xl border border-border bg-card px-4 py-3">
            <div className="flex items-start justify-between gap-3">
                <div className="min-w-0">
                    <p className="text-sm font-medium text-foreground">被动检测</p>
                    <p className="mt-0.5 text-xs leading-5 text-muted-foreground">
                        只读已经流过的日志，不发任何请求、不消耗 token。查的是账目对不对得上、分词器口径是否是同一家。
                    </p>
                </div>
                <Switch checked={enabled} onCheckedChange={toggle} disabled={setSetting.isPending} />
            </div>
            <p className="mt-2 border-t border-border pt-2 text-[11px] leading-5 text-muted-foreground">
                和上面的「立即检测」不同：那个会真的去打上游、花 token。被动检测永远不会。关掉它只是不再往日志里写检查结果，已有数据不受影响。
            </p>
        </div>
    );
}

export function ModelAudit() {
    const t = useTranslations('modelAudit');
    const { data: channelsData, isLoading } = useChannelList();
    const { data: logAnomalies } = useLogAnomalies();
    const { data: scheduledAudit } = useScheduledAudit();
    const runAudit = useRunModelAudit();
    // 后台队列入口：提交后立刻返回 job_id，进度与结果由队列页轮询呈现。
    // 这是「立即检测」现在的真实实现——不守着页面等。
    const enqueueAudit = useEnqueueAudit();
    // 渠道筛选：默认全部。未启用渠道要能被选到（先测后启用是常见流程），
    // 但默认不抢启用渠道的位置。
    const [channelFilter, setChannelFilter] = useState<'all' | 'enabled' | 'disabled'>('all');
    // 渠道来源：**启用与否都收**，但启用的一律排在前面。
    //
    // 原实现是 .filter((item) => item.enabled)，把未启用渠道整个藏掉。问题是
    // "未启用"只是不参与转发，不代表不能测——用户常常是先测通了再启用，藏起来
    // 等于逼人先去启用（那就真会被转发流量打到）。所以这里放开，靠排序和标签
    // 表达轻重：启用在前，未启用垫底并明确标注。
    //
    // 排序稳定：同组内保持接口原序，避免每次渲染顺序抖动。
    const channels = useMemo(() => {
        const all = (channelsData ?? []).map((item) => item.raw);
        const enabled = all.filter((item) => item.enabled);
        const disabled = all.filter((item) => !item.enabled);
        return [...enabled, ...disabled];
    }, [channelsData]);
    const enabledChannels = useMemo(() => channels.filter((item) => item.enabled), [channels]);
    const visibleChannels = useMemo(() => {
        if (channelFilter === 'enabled') return enabledChannels;
        if (channelFilter === 'disabled') return channels.filter((item) => !item.enabled);
        return channels;
    }, [channels, enabledChannels, channelFilter]);

    // 视图模式：single = 守着页面跑一次；queue = 批量入队后台跑、随时回看。
    // 后者是给"一次点一堆模型、不想守着"的场景准备的，检测结论独立落库。
    const [mode, setMode] = useState<'single' | 'queue'>('single');
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
    // 前端不再持有"队列"状态。
    //
    // 这里原本有一整套内存队列（queue / reports / runningModel / inFlight /
    // controllerRef / queueAlive），在浏览器里用 for 循环逐个发同步请求。它有三个
    // 治不好的毛病：页面一关就全丢、进度是前端自己编的不落库、没法回看历史。
    // 而后端队列（model_audit_jobs / model_audit_results）本来就落库、能回看、
    // 能取消、还有失败重试。所以整套删掉，只留"提交"这一个动作。

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
    // 卸载时不再需要 abort：请求已经交给后端队列，关页面不会让后台停下
    // （这正是"点了就走"的意义）。要停就去队列页取消任务。
    const applicableSelected = selected.filter((id) => isApplicable(channel, id, activeModel));
    const runCount = plan === 'quick' ? planIds('quick', channel, activeModel).length : applicableSelected.length;

    const rows = useMemo(() => {
        if (!report) return [];
        const ran = new Set((report.report.results ?? []).map((item) => item.probe_id));
        return orderedRows(
            PROBES.filter((meta) => ran.has(meta.id)).map((meta) => rowFromResult(meta, report, t)),
        );
    }, [report, t]);
    // 指纹结论单独读出来：它是唯一能认出模型的内容，不能只当一行状态显示。
    const fingerprint = useMemo(() => readFingerprint(report?.report), [report]);
    const warnCount = rows.filter((row) => row.tone === 'warn').length;
    const failCount = rows.filter((row) => row.tone === 'fail').length;
    const insufficientCount = rows.filter((row) => row.insufficient).length;
    const copy = report ? headline(report.report.verdict, warnCount, failCount, insufficientCount, t) : null;

    const chooseChannel = (id: number) => {
        setChannelId(id);
        const next = channels.find((item) => item.id === id);
        const nextModels = next ? getSelectedChannelModels(next) : [];
        setModelName(nextModels[0] ?? '');
        if (plan === 'standard') {
            setSelected(AUDIT_PROBE_IDS.filter((probeId) => isApplicable(next, probeId, nextModels[0] ?? '')));
        }
    };

    const choosePlan = (next: PlanId) => {
        setPlan(next);
        if (next === 'quick') setSelected(planIds('quick', channel, activeModel));
        else setSelected(AUDIT_PROBE_IDS.filter((id) => isApplicable(channel, id, activeModel)));
    };

    const toggleProbe = (id: AuditProbeId, checked: boolean) => {
        setPlan('standard');
        setSelected((current) => {
            if (checked) return AUDIT_PROBE_IDS.filter((item) => item === id || current.includes(item));
            return current.filter((item) => item !== id);
        });
    };

    // submitToQueue 把当前选中的模型提交到**后端真队列**，然后立刻返回。
    //
    // 这是「立即检测」与「批量检测」合并后的唯一入口：不管点一个还是一批，
    // 都是往队列里塞，然后用户可以关页面、切标签、隔天回来看。
    //
    // 为什么废弃前端那套内存队列：它只是"在浏览器里一个个发同步请求"，
    // 页面一关就全丢，进度也看不到历史——既不能回看，也不比守着页面强。
    // 后端队列本来就写库、能回看、能取消、还有重试，没有理由在前端再造一遍。
    const submitToQueue = async (targets: string[]) => {
        if (!channel) return;
        const names = targets.filter((name) => name.trim() !== '');
        if (names.length === 0) {
            toast.warning('先选要检测的模型');
            return;
        }
        const probes = selected.filter((id) => isApplicable(channel, id, names[0] ?? ''));
        if (probes.length === 0) {
            toast.warning('这个模型没有可用的检查项');
            return;
        }
        try {
            const res = await enqueueAudit.mutateAsync({
                channel_id: channel.id,
                models: names,
                probes,
            });
            // 提交成功就丢掉上一轮的报告。
            // 不清的话，用户切回详情看到的还是上一轮结论，还可能把它导出成
            // "这次的报告"——那是最糟的一种误导：数据没错，但归属错了。
            setReport(null);
            setPageState('idle');
            toast.success(`已提交 ${names.length} 个模型，后台开始检测（任务 #${res.job_id}）`);
            // 切到队列视图：让用户立刻看到它确实在跑，而不是"点了没反应"。
            setMode('queue');
        } catch (error) {
            toast.error(error instanceof Error ? error.message : '提交失败');
        }
    };

    // 原来的 enqueue / runQueue / removeFromQueue / clearQueue / cancel 已整块删除。
    //
    // 它们把"检测"实现成浏览器里的 for 循环 + 同步请求，是本次合并掉的另一半。
    // 取消现在走队列的 POST /jobs/:id/cancel —— 那才是真的让后台停下，
    // 而不是只 abort 掉当前这一个 HTTP 连接。

    // openStoredReport 把队列里存好的报告重新摊到详情面板上。
    //
    // 后台检测跑完不会自动弹结果——用户可能隔天才回来看。所以报告以
    // 「结果行的 report 字段（后端存的 AuditViewReport JSON）」为准，
    // 点开时现解析，而不是靠前端内存里留一份（那份早随页面关了没了）。
    //
    // 解析失败要明确报错，不能静默显示空报告：空报告会被读成"查过没问题"。
    const openStoredReport = (result: AuditResult) => {
        let view: AuditViewReport;
        try {
            view = JSON.parse(result.report || '{}') as AuditViewReport;
        } catch {
            toast.error('这份报告解析失败，可能已损坏');
            return;
        }
        if (!view || !Array.isArray(view.results)) {
            toast.error('这份报告不完整，无法展示');
            return;
        }
        setReport({
            channel_id: result.channel_id,
            channel_name: result.channel_name,
            upstream_model: result.upstream_model,
            endpoint: '',
            duration_ms: result.duration_ms,
            report: view,
        });
        setPageState('report');
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
                {/* 渠道多时原生 select 是平铺长列表：换 SearchableSelect 带内联模糊搜索
                    （名称/编号/模型名都可搜），键盘上下+回车提交。 */}
                <SearchableSelect
                    value={channel ? String(channel.id) : ''}
                    onValueChange={(value) => chooseChannel(Number(value))}
                    options={visibleChannels.map((item) => ({
                        value: String(item.id),
                        label: item.enabled ? item.name : `${item.name}（未启用）`,
                        // 搜索词带上"未启用"，让人能直接搜这一批，不用先切筛选。
                        keywords: `#${item.id} ${item.enabled ? '启用' : '未启用'} ${(item.selected_models ?? []).join(' ')}`,
                    }))}
                    placeholder={channels.length === 0 ? '暂无渠道' : '选择渠道'}
                    searchPlaceholder="搜渠道名 / 编号 / 模型…"
                    emptyText="没有匹配的渠道"
                    className="h-10 w-full"
                    disabled={visibleChannels.length === 0}
                />
                {/* 启用/未启用筛选。默认「全部」——既能看到未启用渠道，又不至于
                    让它们淹没常用的那几个。切成「仅启用」就是原来的行为。 */}
                <div className="mt-2 flex items-center gap-1 rounded-xl border border-border bg-muted/40 p-0.5 text-xs">
                    {([
                        ['all', `全部 ${channels.length}`],
                        ['enabled', `已启用 ${enabledChannels.length}`],
                        ['disabled', `未启用 ${channels.length - enabledChannels.length}`],
                    ] as const).map(([key, label]) => (
                        <button
                            key={key}
                            type="button"
                            onClick={() => setChannelFilter(key)}
                            className={cn(
                                'flex-1 rounded-lg px-2 py-1 transition-colors',
                                channelFilter === key ? 'bg-background font-medium text-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground',
                            )}
                        >
                            {label}
                        </button>
                    ))}
                </div>
                <label className="mb-1 mt-3 block text-xs text-muted-foreground">模型</label>
                <SearchableSelect
                    value={activeModel}
                    onValueChange={setModelName}
                    options={models.map((item) => ({ value: item, label: item }))}
                    placeholder={models.length === 0 ? '该渠道没有可选模型' : '选择模型'}
                    searchPlaceholder="搜模型名…"
                    emptyText="没有匹配的模型"
                    className="h-10 w-full"
                    disabled={models.length === 0}
                />
                {/* 旧的前端队列 UI 已删除。原来这里有「加入队列 / 全部加入」
                    两个按钮和一个逐项状态列表——它和后端真队列是两套东西，
                    用户在两边看到的进度并不一致。现在统一由下面的「后台检测」
                    提交到后端，进度和回看都在「检测记录」里。 */}
            </section>

            <section className="border-t border-border pt-4">
                <p className="mb-3 text-[11px] font-medium tracking-[0.16em] text-muted-foreground">02 检查</p>
                <div className="grid gap-1">
                    {PROBES.map((probe) => {
                        const disabled = !isApplicable(channel, probe.id, activeModel);
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
                {/* 唯一的执行入口：提交到后台队列后立刻返回。
                    点一个模型和点一批模型走的是同一条路——这就是把原来
                    「立即检测 / 批量检测」两个模式合并掉的结果。 */}
                <Button
                    type="button"
                    className="mt-4 h-11 w-full rounded-xl"
                    disabled={enqueueAudit.isPending || runCount === 0 || !channel}
                    onClick={() => void submitToQueue([activeModel])}
                >
                    {enqueueAudit.isPending ? <Loader2 className="size-4 animate-spin" /> : <Play className="size-4" />}
                    {enqueueAudit.isPending ? '提交中…' : `后台检测 · ${activeModel || '未选模型'}`}
                </Button>
                <Button
                    type="button"
                    variant="outline"
                    className="mt-2 h-9 w-full rounded-xl"
                    disabled={enqueueAudit.isPending || models.length === 0 || !channel}
                    onClick={() => void submitToQueue(models)}
                >
                    检测该渠道全部模型（{models.length}）
                </Button>
                <p className="mt-2 text-center text-[11px] leading-5 text-muted-foreground">
                    提交后可离开本页，进度和结果都在「检测记录」里
                </p>
                <p className="mt-2 text-center text-[11px] leading-5 text-muted-foreground">{t('manualGate', { count: runCount })}</p>
            </section>
        </aside>
    );

    return (
        <PageWrapper className="h-full min-h-0 space-y-4 overflow-y-auto overscroll-contain rounded-t-3xl pb-24 md:pb-4">
            <div className="flex flex-wrap items-end justify-between gap-3 px-1">
                <div>
                    <p className="text-[11px] font-medium tracking-[0.18em] text-muted-foreground">OCTOPUS</p>
                    <h2 className="text-2xl font-bold tracking-tight">模型检测</h2>
                    <p className="mt-1 text-sm text-muted-foreground">对渠道模型做一组轻量能力检查，结果仅作参考线索。</p>
                </div>
            </div>

            {/* 被动检测开关放在两种检测方式之前：它是"不花钱的那一半"，
                用户先理解这件事，后面点「立即检测」时才知道那个是要花钱的。 */}
            <PassiveProbeCard />

            {/* 两个入口是「发起检测」和「查看记录」，不是两种检测方式。
                以前这里是「立即检测 / 批量检测」并列，看起来像两种能力，实际上
                底层都打同一个上游、都花钱，区别只在等不等——所以合并成
                「去检测」和「看记录」，语义才对得上。 */}
            <div className="flex gap-1 rounded-2xl border border-border bg-card p-1">
                <button
                    type="button"
                    onClick={() => setMode('single')}
                    className={cn(
                        'flex-1 rounded-xl px-3 py-2 text-sm transition-colors',
                        mode === 'single' ? 'bg-primary text-primary-foreground' : 'text-muted-foreground hover:bg-muted',
                    )}
                >
                    发起检测
                </button>
                <button
                    type="button"
                    onClick={() => setMode('queue')}
                    className={cn(
                        'flex-1 rounded-xl px-3 py-2 text-sm transition-colors',
                        mode === 'queue' ? 'bg-primary text-primary-foreground' : 'text-muted-foreground hover:bg-muted',
                    )}
                >
                    检测记录
                </button>
            </div>

            {mode === 'queue' && <ModelAuditQueue onOpenReport={openStoredReport} />}

            {/* 单次模式的全部内容：未进入 queue 视图时才渲染，避免两个视图叠加。 */}
            {mode === 'single' && (
            <>
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
                                    {(logAnomalies.probe_findings ?? []).length > 0 && (
                                        <div className="mt-3 border-t border-border pt-3">
                                            <p className="text-xs font-medium text-foreground">账对不上的地方</p>
                                            <p className="mt-0.5 text-[11px] leading-5 text-muted-foreground">
                                                这些是日志里自相矛盾的事实，不需要额外请求就能看出来。命中不代表模型有问题，但值得去查。
                                            </p>
                                            <ul className="mt-2 space-y-2">
                                                {(logAnomalies.probe_findings ?? []).slice(0, 5).map((item) => (
                                                    <li key={`${item.channel_id}-${item.code}-${item.model}`} className="text-sm">
                                                        <span className={item.severity === 'medium' ? 'font-medium text-amber-600 dark:text-amber-400' : 'text-foreground'}>
                                                            {item.title}
                                                        </span>
                                                        <span className="mt-0.5 block text-xs text-muted-foreground">
                                                            {item.channel_name || `渠道 ${item.channel_id}`} · {item.model}
                                                            {item.total > 0 && ` · ${item.count}/${item.total} 条命中`}
                                                        </span>
                                                    </li>
                                                ))}
                                            </ul>
                                        </div>
                                    )}
                                    <p className="mt-2 text-[11px] leading-5 text-muted-foreground">{logAnomalies.disclaimer}</p>
                                    {logAnomalies.passive_observations?.length ? (
                                        <p className="mt-1 text-[11px] leading-5 text-muted-foreground">{t('passiveObserved', { count: logAnomalies.passive_observations.reduce((sum, bucket) => sum + bucket.count, 0) })}</p>
                                    ) : null}
                                </div>
                            )}
                            {scheduledAudit && (
                                <div className="mt-3 w-full max-w-lg rounded-2xl border border-border bg-muted/30 px-4 py-3 text-left">
                                    <p className="text-xs text-muted-foreground">
                                        历史主动检测记录 · {scheduledAudit.last_run_unix ? new Date(scheduledAudit.last_run_unix * 1000).toLocaleString('zh-CN') : '还没跑过'}
                                        {scheduledAudit.last_run_unix ? ` · 跑了 ${scheduledAudit.ran_count}/${scheduledAudit.target_count}` : ''}
                                    </p>
                                    {(scheduledAudit.results ?? []).length === 0 ? (
                                        <p className="mt-2 text-sm">后台自动检测已停用，不再定期打上游；仅手动检测会调用上游。</p>
                                    ) : (
                                        <ul className="mt-2 space-y-2">
                                            {(scheduledAudit.results ?? []).slice(0, 4).map((item) => (
                                                <li key={`${item.channel_id}-${item.model}-${item.trigger}`} className="text-sm">
                                                    <span className="font-medium">{triggerLabel(item.trigger, t)}：{item.skipped ? (item.skip_reason || '本轮跳过') : (item.verdict === 'none' ? '未见异常' : item.verdict === 'unknown' ? '证据不足' : '待复核')}</span>
                                                    <span className="mt-0.5 block text-xs text-muted-foreground">{item.channel_name || `渠道 ${item.channel_id}`} · {item.model}</span>
                                                </li>
                                            ))}
                                        </ul>
                                    )}
                                    <p className="mt-2 text-[11px] leading-5 text-muted-foreground">{scheduledAudit.disclaimer}</p>
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

                    {pageState === 'cancelled' && (
                        <div className="flex min-h-80 flex-col items-center justify-center text-center">
                            <XCircle className="mb-3 size-8 text-muted-foreground" />
                            <h3 className="text-lg font-semibold">{t('cancelledTitle')}</h3>
                            <p className="mt-1 max-w-md text-sm text-muted-foreground">{t('cancelledBody')}</p>
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
                            {fingerprint && <FingerprintCard summary={fingerprint} />}
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
                                                <span className="block truncate text-xs text-muted-foreground">{row.reason || row.meta.desc}</span>
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
            </>
            )}
        </PageWrapper>
    );
}
