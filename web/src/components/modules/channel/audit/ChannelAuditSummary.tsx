'use client';

/**
 * Design 2C：渠道卡片底部「检测 ●●● 3 个正常 · 1 个待复核」。
 * >8 模型用分段条；悬停只列待复核/失败 + 其余摘要；窄屏缩短文案。
 */

import { useMemo } from 'react';
import { ShieldCheck } from 'lucide-react';
import { useChannelLatestAudits } from '@/api/endpoints/model-audit';
import { cn } from '@/lib/utils';
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/animate-ui/components/animate/tooltip';
import { ChipDot } from './AuditTip';
import { countChips, mergeModelsWithLatest } from './types';
import { formatChipTime } from '@/lib/model-audit-verdict';

const DOT_LIMIT = 8;

export function ChannelAuditSummary({
    channelId,
    models,
    className,
}: {
    channelId: number;
    models: string[];
    className?: string;
}) {
    const { data: latest } = useChannelLatestAudits(channelId, models.length > 0);
    const items = useMemo(() => mergeModelsWithLatest(models, latest), [models, latest]);
    const counts = useMemo(() => countChips(items), [items]);

    if (models.length === 0) return null;

    const useBar = models.length > DOT_LIMIT;
    const parts: Array<{ key: string; text: string; className?: string; mobileHide?: boolean }> = [];
    const hasProblem = counts.warn > 0 || counts.fail > 0;
    // 390px 卡片：有问题时只留待复核/失败，丢掉「正常」「没检测过」以免截断。
    if (counts.ok) parts.push({ key: 'ok', text: `${counts.ok} 个正常`, mobileHide: hasProblem });
    if (counts.warn) parts.push({ key: 'warn', text: `${counts.warn} 个待复核`, className: 'text-amber-700 dark:text-amber-400' });
    if (counts.fail) parts.push({ key: 'fail', text: `${counts.fail} 个失败`, className: 'text-rose-700 dark:text-rose-400' });
    if (counts.none) {
        parts.push({ key: 'none', text: `${counts.none} 个没检测过`, mobileHide: true });
    }

    const latestTime = items.map((i) => i.time).filter(Boolean).sort().at(-1);
    const bad = items.filter((i) => i.chip === 'warn' || i.chip === 'fail');

    return (
        <Tooltip side="top" sideOffset={10}>
            <TooltipTrigger asChild>
                <div
                    className={cn(
                        'flex min-w-0 cursor-default items-center gap-1.5 text-[11px] leading-4 text-muted-foreground',
                        className,
                    )}
                    onClick={(e) => e.stopPropagation()}
                >
                    <ShieldCheck className="size-3.5 shrink-0 text-primary" />
                    <span className="shrink-0">检测</span>
                    {useBar ? (
                        <span className="flex h-1.5 w-11 shrink-0 overflow-hidden rounded-full">
                            {(['ok', 'warn', 'fail', 'none'] as const)
                                .filter((k) => counts[k] > 0)
                                .map((k) => (
                                    <span
                                        key={k}
                                        className={cn(
                                            'h-full',
                                            k === 'ok' && 'bg-emerald-500',
                                            k === 'warn' && 'bg-amber-500',
                                            k === 'fail' && 'bg-rose-500',
                                            k === 'none' && 'bg-muted-foreground/30',
                                        )}
                                        style={{ width: `${(counts[k] / models.length) * 100}%` }}
                                    />
                                ))}
                        </span>
                    ) : (
                        <span className="flex shrink-0 items-center gap-0.5">
                            {items.slice(0, DOT_LIMIT).map((item) => (
                                <ChipDot key={item.model} kind={item.chip} />
                            ))}
                        </span>
                    )}
                    <span className="min-w-0 truncate">
                        {parts.map((p, i) => (
                            <span key={p.key} className={cn(p.className, p.mobileHide && 'hidden sm:inline')}>
                                {i > 0 && <span className="text-muted-foreground/60"> · </span>}
                                {p.text}
                            </span>
                        ))}
                    </span>
                </div>
            </TooltipTrigger>
            <TooltipContent className="border-0 bg-transparent p-0 shadow-none">
                <div className="w-[260px] rounded-xl border border-border bg-popover px-3 py-2.5 text-xs text-popover-foreground shadow-lg">
                    <div className="font-medium">
                        模型检测
                        {latestTime && (
                            <span className="ml-1 font-normal text-muted-foreground">
                                · 最近 {formatChipTime(latestTime)}
                            </span>
                        )}
                    </div>
                    <div className="mt-1.5 grid gap-1">
                        {(useBar ? bad : items).slice(0, 10).map((item) => (
                            <div key={item.model} className="flex items-center justify-between gap-2">
                                <span className="flex min-w-0 items-center gap-1.5">
                                    <ChipDot kind={item.chip} />
                                    <span className="truncate font-mono">{item.model}</span>
                                </span>
                                <span className="shrink-0 text-muted-foreground">{item.chipLabel}</span>
                            </div>
                        ))}
                        {useBar && items.length > bad.length && (
                            <div className="text-muted-foreground">
                                另有 {items.length - bad.length} 个正常或未检测
                            </div>
                        )}
                    </div>
                    <div className="mt-2 text-[11px] text-primary">点开渠道看每个模型的记录 →</div>
                </div>
            </TooltipContent>
        </Tooltip>
    );
}
