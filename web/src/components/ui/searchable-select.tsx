'use client';

import * as React from 'react';
import { ChevronDownIcon, SearchIcon } from 'lucide-react';
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover';
import { cn } from '@/lib/utils';

export type SearchableSelectOption = {
    value: string;
    label: string;
    /** Extra text to match against when searching (e.g. id, base url, models). */
    keywords?: string;
    disabled?: boolean;
};

type SearchableSelectProps = {
    value?: string;
    onValueChange: (value: string) => void;
    options: SearchableSelectOption[];
    placeholder?: string;
    searchPlaceholder?: string;
    emptyText?: string;
    className?: string;
    contentClassName?: string;
    disabled?: boolean;
    align?: 'start' | 'center' | 'end';
};

function matchOption(option: SearchableSelectOption, terms: string[]) {
    if (terms.length === 0) return true;
    const haystack = `${option.label} ${option.keywords ?? ''}`.toLowerCase();
    return terms.every((term) => haystack.includes(term));
}

/**
 * A select control with an inline fuzzy-search box. Built on the project's Radix
 * Popover so it inherits theming, portal/stacking and mobile width behaviour.
 * Keyboard: ArrowUp/ArrowDown move the highlight, Enter commits, Esc closes.
 */
export function SearchableSelect({
    value,
    onValueChange,
    options,
    placeholder = '请选择',
    searchPlaceholder = '输入关键词搜索…',
    emptyText = '没有匹配项',
    className,
    contentClassName,
    disabled,
    align = 'start',
}: SearchableSelectProps) {
    const [open, setOpen] = React.useState(false);
    const [query, setQuery] = React.useState('');
    const [highlight, setHighlight] = React.useState(0);
    const listRef = React.useRef<HTMLDivElement>(null);

    const selected = React.useMemo(
        () => options.find((option) => option.value === value),
        [options, value]
    );

    const filtered = React.useMemo(() => {
        const terms = query.toLowerCase().split(/\s+/).filter(Boolean);
        return options.filter((option) => matchOption(option, terms));
    }, [options, query]);

    // On open: clear nothing yet but park the highlight on the current selection.
    // On close: reset the query so the next open starts fresh.
    React.useEffect(() => {
        if (!open) {
            setQuery('');
            return;
        }
        const idx = options.findIndex((option) => option.value === value);
        setHighlight(idx >= 0 ? idx : 0);
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [open]);

    React.useEffect(() => {
        setHighlight((current) => {
            if (filtered.length === 0) return 0;
            return Math.min(current, filtered.length - 1);
        });
    }, [filtered.length]);

    React.useEffect(() => {
        if (!open) return;
        const node = listRef.current?.querySelector<HTMLElement>(`[data-index="${highlight}"]`);
        node?.scrollIntoView({ block: 'nearest' });
    }, [highlight, open]);

    const commit = (option: SearchableSelectOption) => {
        if (option.disabled) return;
        onValueChange(option.value);
        setOpen(false);
    };

    const onKeyDown = (event: React.KeyboardEvent<HTMLInputElement>) => {
        if (event.key === 'ArrowDown') {
            event.preventDefault();
            setHighlight((current) => Math.min(current + 1, filtered.length - 1));
        } else if (event.key === 'ArrowUp') {
            event.preventDefault();
            setHighlight((current) => Math.max(current - 1, 0));
        } else if (event.key === 'Enter') {
            event.preventDefault();
            const option = filtered[highlight];
            if (option) commit(option);
        }
    };

    return (
        <Popover open={open} onOpenChange={setOpen}>
            <PopoverTrigger asChild>
                <button
                    type="button"
                    disabled={disabled}
                    className={cn(
                        'flex h-9 min-w-0 items-center justify-between gap-2 rounded-xl border border-input bg-background px-3 text-sm text-foreground outline-none transition-[color,box-shadow] focus-visible:ring-2 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-50',
                        className
                    )}
                >
                    <span className={cn('min-w-0 truncate text-left', !selected && 'text-muted-foreground')}>
                        {selected ? selected.label : placeholder}
                    </span>
                    <ChevronDownIcon className="size-4 shrink-0 opacity-50" />
                </button>
            </PopoverTrigger>
            <PopoverContent
                align={align}
                sideOffset={6}
                className={cn(
                    'w-[var(--radix-popover-trigger-width)] max-w-[calc(100vw-1rem)] overflow-hidden rounded-xl border border-border/60 bg-popover p-0 shadow-xl',
                    contentClassName
                )}
            >
                <div className="flex items-center gap-2 border-b border-border/60 px-3">
                    <SearchIcon className="size-4 shrink-0 text-muted-foreground" />
                    <input
                        autoFocus
                        value={query}
                        onChange={(event) => setQuery(event.target.value)}
                        onKeyDown={onKeyDown}
                        placeholder={searchPlaceholder}
                        className="h-9 w-full min-w-0 bg-transparent text-sm outline-none placeholder:text-muted-foreground"
                    />
                </div>
                <div ref={listRef} className="max-h-64 overflow-y-auto overscroll-contain p-1">
                    {filtered.length === 0 ? (
                        <div className="px-3 py-6 text-center text-sm text-muted-foreground">{emptyText}</div>
                    ) : (
                        filtered.map((option, index) => {
                            const isSelected = option.value === value;
                            const isActive = index === highlight;
                            return (
                                <button
                                    key={option.value || `__empty_${index}`}
                                    type="button"
                                    data-index={index}
                                    disabled={option.disabled}
                                    onMouseMove={() => setHighlight(index)}
                                    onClick={() => commit(option)}
                                    className={cn(
                                        'flex w-full min-w-0 items-center gap-2 rounded-lg px-2.5 py-2 text-left text-sm transition-colors',
                                        isActive ? 'bg-accent text-accent-foreground' : 'text-foreground',
                                        isSelected && 'font-medium',
                                        option.disabled && 'pointer-events-none opacity-50'
                                    )}
                                >
                                    <span className="min-w-0 truncate">{option.label}</span>
                                    {isSelected ? <span className="ml-auto shrink-0 text-xs text-primary">已选</span> : null}
                                </button>
                            );
                        })
                    )}
                </div>
            </PopoverContent>
        </Popover>
    );
}
