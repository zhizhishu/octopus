'use client';

import { useMemo, useState } from 'react';
import { useChannelList } from '@/api/endpoints/channel';
import { Card } from './Card';
import { useSearchStore, useToolbarViewOptionsStore } from '@/components/modules/toolbar';
import { VirtualizedGrid } from '@/components/common/VirtualizedGrid';
import { cn } from '@/lib/utils';
import {
    CHANNEL_ENDPOINT_FAMILIES,
    type ChannelEndpointFilter,
    getChannelEndpointFamily,
    getPrimaryBaseUrl,
    getPrimaryChannelModel,
    getSelectedChannelModels,
} from './channel-utils';

export function Channel() {
    const { data: channelsData } = useChannelList();
    const pageKey = 'channel' as const;
    const searchTerm = useSearchStore((s) => s.getSearchTerm(pageKey));
    const layout = useToolbarViewOptionsStore((s) => s.getLayout(pageKey));
    const sortField = useToolbarViewOptionsStore((s) => s.getSortField(pageKey));
    const sortOrder = useToolbarViewOptionsStore((s) => s.getSortOrder(pageKey));
    const filter = useToolbarViewOptionsStore((s) => s.channelFilter);
    const [endpointFilter, setEndpointFilter] = useState<ChannelEndpointFilter>('all');

    const sortedChannels = useMemo(() => {
        if (!channelsData) return [];
        return [...channelsData].sort((a, b) => {
            const enabledDiff = Number(b.raw.enabled) - Number(a.raw.enabled);
            if (enabledDiff !== 0) return enabledDiff;

            const diff = sortField === 'name'
                ? a.raw.name.localeCompare(b.raw.name)
                : a.raw.id - b.raw.id;
            return sortOrder === 'asc' ? diff : -diff;
        });
    }, [channelsData, sortField, sortOrder]);

    const visibleChannels = useMemo(() => {
        const term = searchTerm.toLowerCase().trim();
        const byName = !term ? sortedChannels : sortedChannels.filter((c) => {
            const family = getChannelEndpointFamily(c.raw);
            return [
                c.raw.name,
                getSelectedChannelModels(c.raw).join(','),
                getPrimaryBaseUrl(c.raw),
                getPrimaryChannelModel(c.raw),
                family.label,
                family.shortLabel,
            ].some((value) => value.toLowerCase().includes(term));
        });

        if (filter === 'enabled') return byName.filter((c) => c.raw.enabled);
        if (filter === 'disabled') return byName.filter((c) => !c.raw.enabled);

        return byName;
    }, [sortedChannels, searchTerm, filter]);

    const endpointOptions = useMemo(() => {
        const counts = new Map<ChannelEndpointFilter, number>([['all', visibleChannels.length]]);
        visibleChannels.forEach((item) => {
            const family = getChannelEndpointFamily(item.raw);
            counts.set(family.id, (counts.get(family.id) ?? 0) + 1);
        });
        return counts;
    }, [visibleChannels]);

    const endpointFilteredChannels = useMemo(() => {
        if (endpointFilter === 'all') return visibleChannels;
        return visibleChannels.filter((item) => getChannelEndpointFamily(item.raw).id === endpointFilter);
    }, [visibleChannels, endpointFilter]);

    return (
        <div className="flex h-full min-h-0 flex-col gap-3">
            <div className="shrink-0 overflow-x-auto pb-1">
                <div className="flex w-max min-w-full items-center gap-1 rounded-lg border border-border bg-card px-1.5 py-1 shadow-xs">
                    <span className="shrink-0 px-2 text-[11px] font-medium text-muted-foreground">端点筛选</span>
                    <EndpointFilterButton
                        label="全部"
                        count={endpointOptions.get('all') ?? 0}
                        active={endpointFilter === 'all'}
                        onClick={() => setEndpointFilter('all')}
                    />
                    {CHANNEL_ENDPOINT_FAMILIES.map((family) => (
                        <EndpointFilterButton
                            key={family.id}
                            label={family.shortLabel}
                            count={endpointOptions.get(family.id) ?? 0}
                            active={endpointFilter === family.id}
                            onClick={() => setEndpointFilter(family.id)}
                            disabled={(endpointOptions.get(family.id) ?? 0) === 0}
                        />
                    ))}
                </div>
            </div>

            <div className="min-h-0 flex-1">
                {endpointFilteredChannels.length === 0 ? (
                    <div className="rounded-3xl border border-dashed border-border bg-card p-10 text-center">
                        <p className="text-sm font-semibold text-foreground">没有命中的渠道</p>
                        <p className="mt-1 text-sm text-muted-foreground">换个搜索词、状态筛选或端点筛选再看，别和空列表较劲。</p>
                    </div>
                ) : (
                    <VirtualizedGrid
                        items={endpointFilteredChannels}
                        layout={layout}
                        columns={{ default: 1, md: 2, xl: 3 }}
                        estimateItemHeight={layout === 'list' ? 96 : 248}
                        getItemKey={(item) => `channel-${item.raw.id}`}
                        renderItem={(item) => <Card channel={item.raw} stats={item.formatted} layout={layout} />}
                    />
                )}
            </div>
        </div>
    );
}

function EndpointFilterButton({
    label,
    count,
    active,
    disabled = false,
    onClick,
}: {
    label: string;
    count: number;
    active: boolean;
    disabled?: boolean;
    onClick: () => void;
}) {
    return (
        <button
            type="button"
            onClick={onClick}
            disabled={disabled}
            className={cn(
                'group inline-flex h-8 max-w-[9.5rem] shrink-0 items-center gap-1.5 overflow-hidden whitespace-nowrap rounded-md border border-transparent px-2.5 text-xs font-medium leading-none transition-colors disabled:cursor-not-allowed disabled:opacity-40 sm:max-w-none',
                active
                    ? 'border-border bg-background text-foreground shadow-xs'
                    : 'text-muted-foreground hover:bg-muted/70 hover:text-foreground'
            )}
        >
            <span className="min-w-0 truncate">{label}</span>
            <span className={cn(
                'shrink-0 rounded-full px-1.5 py-0.5 text-[10px] tabular-nums',
                active ? 'bg-muted text-foreground' : 'bg-muted/70 text-muted-foreground group-hover:text-foreground'
            )}>
                {count}
            </span>
        </button>
    );
}
