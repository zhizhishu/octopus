'use client';

import { useEffect, useMemo, useRef, useState } from 'react';
import {
    AlertTriangle,
    Binary,
    ChevronRight,
    Dices,
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
    type ModelAuditResponse,
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

export function ModelAudit() {
    const t = useTranslations('modelAudit');
    const { data: channelsData, isLoading } = useChannelList();
    const { data: logAnomalies } = useLogAnomalies();
    const { data: scheduledAudit } = useScheduledAudit();
    const runAudit = useRunModelAudit();
    const channels = useMemo(
        () => (channelsData ?? []).map((item) => item.raw).filter((item) => item.enabled),
        [channelsData],
    );

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
    // 队列：一批要逐个检测的模型。串行执行——一次只有一个请求在飞，跑完一个才开始下一个。
    // 串行不是性能妥协，是刻意的：检测的每个探针都是真实计费请求，并发打同一渠道既容易被
    // 上游限流/风控，也会让"哪个模型出的问题"变得难判。配合后端单飞闸门，重复点击和并发
    // 都不会造成重复计费。
    const [queue, setQueue] = useState<AuditQueueItem[]>([]);
    // 队列里已完成模型的报告，按模型名索引；切换查看不影响正在跑的任务。
    const [reports, setReports] = useState<Record<string, ModelAuditResponse>>({});
    // 当前正在检测的模型名（用于在队列行上标"进行中"）；空串 = 没有在跑。
    const [runningModel, setRunningModel] = useState('');
    // 请求在飞行时为 true。disabled 只是视觉拦截，受渲染时序影响；
    // 这个 ref 是硬性单飞闸门，确保同一时刻只有一个检测任务。
    const inFlight = useRef(false);
    // 当前任务的取消句柄：取消/卸载时 abort，HTTP 断掉，服务端 c.Request.Context 随之停止后续探针。
    const controllerRef = useRef<AbortController | null>(null);
    // 队列是否还要继续往下跑。取消/清空时置 false，正在跑的请求 abort 后循环自然退出，
    // 不会"取消了还把剩下的模型接着跑完"。
    const queueAlive = useRef(false);

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
    // 组件卸载：中断仍在飞行的请求（避免后台继续占用/计费，也不残留 controller）。
    useEffect(() => () => { controllerRef.current?.abort(); }, []);
    const applicableSelected = selected.filter((id) => isApplicable(channel, id, activeModel));
    const runCount = plan === 'quick' ? planIds('quick', channel, activeModel).length : applicableSelected.length;

    const rows = useMemo(() => {
        if (!report) return [];
        const ran = new Set((report.report.results ?? []).map((item) => item.probe_id));
        return orderedRows(
            PROBES.filter((meta) => ran.has(meta.id)).map((meta) => rowFromResult(meta, report, t)),
        );
    }, [report, t]);
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

    // 把勾选的模型加入队列。同一渠道同一模型不重复入队——重复入队等于重复计费，
    // 这是唯一"多花钱"的入口，必须在这里挡住。
    const enqueue = (targets: string[]) => {
        if (!channel) return;
        const names = targets.filter((name) => name.trim() !== '');
        if (names.length === 0) {
            toast.warning('先选要检测的模型');
            return;
        }
        setQueue((current) => {
            // 已跑完(done/failed/skipped)的项允许再次入队——用户想复检同一个模型是正常的。
            // 只挡"还在队列里排队或正在跑"的重复项，那才是真的会重复花钱。
            const active = new Set(
                current.filter((item) => item.state === 'pending' || item.state === 'running').map((item) => item.key),
            );
            const added: AuditQueueItem[] = [];
            for (const name of names) {
                const key = `${channel.id}::${name}`;
                if (active.has(key)) continue;
                active.add(key);
                added.push({
                    key,
                    channelId: channel.id,
                    channelName: channel.name,
                    model: name,
                    state: 'pending',
                });
            }
            if (added.length === 0) {
                toast.info('这些模型已经在队列里了');
                return current;
            }
            // 复检时用新的 pending 项替换掉旧的同 key 记录，列表里不会同一个模型出现两行。
            const replaced = new Set(added.map((item) => item.key));
            return [...current.filter((item) => !replaced.has(item.key)), ...added];
        });
    };

    // 从队列里删掉一项。正在跑的那项不允许直接删——先取消，避免"界面删了但请求还在花钱"。
    const removeFromQueue = (key: string) => {
        setQueue((current) => {
            const target = current.find((item) => item.key === key);
            if (target?.state === 'running') {
                toast.warning('这一项正在检测，请先取消');
                return current;
            }
            return current.filter((item) => item.key !== key);
        });
    };

    // 清空队列。只清掉没在跑的项；正在跑的那项保留并提示先取消，理由同上。
    const clearQueue = () => {
        setQueue((current) => {
            if (current.some((item) => item.state === 'running')) {
                toast.warning('有任务正在检测，请先取消');
                return current;
            }
            return [];
        });
    };

    // 串行把队列跑完。一次只发一个请求：跑完一个才轮到下一个。
    // 取消时 queueAlive 置 false，当前请求 abort，循环立刻退出，后面的项标成 skipped
    // 而不是继续偷偷跑——取消必须是"真的不再花钱"。
    const runQueue = async () => {
        if (inFlight.current) return; // 连点/重复触发的硬闸门
        const pending = queue.filter((item) => item.state === 'pending');
        if (pending.length === 0) {
            toast.warning('队列里没有待检测的模型');
            return;
        }
        if (!channel) return;

        inFlight.current = true;
        queueAlive.current = true;
        setErrorText('');
        // 新一轮开始就把旧报告清掉：否则用户点了开始又立刻取消，界面上还挂着上一轮的
        // 结果，容易被当成这次的结果导出/引用。这条是上一轮明确修过的问题，队列改造
        // 时不能丢。report 只清显示的那份，reports 里已完成模型的历史仍保留，供队列行回看。
        setReport(null);
        setPageState('running');

        for (const item of pending) {
            if (!queueAlive.current) break;
            // 每个模型一个 controller：取消只影响当前这个，不会把队列状态弄乱。
            // 探针必须按**这一项自己的**渠道和模型算，不能借用外层当前选中的渠道：
            // 入队之后用户完全可能切到别的渠道去看，用错渠道算出来的探针集合会把
            // 模型不支持的项目也发上去（例如给非 Claude 模型发签名回放），白白报错花钱。
            const itemChannel = channels.find((entry) => entry.id === item.channelId);
            const itemProbes = plan === 'quick'
                ? planIds('quick', itemChannel, item.model)
                : selected.filter((id) => isApplicable(itemChannel, id, item.model));
            if (itemProbes.length === 0) {
                // 没有任何适用探针就别发请求——发了也是白花钱换一个"不适用"。
                setQueue((current) =>
                    current.map((entry) =>
                        entry.key === item.key
                            ? { ...entry, state: 'failed', error: '这个模型没有可用的检查项' }
                            : entry,
                    ),
                );
                continue;
            }
            const controller = new AbortController();
            controllerRef.current = controller;
            setRunningModel(item.model);
            setQueue((current) =>
                current.map((entry) => (entry.key === item.key ? { ...entry, state: 'running', error: undefined } : entry)),
            );
            try {
                const data = await runAudit.mutateAsync({
                    channel_id: item.channelId,
                    model: item.model,
                    probes: itemProbes,
                    signal: controller.signal,
                });
                if (controller.signal.aborted) {
                    // 取消：不当作成功、不落报告、不标 done。
                    setQueue((current) =>
                        current.map((entry) => (entry.key === item.key ? { ...entry, state: 'skipped' } : entry)),
                    );
                    break;
                }
                setReports((current) => ({ ...current, [item.key]: data }));
                setQueue((current) =>
                    current.map((entry) => (entry.key === item.key ? { ...entry, state: 'done' } : entry)),
                );
                // 每跑完一个就把报告切到它，用户能立刻看到结果，不必等整队跑完。
                setReport(data);
                setPageState('report');
            } catch (error) {
                if (controller.signal.aborted) {
                    setQueue((current) =>
                        current.map((entry) => (entry.key === item.key ? { ...entry, state: 'skipped' } : entry)),
                    );
                    break;
                }
                // 单个模型失败不中断整个队列：剩下的模型继续跑，失败项留在队列里带原因，
                // 用户可以单独重试。这比"一个失败全队作废"更省用户的钱和时间。
                const message = sanitizeChannelTestError(apiErrorMessage(error) || '审计未完成');
                setQueue((current) =>
                    current.map((entry) => (entry.key === item.key ? { ...entry, state: 'failed', error: message } : entry)),
                );
            } finally {
                if (controllerRef.current === controller) controllerRef.current = null;
            }
        }

        // 收尾：被取消时把还没轮到的项标成 skipped，让"取消"在界面上有明确交代。
        setQueue((current) =>
            current.map((entry) => (entry.state === 'pending' ? { ...entry, state: 'skipped' } : entry)),
        );
        const cancelled = !queueAlive.current;
        queueAlive.current = false;
        setRunningModel('');
        inFlight.current = false;
        // 只在被取消时把状态落回 idle。正常跑完必须保留 report —— 那正是用户要看的结论，
        // 之前这里无条件重置会把刚出的报告从界面上抹掉。
        if (cancelled) setPageState((prev) => (prev === 'running' || prev === 'cancelling' ? 'cancelled' : prev));
    };

    // 取消：先关掉队列闸门，再中断当前 HTTP。顺序很重要——先关闸门保证 abort 之后循环
    // 不会再去发下一个模型；服务端 c.Request.Context 随后停止剩余探针。取消与失败分开，
    // 不显示成功、不恢复旧报告、不自动重跑。
    const cancel = () => {
        queueAlive.current = false;
        if (!inFlight.current) return;
        setReport(null);            // 旧报告清空，避免导出
        setPageState('cancelling'); // 未 settle 前按钮保持 disabled/「正在取消」
        controllerRef.current?.abort();
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
                    options={channels.map((item) => ({
                        value: String(item.id),
                        label: item.name,
                        keywords: `#${item.id} ${(item.selected_models ?? []).join(' ')}`,
                    }))}
                    placeholder={channels.length === 0 ? '暂无启用渠道' : '选择渠道'}
                    searchPlaceholder="搜渠道名 / 编号 / 模型…"
                    emptyText="没有匹配的渠道"
                    className="h-10 w-full"
                    disabled={channels.length === 0}
                />
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
                {/* 多模型逐个检测：勾选多个后加入队列，队列串行执行。
                    为什么不做成"选一个跑一个"：中转站通常是"这个渠道到底是不是真货"的问题，
                    要一次看几个模型，逐个点太累；而并发又会同时打上游、容易被限流。
                    入队后由下面那条队列统一推进，一次只有一个请求在飞。 */}
                <div className="mt-2 flex items-center gap-2">
                    <Button
                        type="button"
                        variant="outline"
                        size="sm"
                        className="h-8 flex-1 rounded-lg text-xs"
                        disabled={models.length === 0 || pageState === 'running' || pageState === 'cancelling'}
                        onClick={() => enqueue([activeModel])}
                    >
                        加入队列
                    </Button>
                    <Button
                        type="button"
                        variant="outline"
                        size="sm"
                        className="h-8 flex-1 rounded-lg text-xs"
                        disabled={models.length === 0 || pageState === 'running' || pageState === 'cancelling'}
                        onClick={() => enqueue(models)}
                    >
                        全部加入
                    </Button>
                </div>
            </section>

            {queue.length > 0 && (
                <section className="border-t border-border pt-4">
                    <div className="mb-2 flex items-center justify-between">
                        <p className="text-[11px] font-medium tracking-[0.16em] text-muted-foreground">
                            03 队列 · {queue.filter((item) => item.state === 'pending').length} 待检 / 共 {queue.length}
                        </p>
                        <button
                            type="button"
                            className="text-[11px] text-muted-foreground underline-offset-2 hover:underline"
                            disabled={pageState === 'running' || pageState === 'cancelling'}
                            onClick={clearQueue}
                        >
                            清空
                        </button>
                    </div>
                    <ul className="grid gap-1">
                        {queue.map((item) => (
                            <li
                                key={item.key}
                                className={cn(
                                    'flex items-center gap-2 rounded-lg border border-border/60 px-2 py-1.5 text-xs',
                                    item.state === 'running' && 'border-primary/40 bg-primary/5',
                                    item.state === 'skipped' && 'opacity-50',
                                )}
                            >
                                {/* 已完成的项可点回看：队列跑完往往想比对几个模型，
                                    不必重新检测（重新检测是真的再花钱）。 */}
                                <button
                                    type="button"
                                    className="min-w-0 flex-1 truncate text-left disabled:cursor-default"
                                    title={item.state === 'done' ? '查看这个模型的报告' : item.model}
                                    disabled={item.state !== 'done'}
                                    onClick={() => {
                                        const saved = reports[item.key];
                                        if (!saved) return;
                                        setReport(saved);
                                        setPageState('report');
                                    }}
                                >
                                    {item.model}
                                </button>
                                <span className="shrink-0 text-[10px] text-muted-foreground">
                                    {item.state === 'pending' && '待检'}
                                    {item.state === 'running' && '检测中'}
                                    {item.state === 'done' && '完成'}
                                    {item.state === 'failed' && '失败'}
                                    {item.state === 'skipped' && '已跳过'}
                                </span>
                                {/* 正在跑的那项不给删：先取消，避免"界面删了但请求还在花钱"。 */}
                                <button
                                    type="button"
                                    className="shrink-0 text-muted-foreground hover:text-destructive disabled:opacity-30"
                                    disabled={item.state === 'running'}
                                    title={item.state === 'running' ? '正在检测，请先取消' : '从队列移除'}
                                    onClick={() => removeFromQueue(item.key)}
                                >
                                    ✕
                                </button>
                            </li>
                        ))}
                    </ul>
                    {queue.some((item) => item.state === 'failed') && (
                        <p className="mt-2 text-[11px] text-muted-foreground">
                            失败项：{queue.filter((item) => item.state === 'failed').map((item) => `${item.model}（${item.error ?? '未知原因'}）`).join('；')}
                        </p>
                    )}
                </section>
            )}

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
                <Button
                    type="button"
                    className="mt-4 h-11 w-full rounded-xl"
                    disabled={pageState === 'running' || pageState === 'cancelling' || runCount === 0 || queue.every((item) => item.state !== 'pending')}
                    onClick={() => void runQueue()}
                >
                    {pageState === 'running' || pageState === 'cancelling' ? <Loader2 className="size-4 animate-spin" /> : null}
                    {pageState === 'running'
                        ? `正在检查${runningModel ? ` · ${runningModel}` : ''}`
                        : pageState === 'cancelling'
                            ? t('cancelling')
                            : `开始检测队列（${queue.filter((item) => item.state === 'pending').length}）`}
                </Button>
                {(pageState === 'running' || pageState === 'cancelling') && (
                    <Button type="button" variant="outline" className="mt-2 h-9 w-full rounded-xl" disabled={pageState === 'cancelling'} onClick={cancel}>
                        {pageState === 'cancelling' ? t('cancelling') : t('cancelCheck')}
                    </Button>
                )}
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

            {/* 两种用法：立刻跑一次（守着结果）／批量排队（后台跑、可回看）。
                批量模式的结论独立落库，不受日志保留期影响。 */}
            <div className="flex gap-1 rounded-2xl border border-border bg-card p-1">
                <button
                    type="button"
                    onClick={() => setMode('single')}
                    className={cn(
                        'flex-1 rounded-xl px-3 py-2 text-sm transition-colors',
                        mode === 'single' ? 'bg-primary text-primary-foreground' : 'text-muted-foreground hover:bg-muted',
                    )}
                >
                    {t('modeSingle')}
                </button>
                <button
                    type="button"
                    onClick={() => setMode('queue')}
                    className={cn(
                        'flex-1 rounded-xl px-3 py-2 text-sm transition-colors',
                        mode === 'queue' ? 'bg-primary text-primary-foreground' : 'text-muted-foreground hover:bg-muted',
                    )}
                >
                    {t('modeQueue')}
                </button>
            </div>

            {mode === 'queue' && <ModelAuditQueue />}

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
