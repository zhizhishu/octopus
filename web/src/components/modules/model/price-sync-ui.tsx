'use client';

import type { ReactElement, ReactNode } from 'react';
import { useTranslations } from 'next-intl';
import { CircleHelp } from 'lucide-react';
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/animate-ui/components/animate/tooltip';
import { getModelIcon } from '@/lib/model-icons';
import { cn } from '@/lib/utils';
import { formatClock, formatMonthDay, relativeDay } from './price-sync';

export type ChipTone = 'library' | 'manual' | 'none' | 'new' | 'changed' | 'warn';

const CHIP_TONES: Record<ChipTone, string> = {
    library: 'bg-muted text-muted-foreground',
    manual: 'bg-amber-500/12 text-amber-700 ring-1 ring-amber-500/25 dark:text-amber-400',
    none: 'bg-destructive/10 text-destructive',
    new: 'bg-emerald-500/12 text-emerald-700 dark:text-emerald-400',
    changed: 'bg-sky-500/12 text-sky-700 dark:text-sky-400',
    warn: 'bg-amber-500/15 text-amber-700 dark:text-amber-400',
};

export function Chip({ tone, children, className }: { tone: ChipTone; children: ReactNode; className?: string }) {
    return (
        <span
            className={cn(
                'inline-flex shrink-0 items-center gap-1 whitespace-nowrap rounded-full px-2 py-0.5 text-[11px] font-medium leading-4',
                CHIP_TONES[tone],
                className,
            )}
        >
            {children}
        </span>
    );
}

/** Details live in tooltips; the visible text stays short. */
export function Hint({
    content,
    children,
    side = 'top',
    className,
}: {
    content: ReactNode;
    children?: ReactElement;
    side?: 'top' | 'bottom' | 'left' | 'right';
    className?: string;
}) {
    return (
        <Tooltip side={side} sideOffset={8}>
            <TooltipTrigger asChild>
                {children ?? (
                    <span
                        tabIndex={0}
                        className={cn('inline-flex size-4 shrink-0 cursor-help items-center justify-center text-muted-foreground/70', className)}
                    >
                        <CircleHelp className="size-3.5" />
                    </span>
                )}
            </TooltipTrigger>
            <TooltipContent className="max-w-72 bg-foreground text-background">
                <div className="text-left leading-5">{content}</div>
            </TooltipContent>
        </Tooltip>
    );
}

/** Vendor logo (or a neutral letter for unknown vendors) next to a model name. */
export function ModelLogo({ name, size }: { name: string; size: number }) {
    const { Avatar } = getModelIcon(name);
    return <Avatar size={size} className="shrink-0" />;
}

/** "今天 09:12" / "昨天 09:12" / "明天 09:12" / "10/28 09:12". */
export function useDayTime() {
    const t = useTranslations('model.priceSync');
    return (date: Date, now: Date = new Date()) => {
        const rel = relativeDay(date, now);
        const day = rel ? t(rel) : formatMonthDay(date);
        return `${day} ${formatClock(date)}`;
    };
}
