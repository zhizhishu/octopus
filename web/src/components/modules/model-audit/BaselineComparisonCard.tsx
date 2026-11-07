'use client';

import { useEffect, useState } from 'react';
import { Button } from '@/components/ui/button';
import { SearchableSelect, type SearchableSelectOption } from '@/components/ui/searchable-select';
import { cn } from '@/lib/utils';
import {
    HLWY_AXIS_TICKS,
    HLWY_CHART_HEIGHT,
    hlwyBarHeightPx,
    hlwyBinRange,
    hlwyComparabilityLabel,
    hlwyComparabilityTone,
    hlwyConditionCompareRows,
    hlwyConditionRows,
    hlwyEntryUnavailableText,
    hlwyExecutionLabel,
    hlwyBaselineOptionLabel,
    hlwyReasonMessages,
    hlwySourceLabel,
    hlwyStopReasonLabel,
    type HlwyComparisonView,
    type HlwySaveEligibility,
} from './hlwy';
import type { HlwyBaselineEntry } from '@/api/endpoints/model-audit';

/**
 * 报告页的「基准对照」块。
 *
 * 这一块自己说清四件事，顺序固定：这一轮采样走了多少槽 → 和主对照差多少 →
 * 前三名与差距 → 数字分布长什么样；后面才是采样条件与导出。
 *
 * 两条硬约束：
 *  - 不兼容（incompatible）或样本不足时**不出现任何百分比**。分数只在报告
 *    自己说 scorable 时展示，前端不替它推断。
 *  - 换基准重算只在本卡片里预览，存下来的原报告一个字都不改；页面顶部有明确
 *    提示条与「改回原报告」。
 */

const TONE_CLASS: Record<'ok' | 'warn' | 'ref', string> = {
    ok: 'bg-emerald-50 text-emerald-700 border-emerald-200',
    warn: 'bg-amber-50 text-amber-800 border-amber-200',
    ref: 'bg-muted text-muted-foreground border-border',
};

/** 原因码列表前面的小圆点：不可对照红、有差异黄、不知道灰。 */
const REASON_DOT_CLASS: Record<'incompatible' | 'differs' | 'unknown' | 'other', string> = {
    incompatible: 'bg-red-500',
    differs: 'bg-amber-500',
    unknown: 'bg-muted-foreground/50',
    other: 'bg-muted-foreground/50',
};

/** 条件对照表里的判定标志。 */
const CONDITION_STATE_TEXT: Record<'same' | 'differs' | 'unknown', string> = {
    same: '一致',
    differs: '不同',
    unknown: '未知',
};

const CONDITION_STATE_CLASS: Record<'same' | 'differs' | 'unknown', string> = {
    same: 'bg-emerald-50 text-emerald-700',
    differs: 'bg-amber-50 text-amber-800',
    unknown: 'bg-muted text-muted-foreground',
};

export type BaselineComparisonCardProps = {
    /** 这张卡片属于哪一份报告（结果 id）。变了就把卡片内部的临时选择清掉。 */
    reportKey?: string | number;
    view: HlwyComparisonView;
    /** 换基准重算的临时视图；非空时说明屏幕上的数字不是存下来的那一份。 */
    recompareActive: boolean;
    catalog: HlwyBaselineEntry[];
    onRecompare: (baselineId: string) => void;
    recomparePending: boolean;
    recompareError: string;
    onRestoreOriginal: () => void;
    saveEligibility: HlwySaveEligibility;
    onSave: () => void;
    saving: boolean;
    onRerun: () => void;
    onExportFull: () => void;
    onExportLegacy: () => void;
    canExportLegacy: boolean;
};

