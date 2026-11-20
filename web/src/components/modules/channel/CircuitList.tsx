'use client';

/**
 * 渠道详情「熔断明细」：按 key × 模型列出处于熔断/半开的条目，可单独解除某个模型或某把 key。
 */

import { RotateCcw, ShieldAlert } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { useChannelCircuits, useResetChannelCircuit, type ChannelCircuitEntry } from '@/api/endpoints/channel';
import { toast } from '@/components/common/Toast';
import { MonoSafeText } from '@/components/common/SafeText';

export function CircuitList({ channelId, keyLabel }: { channelId: number; keyLabel: (keyId: number) => string }) {
    const t = useTranslations('channel.circuits');
    const { data } = useChannelCircuits(channelId);
    const reset = useResetChannelCircuit();
    const entries: ChannelCircuitEntry[] = data ?? [];

    if (entries.length === 0) {
        return null;
    }

    const run = (payload: { id: number; key_id?: number; model?: string }) => {
        reset.mutate(payload, {
            onSuccess: () => toast.success(t('resetDone')),
            onError: (error) => toast.error(error.message),
        });
    };

    return (
        <section className="space-y-3">
            <h4 className="flex items-center gap-2 text-xs font-semibold text-muted-foreground uppercase tracking-wider">
                <ShieldAlert className="size-3.5 text-destructive" />
                {t('title', { count: entries.length })}
            </h4>
            <ul className="divide-y divide-border rounded-xl border border-border">
                {entries.map((entry) => (
                    <li
                        key={`${entry.key_id}:${entry.model}:${entry.endpoint ?? ''}:${entry.capability ?? ''}`}
                        className="flex flex-wrap items-center gap-2 px-3 py-2 text-xs"
                    >
                        <MonoSafeText
                            value={entry.model === '*' ? t('anyModel') : entry.model}
                            className="min-w-0 flex-1 truncate font-medium text-card-foreground"
                        />
                        <span className="text-muted-foreground">{entry.key_id === 0 ? t('anyKey') : keyLabel(entry.key_id)}</span>
                        {entry.reason ? <span className="text-muted-foreground">{entry.reason}</span> : null}
                        {entry.endpoint ? <span className="text-muted-foreground">{entry.endpoint}</span> : null}
                        <span className="rounded-md bg-destructive/10 px-1.5 py-0.5 text-destructive">
                            {entry.state === 'half_open'
                                ? t('halfOpen')
                                : entry.state === 'held'
                                    ? t('held', { seconds: entry.remaining_seconds })
                                    : t('remaining', { seconds: entry.remaining_seconds })}
                        </span>
                        <button
                            type="button"
                            className="inline-flex items-center gap-1 rounded-md border border-border px-2 py-1 hover:bg-muted disabled:opacity-50"
                            disabled={reset.isPending}
                            onClick={() => run({ id: channelId, key_id: entry.key_id, model: entry.model })}
                        >
                            <RotateCcw className="size-3" />
                            {t('resetOne')}
                        </button>
                        {entry.model !== '*' ? (
                            <button
                                type="button"
                                className="inline-flex items-center gap-1 rounded-md border border-border px-2 py-1 hover:bg-muted disabled:opacity-50"
                                disabled={reset.isPending}
                                onClick={() => run({ id: channelId, model: entry.model })}
                            >
                                {t('resetModel')}
                            </button>
                        ) : null}
                    </li>
                ))}
            </ul>
        </section>
    );
}
