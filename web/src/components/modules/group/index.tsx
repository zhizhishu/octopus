'use client';

import { useMemo } from 'react';
import { AlertTriangle } from 'lucide-react';
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

    // 跨卡片体检：统计同名分组，供顶部横幅告警 + 每张卡片打「同名 ×N」红标。
    const nameCounts = useMemo(() => {
        const counts = new Map<string, number>();
        (groups ?? []).forEach((g) => counts.set(g.name, (counts.get(g.name) ?? 0) + 1));
        return counts;
    }, [groups]);
    const duplicateNames = useMemo(
        () => [...nameCounts.entries()].filter(([, n]) => n > 1).sort((a, b) => a[0].localeCompare(b[0])),
        [nameCounts]
    );

    return (
        <div className="flex h-full min-h-0 w-full flex-col gap-3">
            <PoolGlobalDefaults />
            {duplicateNames.length > 0 && (
                <div className="flex items-start gap-2 rounded-xl border border-destructive/30 bg-destructive/5 px-3 py-2 text-destructive">
                    <AlertTriangle className="mt-0.5 size-4 shrink-0" />
                    <div className="min-w-0">
                        <div className="text-sm font-semibold">发现 {duplicateNames.length} 组同名分组，可能导致路由走法不确定，建议合并或改名</div>
                        <div className="mt-1 flex flex-wrap gap-x-3 gap-y-0.5 text-xs text-destructive/90">
                            {duplicateNames.map(([name, n]) => (
                                <span key={name} className="font-mono">{name} ×{n}</span>
                            ))}
                        </div>
                    </div>
                </div>
            )}
            <div className="min-h-0 flex-1">
                <VirtualizedGrid
                    items={visibleGroups}
                    columns={{ default: 1, md: 2, lg: 3 }}
                    estimateItemHeight={520}
                    getItemKey={(group, index) => group.id ?? `group-${index}`}
                    renderItem={(group) => <GroupCard group={group} duplicateCount={nameCounts.get(group.name) ?? 1} />}
                />
            </div>
        </div>
    );
}