function MetaLine({ view }: { view: HlwyComparisonView }) {
    const chosen = view.chosen;
    return (
        <div className="mt-1 flex min-w-0 max-w-full flex-wrap gap-x-3 gap-y-1 text-[11px] leading-5 text-muted-foreground [&>span]:min-w-0 [&>span]:max-w-full [&>span]:break-all">
            <span>来源：{chosen ? chosen.sourceText : '未知'}</span>
            <span>采集日期：{chosen?.collectedAt ? chosen.collectedAt.slice(0, 10) : '日期未知'}</span>
            <span>参考样本：{chosen?.validCount ?? '未知'} 个</span>
        </div>
    );
}

function DistributionChart({ view }: { view: HlwyComparisonView }) {
    if (view.bins.length === 0) {
        return <p className="text-[11px] text-muted-foreground">报告里没有可用的分布数据，不画图。</p>;
    }
    // 缩放用的最大值不含 0；全部为 0 时每根柱子都是 0 高，不画假的 1px。
    const scale = view.bins.reduce((max, bin) => Math.max(max, bin.run, bin.baseline ?? 0), 0);
    const hasBaseline = view.bins.some((bin) => bin.baseline !== null);
    return (
        <div>
            <div
                className="flex items-end gap-[1px]"
                style={{ height: `${HLWY_CHART_HEIGHT}px` }}
                role="img"
                aria-label="本渠道与基准的 36 段数字分布对比"
            >
                {view.bins.map((bin, index) => (
                    <div
                        key={bin.from}
                        className="flex h-full min-w-0 flex-1 items-end gap-[1px]"
                        title={`${hlwyBinRange(index).label} 区间`}
                    >
                        <div
                            data-series="run"
                            className="w-1/2 rounded-t-sm bg-primary"
                            style={{ height: `${hlwyBarHeightPx(bin.run, scale)}px` }}
                        />
                        {/* 基准没这一段数据就干脆不画：画一根 0 高的柱子会被读成「基准里没有」。 */}
                        {bin.baseline !== null && (
                            <div
                                data-series="baseline"
                                className="w-1/2 rounded-t-sm bg-muted-foreground/40"
                                style={{ height: `${hlwyBarHeightPx(bin.baseline, scale)}px` }}
                            />
                        )}
                    </div>
                ))}
            </div>
            <div className="relative mt-1 h-3 text-[10px] tabular-nums text-muted-foreground">
                {HLWY_AXIS_TICKS.map((tick) => (
                    <span
                        key={tick.digit}
                        className="absolute top-0 whitespace-nowrap"
                        style={{
                            left: `${tick.pct}%`,
                            transform:
                                tick.pct === 0
                                    ? 'none'
                                    : tick.pct === 100
                                      ? 'translateX(-100%)'
                                      : 'translateX(-50%)',
                        }}
                    >
                        {tick.digit}
                    </span>
                ))}
            </div>
            <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px] text-muted-foreground">
                <span className="flex items-center gap-1">
                    <span className="inline-block h-2 w-2 rounded-sm bg-primary" />本渠道
                </span>
                {hasBaseline && (
                    <span className="flex items-center gap-1">
                        <span className="inline-block h-2 w-2 rounded-sm bg-muted-foreground/40" />主对照基准
                    </span>
                )}
                <span>横轴是数字 1–355 的位置，36 段，每段占比</span>
            </div>
            <details className="mt-1.5 rounded-lg bg-muted/30 px-2 py-1.5">
                <summary className="cursor-pointer list-none text-[11px] font-medium text-muted-foreground">
                    看数字表（36 段，共 355 个数）
                </summary>
                <table className="mt-1.5 w-full table-fixed border-collapse text-[11px] break-all">
                    <thead>
                        <tr className="text-muted-foreground">
                            <th className="py-1 text-left font-medium">区间</th>
                            <th className="py-1 text-right font-medium">本渠道</th>
                            <th className="py-1 text-right font-medium">基准</th>
                        </tr>
                    </thead>
                    <tbody>
                        {view.bins.map((bin, index) => (
                            <tr key={bin.from} className="border-t border-border/60">
                                <td className="py-0.5">{hlwyBinRange(index).label}</td>
                                <td className="py-0.5 text-right">{(bin.run * 100).toFixed(1)}%</td>
                                <td className="py-0.5 text-right">
                                    {bin.baseline === null ? '—' : `${(bin.baseline * 100).toFixed(1)}%`}
                                </td>
                            </tr>
                        ))}
                    </tbody>
                </table>
            </details>
        </div>
    );
}

