'use client';

/**
 * 渠道详情「最近探测」：这个渠道最近的主动探测记录（时间 / 模型 / key / 题目 / 回答 / 耗时 / 结果）。
 *
 * 默认**折叠**，而且和上面熔断条目的多少无关：探测历史是"回头看"的东西，
 * 熔断列表是"现在要处理"的东西，两者不该因为对方的数量互相展开。
 * 折叠时摘要只给最新一条（时间 + 结果），让人不点开也知道最后一次探成什么样。
 */

import { useState } from 'react';
import { ChevronDown, ChevronUp, Radar } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { useChannelProbes, type ChannelProbeHistoryEntry } from '@/api/endpoints/channel';
import { MonoSafeText } from '@/components/common/SafeText';
import { cn } from '@/lib/utils';
import {
    formatProbeClock,
    formatProbeDuration,
    probeHistorySummary,
    probeResultKind,
    probeResultLabel,
} from './probe-format';

/** 最近探测最多显示多少条（后端给多少都不撑爆页面）。 */
export const PROBE_HISTORY_LIMIT = 20;

const RESULT_TONE_CLASS: Record<string, string> = {
    recovered: 'bg-emerald-500/10 text-emerald-700 dark:text-emerald-300',
    failed: 'bg-destructive/10 text-destructive',
    skipped: 'bg-muted text-muted-foreground',
    noVerdict: 'bg-muted text-muted-foreground',
    unknown: 'bg-amber-500/10 text-amber-700 dark:text-amber-300',
};

function ResultBadge({ result }: { result: string | undefined }) {
    const t = useTranslations('channel.circuits');
    const kind = probeResultKind(result);
    return (
        <span
            className={cn('inline-flex w-fit items-center rounded-md px-1.5 py-0.5 text-xs', RESULT_TONE_CLASS[kind])}
            data-probe-result={kind}
        >
            {probeResultLabel(result, t)}
        </span>
    );
}

function dash(value: string | undefined): string {
    const text = (value ?? '').trim();
    return text === '' ? '—' : text;
}

function HistoryRow({ entry }: { entry: ChannelProbeHistoryEntry }) {
    const t = useTranslations('channel.circuits');
    const clock = formatProbeClock(entry.timestamp);
    const duration = formatProbeDuration(entry.duration_ms);
    const keyLabel = entry.key_id ? `#${entry.key_id}` : t('anyKey');
    return (
        <li className="border-b border-border px-3 py-2 text-xs last:border-0">
            {/* 窄屏：两行（模型·key + 结果 / 时间·题目→回答·耗时），不把七个字段挤成一条缝。 */}
            <div className="grid gap-1 sm:hidden">
                <div className="flex min-w-0 items-center justify-between gap-2">
                    <MonoSafeText value={`${entry.model || '—'} · ${keyLabel}`} className="min-w-0 truncate font-medium" />
                    <ResultBadge result={entry.result} />
                </div>
                <MonoSafeText
                    value={[clock, `${dash(entry.prompt)} → ${dash(entry.answer)}`, duration].filter(Boolean).join(' · ')}
                    className="min-w-0 truncate text-muted-foreground"
                />
            </div>
            {/* 宽屏：一行七列，和设计稿一致。 */}
            <div className="hidden min-w-0 grid-cols-[5.5rem_minmax(0,1.4fr)_3rem_minmax(0,1fr)_minmax(0,1fr)_4rem_auto] items-center gap-2 sm:grid">
                <span className="font-mono tabular-nums text-muted-foreground">{dash(clock)}</span>
                <MonoSafeText value={dash(entry.model)} className="min-w-0 truncate" />
                <span className="font-mono text-muted-foreground">{keyLabel}</span>
                <MonoSafeText value={dash(entry.prompt)} className="min-w-0 truncate" />
                <MonoSafeText value={dash(entry.answer)} className="min-w-0 truncate" />
                <span className="font-mono tabular-nums text-muted-foreground">{dash(duration)}</span>
                <ResultBadge result={entry.result} />
            </div>
        </li>
    );
}

export function ProbeHistoryList({ channelId }: { channelId: number }) {
    const t = useTranslations('channel.circuits');
    const { data } = useChannelProbes(channelId);
    // 默认折叠：与熔断条目多少无关，用户点开才看历史。
    const [open, setOpen] = useState(false);
    const history = (data?.history ?? []).slice(0, PROBE_HISTORY_LIMIT);
    const summary = probeHistorySummary(history, t);

    return (
        <section className="space-y-3">
            <button
                type="button"
                onClick={() => setOpen((current) => !current)}
                aria-expanded={open}
                aria-label={t('probe.history.title')}
                className="flex w-full min-w-0 items-center gap-2 rounded-xl text-left transition-colors hover:bg-muted/40 max-sm:min-h-11"
            >
                <Radar className="size-3.5 shrink-0 text-muted-foreground" />
                <span className="min-w-0 shrink-0 text-xs font-semibold uppercase tracking-wider text-muted-foreground">
                    {t('probe.history.title')}
                </span>
                <span
                    className="min-w-0 flex-1 truncate text-xs text-muted-foreground sm:text-right"
                    data-probe-history-summary
                >
                    {summary}
                </span>
                <span className="shrink-0">
                    {open
                        ? <ChevronUp className="size-4 text-muted-foreground" />
                        : <ChevronDown className="size-4 text-muted-foreground" />}
                </span>
            </button>

            {open && (
                <>
                    <div className="hidden min-w-0 grid-cols-[5.5rem_minmax(0,1.4fr)_3rem_minmax(0,1fr)_minmax(0,1fr)_4rem_auto] items-center gap-2 px-3 pb-1 text-xs font-medium text-muted-foreground sm:grid">
                        <span>{t('probe.history.time')}</span>
                        <span>{t('probe.history.model')}</span>
                        <span>{t('probe.history.key')}</span>
                        <span>{t('probe.history.prompt')}</span>
                        <span>{t('probe.history.answer')}</span>
                        <span>{t('probe.history.duration')}</span>
                        <span>{t('probe.history.result')}</span>
                    </div>
                    <ul className="overflow-hidden rounded-xl border border-border">
                        {history.length === 0 ? (
                            <li className="px-3 py-3 text-xs text-muted-foreground">{t('probe.history.empty')}</li>
                        ) : (
                            history.map((entry, index) => (
                                <HistoryRow key={`${entry.timestamp}-${entry.key_id}-${entry.model}-${index}`} entry={entry} />
                            ))
                        )}
                    </ul>
                    <p className="text-xs text-muted-foreground">{t('probe.history.note')}</p>
                </>
            )}
        </section>
    );
}
