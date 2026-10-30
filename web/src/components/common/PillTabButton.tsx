'use client';

import type { ReactNode } from 'react';
import { cn } from '@/lib/utils';

/**
 * 胶囊式分段按钮：带计数徽标、active 高亮、disabled 变灰。
 * 从渠道页的「端点筛选」按钮提取出来，渠道页与设置页的顶部分类标签共用，
 * 两处外观保持一致。不传 icon / role 时渲染结果与原渠道页按钮完全相同。
 */
export function PillTabButton({
    label,
    count,
    active,
    disabled = false,
    onClick,
    icon,
    role,
    id,
    controls,
}: {
    label: string;
    count: number;
    active: boolean;
    disabled?: boolean;
    onClick: () => void;
    icon?: ReactNode;
    /** 作为 tablist 里的标签使用时传 'tab'，会同时带上 aria-selected / aria-controls */
    role?: 'tab';
    id?: string;
    controls?: string;
}) {
    return (
        <button
            type="button"
            onClick={onClick}
            disabled={disabled}
            role={role}
            id={id}
            aria-selected={role === 'tab' ? active : undefined}
            aria-controls={role === 'tab' ? controls : undefined}
            className={cn(
                'group inline-flex h-8 max-w-[9.5rem] shrink-0 items-center gap-1.5 overflow-hidden whitespace-nowrap rounded-md border border-transparent px-2.5 text-xs font-medium leading-none transition-colors disabled:cursor-not-allowed disabled:opacity-40 sm:max-w-none',
                active
                    ? 'border-border bg-background text-foreground shadow-xs'
                    : 'text-muted-foreground hover:bg-muted/70 hover:text-foreground'
            )}
        >
            {icon}
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