export function BaselineComparisonCard({
    view,
    recompareActive,
    catalog,
    onRecompare,
    recomparePending,
    recompareError,
    onRestoreOriginal,
    saveEligibility,
    onSave,
    saving,
    onRerun,
    onExportFull,
    onExportLegacy,
    canExportLegacy,
    reportKey,
}: BaselineComparisonCardProps) {
    const [expandAll, setExpandAll] = useState(false);
    const [recomparePick, setRecomparePick] = useState('');
    const tone = hlwyComparabilityTone(view.comparability);
    const visibleMatches = expandAll ? view.matches : view.top;
    const conditionRows = hlwyConditionRows(view.conditions);
    // 主对照的采样条件从快照表里查；查不到就不做对照，不拿本轮条件充数。
    const chosenSnapshot = view.snapshots.find((snap) => snap.id === view.chosenBaselineId) ?? null;
    const conditionCompare = hlwyConditionCompareRows(view.conditions, chosenSnapshot?.conditions ?? null);
    const reasonMessages = hlwyReasonMessages(view.reasonCodes);
    // 换了一份报告，上一次选的基准不能跟过来：那是 A 的选择，不是 B 的意图。
    // 不清的话，B 卡片上会预先填好一份用户从没为 B 选过的基准，一按就重算。
    useEffect(() => {
        setRecomparePick('');
    }, [reportKey]);
    // 选中的那份如果后来被标成不可用（目录刷新后残留的旧选择），重算也要拦住。
    const recomparePickEntry = catalog.find((entry) => entry.id === recomparePick) ?? null;
    const recompareBlocked = recomparePickEntry?.unavailable === true;
    // 不可用的目录项不能拿去重算：同样的置灰规则与提交页一致。
    // 显式标成 SearchableSelectOption[]：keywords 是**字符串**（搜索时拼在一起比），
    // 顺手让 tsc 守住 disabled 这个字段真的存在，而不是靠替身把契约蒙过去。
    const recompareOptions: SearchableSelectOption[] = catalog.map((entry) => ({
        value: entry.id,
        label: hlwyBaselineOptionLabel(entry),
        keywords: [entry.label, entry.source_kind, hlwySourceLabel(entry.source_kind), entry.declared_model, entry.collected_at, entry.valid_count].filter((value) => value != null).join(' '),
        disabled: entry.unavailable === true,
    }));

    return (
        <div className="w-full min-w-0 max-w-full space-y-3 rounded-xl border border-border p-3 [overflow-wrap:anywhere]">
            {recompareActive && (
                <div className="flex flex-wrap items-center gap-2 rounded-lg border border-amber-200 bg-amber-50 px-2 py-1.5 text-[11px] leading-5 text-amber-900">
                    <span>下面是本地重算的预览，存下来的原报告没有改动。</span>
                    <button type="button" className="underline" onClick={onRestoreOriginal}>
                        改回原报告
                    </button>
                </div>
            )}

            <div className="flex flex-wrap items-center gap-2">
                <span className="text-sm font-semibold">数字习惯对照</span>
                <span className="rounded-full border border-border bg-muted px-1.5 py-px text-[10px] text-muted-foreground">
                    {hlwyExecutionLabel(view.execution)}
                </span>
                <span className={cn('rounded-full border px-1.5 py-px text-[10px]', TONE_CLASS[tone])}>
                    {hlwyComparabilityLabel(view.comparability)}
                </span>
                {view.selfCompare && (
                    <span className="rounded-full border border-amber-200 bg-amber-50 px-1.5 py-px text-[10px] text-amber-800">
                        同一批样本自比，不构成独立证据
                    </span>
                )}
            </div>

            {view.problem && (
                <p className="rounded-lg bg-muted/40 px-2 py-1.5 text-[11px] leading-5 text-muted-foreground">
                    报告只能部分读取：{view.problem}。下面只展示能确认的内容。
                </p>
            )}

            {/* 常态只报有效回答；完整计数保留在计算详情。 */}
            <div className="flex flex-wrap gap-x-3 gap-y-1 text-[11px] leading-5">
                {view.counts ? (
                    <>
                        <span className="font-medium">收到{view.counts.valid}个有效回答</span>
                        {view.counts.invalid > 0 && <span className="text-muted-foreground">无效 {view.counts.invalid}</span>}
                        {view.counts.failed > 0 && <span className="text-muted-foreground">失败 {view.counts.failed}</span>}
                        {view.counts.unresolved > 0 && <span className="text-amber-700">未确认 {view.counts.unresolved}</span>}
                    </>
                ) : (
                    <span className="text-muted-foreground">有效回答数量未知</span>
                )}
            </div>

            {view.stopReason && (
                <p className="text-[11px] leading-5 text-amber-800">{hlwyStopReasonLabel(view.stopReason)}</p>
            )}
            {view.partial && (
                <p className="rounded-lg bg-muted/40 px-2 py-1.5 text-[11px] leading-5 text-muted-foreground">
                    这一轮提前结束了，下面保留了已收到的回答。
                </p>
            )}

            {/* 分数 + 旁边就是来源/日期/样本量：光看分数不足以判断这份基准够不够格。 */}
            <div className="min-w-0 rounded-lg bg-muted/30 px-3 py-2">
                <p className="mb-1 text-sm font-medium break-all">{view.chosen?.label || chosenSnapshot?.label || '参考名称未知'}</p>
                {view.hasScore && view.chosen?.percent ? (
                    <>
                        <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1">
                            <span className="shrink-0 text-2xl font-semibold tabular-nums">{view.chosen.percent}</span>
                            <span className="text-xs text-muted-foreground">相似度</span>
                        </div>
                        <p className="mt-0.5 text-[11px] leading-5 text-muted-foreground">
                            这是回答习惯的相似度，不是模型真伪概率
                        </p>
                        <MetaLine view={view} />
                    </>
                ) : (
                    <>
                        <p className="text-xs leading-5">
                            {view.comparability === 'incompatible'
                                ? '两边协议或题目不兼容，这里不给相似度。'
                                : view.sufficiency === 'insufficient'
                                  ? '有效样本不足，不给相似度分数。'
                                  : view.scorable === false
                                    ? '报告判定这一轮不可评分，不给相似度分数。'
                                    : '这份报告没有给出可展示的相似度。'}
                        </p>
                        <MetaLine view={view} />
                    </>
                )}
            </div>

            {/* 前三名与差距只在有分时出现：没分的时候还列名次，读者就会把名次当分数用。 */}
            {view.hasScore && (
                <div>
                    <div className="flex flex-wrap items-center justify-between gap-2">
                        <span className="text-xs font-medium">
                            {expandAll ? `全部候选（${view.matches.length} 份）` : '最接近的 3 份参考'}
                        </span>
                        {view.matches.length > view.top.length && (
                            <button
                                type="button"
                                className="text-[11px] text-primary underline"
                                onClick={() => setExpandAll((prev) => !prev)}
                            >
                                {expandAll ? '只看前三' : `展开全部 ${view.matches.length} 份`}
                            </button>
                        )}
                    </div>
                    {view.comparability === 'unknown' && (
                        <p className="mt-1 text-[11px] leading-5 text-muted-foreground">参考资料的采样设置不完整，结果仅供参考</p>
                    )}
                    {view.matches.length === 0 ? (
                        <p className="mt-1 text-[11px] text-muted-foreground">报告里没有候选匹配。</p>
                    ) : (
                        <ul className="mt-1.5 space-y-1">
                            {visibleMatches.map((match) => (
                                <li
                                    key={match.baselineId}
                                    className={cn(
                                        'rounded-lg border px-2 py-1.5 text-[11px] leading-5',
                                        match.baselineId === view.chosenBaselineId
                                            ? 'border-primary/40 bg-primary/5'
                                            : 'border-border bg-background'
                                    )}
                                >
                                    <div className="flex min-w-0 items-baseline gap-x-2 gap-y-1">
                                        <span className="shrink-0 text-muted-foreground">#{match.rank}</span>
                                        <span className="min-w-0 flex-1 break-all font-medium">{match.label}</span>
                                        <span className="ml-auto shrink-0 tabular-nums">{match.percent ?? '不给分'}</span>
                                    </div>
                                    <div className="mt-0.5 flex min-w-0 flex-wrap gap-x-3 gap-y-1 text-muted-foreground [&>span]:min-w-0 [&>span]:max-w-full [&>span]:break-all">
                                        <span>{match.sourceText}</span>
                                        <span>{match.collectedAt ? match.collectedAt.slice(0, 10) : '日期未知'}</span>
                                        <span>{match.validCount ?? '未知'} 个样本</span>
                                        {match.gapText && <span>与上一名差 {match.gapText}</span>}
                                        {match.baselineId === view.chosenBaselineId && <span>主对照</span>}
                                    </div>
                                </li>
                            ))}
                        </ul>
                    )}
                </div>
            )}

            <DistributionChart view={view} />

            {/* 分数成分 + 基准快照指纹：只给一个总分等于让人无从复核。 */}
            <details className="rounded-lg bg-muted/30 px-2 py-1.5">
                <summary className="cursor-pointer list-none text-[11px] font-medium text-muted-foreground">
                    计算详情
                </summary>
                {view.counts && (
                    <p className="mt-1.5 text-[11px] leading-5 text-muted-foreground">
                        最多 {view.counts.planned_calls} 次调用 · 已发起 {view.counts.started_slots} · 有效 {view.counts.valid} · 无效 {view.counts.invalid} · 失败 {view.counts.failed} · 未确认 {view.counts.unresolved}
                    </p>
                )}
                <dl className="mt-1.5 grid grid-cols-1 gap-x-3 gap-y-1 text-[11px] sm:grid-cols-2">
                    {view.hasScore && view.chosen ? (
                        <>
                            <div className="flex gap-1.5">
                                <dt className="shrink-0 text-muted-foreground">综合相似度</dt>
                                <dd className="min-w-0 break-words tabular-nums">
                                    {view.chosen.percent ?? '未知'}
                                </dd>
                            </div>
                            <div className="flex gap-1.5">
                                <dt className="shrink-0 text-muted-foreground">余弦相似</dt>
                                <dd className="min-w-0 break-words tabular-nums">
                                    {view.chosen.cosine === null ? '未知' : view.chosen.cosine.toFixed(4)}
                                </dd>
                            </div>
                            <div className="flex gap-1.5">
                                <dt className="shrink-0 text-muted-foreground">JS 距离（越小越像）</dt>
                                <dd className="min-w-0 break-words tabular-nums">
                                    {view.chosen.jsd === null ? '未知' : view.chosen.jsd.toFixed(4)}
                                </dd>
                            </div>
                            <div className="flex gap-1.5">
                                <dt className="shrink-0 text-muted-foreground">众数一致度</dt>
                                <dd className="min-w-0 break-words tabular-nums">
                                    {view.chosen.modeScore === null ? '未知' : view.chosen.modeScore.toFixed(4)}
                                </dd>
                            </div>
                        </>
                    ) : (
                        <div className="flex gap-1.5 sm:col-span-2">
                            <dt className="shrink-0 text-muted-foreground">分数成分</dt>
                            <dd className="min-w-0 break-words">这一轮没有给分，也就没有成分可列。</dd>
                        </div>
                    )}
                    <div className="flex gap-1.5 sm:col-span-2">
                        <dt className="shrink-0 text-muted-foreground">基准快照 sha256</dt>
                        <dd className="min-w-0 break-all font-mono text-[10px]">
                            {view.chosen?.payloadSha256 || '基准快照里没有指纹（旧文件或报告不完整）'}
                        </dd>
                    </div>
                    {view.algorithmVersion && (
                        <div className="flex gap-1.5 sm:col-span-2">
                            <dt className="shrink-0 text-muted-foreground">算法版本</dt>
                            <dd className="min-w-0 break-all font-mono text-[10px]">{view.algorithmVersion}</dd>
                        </div>
                    )}
                </dl>
                {reasonMessages.length > 0 && (
                    <ul className="mt-1.5 space-y-0.5 text-[11px] leading-5">
                        {reasonMessages.map((reason, index) => (
                            <li key={`${reason.code}-${index}`} className="flex items-start gap-1.5">
                                <span className={cn('mt-1.5 size-1.5 shrink-0 rounded-full', REASON_DOT_CLASS[reason.severity])} />
                                <span className="min-w-0 flex-1">{reason.text}</span>
                            </li>
                        ))}
                    </ul>
                )}
                {(view.stats || view.quality) && (
                    <div className="mt-1.5 flex flex-wrap gap-x-3 gap-y-1 text-[11px] leading-5 text-muted-foreground">
                        {view.stats?.median != null && <span>中位数 {view.stats.median}</span>}
                        {view.stats?.mean != null && <span>均值 {view.stats.mean.toFixed(2)}</span>}
                        {view.stats?.mode != null && (
                            <span>众数 {view.stats.mode}{view.stats.modeCount != null ? `（${view.stats.modeCount} 次）` : ''}</span>
                        )}
                        {view.quality?.strictInteger != null && <span>严格整数回答 {view.quality.strictInteger} 个</span>}
                        {view.quality?.truncated != null && <span>被截断 {view.quality.truncated} 个</span>}
                    </div>
                )}
            </details>

            <details className="rounded-lg bg-muted/30 px-2 py-1.5">
                <summary className="cursor-pointer list-none text-[11px] font-medium text-muted-foreground">
                    采样设置
                </summary>
                {conditionCompare.length === 0 ? (
                    <>
                        <p className="mt-1 text-[11px] leading-5 text-muted-foreground">
                            基准快照里没有记采样条件，没法逐项对照。下面是本次这一轮的条件。
                        </p>
                        <dl className="mt-1 grid grid-cols-1 gap-x-3 gap-y-1 text-[11px] sm:grid-cols-2">
                            {conditionRows.map((row) => (
                                <div key={row.label} className="flex gap-1.5">
                                    <dt className="shrink-0 text-muted-foreground">{row.label}</dt>
                                    <dd className="min-w-0 break-words">{row.value}</dd>
                                </div>
                            ))}
                        </dl>
                    </>
                ) : (
                    <table className="mt-1.5 w-full table-fixed border-collapse text-[11px] break-all">
                        <thead>
                            <tr className="text-muted-foreground">
                                <th className="py-1 text-left font-medium">项目</th>
                                <th className="py-1 text-left font-medium">本渠道</th>
                                <th className="py-1 text-left font-medium">基准</th>
                                <th className="py-1 text-right font-medium">对照</th>
                            </tr>
                        </thead>
                        <tbody>
                            {conditionCompare.map((row) => (
                                <tr key={row.label} className="border-t border-border/60">
                                    <td className="py-0.5 text-muted-foreground">{row.label}</td>
                                    <td className="py-0.5 break-words">{row.run}</td>
                                    <td className="py-0.5 break-words">{row.baseline}</td>
                                    <td className="py-0.5 text-right">
                                        <span className={cn('rounded-full px-1.5 py-px text-[10px]', CONDITION_STATE_CLASS[row.state])}>
                                            {CONDITION_STATE_TEXT[row.state]}
                                        </span>
                                    </td>
                                </tr>
                            ))}
                        </tbody>
                    </table>
                )}
            </details>

            {/* 导出：一个完整报告（白名单），一个旧格式（能被别的 hlwy 工具读）。 */}
            <div className="space-y-2 border-t border-border pt-2">
                <div className="flex flex-wrap gap-2">
                    <Button variant="outline" size="sm" onClick={onExportFull}>
                        导出数据
                    </Button>
                    <details className="min-w-0 self-center text-[11px] text-muted-foreground">
                        <summary className="cursor-pointer">更多导出格式</summary>
                        <Button className="mt-1" variant="outline" size="sm" onClick={onExportLegacy} disabled={!canExportLegacy}>
                            导出旧 hlwy 格式
                        </Button>
                        {!canExportLegacy && <p className="mt-1">没有原始数字，导不出旧格式</p>}
                    </details>
                </div>

                <div className="flex flex-wrap items-center gap-2">
                    <Button variant="outline" size="sm" onClick={onSave} disabled={!saveEligibility.eligible || saving}>
                        {saving ? '保存中…' : '保存这次结果作参考'}
                    </Button>
                    <span className="text-[11px] text-muted-foreground">
                        {!saveEligibility.eligible && saveEligibility.reason}
                    </span>
                </div>

                <div className="w-full min-w-0 max-w-full">
                    <div className="flex flex-wrap items-center gap-2">
                        <span className="text-xs font-medium">换个参考比较</span>
                        <span className="text-[11px] text-muted-foreground">使用已有回答，不消耗渠道额度</span>
                    </div>
                    <div className="mt-1 flex w-full min-w-0 max-w-full items-center gap-2">
                        <div className="w-full min-w-0 max-w-full flex-1">
                            <SearchableSelect
                                value={recomparePick}
                                onValueChange={setRecomparePick}
                                options={recompareOptions}
                                className="w-full min-w-0 max-w-full"
                                placeholder="选一份参考"
                                searchPlaceholder="搜名称 / 来源 / 日期"
                                emptyText="没有可选参考"
                            />
                        </div>
                        <Button
                            className="shrink-0"
                            variant="outline"
                            size="sm"
                            disabled={!recomparePick || recomparePending || recompareBlocked}
                            onClick={() => onRecompare(recomparePick)}
                        >
                            {recomparePending ? '比较中…' : '比较'}
                        </Button>
                    </div>
                    {recomparePickEntry && (
                        <div className="mt-1 flex min-w-0 max-w-full flex-wrap gap-x-3 gap-y-1 text-[11px] leading-5 text-muted-foreground [&>span]:min-w-0 [&>span]:max-w-full [&>span]:break-all">
                            <span>来源：{hlwySourceLabel(recomparePickEntry.source_kind)}</span>
                            <span>采集日期：{recomparePickEntry.collected_at?.slice(0, 10) || '日期未知'}</span>
                            <span>有效样本：{recomparePickEntry.valid_count ?? '未知'} 个</span>
                        </div>
                    )}
                    {recompareBlocked && recomparePickEntry && (
                        <p className="mt-1 text-[11px] leading-5 text-amber-700">
                            选中的「{recomparePickEntry.label}」现在不能用：{hlwyEntryUnavailableText(recomparePickEntry)}。换一份再重算。
                        </p>
                    )}
                    {recompareError && <p className="mt-1 text-[11px] text-red-600">重算失败：{recompareError}</p>}
                </div>

                <div className="flex flex-wrap items-center gap-2">
                    <Button variant="outline" size="sm" onClick={onRerun}>
                        重新采样
                    </Button>
                    <span className="text-[11px] text-muted-foreground">只回填表单，点击「开始采样」后才会调用渠道</span>
                </div>
            </div>
        </div>
    );
}
