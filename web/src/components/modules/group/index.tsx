'use client';

import { useMemo } from 'react';
import { GroupCard } from './Card';
import { PoolGlobalDefaults } from './PoolDefaults';
import { type Group as GroupType, useGroupList } from '@/api/endpoints/group';
import { useModelChannelList } from '@/api/endpoints/model';
import { useSearchStore, useToolbarViewOptionsStore } from '@/components/modules/toolbar';
import { VirtualizedGrid } from '@/components/common/VirtualizedGrid';
import { activeModelChannelKeySet, modelChannelKey } from './utils';

function visibleItemCount(group: GroupType, visibleModelKeys?: Set<string>) {
    const items = group.items ?? [];
    if (!visibleModelKeys) return items.length;
    return items.filter((item) => visibleModelKeys.has(modelChannelKey(item.channel_id, item.model_name))).length;
}

export function Group() {
    const { data: groups } = useGroupList();
    const { data: modelChannels } = useModelChannelList();
    const pageKey = 'group' as const;
    const searchTerm = useSearchStore((s) => s.getSearchTerm(pageKey));
    const sortField = useToolbarViewOptionsStore((s) => s.getSortField(pageKey));
    const sortOrder = useToolbarViewOptionsStore((s) => s.getSortOrder(pageKey));
    const filter = useToolbarViewOptionsStore((s) => s.groupFilter);
    const visibleModelKeys = useMemo(() => (
        modelChannels ? activeModelChannelKeySet(modelChannels) : undefined
    ), [modelChannels]);

    const sortedGroups = useMemo(() => {
        if (!groups) return [];
        return [...groups].sort((a, b) => {
            const emptyDiff = Number(visibleItemCount(a, visibleModelKeys) === 0) - Number(visibleItemCount(b, visibleModelKeys) === 0);
            if (emptyDiff !== 0) return emptyDiff;

            const diff = sortField === 'name'
                ? a.name.localeCompare(b.name)
                : (a.id || 0) - (b.id || 0);
            return sortOrder === 'asc' ? diff : -diff;
        });
    }, [groups, sortField, sortOrder, visibleModelKeys]);

    const visibleGroups = useMemo(() => {
        const term = searchTerm.toLowerCase().trim();
        const byName = !term ? sortedGroups : sortedGroups.filter((g) => g.name.toLowerCase().includes(term));

        if (filter === 'with-members') return byName.filter((g) => visibleItemCount(g, visibleModelKeys) > 0);
        if (filter === 'empty') return byName.filter((g) => visibleItemCount(g, visibleModelKeys) === 0);

        return byName;
    }, [sortedGroups, searchTerm, filter, visibleModelKeys]);

    return (
        <div className="flex h-full min-h-0 w-full flex-col gap-3">
            <PoolGlobalDefaults />
            <div className="min-h-0 flex-1">
                <VirtualizedGrid
                    items={visibleGroups}
                    columns={{ default: 1, md: 2, lg: 3 }}
                    estimateItemHeight={520}
                    getItemKey={(group, index) => group.id ?? `group-${index}`}
                    renderItem={(group) => <GroupCard group={group} />}
                />
            </div>
        </div>
    );
}
