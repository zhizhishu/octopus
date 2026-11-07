'use client';

import { useEffect, useMemo, useRef, useState } from 'react';
import {
    AlertTriangle,
    TriangleAlert,
    CircleHelp,
    Check,
    Binary,
    ChevronDown,
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
    useAuditPlan,
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
import { ModelAuditQueue } from './Queue';
import { HlwyModePanel } from './HlwyModePanel';
import { BaselineComparisonCard } from './BaselineComparisonCard';
import {
    downloadJson,
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

// 'hlwy' 不是「更多探针」的一种：它换的是问题本身（同一个数字题问 100/200 次、
// 只看数字分布）。放在同一个 PlanId 里是为了「只有一个提交按钮」——切换模式就
// 切换按钮的意义，而不是页面上多出一个没人管的旧按钮。
type PlanId = 'quick' | 'standard' | 'hlwy';
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

function planIds(plan: PlanId, channel: Channel | undefined, model = ''): AuditProbeId[] {
    const base = plan === 'quick' ? QUICK_IDS : [...AUDIT_STANDARD_PROBE_IDS];
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
                onSuccess: () => toast.success(next ? '日志查账已开启' : '日志查账已关闭'),
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
                    <p className="flex items-center gap-1.5 text-sm font-medium text-foreground">
                        日志查账
                        <span className="rounded-full bg-primary/12 px-1.5 py-px text-[10px] font-medium text-primary">免费</span>
                        <span
                            className="cursor-help text-muted-foreground/80"
                            title={disclaimer}
                            aria-label={disclaimer}
                        >
                            ⓘ
                        </span>
                    </p>
                    <p className="mt-0.5 text-xs leading-5 text-muted-foreground">
                        从日常调用里找对不上账的地方，不额外发请求。
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

function RecentAudits({ onOpenAll }: { onOpenAll: () => void }) {
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
    const { data: logAnomalies } = useLogAnomalies();
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
    const [selected, setSelected] = useState<AuditProbeId[]>([...AUDIT_STANDARD_PROBE_IDS]);
    const [customOpen, setCustomOpen] = useState(false);
    const [pageState, setPageState] = useState<PageState>('idle');
    const [report, setReport] = useState<ModelAuditResponse | null>(null);
    const [errorText, setErrorText] = useState('');
    const [openRow, setOpenRow] = useState<DisplayRow | null>(null);
    // hlwy 基准对照的选择状态。
    //
    // 特意不放进 `selected`（探针列表）：探针是「查几项」，这里是「和谁比」。
    // 混在一起，切回探针模式时就会莫名地带上一轮对照的残留。
    const [hlwyBaselineId, setHlwyBaselineId] = useState('');
    const [hlwySamples, setHlwySamples] = useState<HlwySampleCount>(100);
    // 「再跑一轮」填回去的那个渠道 id。它只是「上次用的是谁」的记录，不是当前选择；
    // 渠道被删掉以后，靠它把「现在选的已经不是那个渠道」说出口。
    const [rerunChannelId, setRerunChannelId] = useState<number | null>(null);
    // 换基准重算的结果：只在内存里预览，不覆盖存下来的原报告。
    const [recompare, setRecompare] = useState<{ baselineId: string; report: Record<string, unknown> } | null>(null);
    // 当前正在看的结果 id。用来丢掉「换了报告以后才回来」的重算响应。
    const reportResultIdRef = useRef<number | null>(null);
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
    // 从渠道页芯片点过来：自动打开对应任务。
    useEffect(() => {
        try {
            const store = typeof window !== 'undefined' ? window.sessionStorage : null;
            const job = store?.getItem('octopus.audit.openJob');
            if (!job) return;
            store?.removeItem('octopus.audit.openJob');
            store?.removeItem('octopus.audit.openResult');
            setMode('queue');
            setPageState('idle');
        } catch {
            // 测试环境可能没有 sessionStorage。
        }
    }, []);

    // 卸载时不再需要 abort：请求已经交给后端队列，关页面不会让后台停下
    // （这正是"点了就走"的意义）。要停就去队列页取消任务。
    const applicableSelected = selected.filter((id) => isApplicable(channel, id, activeModel));
    const runCount = plan === 'quick' ? planIds('quick', channel, activeModel).length : applicableSelected.length;
    const { data: auditPlan } = useAuditPlan(channel?.id ?? 0, activeModel, !!channel && !!activeModel);
    const quickReq = auditPlan?.quick?.requests ?? planIds('quick', channel, activeModel).length;
    const fullReq = auditPlan?.full?.requests ?? planIds('standard', channel, activeModel).length;
    const quickCount = (auditPlan?.quick?.ids ?? QUICK_IDS).filter((id) => isApplicable(channel, id, activeModel)).length;
    const fullCount = (auditPlan?.full?.ids ?? AUDIT_STANDARD_PROBE_IDS).filter((id) => isApplicable(channel, id, activeModel)).length;
    const customReq = useMemo(() => {
        if (!auditPlan?.probes) {
            return applicableSelected.length;
        }
        const byId = new Map(auditPlan.probes.map((p) => [p.id, p]));
        return selected.reduce((sum, id) => {
            if (!isApplicable(channel, id, activeModel)) return sum;
            const row = byId.get(id);
            return sum + (row?.applicable === false ? 0 : (row?.requests ?? 1));
        }, 0);
    }, [auditPlan, selected, channel, activeModel]);

    /* ---------------- hlwy 基准对照 ---------------- */
    // 同一个页面上的第二种检测：不发探针，只问同一题 N 次看数字分布。
    const hlwyMode = plan === 'hlwy';
    const { data: hlwyCatalog } = useHlwyBaselines();
    const enqueueHlwy = useEnqueueHlwyCompare();
    const recompareHlwy = useRecompareHlwy();
    const saveHlwyBaseline = useSaveHlwyBaselineFromResult();
    const { data: hlwySettings } = useSettingList();
    // 开关写字面量、与 BackgroundAuditPanel 同一套做法：键由后端同期实现，
    // 本次不动 setting.ts 的 SettingKey 表。默认关。
    const hlwyFlagEnabled = settingBool(
        hlwySettings?.find((item) => item.key === 'model_audit_hlwy_enabled')?.value,
        false,
    );
    const hlwyPlanQuery = useHlwyPlan({
        channelId: channel?.id ?? 0,
        model: activeModel,
        samples: hlwySamples,
        baselineId: hlwyBaselineId,
        // 不在本模式就不发这个请求：读一份不用的计划也是白白多一次往返。
        enabled: hlwyMode,
    });
    const hlwyPlan = hlwyPlanQuery.data ?? null;
    const hlwyPlanError = hlwyPlanQuery.error
        ? hlwyPlanQuery.error instanceof Error
            ? hlwyPlanQuery.error.message
            : String(hlwyPlanQuery.error)
        : '';
    // 选中的那份基准现在还在不在目录里、是不是被标成不可用。
    // 目录刷新后 baselineId 可能指向一份校验没过的件，这时照样不能提交。
    const hlwySelectedEntry = hlwyCatalog?.baselines.find((entry) => entry.id === hlwyBaselineId) ?? null;
    // 「再跑一轮」把上一轮的渠道填回来，但那个渠道可能已经被删掉/停用了。
    // 渠道选择器会退到第一个渠道（看起来像没事），提交就会真的花在另一个渠道上。
    // 所以这里记住「上一轮用的是哪个渠道」，不见了就抦住按钮并说清楚。
    const rerunChannelMissing =
        rerunChannelId !== null && channels.length > 0 && !channels.some((item) => item.id === rerunChannelId);
    const rerunChannelMissingReason = rerunChannelMissing
        ? `上一轮用的渠道（#${rerunChannelId}）已经不在渠道列表里了，请重新选一个渠道再提交`
        : '';
    // 按钮能不能按、为什么不能按，全部由一个函数回答：页面上只有一个提交动作，
    // 说明也就只能有一处。
    const hlwyStart = hlwyStartState({
        hasTarget: !!channel && !!activeModel,
        channelMissing: rerunChannelMissingReason,
        baselineId: hlwyBaselineId,
        baselineUnavailable: hlwySelectedEntry?.unavailable === true,
        flagEnabled: hlwyFlagEnabled,
        plan: hlwyPlan,
        planError: hlwyPlanError,
        planLoading: hlwyPlanQuery.isLoading === true,
        pending: enqueueHlwy.isPending,
    });


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
    // 换基准重算的视图优先展示，但它只活在内存里：
    // 顶部的「本地重算，原报告未变」提示条就是这个事实的可见形态。
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
        const next = channels.find((item) => item.id === id);
        const nextModels = next ? getSelectedChannelModels(next) : [];
        setModelName(nextModels[0] ?? '');
        if (plan === 'standard') {
            setSelected(AUDIT_STANDARD_PROBE_IDS.filter((probeId) => isApplicable(next, probeId, nextModels[0] ?? '')));
        }
    };

    const choosePlan = (next: PlanId) => {
        setPlan(next);
        setCustomOpen(false);
        if (next === 'quick') setSelected(planIds('quick', channel, activeModel));
        // 切到 hlwy 时不动探针选择：切回来时用户刚刚勾的还在，而这一轮也不会
        // 因为“选择还在”就偷偷跑探针——提交走的是另一条路。
        else if (next === 'standard') setSelected(AUDIT_STANDARD_PROBE_IDS.filter((id) => isApplicable(channel, id, activeModel)));
    };

    const toggleProbe = (id: AuditProbeId, checked: boolean) => {
        setPlan('standard');
        setCustomOpen(true);
        // 手动项勾上时把代价说清楚：录制基线 74 次请求（整单超时已按请求数放大，见 op.AuditTimeoutFor），最好单独跑。
        if (checked && AUDIT_OPT_IN_PROBE_IDS.includes(id)) {
            toast.warning(
                id === 'tokenizer_baseline'
                    ? '录基线要发 74 次请求，建议单独跑，只用确定是正品的渠道'
                    : '分词器识别约 12 次请求（最多 22 次）',
            );
        }
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

    /* ---------------- hlwy 基准对照的四个动作 ---------------- */

    // 提交一轮对照。请求体里只有 samples / baseline_id / protocol_id：
    // 带 probes 就是另一回事，带 log_id 就成了日志回填，两者都不是这个模式。
    const submitHlwy = async () => {
        if (!channel || !activeModel) {
            toast.warning('先选渠道和模型');
            return;
        }
        if (!hlwyPlan) {
            // 没计划就不发：计划里写着这一单最多几次调用、超时多少，
            // 没有它就没有价格承诺，提交等于让人裸点一次。
            toast.error('还没读到这一单的计划，先别提交');
            return;
        }
        if (!hlwyBaselineId) {
            toast.warning('先选一份基准');
            return;
        }
        try {
            const res = await enqueueHlwy.mutateAsync({
                channel_id: channel.id,
                models: [activeModel] as [string],
                kind: 'hlwy_compare',
                hlwy: {
                    samples: hlwySamples,
                    baseline_id: hlwyBaselineId,
                    protocol_id: hlwyPlan.protocol_id,
                },
            });
            // 与探针提交同一口径：提交成功就丢掉上一轮报告，免得导错文件。
            setReport(null);
            setRecompare(null);
            setPageState('idle');
            toast.success(`已提交基准对照（任务 #${res.job_id}），可以先去干别的，回来在「记录」里看`);
            setMode('queue');
        } catch (error) {
            toast.error(error instanceof Error ? error.message : '提交失败');
        }
    };

    // 再跑一轮：把上一轮的渠道、型号、基准、样本量全部填回去，然后**回到提交页**。
    // 不直接入队：再跑一轮会真的再花一次钱，必须让用户自己按下那个按钮。
    const rerunHlwy = () => {
        if (!activeView) return;
        // 渠道/型号也要跟着回来：报告可能是从记录里打开的、跟当前选择不是同一个渠道，
        // 只填基准就等于让下一轮跑在错的渠道上。
        // 记下这个 id：它如果已经不在列表里，提交会被抦住并说明原因，而不是
        // 静静地退到列表第一个渠道上花掉一次钱。
        if (report?.channel_id) {
            setChannelId(report.channel_id);
            setRerunChannelId(report.channel_id);
        }
        const rerunModel = report?.model || report?.upstream_model || '';
        if (rerunModel) setModelName(rerunModel);
        if (activeView.chosenBaselineId) setHlwyBaselineId(activeView.chosenBaselineId);
        const planned = activeView.counts?.planned_calls;
        if (planned === 100 || planned === 200) setHlwySamples(planned);
        setPlan('hlwy');
        setMode('single');
        setPageState('idle');
        setReport(null);
        setRecompare(null);
        toast.success('参数已按上一轮填好，再点一次「开始基准对照」才会真的发请求');
    };

    // 把这次结果存成基准。前端只做一次粗筛，服务端还会独立再校验一遍。
    const saveHlwyResult = () => {
        if (!report || !activeView) return;
        if (!hlwySaveCheck.eligible) {
            toast.warning(hlwySaveCheck.reason);
            return;
        }
        // 资格判断只能担保「保存这件事本身合理」，担保不了这个 id 一定存在：
        // 拿到一个真正的正整数结果编号才发请求。不拿非空断言坑自己，也不
        // 用 0 充数——那会把请求发到一条不存在的记录上。
        const resultId = report.result_id;
        if (typeof resultId !== 'number' || !Number.isInteger(resultId) || resultId <= 0) {
            toast.warning('这条结果没有可用的编号，存不成基准（请从「检测记录」里重新打开它）');
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
        // 同上：重算要拿着一个真实的结果编号去问服务端，没有就不发。
        const resultId = report?.result_id;
        if (typeof resultId !== 'number' || !Number.isInteger(resultId) || resultId <= 0) {
            toast.warning('要从「检测记录」打开这条结果才能重算');
            return;
        }
        // 重算是异步的：等回来时用户可能已经打开另一份报告了。旧请求的响应
        // 千万不能盖到新报告上——那会把 A 的数字当成 B 的预览。
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

    // 当前报告的 id 跟着 report 走：重算响应用它判断自己是不是过期了。
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

    // 「能不能存成基准」只看两个事实：这条结果真的跑完了，报告里的计数也齐。
    const hlwySaveCheck = hlwySaveEligibility(activeView, {
        resultId: report?.result_id,
        resultStatus: report?.result_status,
    });

    // 再测一轮：只重跑指纹这一项，同一渠道、同一型号；题目由后端重新随机生成。
    // 由用户点按钮触发，不在后台自动重测——每一轮都要真实发请求、花 token。
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
            toast.success(`已提交再测一轮（任务 #${res.job_id}），结果在「记录」里看`);
            setReport(null);
            setPageState('idle');
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
        // 报告能不能打开，看它能不能**读**，不看它有没有 results 数组。
        // hlwy 对照本来就没有多探针结论：旧门槛会让一份完整报告显示成
        // 「这份报告不完整」——报告其实好着，是门槛错了。
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
            // 「能不能存成基准」得看这条结果是不是真的跑完了，不是看报告长得像不像。
            result_status: result.status,
        });
        // 换一个结果就是换一份报告，上一轮的重算预览不能跟过来。
        setRecompare(null);
        // 「完整报告」必须切回报告页，不能停在记录列表。
        setMode('single');
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

    const renderProbeRow = (probe: ProbeMeta) => {
        const disabled = !isApplicable(channel, probe.id, activeModel);
        const checked = selected.includes(probe.id) && !disabled;
        if (probe.id === 'fingerprint' && checked) {
            return (
                <div key={probe.id}>
                    {renderProbeLabel(probe, disabled, checked)}
                    <FingerprintPromptToggle />
                </div>
            );
        }
        return renderProbeLabel(probe, disabled, checked);
    };

    const renderProbeLabel = (probe: ProbeMeta, disabled: boolean, checked: boolean) => {
        return (
            <label key={probe.id} title={probe.desc} className={cn('flex items-center gap-2 rounded-xl px-1 py-1.5 text-sm', disabled && 'opacity-40')}>
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
    };

    const renderParams = () => (
        <aside className="flex min-h-0 w-full flex-col gap-5 rounded-3xl border border-border bg-card p-5">
            <section>
                <div className="grid gap-4 sm:grid-cols-2 md:grid-cols-1 lg:grid-cols-2">
                    <div>
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
                    <div>
                        <label className="mb-1 block text-xs text-muted-foreground">模型</label>
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
                    <button
                        type="button"
                        onClick={() => choosePlan('quick')}
                        className={cn(
                            'rounded-2xl border px-4 py-3.5 text-left transition-colors',
                            plan === 'quick' ? 'border-2 border-primary bg-primary/5 -m-px' : 'border-border',
                        )}
                    >
                        <div className="flex items-center gap-2">
                            <span className={cn(
                                'flex size-4 items-center justify-center rounded-full',
                                plan === 'quick' ? 'bg-primary' : 'border-2 border-border',
                            )}>
                                {plan === 'quick' && <span className="size-1.5 rounded-full bg-white" />}
                            </span>
                            <span className="text-sm font-semibold">快速</span>
                            <span className="ml-auto text-xs text-muted-foreground">{quickCount} 项 · 约 {quickReq} 次请求</span>
                        </div>
                        <p className="mt-1.5 pl-6 text-xs leading-5 text-muted-foreground">
                            通不通、回复有没有被改、长内容有没有被截断。
                        </p>
                    </button>
                    <button
                        type="button"
                        onClick={() => choosePlan('standard')}
                        className={cn(
                            'rounded-2xl border px-4 py-3.5 text-left transition-colors',
                            plan === 'standard' && !customOpen ? 'border-2 border-primary bg-primary/5 -m-px' : 'border-border',
                        )}
                    >
                        <div className="flex items-center gap-2">
                            <span className={cn(
                                'flex size-4 items-center justify-center rounded-full',
                                plan === 'standard' && !customOpen ? 'bg-primary' : 'border-2 border-border',
                            )}>
                                {plan === 'standard' && !customOpen && <span className="size-1.5 rounded-full bg-white" />}
                            </span>
                            <span className="text-sm font-semibold">全面</span>
                            <span className="rounded-full bg-primary/15 px-1.5 py-px text-[10px] font-medium text-primary">推荐</span>
                            <span className="ml-auto text-xs text-muted-foreground">{fullCount} 项 · 约 {fullReq} 次请求</span>
                        </div>
                        <p className="mt-1.5 pl-6 text-xs leading-5 text-muted-foreground">
                            再加上：认出背后是哪个模型、查用量有没有虚报。
                        </p>
                    </button>
                    <button
                        type="button"
                        onClick={() => choosePlan('hlwy')}
                        className={cn(
                            'rounded-2xl border px-4 py-3.5 text-left transition-colors',
                            plan === 'hlwy' ? 'border-2 border-primary bg-primary/5 -m-px' : 'border-border',
                        )}
                    >
                        <div className="flex items-center gap-2">
                            <span className={cn(
                                'flex size-4 items-center justify-center rounded-full',
                                plan === 'hlwy' ? 'bg-primary' : 'border-2 border-border',
                            )}>
                                {plan === 'hlwy' && <span className="size-1.5 rounded-full bg-white" />}
                            </span>
                            <span className="text-sm font-semibold">基准对照</span>
                            <span className="ml-auto text-xs text-muted-foreground">{hlwySamples} 次采样</span>
                        </div>
                        <p className="mt-1.5 pl-6 text-xs leading-5 text-muted-foreground">
                            同一题问很多次，只比数字分布和基准像不像；不判定真假，也不进风险计分。
                        </p>
                    </button>
                </div>

                {hlwyMode && (
                    <div className="mt-3">
                        <HlwyModePanel
                            baselineId={hlwyBaselineId}
                            onBaselineChange={setHlwyBaselineId}
                            samples={hlwySamples}
                            onSamplesChange={setHlwySamples}
                            plan={hlwyPlan}
                            planLoading={hlwyPlanQuery.isLoading === true}
                            planError={hlwyPlanError}
                        />
                    </div>
                )}

                {/* 探针自选只属于探针模式：切到基准对照后把它整块拿掉，
                    否则屏幕上会同时存在两套「要测什么」。 */}
                {!hlwyMode && (
                <>
                <button
                    type="button"
                    className="mt-3 flex w-full items-center gap-1.5 px-1 text-xs text-muted-foreground"
                    onClick={() => setCustomOpen((v) => !v)}
                >
                    {customOpen ? <ChevronDown className="size-3.5" /> : <ChevronRight className="size-3.5" />}
                    自定义：自己挑检查项
                    <span className="ml-1 text-muted-foreground/70">
                        {customOpen
                            ? ''
                            : `（共 ${PROBES.length} 项，现在选了 ${applicableSelected.length} 项）`}
                    </span>
                    {customOpen && (
                        <span className="ml-auto">已选 {applicableSelected.length} 项 · 约 {customReq} 次请求</span>
                    )}
                </button>
                {customOpen && (
                    <div className="mt-2 rounded-2xl border border-border bg-muted/20 px-2 py-2">
                        <div className="grid gap-1">
                            {PROBES.filter((probe) => !ADVANCED_PROBE_IDS.includes(probe.id)).map(renderProbeRow)}
                        </div>
                        <details
                            className="mt-2 rounded-xl bg-muted/30 px-2 py-1.5"
                            open={advancedOpen || advancedSelected}
                            onToggle={(event) => setAdvancedOpen((event.currentTarget as HTMLDetailsElement).open)}
                        >
                            <summary className="cursor-pointer text-[11px] leading-5 text-muted-foreground" title={ADVANCED_TOOLTIP}>
                                {ADVANCED_SUMMARY}
                            </summary>
                            <div className="mt-1 grid gap-1">
                                {PROBES.filter((probe) => ADVANCED_PROBE_IDS.includes(probe.id)).map(renderProbeRow)}
                            </div>
                        </details>
                    </div>
                )}
                </>
                )}
            </section>

            {/* 后台检测的长期策略（被动开关 / 抽检额度）：摆进检测表单这一列，
                不放进单次报告结果区——这些设置跟「这一次测什么」无关，
                换渠道、看报告时也要能改。 */}
            <section className="border-t border-border pt-4">
                <BackgroundAuditPanel />
            </section>

            <section className="border-t border-border pt-4">
                {/* 全页只有一个提交动作：模式决定它的含义，旧按钮不会在切模式后
                    还亮着、点下去却按老规矩跑一轮探针。 */}
                <Button
                    type="button"
                    className="h-12 w-full rounded-xl text-base font-semibold"
                    disabled={hlwyMode ? hlwyStart.disabled : (enqueueAudit.isPending || runCount === 0 || !channel)}
                    onClick={() => void (hlwyMode ? submitHlwy() : submitToQueue([activeModel]))}
                    title={activeModel ? (hlwyMode ? `开始基准对照 · ${activeModel}` : `开始检测 · ${activeModel}`) : undefined}
                >
                    {(hlwyMode ? enqueueHlwy.isPending : enqueueAudit.isPending)
                        ? <Loader2 className="size-4 animate-spin" />
                        : hlwyMode ? <Binary className="size-4" /> : <Play className="size-4 fill-current" />}
                    <span className="min-w-0 truncate">
                        {(hlwyMode ? enqueueHlwy.isPending : enqueueAudit.isPending)
                            ? '提交中…'
                            : hlwyMode ? '开始基准对照' : '开始检测'}
                    </span>
                </Button>
                {hlwyMode && hlwyStart.disabled && hlwyStart.reason && (
                    <p className="mt-2 text-center text-xs leading-5 text-amber-700">{hlwyStart.reason}</p>
                )}
                <p className="mt-2.5 flex items-center justify-center gap-1 text-center text-xs text-muted-foreground">
                    {hlwyMode
                        ? `${hlwyPlan
                              ? `会真的调用上游最多 ${hlwyPlan.max_calls} 次，按正常价格计费。`
                              : '计划读取后可查看调用上限，按正常价格计费。'}提交后可以离开本页。`
                        : '会真的调用上游，按正常价格计费。提交后可以离开本页。'}
                    <span className="cursor-help text-muted-foreground/80" title="次数是估算：个别检查会多轮或重试，实际以记录为准。超时跟随渠道检测设置。">ⓘ</span>
                </p>
                {!hlwyMode && (
                    <button
                        type="button"
                        className="mt-2 w-full text-center text-xs text-primary underline-offset-2 hover:underline disabled:opacity-50"
                        disabled={enqueueAudit.isPending || models.length === 0 || !channel}
                        onClick={() => void submitToQueue(models)}
                    >
                        或者，一次测完这个渠道的全部 {models.length} 个模型
                    </button>
                )}
            </section>
        </aside>
    );

    return (
        <PageWrapper className="h-full min-h-0 space-y-4 overflow-y-auto overscroll-contain rounded-t-3xl pb-24 md:pb-4">
            {/* 顶栏已有「检测」标题，这里只留副标题 + 检测/记录切换，避免手机上标题说两遍。 */}
            <div className="flex flex-wrap items-center justify-between gap-3 px-1">
                <p className="min-w-0 flex-1 text-sm text-muted-foreground">
                    看看渠道给你的模型，是不是它说的那个。
                </p>
                <div className="flex shrink-0 gap-1 rounded-2xl border border-border bg-card p-1">
                    <button
                        type="button"
                        onClick={() => setMode('single')}
                        className={cn(
                            'rounded-xl px-3 py-1.5 text-sm transition-colors',
                            mode === 'single' ? 'bg-primary text-primary-foreground' : 'text-muted-foreground hover:bg-muted',
                        )}
                    >
                        检测
                    </button>
                    <button
                        type="button"
                        onClick={() => setMode('queue')}
                        className={cn(
                            'rounded-xl px-3 py-1.5 text-sm transition-colors',
                            mode === 'queue' ? 'bg-primary text-primary-foreground' : 'text-muted-foreground hover:bg-muted',
                        )}
                    >
                        记录
                    </button>
                </div>
            </div>

            {mode === 'queue' && <ModelAuditQueue onOpenReport={openStoredReport} />}

            {mode === 'single' && (
            <>
            {/* 手机：单列表单（渠道/模型/快速全面/开始检测），不再用「调整参数」折叠卡。 */}
            <div className="md:hidden">{renderParams()}</div>

            <div className="flex min-h-0 flex-col gap-4 md:flex-row">
                <div className="hidden md:block md:w-[min(100%,420px)] md:shrink-0">{renderParams()}</div>

                {/* 有结果才展示结果面板；空闲时桌面右侧放最近检测 + 日志查账。 */}
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
                                <p className="mt-1 text-sm text-muted-foreground">{copy.sub}</p>
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
                    <RecentAudits onOpenAll={() => setMode('queue')} />
                    <PassiveProbeCard />
                </div>
                )}
            </div>

            {/* 手机：表单下方接日志查账 + 最近检测（对齐 mobile 稿）。 */}
            <div className="grid gap-4 md:hidden">
                <PassiveProbeCard />
                <RecentAudits onOpenAll={() => setMode('queue')} />
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
