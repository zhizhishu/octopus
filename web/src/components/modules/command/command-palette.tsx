'use client';

import * as React from 'react';
import * as DialogPrimitive from '@radix-ui/react-dialog';
import { Search } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { cn } from '@/lib/utils';
import { useNavStore, type NavItem } from '@/components/modules/navbar/nav-store';
import { routesForRole } from '@/route/config';
import { useAuthStore } from '@/api/endpoints/user';
import { useChannelList } from '@/api/endpoints/channel';
import { useSearchStore } from '@/components/modules/toolbar';
import { buttonVariants } from '@/components/ui/button';
import { useCommandStore } from './command-store';

type CommandEntry = {
    key: string;
    group: string;
    label: string;
    keywords: string;
    icon?: React.ReactNode;
    onSelect: () => void;
};

/** Magnifier button for the top bar; opens the global command palette. */
export function CommandSearchButton() {
    const setOpen = useCommandStore((s) => s.setOpen);
    return (
        <button
            type="button"
            aria-label="全局搜索"
            title="全局搜索 (Ctrl/⌘ + K)"
            onClick={() => setOpen(true)}
            className={buttonVariants({
                variant: 'ghost',
                size: 'icon',
                className: 'rounded-xl transition-none hover:bg-transparent text-muted-foreground hover:text-foreground',
            })}
        >
            <Search className="size-4 transition-colors duration-300" />
        </button>
    );
}

/** Global command palette: fuzzy-search pages (and channels for admins) and
 *  jump straight there. Opened by Ctrl/Cmd+K or the top-bar magnifier. */
