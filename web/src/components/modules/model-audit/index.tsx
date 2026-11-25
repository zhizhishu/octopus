'use client';

import { useEffect, useMemo, useRef, useState } from 'react';
import {
    AlertTriangle,
    TriangleAlert,
    CircleHelp,
    Check,
    Binary,
    ChevronRight,
    Database,
    Dices,
    Fingerprint,
    Hash,
    Loader2,
    Play,
    MessageSquareQuote,
    Radar,
    Repeat2,
    ScrollText,
    Signature,
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
    AUDIT_OPT_IN_PROBE_IDS,
    AUDIT_STANDARD_PROBE_IDS,
    useLogAnomalies,
    useSaveTokenizerBaseline,
    type AuditFinding,
    type AuditProbeId,
    type AuditProbeResult,
    type AuditVerdict,
    type AuditResult,
    type AuditViewReport,
    type ModelAuditResponse,
    useEnqueueAudit,
    useAuditPlans,
    useAuditBudget,
    useAuditJobs,
    type AuditJob,
    useEnqueueHlwyCompare,
    useHlwyBaselines,
    useHlwyPlan,
    useRecompareHlwy,
    useSaveHlwyBaselineFromResult,
    type HlwySampleCount,
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
import { highlightRows, provenanceView, TONE_DOT_CLASS } from '@/lib/audit-highlights';
import { getSelectedChannelModels } from '@/components/modules/channel/channel-utils';
import { sanitizeChannelTestError } from '@/lib/channel-test';
import { HlwyModePanel } from './HlwyModePanel';
import { MultiPicker } from './MultiPicker';
import { TasksView } from './TasksView';
import { PelicanTest } from './PelicanTest';
import {
    IDENTITY_GROUP,
    autoBaselineFor,
    groupModelsByProbes,
    isActiveJob,
    requestsFor,
    totalRequests,
    type AuditPreset,
} from './v6';
import { BaselineComparisonCard } from './BaselineComparisonCard';
import {
    downloadJson,
    hlwyBaselineOptionLabel,
    hlwyComparabilityLabel,
    hlwyExecutionLabel,
    hlwyExportFull,
    hlwyExportLegacy,
    hlwySaveEligibility,
    hlwyStartState,
    readHlwyComparison,
    readStoredReport,
} from './hlwy';
import { settingBool } from '@/lib/audit-budget';
import { Switch } from '@/components/ui/switch';
import { useSettingList, useSetSetting, SettingKey } from '@/api/endpoints/setting';

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

import { readFingerprint } from './fingerprint';
import { FingerprintCard } from './FingerprintCard';
import { ProvenanceCard } from './ProvenanceCard';
import { BackgroundAuditPanel } from './BackgroundAuditPanel';
import {
    readTokenizer,
    readTokenizerRecording,
    describeTokenizer,
    formatShare,
    tokenizerRowStatus,
    type TokenizerRecording,
    type TokenizerSummary,
} from './tokenizer';

const PROBES: ProbeMeta[] = [
    { id: 'liveness', name: '连通', hint: '1次', desc: '看渠道能不能正常回话', icon: Wifi },
    { id: 'identity', name: '自报身份', hint: '仅参考', desc: '问它自己是谁，口供可改，只作参考', icon: Fingerprint },
    // 复述系统提示词：问它收到的系统提示词长什么样。中转站常在系统提示词里写别家平台的名字，
    // 这比「自报身份」难改，但同样只是线索（提示词可以被删掉再重写）。
    { id: 'system_prompt', name: '复述系统提示词', hint: '1次', desc: '让模型复述它收到的系统提示词，看里面有没有别家平台的字样', icon: ScrollText },
    { id: 'glitch', name: '生僻词反应', hint: '仅参考', desc: '看生僻词反应像哪家，只作参考', icon: Binary },
    { id: 'token_delta', name: '用量虚报', hint: '2次', desc: '核对上游自报的输入用量是否虚高', icon: Radar },
    { id: 'echo_rewrite', name: '回复改写', hint: '1次', desc: '检查指定文本是否被原样回传', icon: Repeat2 },
    { id: 'context_canary', name: '长内容截断', hint: '1次', desc: '确认长上下文尾部标记未被截断', icon: TextQuote },
    { id: 'signature', name: 'Claude 签名', hint: '仅Claude', desc: '检查思考块是否附带可验证签名', icon: Signature },
    // 随机数指纹：要发三道题、每题几百个整数，是整套检测里最贵的一条，
    // 所以单独标"高开销"，让用户在勾选前就知道代价。
    { id: 'fingerprint', name: '模型指纹', hint: '3次', desc: '按取值习惯比对参考库，看最像谁', icon: Dices },
    // 以下两项只能手动勾选（AUDIT_OPT_IN_PROBE_IDS），不进「全面」方案。
    // 分词器识别默认拿内置 lm-detector 分词器库当基线，不需要先录基线。
    { id: 'tokenizer_id', name: '生僻词反应（分词器）', hint: '约12次', desc: '对照内置分词器库看上游用哪套；只作参考，不计分', icon: Hash },
    // 录基线放在「02 检查」底下默认收起的高级区，主列表里不出现（见 ADVANCED_PROBE_IDS）。
    { id: 'tokenizer_baseline', name: '录一份正品基线', hint: '74次', desc: '在正品渠道上录分词器基线；后台无法判断渠道真假', icon: Database },
];

/** 只在高级区里出现的检查项：主检测列表不渲染它们。 */
const ADVANCED_PROBE_IDS: readonly AuditProbeId[] = ['tokenizer_baseline'];
const ADVANCED_SUMMARY = '高级：用你确定是正品的渠道录一份自己的基线（库里没有的新模型才需要）';
const ADVANCED_TOOLTIP =
    '分词器识别默认对照内置库，不录也能用。库里没有的新型号（如 Claude、Grok）才需要：在确定是正品的渠道上发 74 次请求录一份，到检测记录里保存后，之后的分词器识别会一起对照。后台没法判断渠道真假，录错会把冒牌当正品。';

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
    // 分词器识别只作参考（照抄 lm-detector）：后端 0 分，这里也不进「待复核」。
    if (meta.id === 'tokenizer_id' && result?.ok) {
        const summary = readTokenizer(report.report);
        return { meta, tone: 'info', status: summary ? tokenizerRowStatus(summary) : '仅供参考', result, findings };
    }
    const scored = findings.filter((item) => (item.score ?? 0) > 0);
    if (scored.some((item) => item.severity === 'medium' || item.severity === 'high')) {
        return { meta, tone: 'warn', status: '待复核', result, findings: scored };
    }
    if (scored.length > 0) {
        return { meta, tone: 'warn', status: '待复核', result, findings: scored };
    }
    // 仅有 0 分参考项：展示但不进复核。
    if (findings.length > 0) {
        return { meta, tone: 'info', status: '仅参考', result, findings };
    }
    // 录制本身不是检查：只说明录没录成，不进 pass/warn。
    if (meta.id === 'tokenizer_baseline' && result?.ok) {
        return {
            meta,
            tone: 'info',
            status: data.recordable === true ? '已录制，可保存为基线' : '不可录制',
            result,
            findings,
            reason: typeof data.reason === 'string' ? data.reason : undefined,
        };
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
 * 分词器结论卡（lm-detector 分词器差分探测）。
 *
 * 测出来的是「上游用哪套分词器」，不是「哪个模型」：同一分词器常被多个模型共用，
 * 所以这里只说相容/不符，不说"就是某模型"。测成 o200k/cl100k 时要提示中转站本地估算的可能。
 */
function TokenizerCard({ summary }: { summary: TokenizerSummary }) {
    const share = formatShare(summary.confidence);
    const claimTone =
        summary.claim === 'inconsistent' && summary.usable
            ? 'bg-amber-500/15 text-amber-700 dark:text-amber-400'
            : summary.claim === 'consistent' && summary.usable
              ? 'bg-primary/10 text-primary'
              : 'bg-muted text-muted-foreground';
    const claimText =
        !summary.usable ? '仅供参考'
        : summary.claim === 'consistent' ? '与声明相容'
        : summary.claim === 'inconsistent' ? '与声明不符（仅供参考）'
        : summary.claim === 'uncertain' ? '不确定'
        : '无法核对';
    return (
        <div className="rounded-2xl border border-border bg-card px-4 py-3">
            <div className="flex flex-wrap items-start justify-between gap-3">
                <div className="min-w-0">
                    <p className="text-[11px] font-medium tracking-[0.16em] text-muted-foreground">分词器识别 · 输入 token 差分</p>
                    <p className="mt-1 flex flex-wrap items-baseline gap-2">
                        <span className="text-lg font-semibold">{summary.errorText ? '未测出' : summary.series || '库内无匹配'}</span>
                        {summary.lab && <span className="rounded bg-muted px-1.5 py-0.5 text-[11px] text-muted-foreground">{summary.lab}</span>}
                        {summary.local && <span className="rounded bg-muted px-1.5 py-0.5 text-[11px] text-muted-foreground">本地基线</span>}
                        <span className={cn('rounded px-1.5 py-0.5 text-[11px]', claimTone)}>{claimText}</span>
                    </p>
                    <p className="mt-0.5 text-xs leading-5 text-muted-foreground">{describeTokenizer(summary)}</p>
                </div>
                {share && !summary.errorText && (
                    <div className="shrink-0 rounded-xl bg-muted/50 px-3 py-2 text-center">
                        <p className="text-[11px] text-muted-foreground">后验</p>
                        <p className="mt-0.5 text-lg font-semibold">{share}</p>
                    </div>
                )}
            </div>
            {summary.candidates.length > 0 && (
                <ol className="mt-3 space-y-1 border-t border-border pt-2">
                    {summary.candidates.map((candidate, index) => (
                        <li key={candidate.id} className="flex items-center gap-2 text-xs">
                            <span className="w-4 shrink-0 text-muted-foreground">{index + 1}</span>
                            <span className="min-w-0 flex-1 truncate">{candidate.series}</span>
                            {candidate.lab && <span className="shrink-0 text-[11px] text-muted-foreground">{candidate.lab}</span>}
                            <span className="w-12 shrink-0 text-right text-[11px] text-muted-foreground">{formatShare(candidate.exact)}</span>
                        </li>
                    ))}
                </ol>
            )}
            <p
                className="mt-2 truncate border-t border-border pt-2 text-[11px] leading-5 text-muted-foreground"
                title={[
                    '也可能是中转站自己估算的用量。',
                    summary.requests > 0 ? `本次 ${summary.requests} 次请求。` : '',
                    summary.bankClasses > 0 ? `对照分词器库 ${summary.bankClasses} 类${summary.bankGeneratedAt ? `（${summary.bankGeneratedAt}）` : ''}。` : '',
                    ...summary.warnings,
                ].filter(Boolean).join('\n')}
            >
                {TOKENIZER_FOOTNOTE}
                {summary.warnings.length > 0 && ` 跳过了 ${summary.warnings.length} 条本地基线。`}
            </p>
        </div>
    );
}

const TOKENIZER_FOOTNOTE = '只作参考：同一分词器常被多个模型共用，测得一致不等于原厂。';
const RECORDING_SUMMARY = '高级：这次录的基线，确定渠道是正品再保存';
const RECORDING_TOOLTIP = '保存后，之后的分词器识别会把它和内置库一起对照。后台没法判断渠道真假，录错会把冒牌当正品。';

/** 录制结果卡：默认收起；可录制时给「保存为可信基线」按钮（需要队列结果 id）。 */
function TokenizerRecordingCard({ recording, resultId }: { recording: TokenizerRecording; resultId?: number }) {
    const save = useSaveTokenizerBaseline();
    const [saved, setSaved] = useState(false);
    const submit = async (replace: boolean) => {
        if (!resultId) return;
        try {
            await save.mutateAsync({ result_id: resultId, replace });
            setSaved(true);
            toast.success('已保存为可信基线，之后的分词器识别会用到它');
        } catch (error) {
            const message = error instanceof Error ? error.message : '保存失败';
            if (!replace && message.includes('已有')) {
                if (window.confirm(`${message}。要用这次录制覆盖吗？`)) await submit(true);
                return;
            }
            toast.error(message);
        }
    };
    return (
        <details className="rounded-2xl border border-border bg-card px-4 py-3">
            <summary className="cursor-pointer text-[11px] leading-5 text-muted-foreground" title={RECORDING_TOOLTIP}>
                {RECORDING_SUMMARY}
            </summary>
            <p className="mt-1 text-sm">
                {recording.recordable
                    ? `${recording.targetModel || '该型号'}：${recording.probeCount} 道题计数已录下。`
                    : `这次录制不能用作基线：${recording.reason || '未知原因'}`}
            </p>
            {recording.recordable && (
                <div className="mt-2">
                    {resultId ? (
                        <button
                            type="button"
                            disabled={save.isPending || saved}
                            onClick={() => void submit(false)}
                            className="rounded-lg border border-border px-3 py-1.5 text-xs hover:bg-muted disabled:opacity-50"
                        >
                            {saved ? '已保存' : save.isPending ? '保存中…' : '保存为可信基线'}
                        </button>
                    ) : (
                        <p className="text-[11px] text-muted-foreground">从「检测记录」打开这条结果才能保存。</p>
                    )}
                </div>
            )}
        </details>
    );
}

/**
 * 被动检测开关卡。
 *
 * 摆在检测页而不是设置页，是因为它和主动检测是同一件事的两面，用户需要放在
 * 一起看才能理解差别：主动检测要发请求、烧 token；被动检测只读已经在流的日志，
 * 一分钱不花。参考项目里那些检测工具多多少少都要花钱，这里明确区分开。
 */
/** 窗口小时数 → 人话「近 N 天」。不足一天也按 1 天说。 */
function windowDays(hours: number | undefined): number {
    if (!hours || hours <= 0) return 7;
    return Math.max(1, Math.round(hours / 24));
}

/**
 * 指纹题目：随机出题（默认）还是固定 36 题。随机题防止中转站认出公开的固定题；
 * 固定题留作对照。只换题目文字，出站形状一模一样。
 */
function FingerprintPromptToggle() {
    const { data: settings } = useSettingList();
    const setSetting = useSetSetting();
    const random = settings?.find((s) => s.key === SettingKey.ModelAuditFingerprintRandomPrompts)?.value !== 'false';
    const toggle = (next: boolean) =>
        setSetting.mutate(
            { key: SettingKey.ModelAuditFingerprintRandomPrompts, value: next ? 'true' : 'false' },
            {
                onSuccess: () => toast.success(next ? '指纹改为随机出题' : '指纹改为固定题（对照）'),
                onError: (e) => toast.error('保存失败', { description: e instanceof Error ? e.message : String(e) }),
            },
        );
    return (
        <div className="ml-6 flex items-center justify-between gap-2 rounded-lg bg-muted/40 px-2 py-1.5 text-[11px] text-muted-foreground">
            <span className="min-w-0">{random ? '随机出题：每次换语言和措辞，防被认出' : '固定题：对照用，题目公开'}</span>
            <Switch checked={random} onCheckedChange={toggle} disabled={setSetting.isPending} aria-label="指纹随机出题" />
        </div>
    );
}

function PassiveProbeCard({ onOpenFindings }: { onOpenFindings?: () => void } = {}) {
    const { data: settings } = useSettingList();
    const { data: logAnomalies } = useLogAnomalies();
    const setSetting = useSetSetting();
    const enabled = settings?.find((s) => s.key === SettingKey.LogPassiveProbeEnabled)?.value !== 'false';
    const probeN = (logAnomalies?.probe_findings ?? []).length + (logAnomalies?.findings ?? []).length;
    const days = windowDays(logAnomalies?.window_hours);
    const disclaimer =
        logAnomalies?.disclaimer
        || '检查结果提供参考，不等同模型真实性证明。输出塌陷、延迟变形、回显不符都只是线索。';

    const toggle = (next: boolean) =>
        setSetting.mutate(
            { key: SettingKey.LogPassiveProbeEnabled, value: next ? 'true' : 'false' },
            {
                onSuccess: () => toast.success(next ? '日常监测已开启' : '日常监测已关闭'),
                onError: (e) =>
                    toast.error('保存失败', {
                        description: e instanceof Error ? e.message : String(e),
                    }),
            }
        );

    return (
        <div className="rounded-3xl border border-border bg-card p-5">
            <div className="flex items-start justify-between gap-3">
                <div className="min-w-0">
                    <p className="flex items-center gap-1.5 text-sm font-semibold text-foreground">
                        日常监测
                        <span className="rounded-full bg-primary/12 px-1.5 py-px text-[10px] font-medium text-primary">免费 · {enabled ? '开着' : '关着'}</span>
                        <span
                            className="cursor-help text-muted-foreground/80"
                            title={disclaimer}
                            aria-label={disclaimer}
                        >
                            ⓘ
                        </span>
                    </p>
                    <p className="mt-0.5 text-xs leading-5 text-muted-foreground">
                        不额外花请求，只看平时转发留下的线索（用量对不上、上游自报的模型名）。
                    </p>
                </div>
                <Switch checked={enabled} onCheckedChange={toggle} disabled={setSetting.isPending} />
            </div>
            {enabled && logAnomalies?.too_few_samples && (
                <p className="mt-2 rounded-xl bg-muted/50 px-3 py-2 text-xs text-muted-foreground">
                    近 {days} 天调用太少，暂时看不出问题
                </p>
            )}
            {enabled && !logAnomalies?.too_few_samples && probeN > 0 && (
                <button
                    type="button"
                    onClick={onOpenFindings}
                    className="mt-2 flex w-full items-center justify-between rounded-xl bg-amber-500/10 px-3 py-2 text-left text-xs text-amber-800 dark:text-amber-300"
                >
                    <span>⚠ 近 {days} 天发现 {probeN} 处用量对不上</span>
                    <ChevronRight className="size-4" />
                </button>
            )}
        </div>
    );
}

function recentJobLabel(job: AuditJob): { title: string; sub: string; status: string; tone: 'ok' | 'warn' | 'ref' | 'run' } {
    const running = job.status === 'running' || job.status === 'queued';
    if (running) {
        return {
            title: `${job.channel_name || '渠道'} · 全部 ${job.total} 个`,
            sub: '',
            status: `${job.finished}/${job.total}`,
            tone: 'run',
        };
    }
    const model = job.single_model || (job.total > 1 ? `${job.total} 个模型` : '检测');
    const headline = job.plain_headline || '';
    let tone: 'ok' | 'warn' | 'ref' = 'ok';
    let status = headline || '已完成';
    if (/可疑|复核/.test(headline) || job.single_verdict === 'medium' || job.single_verdict === 'high') {
        tone = 'warn';
        status = headline.includes('可疑') ? '有可疑点' : (headline || '有可疑点');
    } else if (/不够|没测成|失败|没法判断/.test(headline) || job.single_status === 'failed' || job.single_verdict === 'unknown') {
        tone = 'ref';
        status = /不够|没法判断/.test(headline) ? '数据不够' : (headline || '没测成');
    } else if (/没发现|正常/.test(headline) || job.single_verdict === 'none' || job.single_verdict === 'low') {
        tone = 'ok';
        status = '没发现问题';
    }
    const when = job.report_at || job.ended_at || job.created_at;
    let whenLabel = '';
    if (when) {
        const d = new Date(when);
        const now = new Date();
        const sameDay = d.toDateString() === now.toDateString();
        whenLabel = sameDay
            ? d.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit', hour12: false })
            : '昨天';
    }
    return {
        title: model,
        sub: [job.channel_name, whenLabel].filter(Boolean).join(' · '),
        status,
        tone,
    };
}

function RecentAudits({ onOpenAll, onOpenJob }: { onOpenAll: () => void; onOpenJob?: (jobId: number) => void }) {
    const { data: jobs } = useAuditJobs(8);
    const items = (jobs ?? []).slice(0, 4);
    if (items.length === 0) return null;
    return (
        <section className="rounded-3xl border border-border bg-card p-5">
            <div className="flex items-center justify-between">
                <h3 className="text-sm font-semibold">最近检测</h3>
                <button
                    type="button"
                    onClick={onOpenAll}
                    className="flex items-center gap-0.5 text-xs text-muted-foreground hover:text-foreground"
                >
                    全部记录
                    <ChevronRight className="size-3.5" />
                </button>
            </div>
            <ul className="mt-3 space-y-1">
                {items.map((job) => {
                    const row = recentJobLabel(job);
                    const pct = job.total > 0 ? Math.min(100, Math.round((job.finished / job.total) * 100)) : 0;
                    return (
                        <li key={job.id} className="flex items-center gap-2.5 rounded-xl px-2 py-2">
                            <span className={cn(
                                'flex size-6 shrink-0 items-center justify-center rounded-full',
                                row.tone === 'ok' && 'bg-primary/15 text-primary',
                                row.tone === 'warn' && 'bg-amber-500/15 text-amber-700 dark:text-amber-300',
                                row.tone === 'ref' && 'bg-muted text-muted-foreground',
                                row.tone === 'run' && 'bg-primary/10 text-primary',
                            )}>
                                {row.tone === 'run' && <Loader2 className="size-3.5 animate-spin" />}
                                {row.tone === 'ok' && <Check className="size-3.5" />}
                                {row.tone === 'warn' && <TriangleAlert className="size-3.5" />}
                                {row.tone === 'ref' && <CircleHelp className="size-3.5" />}
                            </span>
                            <div className="min-w-0 flex-1">
                                <p className={cn('truncate text-sm', job.single_model && 'font-mono text-[13px]')}>{row.title}</p>
                                {row.tone === 'run' ? (
                                    <div className="mt-1 h-1 overflow-hidden rounded-full bg-muted">
                                        <div className="h-full rounded-full bg-primary" style={{ width: `${pct}%` }} />
                                    </div>
                                ) : (
                                    <p className="truncate text-xs text-muted-foreground">{row.sub}</p>
                                )}
                            </div>
                            <span className={cn(
                                'shrink-0 text-xs',
                                row.tone === 'ok' && 'text-primary',
                                row.tone === 'warn' && 'text-amber-700 dark:text-amber-300',
                                row.tone === 'ref' && 'text-muted-foreground',
                                row.tone === 'run' && 'text-muted-foreground',
                            )}>
                                {row.status}
                            </span>
                            {onOpenJob && row.tone !== 'run' && (
                                <button
                                    type="button"
                                    onClick={() => onOpenJob(job.id)}
                                    className="flex shrink-0 items-center text-xs font-medium text-primary hover:underline"
                                >
                                    报告
                                    <ChevronRight className="size-3" />
                                </button>
                            )}
                        </li>
                    );
                })}
            </ul>
        </section>
    );
}

export function ModelAudit() {
    const t = useTranslations('modelAudit');
    const { data: channelsData, isLoading } = useChannelList();
    // 后台队列入口：提交后立刻返回 job_id，进度与结果在「任务」里看。
    const enqueueAudit = useEnqueueAudit();
    // 顶部三栏的「任务」徽标：有几个在跑/在排队。
    const { data: allJobs } = useAuditJobs(50);
    const activeJobCount = (allJobs ?? []).filter(isActiveJob).length;
    const { data: budget } = useAuditBudget();
    // 渠道筛选：默认全部。未启用渠道要能被选到（先测后启用是常见流程），
    // 但默认不抢启用渠道的位置。
    const [channelFilter, setChannelFilter] = useState<'all' | 'enabled' | 'disabled'>('all');
    // 渠道来源：**启用与否都收**，但启用的一律排在前面（未启用只是不参与转发，不代表不能测）。
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

    // 顶部三栏：检测（左参数右结果）/ 任务（进度 + 记录）/ 鹈鹕测试。
    const [tab, setTab] = useState<'test' | 'tasks' | 'pelican'>('test');
    const [channelId, setChannelId] = useState<number>(0);
    // 模型可以多选；空数组 = 默认用这个渠道的第一个模型。
    const [modelNames, setModelNames] = useState<string[]>([]);
    const [preset, setPreset] = useState<AuditPreset>('full');
    // 自定义里勾的检查项（连通会自动带上，不用勾）。
    const [selected, setSelected] = useState<AuditProbeId[]>([...AUDIT_STANDARD_PROBE_IDS]);
    const [pageState, setPageState] = useState<PageState>('idle');
    const [report, setReport] = useState<ModelAuditResponse | null>(null);
    const [errorText] = useState('');
    const [openRow, setOpenRow] = useState<DisplayRow | null>(null);
    // 数字习惯（hlwy 对照）：只在「自定义」里能勾。它换的是问题本身（同一题问 100/200 次），
    // 所以和探针分开记：提交时是另一种任务，一个模型一单。
    const [hlwyChecked, setHlwyChecked] = useState(false);
    // 手动选的对比基准；空 = 自动（同模型优先自录，没有用 hlwy 内置）。
    const [hlwyBaselineId, setHlwyBaselineId] = useState('');
    const [hlwySamples, setHlwySamples] = useState<HlwySampleCount>(100);
    const [hlwyPanelOpen, setHlwyPanelOpen] = useState(false);
    // 「再跑一轮」填回去的那个渠道 id：渠道被删以后，靠它把「现在选的已经不是那个渠道」说出口。
    const [rerunChannelId, setRerunChannelId] = useState<number | null>(null);
    // 换基准重算的结果：只在内存里预览，不覆盖存下来的原报告。
    const [recompare, setRecompare] = useState<{ baselineId: string; report: Record<string, unknown> } | null>(null);
    // 放到后台跑：检测本来就在服务器队列里跑，关页面不会停。不勾 = 提交后直接跳到「任务」看着它跑。
    const [background, setBackground] = useState(true);
    // 从「最近检测」点「报告」：跳到任务页并展开那一条。
    const [focusJobId, setFocusJobId] = useState<number | null>(null);
    const [submitting, setSubmitting] = useState(false);
    // 当前正在看的结果 id。用来丢掉「换了报告以后才回来」的重算响应。
    const reportResultIdRef = useRef<number | null>(null);

    const channel = channels.find((item) => item.id === channelId) ?? channels[0];
    const models = useMemo(
        () => (channel ? getSelectedChannelModels(channel) : []),
        [channel],
    );
    // 真正要测的模型：选了的里面还在这个渠道上的；一个没选就测第一个。
    const targetModels = useMemo(() => {
        const picked = modelNames.filter((name) => models.includes(name));
        if (picked.length > 0) return picked;
        return models[0] ? [models[0]] : [];
    }, [modelNames, models]);
    const activeModel = targetModels[0] ?? '';

    useEffect(() => {
        if (!channel) return;
        if (channelId !== channel.id) setChannelId(channel.id);
    }, [channel, channelId]);
    // 从渠道页芯片点过来：自动打开任务页。
    useEffect(() => {
        try {
            const store = typeof window !== 'undefined' ? window.sessionStorage : null;
            const job = store?.getItem('octopus.audit.openJob');
            if (!job) return;
            store?.removeItem('octopus.audit.openJob');
            store?.removeItem('octopus.audit.openResult');
            const id = Number(job);
            if (Number.isInteger(id) && id > 0) setFocusJobId(id);
            setTab('tasks');
            setPageState('idle');
        } catch {
            // 测试环境可能没有 sessionStorage。
        }
    }, []);

    // 每个模型各自的计划（零请求）：多选时「约 N 次」按各自的计划加起来。
    const planQueries = useAuditPlans(channel?.id ?? 0, targetModels, !!channel);
    const planOf = (name: string) => planQueries[targetModels.indexOf(name)]?.data;

    /** 某一档对某个模型要跑的检查项（不适用的已经去掉）。 */
    const idsFor = (which: AuditPreset, name: string): AuditProbeId[] => {
        if (which === 'quick') return QUICK_IDS.filter((id) => isApplicable(channel, id, name));
        if (which === 'full') return AUDIT_STANDARD_PROBE_IDS.filter((id) => isApplicable(channel, id, name));
        const picked = AUDIT_PROBE_IDS.filter((id) => selected.includes(id) && id !== 'liveness' && isApplicable(channel, id, name));
        // 连通每次都带上：先确认通了，后面「没跑成」才说得清是谁的问题。
        return picked.length > 0 ? ['liveness', ...picked] : [];
    };
    const quickReq = totalRequests(targetModels, planOf, (name) => idsFor('quick', name));
    const fullReq = totalRequests(targetModels, planOf, (name) => idsFor('full', name));
    const probeReq = totalRequests(targetModels, planOf, (name) => idsFor('custom', name));
    const quickCount = idsFor('quick', activeModel).length;
    const fullCount = idsFor('full', activeModel).length;
    const isClaudeTarget = targetModels.some((name) => isApplicable(channel, 'signature', name));
    /** 自定义列表里每一行的请求数（按第一个模型的计划）。 */
    const rowReq = (ids: readonly AuditProbeId[]) => requestsFor(planOf(activeModel), ids.filter((id) => isApplicable(channel, id, activeModel)));

    /* ---------------- 数字习惯（hlwy 基准对照） ---------------- */
    const hlwyOn = preset === 'custom' && hlwyChecked;
    const { data: hlwyCatalog } = useHlwyBaselines();
    const enqueueHlwy = useEnqueueHlwyCompare();
    const recompareHlwy = useRecompareHlwy();
    const saveHlwyBaseline = useSaveHlwyBaselineFromResult();
    const { data: hlwySettings } = useSettingList();
    const hlwyFlagEnabled = settingBool(
        hlwySettings?.find((item) => item.key === 'model_audit_hlwy_enabled')?.value,
        false,
    );
    const catalogEntries = hlwyCatalog?.baselines ?? [];
    const autoBaseline = autoBaselineFor(activeModel, catalogEntries);
    // 计划和按钮看的是「第一个模型这一单」用哪份基准；多选时每个模型各自自动挑。
    const effectiveBaselineId = hlwyBaselineId || autoBaseline?.id || '';
    const hlwyPlanQuery = useHlwyPlan({
        channelId: channel?.id ?? 0,
        model: activeModel,
        samples: hlwySamples,
        baselineId: effectiveBaselineId,
        // 没勾数字习惯就不读：读一份不用的计划也是白多一次往返。
        enabled: hlwyOn,
    });
    const hlwyPlan = hlwyPlanQuery.data ?? null;
    const hlwyPlanError = hlwyPlanQuery.error
        ? hlwyPlanQuery.error instanceof Error
            ? hlwyPlanQuery.error.message
            : String(hlwyPlanQuery.error)
        : '';
    const hlwySelectedEntry = catalogEntries.find((entry) => entry.id === effectiveBaselineId) ?? null;
    // 「再跑一轮」把上一轮的渠道填回来，但那个渠道可能已经被删掉了：不悄悄换一个渠道花钱。
    const rerunChannelMissing =
        rerunChannelId !== null && channels.length > 0 && !channels.some((item) => item.id === rerunChannelId);
    const rerunChannelMissingReason = rerunChannelMissing
        ? `上一轮用的渠道（#${rerunChannelId}）已经不在渠道列表里了，请重新选一个渠道再提交`
        : '';
    const hlwyStart = hlwyStartState({
        hasTarget: !!channel && !!activeModel,
        channelMissing: rerunChannelMissingReason,
        baselineId: effectiveBaselineId,
        baselineUnavailable: hlwySelectedEntry?.unavailable === true,
        flagEnabled: hlwyFlagEnabled,
        plan: hlwyPlan,
        planError: hlwyPlanError,
        planLoading: hlwyPlanQuery.isLoading === true,
        pending: enqueueHlwy.isPending,
    });
    // 数字习惯每个模型一单，最多 hlwyPlan.max_calls 次（没读到计划就按采样次数估）。
    const hlwyReq = hlwyOn ? (hlwyPlan?.max_calls ?? hlwySamples) * targetModels.length : 0;
    const customReq = probeReq + hlwyReq;
    const totalReq = preset === 'quick' ? quickReq : preset === 'full' ? fullReq : customReq;
    const probeGroups = groupModelsByProbes(targetModels, (name) => idsFor(preset, name));
    const nothingToRun = probeGroups.length === 0 && !hlwyOn;
    const startDisabled =
        submitting || enqueueAudit.isPending || !channel || targetModels.length === 0 || nothingToRun || (hlwyOn && hlwyStart.disabled) || !!rerunChannelMissingReason;
    const startReason = !channel
        ? '先选渠道'
        : rerunChannelMissingReason
          ? rerunChannelMissingReason
          : nothingToRun
            ? '至少勾一项检查'
            : hlwyOn && hlwyStart.disabled
              ? hlwyStart.reason
              : '';

    const rows = useMemo(() => {
        if (!report) return [];
        const ran = new Set((report.report.results ?? []).map((item) => item.probe_id));
        return orderedRows(
            PROBES.filter((meta) => ran.has(meta.id)).map((meta) => rowFromResult(meta, report, t)),
        );
    }, [report, t]);
    // 指纹结论单独读出来：它是唯一能认出模型的内容，不能只当一行状态显示。
    const fingerprint = useMemo(() => readFingerprint(report?.report), [report]);
    const tokenizer = useMemo(() => readTokenizer(report?.report), [report]);
    // 高级区默认收起；勾着高级项时强制展开，免得「看不见但会跑」。
    const [advancedOpen, setAdvancedOpen] = useState(false);
    const advancedSelected = selected.some((id) => ADVANCED_PROBE_IDS.includes(id));
    const recording = useMemo(() => readTokenizerRecording(report?.report), [report]);
    // 要点行与渠道来源卡都只读后端算好的结论（百分比不自己算、线索不自己判）。
    const highlights = useMemo(() => highlightRows(report?.report), [report]);
    const provenance = useMemo(() => provenanceView(report?.report), [report]);
    // 复核数只计 Score>0；tokenizer 等参考项（tone=info）永不进「需要复核」。
    const warnCount = report ? (report.report.findings ?? []).filter((f) => (f.score ?? 0) > 0).length : 0;
    const failCount = rows.filter((row) => row.tone === 'fail').length;
    const insufficientCount = rows.filter((row) => row.insufficient).length;
    const hlwyView = useMemo(() => readHlwyComparison(report?.report), [report]);
    // 换基准重算的视图优先展示，但它只活在内存里。
    const recompareView = useMemo(() => readHlwyComparison(recompare?.report), [recompare]);
    const activeView = recompareView ?? hlwyView;
    const copy = activeView
        ? {
            title: activeView.headline || hlwyExecutionLabel(activeView.execution),
            sub: activeView.summary || hlwyComparabilityLabel(activeView.comparability),
        }
        : report
        ? (report.report.plain?.headline
            ? { title: report.report.plain.headline, sub: report.report.plain.meaning || report.report.plain.summary || '' }
            : headline(report.report.verdict, warnCount, failCount, insufficientCount, t))
        : null;

    const chooseChannel = (id: number) => {
        setChannelId(id);
        // 用户自己重新选了渠道：上一条「原来的渠道不见了」的提醒就失效了。
        setRerunChannelId(null);
        setModelNames([]);
    };

    const choosePreset = (next: AuditPreset) => {
        setPreset(next);
    };

    const toggleProbes = (ids: readonly AuditProbeId[], checked: boolean) => {
        setPreset('custom');
        // 手动项勾上时把代价说清楚。
        if (checked && ids.some((id) => AUDIT_OPT_IN_PROBE_IDS.includes(id))) {
            toast.warning(
                ids.includes('tokenizer_baseline')
                    ? '录基线要发 74 次请求，建议单独跑，只用确定是正品的渠道'
                    : '分词器识别约 12 次请求（最多 22 次）',
            );
        }
        setSelected((current) => {
            if (checked) return AUDIT_PROBE_IDS.filter((item) => ids.includes(item) || current.includes(item));
            return current.filter((item) => !ids.includes(item));
        });
    };

    // 唯一的提交入口：把选中的模型交给**后端真队列**，然后立刻返回。
    //
    // 多探针检测：同一套检查的模型合成一单（Claude 和非 Claude 的签名项不同，分开交）。
    // 数字习惯：一个模型一单（后端要求），基准按「自动」或手动选的那份。
    // 不管点一个还是一批，都可以关页面、隔天回来在「任务」里看。
    const submitAll = async () => {
        if (!channel) return;
        const names = targetModels;
        if (names.length === 0) {
            toast.warning('先选要检测的模型');
            return;
        }
        if (nothingToRun) {
            toast.warning('这个模型没有可用的检查项');
            return;
        }
        if (hlwyOn && hlwyStart.disabled) {
            toast.warning(hlwyStart.reason || '数字习惯现在还不能提交');
            return;
        }
        const hlwyRuns = hlwyOn
            ? names.map((name) => ({ name, baselineId: hlwyBaselineId || autoBaselineFor(name, catalogEntries)?.id || '' }))
            : [];
        if (hlwyRuns.some((run) => !run.baselineId)) {
            toast.warning('没有能用的对比基准：先导入一份，或在正品渠道上录一份');
            return;
        }
        setSubmitting(true);
        const jobIds: number[] = [];
        const errors: string[] = [];
        try {
            for (const group of probeGroups) {
                try {
                    const res = await enqueueAudit.mutateAsync({ channel_id: channel.id, models: group.models, probes: group.probes });
                    jobIds.push(res.job_id);
                } catch (error) {
                    errors.push(error instanceof Error ? error.message : '提交失败');
                }
            }
            for (const run of hlwyRuns) {
                try {
                    const res = await enqueueHlwy.mutateAsync({
                        channel_id: channel.id,
                        models: [run.name] as [string],
                        kind: 'hlwy_compare',
                        hlwy: {
                            samples: hlwySamples,
                            baseline_id: run.baselineId,
                            protocol_id: hlwyPlan?.protocol_id ?? '',
                        },
                    });
                    jobIds.push(res.job_id);
                } catch (error) {
                    errors.push(`数字习惯 · ${run.name}：${error instanceof Error ? error.message : '提交失败'}`);
                }
            }
        } finally {
            setSubmitting(false);
        }
        if (jobIds.length > 0) {
            // 提交成功就丢掉上一轮的报告：不清的话，用户可能把上一轮结论当成「这次的报告」导出。
            setReport(null);
            setRecompare(null);
            setPageState('idle');
            toast.success(
                `已提交 ${names.length} 个模型，后台开始检测（任务 ${jobIds.map((id) => `#${id}`).join('、')}）`,
            );
            if (!background) setTab('tasks');
        }
        if (errors.length > 0) toast.error(errors.join('；'));
    };

    // 再跑一轮数字习惯：把上一轮的渠道、型号、基准、样本量全部填回去，然后**回到提交页**。
    // 不直接入队：再跑一轮会真的再花一次钱，必须让用户自己按下那个按钮。
    const rerunHlwy = () => {
        if (!activeView) return;
        if (report?.channel_id) {
            setChannelId(report.channel_id);
            setRerunChannelId(report.channel_id);
        }
        const rerunModel = report?.model || report?.upstream_model || '';
        if (rerunModel) setModelNames([rerunModel]);
        if (activeView.chosenBaselineId) setHlwyBaselineId(activeView.chosenBaselineId);
        const planned = activeView.counts?.planned_calls;
        if (planned === 100 || planned === 200) setHlwySamples(planned);
        // 只重跑数字习惯：其他检查项先都不勾，免得顺手多花一单。
        setPreset('custom');
        setSelected([]);
        setHlwyChecked(true);
        setHlwyPanelOpen(true);
        setTab('test');
        setPageState('idle');
        setReport(null);
        setRecompare(null);
        toast.success('参数已按上一轮填好，再点一次「开始检测」才会真的发请求');
    };

    // 把这次结果存成基准。前端只做一次粗筛，服务端还会独立再校验一遍。
    const saveHlwyResult = () => {
        if (!report || !activeView) return;
        if (!hlwySaveCheck.eligible) {
            toast.warning(hlwySaveCheck.reason);
            return;
        }
        const resultId = report.result_id;
        if (typeof resultId !== 'number' || !Number.isInteger(resultId) || resultId <= 0) {
            toast.warning('这条结果没有可用的编号，存不成基准（请从「任务」的记录里重新打开它）');
            return;
        }
        const label = `${report.model || report.upstream_model || '模型'} · 自采`;
        saveHlwyBaseline.mutate(
            { result_id: resultId, label },
            {
                onSuccess: (data) => {
                    toast.success(`已存成基准：${data.baseline.label}`);
                    setHlwyBaselineId(data.baseline.id);
                },
                onError: (error) =>
                    toast.error('存成基准失败', {
                        description: error instanceof Error ? error.message : String(error),
                    }),
            },
        );
    };

    // 换一份基准重算：离线，不发请求、不写库、不改原报告。
    const recompareWith = (baselineId: string) => {
        const resultId = report?.result_id;
        if (typeof resultId !== 'number' || !Number.isInteger(resultId) || resultId <= 0) {
            toast.warning('要从「任务」的记录里打开这条结果才能重算');
            return;
        }
        // 旧请求的响应不能盖到新报告上。
        const stillCurrent = () => reportResultIdRef.current === resultId;
        recompareHlwy.mutate(
            { result_id: resultId, baseline_id: baselineId },
            {
                onSuccess: (data) => {
                    if (!stillCurrent()) return;
                    setRecompare({ baselineId, report: data.report });
                },
                onError: (error) => {
                    if (!stillCurrent()) return;
                    toast.error('重算失败', {
                        description: error instanceof Error ? error.message : String(error),
                    });
                },
            },
        );
    };

    useEffect(() => {
        reportResultIdRef.current = report?.result_id ?? null;
    }, [report]);

    const exportHlwyFull = () => {
        if (!report) return;
        const payload = hlwyExportFull(report.report);
        if (!payload) {
            toast.error('这份报告里没有对照细节，导不出来');
            return;
        }
        downloadJson(`hlwy-compare-${report.result_id ?? 'report'}.json`, payload);
    };

    const exportHlwyLegacy = () => {
        if (!report) return;
        const payload = hlwyExportLegacy(activeView, {
            model: report.model || report.upstream_model,
            timestamp: report.finished_at,
        });
        if (!payload) {
            toast.error('没有原始数字，导不出旧格式');
            return;
        }
        downloadJson(`hlwy-${report.result_id ?? 'report'}-legacy.json`, payload);
    };

    const hlwySaveCheck = hlwySaveEligibility(activeView, {
        resultId: report?.result_id,
        resultStatus: report?.result_status,
    });

    // 再测一轮：只重跑指纹这一项，同一渠道、同一型号；题目由后端重新随机生成。
    const retestFingerprint = async () => {
        if (!report) return;
        const model = report.model || report.upstream_model;
        if (!model) return;
        try {
            const res = await enqueueAudit.mutateAsync({
                channel_id: report.channel_id,
                models: [model],
                probes: ['fingerprint'],
            });
            toast.success(`已提交再测一轮（任务 #${res.job_id}），结果在「任务」里看`);
            setReport(null);
            setPageState('idle');
            setTab('tasks');
        } catch (error) {
            toast.error(error instanceof Error ? error.message : '提交失败');
        }
    };

    // 把记录里存好的报告重新摊到「检测」页右边。报告以结果行的 report 字段为准，点开时现解析；
    // 解析失败要明确报错，不能静默显示空报告（空报告会被读成「查过没问题」）。
    const openStoredReport = (result: AuditResult) => {
        const read = readStoredReport(result.report || '');
        if (!read.ok) {
            toast.error(read.error);
            return;
        }
        const view = read.view as unknown as AuditViewReport;
        setReport({
            result_id: result.id,
            channel_id: result.channel_id,
            channel_name: result.channel_name,
            upstream_model: result.upstream_model,
            endpoint: '',
            duration_ms: result.duration_ms,
            report: view,
            finished_at: result.finished_at,
            model: result.model,
            result_status: result.status,
        });
        setRecompare(null);
        // 「完整报告」切回检测页右边。
        setTab('test');
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

    const openRecentReport = (jobId: number) => {
        setFocusJobId(jobId);
        setTab('tasks');
    };

    /** 自定义里的一行勾选。ids 是这一行代表的检查（「自报身份/系统提示/怪词」是三项合一）。 */
    const renderCheckRow = (key: string, label: string, ids: readonly AuditProbeId[], options: { disabled?: boolean; reqText?: string; title?: string } = {}) => {
        const disabled = options.disabled === true;
        const checked = !disabled && ids.every((id) => selected.includes(id));
        return (
            <label key={key} title={options.title} className={cn('flex items-center gap-2 rounded-xl px-1 py-1.5 text-sm', disabled && 'opacity-40')}>
                <input
                    type="checkbox"
                    checked={checked}
                    disabled={disabled}
                    onChange={(event) => toggleProbes(ids, event.target.checked)}
                />
                <span className="min-w-0 flex-1 truncate">{label}</span>
                <span className="text-[11px] tabular-nums text-muted-foreground">{options.reqText ?? (disabled ? '—' : rowReq(ids))}</span>
            </label>
        );
    };

    const groupTitle = (index: string, title: string, tag: string) => (
        <p className="flex items-center gap-1.5 px-1 pt-1 text-xs font-semibold">
            {index} {title}
            <span className="rounded-full bg-sky-500/10 px-1.5 py-px text-[10px] font-medium text-sky-700 dark:text-sky-300">{tag}</span>
        </p>
    );

    const presetCard = (id: AuditPreset, title: string, meta: string, desc: string, badge?: string) => (
        <button
            type="button"
            onClick={() => choosePreset(id)}
            aria-pressed={preset === id}
            className={cn(
                'rounded-2xl border px-4 py-3.5 text-left transition-colors',
                preset === id ? 'border-2 border-primary bg-primary/5 -m-px' : 'border-border',
            )}
        >
            <div className="flex items-center gap-2">
                <span className={cn('flex size-4 shrink-0 items-center justify-center rounded-full', preset === id ? 'bg-primary' : 'border-2 border-border')}>
                    {preset === id && <span className="size-1.5 rounded-full bg-white" />}
                </span>
                <span className="text-sm font-semibold">{title}</span>
                {badge && <span className="rounded-full bg-primary/15 px-1.5 py-px text-[10px] font-medium text-primary">{badge}</span>}
                <span className="ml-auto text-right text-xs text-muted-foreground">{meta}</span>
            </div>
            <p className="mt-1.5 pl-6 text-xs leading-5 text-muted-foreground">{desc}</p>
        </button>
    );

    const modelsSuffix = targetModels.length > 1 ? `（${targetModels.length} 个模型合计）` : '';

    const renderParams = () => (
        <aside className="flex min-h-0 w-full flex-col gap-5 rounded-3xl border border-border bg-card p-5">
            <section>
                <div className="grid gap-4 sm:grid-cols-2 md:grid-cols-1 lg:grid-cols-2">
                    <div className="min-w-0">
                        <label className="mb-1 block text-xs text-muted-foreground">渠道</label>
                        <SearchableSelect
                            value={channel ? String(channel.id) : ''}
                            onValueChange={(value) => chooseChannel(Number(value))}
                            options={visibleChannels.map((item) => ({
                                value: String(item.id),
                                label: item.enabled ? item.name : `${item.name}（未启用）`,
                                keywords: `#${item.id} ${item.enabled ? '启用' : '未启用'} ${(item.selected_models ?? []).join(' ')}`,
                            }))}
                            placeholder={channels.length === 0 ? '暂无渠道' : '选择渠道'}
                            searchPlaceholder="搜渠道名 / 编号 / 模型…"
                            emptyText="没有匹配的渠道"
                            className="h-10 w-full"
                            disabled={visibleChannels.length === 0}
                        />
                    </div>
                    <div className="min-w-0">
                        <div className="mb-1 flex items-center justify-between gap-2">
                            <label className="block text-xs text-muted-foreground">模型（可多选）</label>
                            {models.length > 1 && (
                                <button
                                    type="button"
                                    className="text-[11px] text-primary hover:underline"
                                    onClick={() => setModelNames(targetModels.length === models.length ? [] : [...models])}
                                >
                                    {targetModels.length === models.length ? '只测第一个' : `全选 ${models.length} 个`}
                                </button>
                            )}
                        </div>
                        <MultiPicker
                            value={targetModels}
                            onChange={setModelNames}
                            options={models.map((item) => ({ value: item, label: item }))}
                            placeholder={models.length === 0 ? '该渠道没有可选模型' : '选择模型'}
                            searchPlaceholder="搜模型名…"
                            emptyText="没有匹配的模型"
                            className="w-full"
                            disabled={models.length === 0}
                            ariaLabel="选择要检测的模型"
                        />
                    </div>
                </div>
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
            </section>

            <section>
                <p className="mb-3 text-xs font-medium text-muted-foreground">查多细</p>
                <div className="grid gap-3">
                    {presetCard('quick', '快速', `${quickCount} 项 · 约 ${quickReq} 次${modelsSuffix}`, '通不通、回复有没有被改、长内容有没有被截断。')}
                    {presetCard(
                        'full',
                        '全面',
                        `${fullCount} 项 · 约 ${fullReq} 次${modelsSuffix}`,
                        `再加身份指纹、自报身份、token 对比${isClaudeTarget ? '、Claude 签名' : ''}。不含分词器和数字习惯。`,
                        '推荐',
                    )}
                    {presetCard('custom', '自定义', `约 ${customReq} 次${modelsSuffix}`, '自己勾：分词器、数字习惯这些慢的、贵的在这里加。')}
                </div>

                {preset === 'custom' && (
                    <div className="mt-3 space-y-2 rounded-2xl border border-border bg-muted/20 px-2 py-2">
                        {groupTitle('①', '身份', '主动')}
                        <div className="grid gap-0.5">
                            {renderCheckRow('fingerprint', '取数指纹', ['fingerprint'])}
                            {selected.includes('fingerprint') && <FingerprintPromptToggle />}
                            {renderCheckRow('identity', '自报身份 / 系统提示 / 怪词', IDENTITY_GROUP)}
                            {renderCheckRow('tokenizer_id', '分词器（准，慢）', ['tokenizer_id'], {
                                reqText: `约 ${rowReq(['tokenizer_id'])}`,
                                title: '对照内置分词器库看上游用哪套；只作参考，不计分',
                            })}
                            {renderCheckRow('signature', isClaudeTarget ? 'Claude 签名' : 'Claude 签名（不是 Claude，跳过）', ['signature'], {
                                disabled: !isClaudeTarget,
                                title: '检查思考块是否附带可验证签名，只有 Anthropic 渠道上的 Claude 能查',
                            })}
                        </div>
                        <div className="border-t border-border/60" />
                        {groupTitle('②', '降级 / 量化', '主动')}
                        <div className="grid gap-0.5">
                            {renderCheckRow('context_canary', '长内容不被截断', ['context_canary'])}
                            {renderCheckRow('echo_rewrite', '回复不被改写', ['echo_rewrite'])}
                            <label className="flex items-center gap-2 rounded-xl px-1 py-1.5 text-sm">
                                <input
                                    type="checkbox"
                                    checked={hlwyChecked}
                                    onChange={(event) => setHlwyChecked(event.target.checked)}
                                    aria-label="数字习惯"
                                />
                                <span className="min-w-0 flex-1 truncate">数字习惯</span>
                                <span className="inline-flex rounded-lg border border-border bg-background p-0.5" role="group" aria-label="数字习惯采样次数">
                                    {([100, 200] as const).map((n) => (
                                        <button
                                            key={n}
                                            type="button"
                                            aria-pressed={hlwySamples === n}
                                            onClick={(event) => { event.preventDefault(); setHlwySamples(n); }}
                                            className={cn('rounded-md px-1.5 py-px text-[11px] tabular-nums', hlwySamples === n ? 'bg-primary text-primary-foreground' : 'text-muted-foreground')}
                                        >
                                            {n} 次
                                        </button>
                                    ))}
                                </span>
                            </label>
                            {hlwyChecked && (
                                <div className="ml-6 space-y-2 rounded-xl bg-muted/50 px-3 py-2 text-xs leading-5">
                                    <p>
                                        对比基准：
                                        {hlwyBaselineId ? (
                                            <span className="font-medium">{hlwySelectedEntry ? hlwyBaselineOptionLabel(hlwySelectedEntry) : hlwyBaselineId}</span>
                                        ) : (
                                            <>
                                                <span className="font-medium">自动</span>
                                                <span className="text-muted-foreground">（同模型优先自己录的，没有用 hlwy 内置）</span>
                                                {autoBaseline && <span className="block text-muted-foreground">这次会用：{hlwyBaselineOptionLabel(autoBaseline)}</span>}
                                                {!autoBaseline && catalogEntries.length > 0 && <span className="block text-amber-700">没有能用的基准，点「换」导入一份</span>}
                                            </>
                                        )}
                                    </p>
                                    <div className="flex flex-wrap gap-3">
                                        <button type="button" className="font-medium text-primary hover:underline" onClick={() => setHlwyPanelOpen((v) => !v)}>
                                            {hlwyPanelOpen ? '收起' : '换 ▾'}
                                        </button>
                                        {hlwyBaselineId && (
                                            <button type="button" className="text-primary hover:underline" onClick={() => setHlwyBaselineId('')}>
                                                改回自动
                                            </button>
                                        )}
                                    </div>
                                    {hlwyPanelOpen && (
                                        <HlwyModePanel
                                            baselineId={effectiveBaselineId}
                                            onBaselineChange={setHlwyBaselineId}
                                            samples={hlwySamples}
                                            onSamplesChange={setHlwySamples}
                                            plan={hlwyPlan}
                                            planLoading={hlwyPlanQuery.isLoading === true}
                                            planError={hlwyPlanError}
                                        />
                                    )}
                                </div>
                            )}
                        </div>
                        <div className="border-t border-border/60" />
                        {groupTitle('③', 'Token', '主动')}
                        <div className="grid gap-0.5">
                            {renderCheckRow('token_delta', '一短一长两条对比', ['token_delta'])}
                        </div>
                        <p className="px-1 text-[11px] leading-5 text-muted-foreground">连通检查会自动带上（1 次），先确认渠道通了再查别的。</p>
                        <details
                            className="rounded-xl bg-muted/30 px-2 py-1.5"
                            open={advancedOpen || advancedSelected}
                            onToggle={(event) => setAdvancedOpen((event.currentTarget as HTMLDetailsElement).open)}
                        >
                            <summary className="cursor-pointer text-[11px] leading-5 text-muted-foreground" title={ADVANCED_TOOLTIP}>
                                {ADVANCED_SUMMARY}
                            </summary>
                            <div className="mt-1 grid gap-1">
                                {renderCheckRow('tokenizer_baseline', '录一份正品基线', ['tokenizer_baseline'], { reqText: `约 ${rowReq(['tokenizer_baseline'])}` })}
                            </div>
                        </details>
                    </div>
                )}
            </section>

            <section className="border-t border-border pt-4">
                <div className="mb-3 flex flex-wrap items-center justify-between gap-2 text-sm">
                    <label className="flex items-center gap-2" title="检测本来就在服务器上排队跑，关掉页面也不会停。不勾的话，提交后直接跳到「任务」看着它跑。">
                        <input type="checkbox" checked={background} onChange={(event) => setBackground(event.target.checked)} />
                        放到后台跑，关掉页面也继续
                    </label>
                    <button type="button" className="text-xs text-muted-foreground hover:text-foreground" onClick={() => setTab('tasks')}>
                        去「任务」看进度
                    </button>
                </div>
                {/* 全页只有一个提交动作：查多细决定它提交什么。 */}
                <Button
                    type="button"
                    className="h-12 w-full rounded-xl text-base font-semibold"
                    disabled={startDisabled}
                    onClick={() => void submitAll()}
                    title={startReason || undefined}
                >
                    {submitting || enqueueAudit.isPending ? <Loader2 className="size-4 animate-spin" /> : <Play className="size-4 fill-current" />}
                    <span className="min-w-0 truncate">
                        {submitting || enqueueAudit.isPending
                            ? '提交中…'
                            : `开始检测 · ${targetModels.length} 个模型 · 约 ${totalReq} 次`}
                    </span>
                </Button>
                {startReason && !submitting && (
                    <p className="mt-2 text-center text-xs leading-5 text-amber-700">{startReason}</p>
                )}
                <p className="mt-2.5 flex flex-wrap items-center justify-center gap-1 text-center text-xs text-muted-foreground">
                    {!hlwyOn
                        ? '会真的调用上游，按正常价格计费。'
                        : hlwyPlan
                          ? `会真的调用上游，按正常价格计费；数字习惯每个模型最多调用${hlwyPlan.max_calls ?? hlwySamples}次，会消耗渠道额度。`
                          : '会真的调用上游，按正常价格计费；数字习惯的调用上限要等计划读出来才知道。'}
                    <span className="cursor-help text-muted-foreground/80" title="次数是估算：个别检查会多轮或重试，实际以记录为准。超时跟随渠道检测设置。">ⓘ</span>
                </p>
                {budget && (
                    <p className="mt-1 text-center text-xs tabular-nums text-muted-foreground">
                        今天检测额度还剩 {budget.remaining} / {budget.limit} 次（定时体检和鹈鹕测试从这里扣）
                    </p>
                )}
            </section>
        </aside>
    );

    const sideCards = (
        <>
            <RecentAudits onOpenAll={() => setTab('tasks')} onOpenJob={openRecentReport} />
            <PassiveProbeCard />
            <section className="rounded-3xl border border-border bg-card p-5">
                <h3 className="text-sm font-semibold">定时体检</h3>
                <p className="mt-0.5 mb-3 text-xs leading-5 text-muted-foreground">后台按天抽查，花的是每日检测额度；关着时什么都不发。</p>
                <BackgroundAuditPanel />
            </section>
        </>
    );

    const tabButton = (id: 'test' | 'tasks' | 'pelican', label: string, badge?: string) => (
        <button
            key={id}
            type="button"
            onClick={() => setTab(id)}
            aria-pressed={tab === id}
            className={cn(
                'flex items-center gap-1.5 rounded-xl px-3 py-1.5 text-sm transition-colors',
                tab === id ? 'bg-primary text-primary-foreground' : 'text-muted-foreground hover:bg-muted',
            )}
        >
            {label}
            {badge && (
                <span className={cn('rounded-full px-1.5 py-px text-[10px] font-medium', tab === id ? 'bg-white/20' : 'bg-primary/15 text-primary')}>
                    {badge}
                </span>
            )}
        </button>
    );

    return (
        <PageWrapper className="h-full min-h-0 space-y-4 overflow-y-auto overscroll-contain rounded-t-3xl pb-24 md:pb-4">
            {/* 顶栏已有「检测」标题，这里只留副标题 + 三栏切换。 */}
            <div className="flex flex-wrap items-center justify-between gap-3 px-1">
                <p className="min-w-0 flex-1 text-sm text-muted-foreground">
                    看看渠道给你的模型，是不是它说的那个。
                </p>
                <div className="flex shrink-0 gap-1 rounded-2xl border border-border bg-card p-1">
                    {tabButton('test', '检测')}
                    {tabButton('tasks', '任务', activeJobCount > 0 ? `${activeJobCount} 个在跑` : undefined)}
                    {tabButton('pelican', '鹈鹕测试')}
                </div>
            </div>

            {tab === 'tasks' && <TasksView onOpenReport={openStoredReport} focusJobId={focusJobId} />}

            {tab === 'pelican' && <PelicanTest channels={channels} />}

            {tab === 'test' && (
            <>
            {/* 手机：单列（参数在上，最近检测/日常监测/定时体检在下）。 */}
            <div className="md:hidden">{renderParams()}</div>

            <div className="flex min-h-0 flex-col gap-4 md:flex-row">
                <div className="hidden md:block md:w-[min(100%,440px)] md:shrink-0">{renderParams()}</div>

                {/* 有报告才展示结果面板；空闲时桌面右侧放最近检测 + 日常监测 + 定时体检。 */}
                {pageState !== 'idle' ? (
                <section className="min-w-0 flex-1 rounded-3xl border border-border bg-card p-5">
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
                                <p className="text-xs text-muted-foreground">本次报告 {report.finished_at ? new Date(report.finished_at).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit', hour12: false }) : '—'}</p>
                                {/* hlwy 报告的导出在卡片内部（一份完整、一份旧格式），
                                    这里不再放一个“整份 spread 出去”的按钮。 */}
                                {!activeView && (
                                    <Button type="button" variant="outline" size="sm" className="rounded-full" onClick={exportJson}>导出 JSON</Button>
                                )}
                            </div>
                            <div>
                                <h3 className="flex items-center gap-2 text-2xl font-semibold tracking-tight">
                                    {activeView
                                        ? <Binary className="size-5 text-muted-foreground" />
                                        : <AlertTriangle className={cn('size-5', warnCount > 0 ? 'text-amber-600' : 'text-muted-foreground')} />}
                                    {copy.title}
                                </h3>
                                <p className="mt-1 min-w-0 text-sm text-muted-foreground [overflow-wrap:anywhere]">{copy.sub}</p>
                                {/* 要点行：后端给几条短句就列几条；没有就整块不渲染。 */}
                                {highlights.length > 0 && (
                                    <ul className="mt-2 grid gap-1">
                                        {highlights.map((row) => (
                                            <li key={row.key} className="flex items-start gap-1.5 text-xs leading-5">
                                                <span className={cn('mt-1.5 size-1.5 shrink-0 rounded-full', TONE_DOT_CLASS[row.tone])} />
                                                <span className="min-w-0 flex-1">{row.text}</span>
                                                {row.percentText && (
                                                    <span className="shrink-0 tabular-nums text-muted-foreground">{row.percentText}</span>
                                                )}
                                            </li>
                                        ))}
                                    </ul>
                                )}
                            </div>
                            {activeView ? (
                                <BaselineComparisonCard
                                    // 报告身份跟着结果走：换一份报告就是另一张卡片，
                                    // 上一份选的「用哪个基准重算」不会带过来。
                                    reportKey={report.result_id ?? report.finished_at ?? ''}
                                    view={activeView}
                                    recompareActive={recompareView !== null}
                                    catalog={hlwyCatalog?.baselines ?? []}
                                    onRecompare={recompareWith}
                                    recomparePending={recompareHlwy.isPending}
                                    recompareError={
                                        recompareHlwy.error
                                            ? recompareHlwy.error instanceof Error
                                                ? recompareHlwy.error.message
                                                : String(recompareHlwy.error)
                                            : ''
                                    }
                                    onRestoreOriginal={() => setRecompare(null)}
                                    saveEligibility={hlwySaveCheck}
                                    onSave={saveHlwyResult}
                                    saving={saveHlwyBaseline.isPending}
                                    onRerun={rerunHlwy}
                                    onExportFull={exportHlwyFull}
                                    onExportLegacy={exportHlwyLegacy}
                                    canExportLegacy={!!activeView.rawNumbers}
                                />
                            ) : (
                            <>
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
                            {fingerprint && (
                                <FingerprintCard
                                    summary={fingerprint}
                                    declared={report.model || report.upstream_model}
                                    onRetest={retestFingerprint}
                                    retesting={enqueueAudit.isPending}
                                />
                            )}
                            {tokenizer && <TokenizerCard summary={tokenizer} />}
                            {recording && <TokenizerRecordingCard recording={recording} resultId={report.result_id} />}
                            {/* 渠道来源：observed>0 才出，看不够样本时宁可不说。 */}
                            {provenance && <ProvenanceCard view={provenance} />}
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
                            </>
                            )}
                        </div>
                    )}
                </section>
                ) : (
                <div className="hidden min-w-0 flex-1 flex-col gap-4 md:flex">
                    {sideCards}
                </div>
                )}
            </div>

            <div className="grid gap-4 md:hidden">
                {sideCards}
            </div>

            {isLoading && (
                <p className="px-1 text-xs text-muted-foreground">正在读取渠道列表…</p>
            )}

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
