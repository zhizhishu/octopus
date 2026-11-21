'use client';

/**
 * 渠道详情里的「主动探测」开关（P3）+ 今日用量。
 *
 * 三个刻意的选择：
 *   - 关掉时**显式**发 probe_enabled: false。后端按 patch 语义处理缺省字段，
 *     少发这个键等于"不改"，用户点了关就关不掉。
 *   - 今日用量显示两档：本渠道 X / Y（后端给的 cap）+ 全站 X / Y，
 *     因为"这个渠道探满了"和"全站探满了"是两种完全不同的处置办法。
 *   - 数字取不到就显示 —，不写 0：0 会被读成"今天一次都没探过"。
 */

import { Radar } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { Switch } from '@/components/ui/switch';
import { useChannelProbes, useUpdateChannel, type Channel } from '@/api/endpoints/channel';
import { toast } from '@/components/common/Toast';

export function ChannelProbeToggle({ channel }: { channel: Channel }) {
    const t = useTranslations('channel.detail');
    const update = useUpdateChannel();
    const { data } = useChannelProbes(channel.id);
    const enabled = channel.probe_enabled ?? false;

    const used = data ? String(data.totals.used) : '—';
    const cap = data && data.totals.cap > 0 ? String(data.totals.cap) : '';
    const globalUsed = data ? String(data.global_totals.used) : '—';
    const globalCap = data && data.global_totals.cap > 0 ? String(data.global_totals.cap) : '';

    const toggle = (next: boolean) => {
        // 显式 false 必须发出去；这里的对象字面量里 false 会正常序列化。
        update.mutate({ id: channel.id, probe_enabled: next }, {
            onError: (error) => toast.error(error?.message || t('probe.saveFailed')),
        });
    };

    return (
        <section
            className="flex min-w-0 flex-wrap items-center justify-between gap-2 rounded-xl border border-border px-3 py-2"
            data-channel-probe
        >
            <div className="flex min-w-0 items-center gap-2">
                <Radar className="size-4 shrink-0 text-muted-foreground" />
                <span className="min-w-0 truncate text-sm font-medium">{t('probe.label')}</span>
                <span className="shrink-0 rounded-md bg-muted px-1.5 py-0.5 font-mono text-xs tabular-nums text-muted-foreground">
                    {`${t('probe.today')} ${cap ? `${used} / ${cap}` : used}`}
                </span>
                <span className="shrink-0 rounded-md bg-muted px-1.5 py-0.5 font-mono text-xs tabular-nums text-muted-foreground">
                    {`${t('probe.global')} ${globalCap ? `${globalUsed} / ${globalCap}` : globalUsed}`}
                </span>
            </div>
            <Switch
                checked={enabled}
                disabled={update.isPending}
                onCheckedChange={toggle}
                aria-label={t('probe.label')}
                className="shrink-0"
            />
        </section>
    );
}
