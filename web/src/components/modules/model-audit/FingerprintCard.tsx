'use client';

import { useMemo, useState } from 'react';
import { ArrowRight, Check, ChevronDown, CircleAlert, Info, Loader2, RotateCcw, TriangleAlert, X } from 'lucide-react';
import { getModelIcon } from '@/lib/model-icons';
import { cn } from '@/lib/utils';
import { describeDecision, formatProbability, type FingerprintCandidate, type FingerprintSummary } from './fingerprint';

/**
 * 模型指纹结论卡。
 *
 * 一眼要能看到四件事：声明的型号 → 实测最像谁、有多确定；三条回答是否都有效、
 * 像不像模型手写；声明的型号在库里排第几；最像的那个本来就和谁难分开。
 *
 * 边界照抄后端措辞，一个字都不放宽：这是**参考库内的相对排名**，库外模型也会得到
 * 一个"最像"的答案；"不像模型手写""取值异常""置信度低"都只算证据不足，不定罪。
 */

const VISIBLE_CANDIDATES = 5;

type Tone = 'pass' | 'warn' | 'fail' | 'muted';

function toneClass(tone: Tone) {
    return cn(
        tone === 'pass' && 'bg-primary/15 text-primary',
        tone === 'warn' && 'bg-amber-500/15 text-amber-800 dark:text-amber-300',
        tone === 'fail' && 'bg-destructive/10 text-destructive',
        tone === 'muted' && 'bg-muted text-muted-foreground',
    );
}

function RetestButton({ onRetest, retesting, prominent }: { onRetest?: () => void; retesting?: boolean; prominent?: boolean }) {
    if (!onRetest) return null;
    return (
        <button
            type="button"
            onClick={onRetest}
            disabled={retesting}
            className={cn(
                'inline-flex shrink-0 items-center gap-1 self-start rounded-full sm:self-auto px-2.5 py-1 text-[11px] font-medium transition-colors disabled:opacity-60',
                prominent
                    ? 'bg-amber-600 text-white hover:bg-amber-700 dark:bg-amber-500 dark:text-amber-950'
                    : 'border border-border text-foreground hover:bg-muted',
            )}
        >
            {retesting ? <Loader2 className="size-3 animate-spin" /> : <RotateCcw className="size-3" />}
            再测一轮
        </button>
    );
}

function CandidateRow({ candidate, rank, isDeclared, isTop }: { candidate: FingerprintCandidate; rank: number; isDeclared: boolean; isTop: boolean }) {
    const hasProbability = candidate.probability !== undefined;
    return (
        <li
            className={cn(
                'grid grid-cols-[22px_minmax(0,1fr)_auto] items-center gap-x-2 gap-y-1 border-t border-border/60 py-2 text-xs first:border-t-0',
                'sm:grid-cols-[22px_minmax(0,1.6fr)_52px_minmax(64px,1fr)_44px]',
                isDeclared && '-mx-2 rounded-lg border-t-0 bg-primary/8 px-2',
            )}
        >
            <span className="text-muted-foreground tabular-nums">{rank}</span>
            <span className="flex min-w-0 items-center gap-1.5">
                <span className={cn('truncate', isTop && 'font-semibold')}>{candidate.displayName}</span>
                {isDeclared && <span className="shrink-0 rounded bg-primary/15 px-1 text-[10px] font-medium text-primary">声明</span>}
            </span>
            <span className="hidden truncate text-[11px] text-muted-foreground sm:block">{candidate.familyName}</span>
            {hasProbability ? (
                <span className="col-span-3 col-start-1 h-1.5 overflow-hidden rounded-full bg-muted sm:col-span-1 sm:col-start-auto">
                    <span
                        className={cn('block h-full rounded-full', isTop ? 'bg-primary' : 'bg-primary/45')}
                        style={{ width: `${Math.max(1.5, (candidate.probability ?? 0) * 100)}%` }}
                    />
                </span>
            ) : (
                <span className="hidden sm:block" />
            )}
            <span className="col-start-3 row-start-1 text-right tabular-nums text-muted-foreground sm:col-start-auto sm:row-start-auto">
                {formatProbability(candidate.probability) || '—'}
            </span>
        </li>
    );
}

