'use client';

import type { ReactNode } from 'react';
import { Check, CircleHelp, Minus } from 'lucide-react';
import { cn } from '@/lib/utils';

// 用户页自己用的几个小零件。项目里没有 tooltip / checkbox 组件，这里用最小实现：
// 悬停提示走 CSS（group-hover / focus-within），同时带 aria-label，键盘聚焦也能看到。

export function Tip({ content, children, className, side = 'bottom', align = 'start' }: {
    content: ReactNode;
    children: ReactNode;
    className?: string;
    side?: 'top' | 'bottom';
    align?: 'start' | 'end' | 'center';
}) {
    return (
        <span className={cn('group/tip relative inline-flex', className)}>
            {children}
            <span
                role="tooltip"
                className={cn(
                    'pointer-events-none invisible absolute z-50 w-max max-w-64 rounded-md bg-muted px-3 py-1.5 text-left text-xs leading-5 font-normal whitespace-normal text-muted-foreground opacity-0 shadow-md ring-1 ring-border transition-opacity group-hover/tip:visible group-hover/tip:opacity-100 group-focus-within/tip:visible group-focus-within/tip:opacity-100',
                    side === 'bottom' ? 'top-full mt-2' : 'bottom-full mb-2',
                    align === 'start' ? 'left-0' : align === 'end' ? 'right-0' : 'left-1/2 -translate-x-1/2'
                )}
            >
                {content}
            </span>
        </span>
    );
}

export function HelpTip({ content, align }: { content: ReactNode; align?: 'start' | 'end' | 'center' }) {
    return (
        <Tip content={content} align={align}>
            <span tabIndex={0} aria-label={typeof content === 'string' ? content : undefined} className="inline-flex size-3.5 cursor-help items-center justify-center text-muted-foreground/60 outline-none">
                <CircleHelp className="size-3.5" />
            </span>
        </Tip>
    );
}

export function Checkbox({ checked, indeterminate, onChange, label, className }: {
    checked: boolean;
    indeterminate?: boolean;
    onChange: (next: boolean) => void;
    label: string;
    className?: string;
}) {
    const on = checked || indeterminate;
    return (
        <button
            type="button"
            role="checkbox"
            aria-checked={indeterminate ? 'mixed' : checked}
            aria-label={label}
            onClick={(e) => { e.stopPropagation(); onChange(!checked); }}
            className={cn(
                'inline-flex size-4 shrink-0 items-center justify-center rounded-[4px] border transition-colors',
                on ? 'border-primary bg-primary text-primary-foreground' : 'border-input bg-background hover:border-primary/60',
                className
            )}
        >
            {indeterminate ? <Minus className="size-3" /> : checked ? <Check className="size-3" /> : null}
        </button>
    );
}

/** 只画一个勾选框（放在可点的整行按钮里用，避免按钮套按钮）。 */
export function CheckMark({ checked }: { checked: boolean }) {
    return (
        <span aria-hidden className={cn('inline-flex size-4 shrink-0 items-center justify-center rounded-[4px] border', checked ? 'border-primary bg-primary text-primary-foreground' : 'border-input bg-background')}>
            {checked && <Check className="size-3" />}
        </span>
    );
}

export function Seg<T extends string>({ value, options, onChange, className, itemClassName, size = 'md' }: {
    value: T;
    options: { value: T; label: ReactNode }[];
    onChange: (v: T) => void;
    className?: string;
    itemClassName?: string;
    size?: 'sm' | 'md';
}) {
    return (
        <div role="tablist" className={cn('inline-flex items-center rounded-lg bg-muted p-[3px] text-muted-foreground', size === 'sm' ? 'h-8' : 'h-9', className)}>
            {options.map((o) => (
                <button
                    key={o.value}
                    type="button"
                    role="tab"
                    aria-selected={o.value === value}
                    onClick={() => onChange(o.value)}
                    className={cn(
                        'inline-flex h-full items-center justify-center rounded-md px-3 font-medium whitespace-nowrap transition-colors',
                        size === 'sm' ? 'text-xs' : 'text-sm',
                        o.value === value ? 'bg-background text-foreground shadow-sm' : 'hover:text-foreground',
                        itemClassName
                    )}
                >
                    {o.label}
                </button>
            ))}
        </div>
    );
}

export function Bar({ value, tone = 'primary', className }: { value: number; tone?: 'primary' | 'danger' | 'chart3' | 'chart5'; className?: string }) {
    const pct = Math.max(0, Math.min(100, Number.isFinite(value) ? value : 0));
    return (
        <div className={cn('h-1.5 w-full overflow-hidden rounded-full bg-muted', className)}>
            <i
                className={cn(
                    'block h-full rounded-full',
                    tone === 'danger' ? 'bg-destructive' : tone === 'chart3' ? 'bg-chart-3' : tone === 'chart5' ? 'bg-chart-5' : 'bg-primary'
                )}
                style={{ width: `${pct}%` }}
            />
        </div>
    );
}

type BadgeTone = 'ok' | 'off' | 'warn' | 'bad' | 'pri' | 'outline';

export function Pill({ tone = 'outline', className, children, title }: { tone?: BadgeTone; className?: string; children: ReactNode; title?: string }) {
    return (
        <span
            title={title}
            className={cn(
                'inline-flex max-w-full min-w-0 items-center gap-1 overflow-hidden rounded-full border px-2 py-0.5 text-xs font-medium whitespace-nowrap',
                tone === 'ok' && 'border-transparent bg-primary/15 text-primary',
                tone === 'off' && 'border-transparent bg-muted text-muted-foreground',
                tone === 'warn' && 'border-transparent bg-amber-500/15 text-amber-700 dark:text-amber-400',
                tone === 'bad' && 'border-transparent bg-destructive/10 text-destructive',
                tone === 'pri' && 'border-transparent bg-primary text-primary-foreground',
                tone === 'outline' && 'border-border text-foreground',
                className
            )}
        >
            {children}
        </span>
    );
}

export const btn = 'inline-flex h-9 items-center gap-1.5 rounded-xl border border-border bg-background px-3 text-sm font-medium whitespace-nowrap transition-colors hover:bg-muted disabled:pointer-events-none disabled:opacity-50';
export const btnPrimary = 'inline-flex h-9 items-center gap-1.5 rounded-xl bg-primary px-3 text-sm font-medium whitespace-nowrap text-primary-foreground transition-opacity hover:opacity-90 disabled:pointer-events-none disabled:opacity-50';
export const btnDanger = 'inline-flex h-9 items-center gap-1.5 rounded-xl bg-destructive/10 px-3 text-sm font-medium whitespace-nowrap text-destructive transition-colors hover:bg-destructive hover:text-destructive-foreground disabled:pointer-events-none disabled:opacity-50';
export const iconBtn = 'inline-flex size-8 items-center justify-center rounded-lg text-muted-foreground transition-colors hover:bg-muted hover:text-foreground';
export const chip = 'inline-flex h-8 items-center gap-1.5 rounded-xl border border-dashed border-border bg-background/60 px-2.5 text-xs font-medium whitespace-nowrap text-muted-foreground transition-colors hover:text-foreground';
export const chipOn = 'border-solid border-primary/40 bg-primary/10 text-primary';
export const label = 'mb-1.5 flex items-center gap-1 text-xs text-muted-foreground';
