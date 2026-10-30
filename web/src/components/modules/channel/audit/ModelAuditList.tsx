'use client';

/**
 * Design 2B：渠道详情「模型 · 最近一次检测」列表。
 * >8 行折叠；搜索 + 快速筛选；点行跳转检测记录。
 */

import { useMemo, useState } from 'react';
import { Layers, Search } from 'lucide-react';
import { useChannelLatestAudits } from '@/api/endpoints/model-audit';
import { useNavStore } from '@/components/modules/navbar/nav-store';
import { cn } from '@/lib/utils';
import { chipBadgeClass, formatChipTime } from '@/lib/model-audit-verdict';
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/animate-ui/components/animate/tooltip';
import { AuditTip, ChipDot } from './AuditTip';
import { countChips, mergeModelsWithLatest, type ChipFilter, type ModelAuditChip } from './types';

const COLLAPSE_AT = 8;

export function ModelAuditList({
    channelId,
    models,
}: {
    channelId: number;
    models: string[];
}) {
    const { data: latest } = useChannelLatestAudits(channelId, models.length > 0);
    const setActiveItem = useNavStore((s) => s.setActiveItem);
    const [query, setQuery] = useState('');
    const [filter, setFilter] = useState<ChipFilter>('all');
    const [expanded, setExpanded] = useState(false);

    const items = useMemo(() => mergeModelsWithLatest(models, latest), [models, latest]);
    const counts = useMemo(() => countChips(items), [items]);

    const filtered = useMemo(() => {
        const q = query.trim().toLowerCase();
        return items.filter((item) => {
            if (filter !== 'all' && item.chip !== filter) return false;
            if (q && !item.model.toLowerCase().includes(q)) return false;
            return true;
        });
    }, [items, query, filter]);

    const visible = expanded || query || filter !== 'all' ? filtered : filtered.slice(0, COLLAPSE_AT);
    const openRecord = (item: ModelAuditChip) => {
        // 跳到检测页；具体任务由检测页历史自行展示。带 query 不便跨页，先切过去。
        setActiveItem('audit');
        if (typeof window !== 'undefined' && item.jobId) {
            window.sessionStorage.setItem('octopus.audit.openJob', String(item.jobId));
            if (item.resultId) window.sessionStorage.setItem('octopus.audit.openResult', String(item.resultId));
        }
    };

    if (models.length === 0) return null;

    return (
        <section className="space-y-3">
            <h4 className="flex items-center gap-2 text-xs font-semibold uppercase tracking-wider text-muted-foreground">
                <Layers className="size-3.5" />
                模型
                <span className="font-normal normal-case tracking-normal">· 最近一次检测</span>
            </h4>

            {(models.length > 6 || counts.warn + counts.fail + counts.none > 0) && (
                <div className="flex flex-wrap items-center gap-2">
                    <label className="flex h-7 min-w-[140px] max-w-[220px] flex-1 items-center gap-1.5 rounded-md border bg-background px-2.5 text-xs text-muted-foreground">
                        <Search className="size-3.5 shrink-0" />
                        <input
                            value={query}
                            onChange={(e) => setQuery(e.target.value)}
                            placeholder="搜索模型"
                            className="min-w-0 flex-1 bg-transparent outline-none placeholder:text-muted-foreground/70"
                        />
                        {query && (
                            <button type="button" className="text-muted-foreground" onClick={() => setQuery('')}>
                                ×
                            </button>
                        )}
                    </label>
                    <div className="flex flex-wrap items-center gap-0.5">
                        {(
                            [
                                ['all', `全部`, undefined],
                                ['warn', `待复核 ${counts.warn}`, counts.warn],
                                ['fail', `失败 ${counts.fail}`, counts.fail],
                                ['none', `没检测过 ${counts.none}`, counts.none],
                            ] as const
                        ).map(([key, label, n]) => (
                            <button
                                key={key}
                                type="button"
                                onClick={() => setFilter(key)}
                                className={cn(
                                    'inline-flex h-7 items-center gap-1 rounded-md px-2 text-xs',
                                    filter === key ? 'bg-muted font-medium text-foreground' : 'text-muted-foreground hover:bg-muted/60',
                                    key !== 'all' && !n && 'opacity-50',
                                )}
                            >
                                {key !== 'all' && <ChipDot kind={key} />}
                                {label}
                            </button>
                        ))}
                    </div>
                    {(query || filter !== 'all') && (
                        <span className="ml-auto text-[11px] text-muted-foreground">
                            匹配 {filtered.length} / {items.length}
                        </span>
                    )}
                </div>
            )}

            <div className="overflow-visible rounded-2xl border bg-card">
                {visible.map((item) => (
                    <Tooltip key={item.model} side="top" sideOffset={8}>
                        <TooltipTrigger asChild>
                            <button
                                type="button"
                                onClick={() => openRecord(item)}
                                className="flex w-full cursor-pointer items-center gap-3 border-b px-3 py-2.5 text-left transition-colors last:border-0 hover:bg-accent/5 sm:px-4"
                            >
                                <span className="min-w-0 flex-1 truncate font-mono text-sm">{item.model}</span>
                                <div className="flex shrink-0 items-center gap-2">
                                    {item.time && (
                                        <span className="hidden whitespace-nowrap text-xs tabular-nums text-muted-foreground sm:inline-block">
                                            {formatChipTime(item.time)}
                                        </span>
                                    )}
                                    <span className={cn('rounded-full px-2 py-0.5 text-[11px]', chipBadgeClass(item.chip))}>
                                        {item.chipLabel}
                                    </span>
                                </div>
                            </button>
                        </TooltipTrigger>
                        <TooltipContent className="border-0 bg-transparent p-0 shadow-none">
                            <AuditTip item={item} />
                        </TooltipContent>
                    </Tooltip>
                ))}
                {filtered.length === 0 && (
                    <div className="p-4 text-center text-sm text-muted-foreground">没有匹配的模型</div>
                )}
                {!query && filter === 'all' && items.length > COLLAPSE_AT && (
                    <button
                        type="button"
                        onClick={() => setExpanded((v) => !v)}
                        className="flex w-full items-center justify-center border-t py-2 text-xs text-muted-foreground hover:bg-muted/40"
                    >
                        {expanded ? '收起' : `展开全部 ${items.length} 个`}
                    </button>
                )}
            </div>
        </section>
    );
}