export function FingerprintCard({
    summary,
    declared,
    onRetest,
    retesting,
}: {
    summary: FingerprintSummary;
    /** 用户检测时选的型号（声明）。 */
    declared: string;
    onRetest?: () => void;
    retesting?: boolean;
}) {
    const [showAll, setShowAll] = useState(false);
    const top = summary.candidates[0];
    const second = summary.candidates[1];
    const ranked = !summary.errorText && summary.decision !== 'not_model' && summary.decision !== 'unscorable' && !!top;
    const probability = summary.probability;
    const builtAt = summary.referenceBuiltAt.slice(0, 10);
    const modelCount = summary.modelCount || summary.candidates.length;

    const declaredCandidate = useMemo(
        () => (summary.declaredModel ? summary.candidates.find((c) => c.model === summary.declaredModel) : undefined),
        [summary.candidates, summary.declaredModel],
    );
    const declaredRank = summary.declaredRank;

    let verdict: { text: string; tone: Tone } | null = null;
    if (ranked) {
        if (!declaredCandidate) verdict = { text: '声明的型号不在参考库里，只能看最像谁', tone: 'muted' };
        else if (declaredRank === 1) verdict = { text: '和声明一致（不等于正品证明）', tone: 'pass' };
        else if (declaredCandidate.familyName && declaredCandidate.familyName === top.familyName)
            verdict = { text: '同家族、不同型号 · 待复核', tone: 'warn' };
        else verdict = { text: '和声明不是一个家族 · 待复核', tone: 'fail' };
    }

    const lead =
        probability !== undefined && second?.probability !== undefined
            ? Math.round((probability - second.probability) * 100)
            : undefined;

    // 候选：默认前五；声明型号排在后面时单独补一行，免得要展开才看得到。
    const visible = showAll ? summary.candidates : summary.candidates.slice(0, VISIBLE_CANDIDATES);
    const declaredOutside =
        !showAll && declaredCandidate && declaredRank !== undefined && declaredRank > VISIBLE_CANDIDATES ? declaredCandidate : undefined;

    const anomalous = summary.anomalousSamples.length > 0;
    const needsRetest =
        !!summary.errorText || summary.decision === 'not_model' || summary.decision === 'unscorable' || summary.lowConfidence || anomalous;
    const { Avatar, color } = getModelIcon(top?.model ?? '');

    return (
        <div data-fp-card className="overflow-hidden rounded-2xl border border-border bg-card">
            {/* 1. 结论 */}
            <div className="px-4 pt-3 pb-4">
                <div className="flex items-center justify-between gap-2 text-[11px] text-muted-foreground">
                    <span className="font-medium tracking-[0.16em]">模型指纹 · 参考库比对</span>
                    {builtAt && (
                        <span className="hidden sm:inline">
                            参考库 {builtAt}
                            {modelCount > 0 && ` · ${modelCount} 个模型`}
                        </span>
                    )}
                </div>

                {ranked ? (
                    <>
                        <div className="mt-3 flex items-start justify-between gap-3">
                            <div className="min-w-0">
                                <div className="flex min-w-0 flex-wrap items-center gap-1.5 text-xs text-muted-foreground">
                                    <span>声明</span>
                                    <span className="max-w-[60vw] truncate rounded-md bg-muted px-1.5 py-0.5 font-mono text-[11px] text-foreground sm:max-w-none">
                                        {declared || '—'}
                                    </span>
                                    <ArrowRight className="size-3.5" />
                                    <span>实测最像</span>
                                </div>
                                <div className="mt-1.5 flex items-center gap-2.5">
                                    <span className="grid size-9 shrink-0 place-items-center rounded-xl bg-muted/60">
                                        <Avatar size={22} style={{ color }} />
                                    </span>
                                    <div className="min-w-0">
                                        <p className="truncate text-lg font-semibold leading-tight sm:text-xl">{summary.predictionName}</p>
                                        {summary.familyName && <p className="text-xs text-muted-foreground">{summary.familyName} 家族</p>}
                                    </div>
                                </div>
                            </div>
                            {probability !== undefined ? (
                                <div className="shrink-0 text-right">
                                    <p className="text-2xl font-semibold tabular-nums leading-none sm:text-3xl">{formatProbability(probability)}</p>
                                    <p className="mt-1 text-[11px] text-muted-foreground">库内置信度</p>
                                    {lead !== undefined && lead > 0 && (
                                        <p className="hidden text-[11px] text-muted-foreground sm:block">比第二名高 {lead} 个百分点</p>
                                    )}
                                </div>
                            ) : (
                                <div className="shrink-0 rounded-xl bg-muted/50 px-2.5 py-1.5 text-right text-[11px] text-muted-foreground">
                                    只给排名
                                    <br />
                                    不给置信度
                                </div>
                            )}
                        </div>
                        <div className="mt-3 flex flex-wrap items-center gap-1.5">
                            {verdict && (
                                <span className={cn('inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-[11px] font-medium', toneClass(verdict.tone))}>
                                    {verdict.tone === 'pass' ? <Check className="size-3" /> : verdict.tone === 'muted' ? <Info className="size-3" /> : <TriangleAlert className="size-3" />}
                                    {verdict.text}
                                </span>
                            )}
                            {declaredCandidate && declaredRank !== undefined && declaredRank > 1 && (
                                <span className="rounded-full bg-muted px-2 py-0.5 text-[11px] text-muted-foreground">
                                    声明的型号排第 {declaredRank} 名
                                    {summary.declaredProbability !== undefined && ` · ${formatProbability(summary.declaredProbability)}`}
                                </span>
                            )}
                            {summary.decision === 'partial' && (
                                <span className="rounded-full bg-muted px-2 py-0.5 text-[11px] text-muted-foreground">有效回答不足三条</span>
                            )}
                        </div>
                    </>
                ) : (
                    <div className="mt-3 flex items-start gap-2.5">
                        <span className="grid size-9 shrink-0 place-items-center rounded-xl bg-amber-500/12 text-amber-700 dark:text-amber-300">
                            <CircleAlert className="size-5" />
                        </span>
                        <div className="min-w-0">
                            <p className="text-lg font-semibold leading-tight">
                                {summary.errorText ? '这次没测成' : summary.decision === 'not_model' ? '回答像脚本生成' : '证据不足'}
                            </p>
                            <p className="mt-1 text-xs leading-5 text-muted-foreground">{describeDecision(summary)}</p>
                            <p className="mt-1 text-[11px] text-muted-foreground">不给排名、不打风险分。</p>
                        </div>
                    </div>
                )}
            </div>

            {/* 2. 三条样本 + 像不像模型手写 */}
            {summary.samples.length > 0 && (
                <div className="border-t border-border bg-muted/25 px-4 py-3">
                    <div className="grid grid-cols-3 gap-2">
                        {summary.samples.map((sample) => {
                            const ratio = sample.expected ? Math.min(100, (sample.parsed / sample.expected) * 100) : sample.accepted ? 100 : 30;
                            return (
                                <div
                                    key={sample.index}
                                    className={cn(
                                        'min-w-0 rounded-xl border bg-card px-2.5 py-2',
                                        !sample.accepted ? 'border-amber-500/40' : sample.anomalous ? 'border-amber-500/30' : 'border-border',
                                    )}
                                >
                                    <div className="flex items-center justify-between gap-1">
                                        <span className="text-[11px] font-medium">回答 {sample.index + 1}</span>
                                        {!sample.accepted ? (
                                            <X className="size-3.5 text-amber-600" />
                                        ) : sample.anomalous ? (
                                            <TriangleAlert className="size-3.5 text-amber-600" />
                                        ) : (
                                            <Check className="size-3.5 text-primary" />
                                        )}
                                    </div>
                                    <p className="mt-0.5 flex flex-col text-[11px] tabular-nums text-muted-foreground sm:flex-row sm:gap-1">
                                        <span className="font-mono">
                                            {sample.parsed}
                                            {sample.expected !== undefined && <span className="opacity-60">/{sample.expected}</span>}
                                        </span>
                                        {sample.language && (
                                            <span className="truncate">
                                                <span className="hidden sm:inline">· </span>
                                                {sample.language}
                                            </span>
                                        )}
                                    </p>
                                    <div className="mt-1.5 h-1 overflow-hidden rounded-full bg-muted">
                                        <div
                                            className={cn('h-full rounded-full', sample.accepted && !sample.anomalous ? 'bg-primary/70' : 'bg-amber-500/70')}
                                            style={{ width: `${ratio}%` }}
                                        />
                                    </div>
                                    {sample.anomalous && <p className="mt-1 truncate text-[10px] text-amber-700 dark:text-amber-300">取值偏大</p>}
                                    {!sample.accepted && <p className="mt-1 truncate text-[10px] text-amber-700 dark:text-amber-300">数字太少，没采用</p>}
                                </div>
                            );
                        })}
                    </div>
                    {summary.modelLike !== undefined && (
                        <p className="mt-2 flex items-center gap-1.5 text-[11px]">
                            {summary.modelLike ? (
                                <>
                                    <Check className="size-3 text-primary" />
                                    <span className="text-foreground">回答像模型手写</span>
                                </>
                            ) : (
                                <>
                                    <TriangleAlert className="size-3 text-amber-600" />
                                    <span className="text-amber-800 dark:text-amber-300">回答像脚本生成</span>
                                </>
                            )}
                        </p>
                    )}
                </div>
            )}

            {/* 3. 需要再测一轮的提示（低置信度 / 取值异常 / 证据不足） */}
            {needsRetest && (
                <div className="flex flex-col gap-2 border-t border-amber-500/25 bg-amber-500/8 px-4 py-2.5 text-xs text-amber-900 sm:flex-row sm:items-center dark:text-amber-200">
                    <div className="flex min-w-0 flex-1 gap-2">
                        <CircleAlert className="mt-0.5 size-3.5 shrink-0" />
                        <div className="min-w-0 space-y-0.5">
                            {summary.lowConfidence && <p>置信度低于 75%：这一轮还不够确定，建议再测一轮，看结论是否稳定。</p>}
                            {anomalous && (
                                <p>
                                    第 {summary.anomalousSamples.map((i) => i + 1).join('、')} 条回答的数字整体偏大（最小的都不小于 200），和模型平时的写法不一样，结论仅供参考。
                                </p>
                            )}
                            {!summary.lowConfidence && !anomalous && <p>这一轮证据不够，换一批题再测一轮通常就好。</p>}
                        </div>
                    </div>
                    <RetestButton onRetest={onRetest} retesting={retesting} prominent />
                </div>
            )}

            {/* 4. 候选排名 */}
            {ranked && summary.candidates.length > 0 && (
                <div className="border-t border-border px-4 pt-2 pb-1">
                    <div className="flex items-center justify-between py-1 text-[11px] text-muted-foreground">
                        <span>候选排名</span>
                        {probability !== undefined && <span>只在参考库里比，合计 100%</span>}
                    </div>
                    <ol>
                        {visible.map((candidate, index) => (
                            <CandidateRow
                                key={candidate.model}
                                candidate={candidate}
                                rank={index + 1}
                                isTop={index === 0}
                                isDeclared={candidate.model === summary.declaredModel}
                            />
                        ))}
                    </ol>
                    {declaredOutside && declaredRank !== undefined && (
                        <ol className="border-t border-dashed border-border/80">
                            <CandidateRow candidate={declaredOutside} rank={declaredRank} isTop={false} isDeclared />
                        </ol>
                    )}
                    {summary.candidates.length > VISIBLE_CANDIDATES && (
                        <button
                            type="button"
                            onClick={() => setShowAll((value) => !value)}
                            className="flex items-center gap-1 py-2 text-[11px] font-medium text-primary"
                        >
                            <ChevronDown className={cn('size-3.5 transition-transform', showAll && 'rotate-180')} />
                            {showAll ? '收起' : `显示全部 ${summary.candidates.length} 项`}
                        </button>
                    )}
                </div>
            )}

            {/* 5. 本来就难分开的型号 */}
            {ranked && summary.neighbors.length > 0 && (
                <div className="border-t border-border px-4 py-3">
                    <p className="text-[11px] text-muted-foreground">
                        <span className="text-foreground">{summary.predictionName}</span> 和这几个型号本来就难分开：
                    </p>
                    <div className="mt-1.5 flex flex-wrap gap-1.5">
                        {summary.neighbors.map((name) => (
                            <span key={name} className="rounded-lg border border-border bg-muted/30 px-2 py-1 text-[11px]">
                                {name}
                            </span>
                        ))}
                    </div>
                </div>
            )}

            {/* 6. 怎么理解 + 这轮怎么测的 */}
            <details className="group border-t border-border px-4 py-2.5 text-[11px] leading-5 text-muted-foreground">
                <summary className="flex cursor-pointer list-none items-center gap-1.5">
                    <Info className="size-3.5 shrink-0" />
                    <span className="min-w-0 flex-1">怎么理解：这是参考库内的相对排名，只能证伪、不能认证</span>
                    <ChevronDown className="size-3.5 shrink-0 transition-transform group-open:rotate-180" />
                </summary>
                <div className="mt-1.5 space-y-1">
                    <p>库外的模型也会得到一个「最像」的答案；相邻小版本本来就难分开。声明是 A、却怎么测都不像 A，才值得追问。</p>
                    <p>
                        {summary.promptMode === 'fixed' ? '这轮用的是固定题（对照用）。' : '这轮的题目是随机生成的（语言和措辞每次都换，防止被认出来）。'}
                        {summary.usedOutputs > 0 && `用了 ${summary.usedOutputs} 条有效回答。`}
                        {builtAt && `参考库构建于 ${builtAt}${modelCount > 0 ? `，共 ${modelCount} 个模型` : ''}。`}
                    </p>
                </div>
            </details>
            {!needsRetest && onRetest && (
                <div className="flex items-center justify-between gap-2 border-t border-border px-4 py-2 text-[11px] text-muted-foreground">
                    <span>想确认结论稳不稳，可以换一批题再测。</span>
                    <RetestButton onRetest={onRetest} retesting={retesting} />
                </div>
            )}
        </div>
    );
}
