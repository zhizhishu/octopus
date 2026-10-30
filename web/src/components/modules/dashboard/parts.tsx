'use client';

import { useState, type ReactNode } from 'react';
import { CircleHelp } from 'lucide-react';
import { Area, AreaChart, ResponsiveContainer, YAxis } from 'recharts';
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover';
import { cn } from '@/lib/utils';

/**
 * 小问号：桌面上鼠标移上去就出，手机上点一下出。细节都放这里，正文只留短句。
 */
export function InfoTip({ children, className, label = '?' }: { children: ReactNode; className?: string; label?: string }) {
    const [open, setOpen] = useState(false);
    return (
        <Popover open={open} onOpenChange={setOpen}>
            <PopoverTrigger
                aria-label={label}
                className={cn('inline-flex size-5 shrink-0 items-center justify-center rounded-full text-muted-foreground/70 transition-colors hover:text-foreground', className)}
                onPointerEnter={(e) => { if (e.pointerType === 'mouse') setOpen(true); }}
                onPointerLeave={(e) => { if (e.pointerType === 'mouse') setOpen(false); }}
            >
                <CircleHelp className="size-3.5" />
            </PopoverTrigger>
            <PopoverContent side="top" className="w-72 rounded-xl p-3 text-xs leading-relaxed text-muted-foreground" onOpenAutoFocus={(e) => e.preventDefault()}>
                {children}
            </PopoverContent>
        </Popover>
    );
}

export function Panel({ title, tip, right, children, className }: { title: ReactNode; tip?: ReactNode; right?: ReactNode; children: ReactNode; className?: string }) {
    return (
        <section className={cn('min-w-0 rounded-3xl border border-card-border bg-card p-4 text-card-foreground custom-shadow md:p-5', className)}>
            <div className="mb-3 flex min-w-0 items-center justify-between gap-2">
                <div className="flex min-w-0 items-center gap-1">
                    <h3 className="truncate text-base font-semibold">{title}</h3>
                    {tip ? <InfoTip>{tip}</InfoTip> : null}
                </div>
                {right}
            </div>
            {children}
        </section>
    );
}

export interface SegmentOption<T extends string> {
    value: T;
    label: ReactNode;
    disabled?: boolean;
    className?: string;
}

/** 一排药丸按钮（今天/昨天/7 天…、模型/渠道/方案…）。 */
export function Segmented<T extends string>({ value, options, onChange, className, size = 'md', ariaLabel }: {
    value: T | null;
    options: SegmentOption<T>[];
    onChange: (value: T) => void;
    className?: string;
    size?: 'sm' | 'md';
    ariaLabel?: string;
}) {
    return (
        <div role="tablist" aria-label={ariaLabel} className={cn('inline-flex min-w-0 items-center gap-0.5 rounded-xl bg-muted p-0.5', className)}>
            {options.map((option) => {
                const active = option.value === value;
                return (
                    <button
                        key={option.value}
                        type="button"
                        role="tab"
                        aria-selected={active}
                        disabled={option.disabled}
                        onClick={() => onChange(option.value)}
                        className={cn(
                            'shrink-0 whitespace-nowrap rounded-[10px] font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-40',
                            size === 'sm' ? 'px-2.5 py-1 text-xs' : 'px-3 py-1.5 text-sm',
                            active ? 'bg-background text-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground',
                            option.className,
                        )}
                    >
                        {option.label}
                    </button>
                );
            })}
        </div>
    );
}

/** ▲ 12% / ▼ 4%；没法比时显示「—」。 */
export function Delta({ value, className }: { value: number | null; className?: string }) {
    if (value === null || !Number.isFinite(value)) {
        return <span className={cn('text-muted-foreground', className)}>—</span>;
    }
    const pct = Math.abs(value * 100);
    const text = `${pct >= 1 ? pct.toFixed(0) : pct.toFixed(1)}%`;
    if (Math.abs(value) < 0.0005) return <span className={cn('text-muted-foreground', className)}>±0%</span>;
    const up = value > 0;
    return (
        <span className={cn(up ? 'text-primary' : 'text-destructive', 'whitespace-nowrap font-medium', className)}>
            {up ? '▲' : '▼'} {text}
        </span>
    );
}

/** 卡片底部的小折线。 */
export function Sparkline({ values, className }: { values: (number | null)[]; className?: string }) {
    const data = values.map((v, i) => ({ i, v: v ?? 0 }));
    if (data.length < 2) return <div className={cn('h-8', className)} />;
    // 纵轴留出下方余量，起伏看着跟真实幅度相称，不会被拉满成锯齿。
    const nums = data.map((d) => d.v);
    const lo = Math.min(...nums);
    const hi = Math.max(...nums);
    const pad = hi > lo ? (hi - lo) * 0.8 : Math.abs(hi) || 1;
    const domain: [number, number] = [Math.max(0, lo - pad), hi + (hi - lo) * 0.1];
    return (
        <div className={cn('h-8 w-full', className)}>
            <ResponsiveContainer width="100%" height="100%">
                <AreaChart data={data} margin={{ top: 2, right: 0, bottom: 2, left: 0 }}>
                    <YAxis hide domain={domain} />
                    <Area type="linear" dataKey="v" stroke="var(--chart-1)" strokeWidth={1.5} fill="var(--chart-1)" fillOpacity={0.08} isAnimationActive={false} dot={false} />
                </AreaChart>
            </ResponsiveContainer>
        </div>
    );
}

/** 花费条（表格里的横条）。 */
export function CostBar({ ratio, className }: { ratio: number; className?: string }) {
    const pct = Math.max(0, Math.min(1, Number.isFinite(ratio) ? ratio : 0)) * 100;
    return (
        <div className={cn('h-1.5 w-full overflow-hidden rounded-full bg-muted', className)}>
            <div className="h-full rounded-full bg-primary" style={{ width: `${pct}%` }} />
        </div>
    );
}

/** 「花在哪」和「前 5 名」共用的配色：绿系为主，最后一个灰色给「其他」。 */
export const SLICE_COLORS = ['#5fa86a', '#3f8a52', '#2f5f3e', '#d39b45', '#c0675a', '#7c8a7f'];
export const OTHERS_COLOR = '#8a8a85';
