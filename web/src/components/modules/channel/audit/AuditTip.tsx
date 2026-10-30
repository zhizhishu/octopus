'use client';

import { cn } from '@/lib/utils';
import { chipDotClass, formatChipTime, type ChipKind } from '@/lib/model-audit-verdict';
import type { ModelAuditChip } from './types';

export function AuditTip({
    item,
    footer,
    className,
}: {
    item: ModelAuditChip;
    footer?: string;
    className?: string;
}) {
    const points = item.points.length > 0 ? item.points.slice(0, 3) : [item.summary || item.headline || item.chipLabel];
    return (
        <div
            className={cn(
                'z-50 w-64 rounded-xl border border-border bg-popover px-3 py-2.5 text-xs text-popover-foreground shadow-lg',
                className,
            )}
        >
            <div className="flex items-center gap-1.5 font-medium">
                <span className={cn('inline-block size-2 shrink-0 rounded-full', chipDotClass(item.chip))} />
                <span>{item.chipLabel}</span>
                {item.time && (
                    <span className="font-normal text-muted-foreground">· {formatChipTime(item.time)}</span>
                )}
            </div>
            <ul className="mt-1.5 space-y-0.5 text-muted-foreground">
                {points.map((p) => (
                    <li key={p} className="leading-5">
                        · {p}
                    </li>
                ))}
            </ul>
            <div className="mt-2 text-[11px] text-primary">
                {footer ?? (item.chip === 'none' ? '点击去检测 →' : '点击查看检测记录 →')}
            </div>
        </div>
    );
}

export function ChipDot({ kind, className }: { kind: ChipKind; className?: string }) {
    return <span className={cn('inline-block size-2 shrink-0 rounded-full', chipDotClass(kind), className)} />;
}
