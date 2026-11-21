'use client';

/**
 * 渠道详情「熔断明细」：按 key × 模型列出处于熔断/半开的条目，可单独解除某个模型或某把 key，
 * 也可以对这个 key×模型立刻发一次主动探测（P3）。
 *
 * 两条交互约定：
 *   - 有熔断条目时默认展开、没有条目整块不渲染；用户点过折叠就按用户的，不被轮询数据抢回。
 *   - 探测状态只显示后端给的安排（probe.next_in_seconds / remaining_seconds），前端不倒计时猜。
 */

import { useState } from 'react';
import { ChevronDown, ChevronUp, Radar, RotateCcw, ShieldAlert } from 'lucide-react';
import { useTranslations } from 'next-intl';
import {
    useChannelCircuits,
    useResetChannelCircuit,
    useRunChannelProbe,
    type ChannelCircuitEntry,
    type ChannelProbeError,
} from '@/api/endpoints/channel';
import { toast } from '@/components/common/Toast';
import { MonoSafeText } from '@/components/common/SafeText';
import { cn } from '@/lib/utils';
import { probeStateLabel, probeStateView } from './probe-format';

const PROBE_TONE_CLASS: Record<string, string> = {
    scheduled: 'bg-sky-500/10 text-sky-700 dark:text-sky-300',
    probing: 'bg-sky-500/10 text-sky-700 dark:text-sky-300',
    waitUpstream: 'bg-amber-500/10 text-amber-700 dark:text-amber-300',
    skippedTraffic: 'bg-muted text-muted-foreground',
    notProbed: 'bg-muted text-muted-foreground',
    off: 'bg-muted text-muted-foreground',
    stopped: 'bg-muted text-muted-foreground',
    unknown: 'bg-muted text-muted-foreground',
};

const ACTION_BUTTON_CLASS =
    'inline-flex items-center justify-center gap-1 rounded-md border border-border px-2 py-1 text-xs transition-colors hover:bg-muted disabled:opacity-50 max-sm:min-h-11';