export function CommandPalette() {
    const open = useCommandStore((s) => s.open);
    const setOpen = useCommandStore((s) => s.setOpen);
    const toggle = useCommandStore((s) => s.toggle);
    const t = useTranslations('navbar');
    const role = useAuthStore((s) => s.user?.role);
    const setActiveItem = useNavStore((s) => s.setActiveItem);
    const setSearchTerm = useSearchStore((s) => s.setSearchTerm);
    const { data: channels = [] } = useChannelList();
    const [query, setQuery] = React.useState('');
    const [highlight, setHighlight] = React.useState(0);
    const listRef = React.useRef<HTMLDivElement>(null);

    React.useEffect(() => {
        const onKey = (event: KeyboardEvent) => {
            if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'k') {
                event.preventDefault();
                toggle();
            }
        };
        window.addEventListener('keydown', onKey);
        return () => window.removeEventListener('keydown', onKey);
    }, [toggle]);

    const goPage = React.useCallback((id: NavItem) => {
        setActiveItem(id);
        setOpen(false);
    }, [setActiveItem, setOpen]);

    const goChannel = React.useCallback((name: string) => {
        setActiveItem('channel');
        setSearchTerm('channel', name);
        setOpen(false);
    }, [setActiveItem, setSearchTerm, setOpen]);

    const entries = React.useMemo<CommandEntry[]>(() => {
        const pages = routesForRole(role).map((route) => {
            const Icon = route.icon;
            return {
                key: `page:${route.id}`,
                group: '页面',
                label: t(route.id),
                keywords: `${route.id} ${route.label} ${t(route.id)}`,
                icon: <Icon className="size-4" />,
                onSelect: () => goPage(route.id as NavItem),
            };
        });
        const channelEntries: CommandEntry[] = role === 'admin'
            ? channels.map((channel) => ({
                key: `channel:${channel.raw.id}`,
                group: '渠道',
                label: `#${channel.raw.id} ${channel.raw.name}`,
                keywords: `${channel.raw.id} ${channel.raw.name}`,
                onSelect: () => goChannel(channel.raw.name),
            }))
            : [];
        return [...pages, ...channelEntries];
    }, [role, channels, t, goPage, goChannel]);

    const filtered = React.useMemo(() => {
        const terms = query.toLowerCase().split(/\s+/).filter(Boolean);
        if (terms.length === 0) return entries;
        return entries.filter((entry) => {
            const haystack = `${entry.label} ${entry.keywords}`.toLowerCase();
            return terms.every((term) => haystack.includes(term));
        });
    }, [entries, query]);

    React.useEffect(() => {
        if (open) {
            setQuery('');
            setHighlight(0);
        }
    }, [open]);

    React.useEffect(() => {
        setHighlight((current) => (filtered.length === 0 ? 0 : Math.min(current, filtered.length - 1)));
    }, [filtered.length]);

    React.useEffect(() => {
        const node = listRef.current?.querySelector<HTMLElement>(`[data-index="${highlight}"]`);
        node?.scrollIntoView({ block: 'nearest' });
    }, [highlight]);

    const onKeyDown = (event: React.KeyboardEvent<HTMLInputElement>) => {
        if (event.key === 'ArrowDown') {
            event.preventDefault();
            setHighlight((c) => Math.min(c + 1, filtered.length - 1));
        } else if (event.key === 'ArrowUp') {
            event.preventDefault();
            setHighlight((c) => Math.max(c - 1, 0));
        } else if (event.key === 'Enter') {
            event.preventDefault();
            filtered[highlight]?.onSelect();
        }
    };

    let lastGroup = '';

    return (
        <DialogPrimitive.Root open={open} onOpenChange={setOpen}>
            <DialogPrimitive.Portal>
                <DialogPrimitive.Overlay className="fixed inset-0 z-50 bg-black/40 backdrop-blur-sm data-[state=open]:animate-in data-[state=closed]:animate-out data-[state=closed]:fade-out-0 data-[state=open]:fade-in-0" />
                <DialogPrimitive.Content
                    onOpenAutoFocus={(event) => event.preventDefault()}
                    className="fixed left-1/2 top-[12vh] z-50 flex max-h-[70vh] w-[calc(100vw-1.5rem)] max-w-xl -translate-x-1/2 flex-col overflow-hidden rounded-2xl border border-border/60 bg-popover text-popover-foreground shadow-2xl data-[state=open]:animate-in data-[state=closed]:animate-out data-[state=closed]:fade-out-0 data-[state=open]:fade-in-0 data-[state=closed]:zoom-out-95 data-[state=open]:zoom-in-95"
                >
                    <DialogPrimitive.Title className="sr-only">全局搜索</DialogPrimitive.Title>
                    <DialogPrimitive.Description className="sr-only">搜索并跳转到页面或渠道</DialogPrimitive.Description>
                    <div className="flex items-center gap-2 border-b border-border/60 px-4">
                        <Search className="size-4 shrink-0 text-muted-foreground" />
                        <input
                            autoFocus
                            value={query}
                            onChange={(event) => setQuery(event.target.value)}
                            onKeyDown={onKeyDown}
                            placeholder="搜索页面、渠道…"
                            className="h-12 w-full min-w-0 bg-transparent text-sm outline-none placeholder:text-muted-foreground"
                        />
                        <kbd className="hidden shrink-0 rounded border border-border/60 px-1.5 py-0.5 text-[10px] text-muted-foreground sm:inline">Esc</kbd>
                    </div>
                    <div
                        ref={listRef}
                        className="min-h-0 flex-1 overflow-y-auto overscroll-contain p-2 pb-[calc(0.5rem+env(safe-area-inset-bottom))]"
                    >
                        {filtered.length === 0 ? (
                            <div className="px-3 py-10 text-center text-sm text-muted-foreground">没有匹配项，换个词试试</div>
                        ) : (
                            filtered.map((entry, index) => {
                                const showGroup = entry.group !== lastGroup;
                                lastGroup = entry.group;
                                const isActive = index === highlight;
                                return (
                                    <React.Fragment key={entry.key}>
                                        {showGroup ? (
                                            <div className="px-2 pb-1 pt-2 text-[11px] font-medium text-muted-foreground">
                                                {entry.group}
                                            </div>
                                        ) : null}
                                        <button
                                            type="button"
                                            data-index={index}
                                            onMouseMove={() => setHighlight(index)}
                                            onClick={() => entry.onSelect()}
                                            className={cn(
                                                'flex w-full min-w-0 items-center gap-2.5 rounded-xl px-3 py-2.5 text-left text-sm transition-colors',
                                                isActive ? 'bg-accent text-accent-foreground' : 'text-foreground'
                                            )}
                                        >
                                            {entry.icon ? <span className="shrink-0 text-muted-foreground">{entry.icon}</span> : null}
                                            <span className="min-w-0 truncate">{entry.label}</span>
                                        </button>
                                    </React.Fragment>
                                );
                            })
                        )}
                    </div>
                </DialogPrimitive.Content>
            </DialogPrimitive.Portal>
        </DialogPrimitive.Root>
    );
}
