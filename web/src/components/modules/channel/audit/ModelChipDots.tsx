'use client';

/**
 * Design 2A：编辑对话框「已启用模型」芯片前的彩色圆点 + 搜索/筛选/折叠。
 */

import { useMemo, useState, type ReactNode } from 'react';
import { Search } from 'lucide-react';
import { useChannelLatestAudits } from '@/api/endpoints/model-audit';
import { useNavStore } from '@/components/modules/navbar/nav-store';
import { cn } from '@/lib/utils';
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/animate-ui/components/animate/tooltip';
import { AuditTip, ChipDot } from './AuditTip';
import { countChips, mergeModelsWithLatest, type ChipFilter, type ModelAuditChip } from './types';

/** 兜底：大约两行芯片的数量估计；真实折叠按两行高度裁切。 */
const COLLAPSE_COUNT = 12;

export function useModelAuditChips(channelId: number | undefined, models: string[]) {
    const { data: latest } = useChannelLatestAudits(channelId ?? 0, !!channelId && models.length > 0);
    return useMemo(() => mergeModelsWithLatest(models, latest), [models, latest]);
}

export function ModelAuditChipToolbar({
    items,
    query,
    setQuery,
    filter,
    setFilter,
}: {
    items: ModelAuditChip[];
    query: string;
    setQuery: (v: string) => void;
    filter: ChipFilter;
    setFilter: (v: ChipFilter) => void;
}) {
    const counts = countChips(items);
    if (items.length <= 6) return null;
    const matched = items.filter((item) => {
        if (filter !== 'all' && item.chip !== filter) return false;
        const q = query.trim().toLowerCase();
        if (q && !item.model.toLowerCase().includes(q)) return false;
        return true;
    }).length;
    return (
        <div className="mb-2 flex flex-wrap items-center gap-2">
            <label className="flex h-7 min-w-[140px] max-w-[220px] flex-1 items-center gap-1.5 rounded-md border bg-background px-2.5 text-xs text-muted-foreground">
                <Search className="size-3.5 shrink-0" />
                <input
                    value={query}
                    onChange={(e) => setQuery(e.target.value)}
                    placeholder="搜索模型"
                    className="min-w-0 flex-1 bg-transparent outline-none"
                />
            </label>
            <div className="flex flex-wrap items-center gap-0.5">
                {(
                    [
                        ['all', '全部'],
                        ['warn', `待复核 ${counts.warn}`],
                        ['fail', `失败 ${counts.fail}`],
                        ['none', `没检测过 ${counts.none}`],
                    ] as const
                ).map(([key, label]) => (
                    <button
                        key={key}
                        type="button"
                        onClick={() => setFilter(key)}
                        className={cn(
                            'inline-flex h-7 items-center gap-1 rounded-md px-2 text-xs',
                            filter === key ? 'bg-muted font-medium' : 'text-muted-foreground hover:bg-muted/60',
                        )}
                    >
                        {key !== 'all' && <ChipDot kind={key} />}
                        {label}
                    </button>
                ))}
            </div>
            {(query || filter !== 'all') && (
                <span className="ml-auto text-[11px] text-muted-foreground">
                    匹配 {matched} / {items.length}
                </span>
            )}
        </div>
    );
}

export function ModelAuditChipDot({
    channelId,
    model,
    children,
}: {
    channelId?: number;
    model: string;
    children: ReactNode;
}) {
    const items = useModelAuditChips(channelId, [model]);
    const item = items[0];
    const setActiveItem = useNavStore((s) => s.setActiveItem);
    if (!item) return <>{children}</>;
    return (
        <Tooltip side="top" sideOffset={8}>
            <TooltipTrigger asChild>
                <span
                    className="inline-flex max-w-full cursor-pointer items-center gap-1"
                    onClick={(e) => {
                        e.stopPropagation();
                        setActiveItem('audit');
                        if (item.jobId) {
                            window.sessionStorage.setItem('octopus.audit.openJob', String(item.jobId));
                            if (item.resultId) window.sessionStorage.setItem('octopus.audit.openResult', String(item.resultId));
                        }
                    }}
                >
                    <ChipDot kind={item.chip} />
                    {children}
                </span>
            </TooltipTrigger>
            <TooltipContent className="border-0 bg-transparent p-0 shadow-none">
                <AuditTip item={item} />
            </TooltipContent>
        </Tooltip>
    );
}

export function filterAuditChips(
    items: ModelAuditChip[],
    query: string,
    filter: ChipFilter,
): ModelAuditChip[] {
    const q = query.trim().toLowerCase();
    return items.filter((item) => {
        if (filter !== 'all' && item.chip !== filter) return false;
        if (q && !item.model.toLowerCase().includes(q)) return false;
        return true;
    });
}

export function ModelAuditChipExpand({
    total,
    needsExpand,
    expanded,
    onToggle,
}: {
    total: number;
    needsExpand: boolean;
    expanded: boolean;
    onToggle: () => void;
}) {
    if (!needsExpand && !expanded) return null;
    return (
        <button
            type="button"
            onClick={onToggle}
            className="mt-1.5 w-full text-center text-[11px] text-muted-foreground hover:text-foreground"
        >
            {expanded ? '收起' : `展开全部 ${total} 个`}
        </button>
    );
}

/** 大约两行芯片的兜底高度；真实高度由容器测量。 */
export { COLLAPSE_COUNT };