export function CircuitList({ channelId, keyLabel }: { channelId: number; keyLabel: (keyId: number) => string }) {
    const t = useTranslations('channel.circuits');
    const { data } = useChannelCircuits(channelId);
    const reset = useResetChannelCircuit();
    const activeProbe = useRunChannelProbe();
    const entries: ChannelCircuitEntry[] = data ?? [];

    const [expanded, setExpanded] = useState(false);

    if (entries.length === 0) {
        return null;
    }

    const run = (payload: { id: number; key_id?: number; model?: string }) => {
        reset.mutate(payload, {
            onSuccess: () => toast.success(t('resetDone')),
            onError: (error) => toast.error(error.message),
        });
    };

    // 手动探测：花一次额度、计入今日上限，不受随机延迟限制（后端说了算，这里只发请求）。
    const runProbe = (entry: ChannelCircuitEntry) => {
        activeProbe.mutate(
            {
                id: channelId,
                key_id: entry.key_id,
                model: entry.model,
                endpoint: entry.endpoint ?? '',
                capability: entry.capability ?? '',
            },
            {
                onSuccess: (result) => {
                    const view = probeStateView(result);
                    toast.success(view ? probeStateLabel(view, t) : t('probe.accepted'));
                },
                onError: (error: ChannelProbeError) => {
                    const view = error?.probe ? probeStateView(error.probe) : null;
                    // 拒绝时后端给了状态和剩下的秒数：说清楚“还要等多久”，不是只报一句失败。
                    toast.error(view ? probeStateLabel(view, t) : (error?.message || t('probe.failed')));
                },
            }
        );
    };

    return (
        <section className="space-y-3">
            <button
                type="button"
                onClick={() => setExpanded((open) => !open)}
                aria-expanded={expanded}
                aria-label={t('title', { count: entries.length })}
                className="flex w-full min-w-0 items-center gap-2 rounded-xl text-left transition-colors hover:bg-muted/40 max-sm:min-h-11"
            >
                <ShieldAlert className="size-3.5 shrink-0 text-destructive" />
                <span className="min-w-0 text-xs font-semibold uppercase tracking-wider text-muted-foreground">
                    {t('title', { count: entries.length })}
                </span>
                <span className="ml-auto shrink-0">
                    {expanded
                        ? <ChevronUp className="size-4 text-muted-foreground" />
                        : <ChevronDown className="size-4 text-muted-foreground" />}
                </span>
            </button>

            {expanded && (
                <>
                    <ul className="divide-y divide-border rounded-xl border border-border">
                        {entries.map((entry) => {
                            const probeView = probeStateView(entry.probe);
                            // 通配模型（*）没有具体模型名，手探与「解除该模型」都无处落脚：禁用并说清楚原因。
                            const wildcard = entry.model === '*';
                            return (
                                <li
                                    key={`${entry.key_id}:${entry.model}:${entry.endpoint ?? ''}:${entry.capability ?? ''}`}
                                    className="min-w-0 px-3 py-2.5 text-xs sm:flex sm:flex-wrap sm:items-center sm:gap-2"
                                >
                                    <div className="flex min-w-0 items-center justify-between gap-2 sm:min-w-0 sm:flex-1 sm:justify-start">
                                        <MonoSafeText
                                            value={wildcard ? t('anyModel') : entry.model}
                                            className="min-w-0 truncate font-medium text-card-foreground"
                                        />
                                        <span className="shrink-0 rounded-md bg-destructive/10 px-1.5 py-0.5 text-destructive">
                                            {entry.state === 'half_open'
                                                ? t('halfOpen')
                                                : entry.state === 'held'
                                                    ? t('held', { seconds: entry.remaining_seconds })
                                                    : t('remaining', { seconds: entry.remaining_seconds })}
                                        </span>
                                    </div>
                                    <span className="mt-1 block min-w-0 truncate text-muted-foreground sm:mt-0 sm:shrink-0">
                                        {entry.key_id === 0 ? t('anyKey') : keyLabel(entry.key_id)}
                                        {entry.reason ? ` · ${entry.reason}` : ''}
                                        {entry.endpoint ? ` · ${entry.endpoint}` : ''}
                                    </span>
                                    {probeView ? (
                                        <span
                                            className={cn(
                                                'mt-1 inline-flex w-fit shrink-0 items-center gap-1 rounded-md px-1.5 py-0.5 sm:mt-0',
                                                PROBE_TONE_CLASS[probeView.kind] ?? PROBE_TONE_CLASS.unknown
                                            )}
                                            data-probe-state={probeView.kind}
                                        >
                                            <Radar className="size-3" />
                                            {probeStateLabel(probeView, t)}
                                        </span>
                                    ) : null}
                                    {/* 移动端整行三列等宽；桌面靠右保持原来的紧凑排布。 */}
                                    <div className="mt-2 grid w-full grid-cols-3 gap-1.5 sm:mt-0 sm:ml-auto sm:flex sm:w-auto sm:items-center sm:gap-2">
                                        <button
                                            type="button"
                                            className={ACTION_BUTTON_CLASS}
                                            disabled={activeProbe.isPending || wildcard}
                                            title={wildcard ? t('probe.noModel') : undefined}
                                            onClick={() => runProbe(entry)}
                                        >
                                            <Radar className="size-3" />
                                            {t('probe.run')}
                                        </button>
                                        <button
                                            type="button"
                                            className={ACTION_BUTTON_CLASS}
                                            disabled={reset.isPending}
                                            onClick={() => run({ id: channelId, key_id: entry.key_id, model: entry.model })}
                                        >
                                            <RotateCcw className="size-3" />
                                            {t('resetOne')}
                                        </button>
                                        <button
                                            type="button"
                                            className={ACTION_BUTTON_CLASS}
                                            disabled={reset.isPending || wildcard}
                                            title={wildcard ? t('resetModelSameScope') : undefined}
                                            onClick={() => run({ id: channelId, model: entry.model })}
                                        >
                                            {t('resetModel')}
                                        </button>
                                    </div>
                                </li>
                            );
                        })}
                    </ul>
                    <p className="text-xs text-muted-foreground">{t('probe.runHint')}</p>
                </>
            )}
        </section>
    );
}
