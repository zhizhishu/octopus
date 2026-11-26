'use client';

import { useState } from 'react';
import { BookOpen, ChartColumn, CircleCheck, Copy, Ellipsis, Logs, Ban, Trash2 } from 'lucide-react';
import { useTranslations } from 'next-intl';
import type { APIKey } from '@/api/endpoints/apikey';
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover';
import { cn } from '@/lib/utils';
import { iconBtn } from '@/components/modules/user/components/ui-bits';

export interface KeyRowHandlers {
    onEdit: (key: APIKey, tab?: 'settings' | 'usage') => void;
    onGuide: (key: APIKey) => void;
    onToggle: (key: APIKey) => void;
    onCopy: (key: APIKey) => void;
    onViewLogs: (key: APIKey) => void;
    onDelete: (key: APIKey) => void;
}

// 行尾「⋯」：接入说明、看用量、停用/启用、复制完整 Key、看它的请求、删除。
export function KeyRowActions({ apiKey, handlers, className }: { apiKey: APIKey; handlers: KeyRowHandlers; className?: string }) {
    const t = useTranslations('keys.menu');
    const [open, setOpen] = useState(false);
    const run = (fn: () => void) => () => { setOpen(false); fn(); };
    const item = 'flex w-full items-center gap-2 rounded-lg px-2 py-1.5 text-left text-sm hover:bg-muted';
    return (
        <Popover open={open} onOpenChange={setOpen}>
            <PopoverTrigger asChild>
                <button type="button" aria-label={t('more')} className={cn(iconBtn, open && 'bg-muted text-foreground', className)}>
                    <Ellipsis className="size-4" />
                </button>
            </PopoverTrigger>
            <PopoverContent align="end" className="w-44 p-1" onClick={(e) => e.stopPropagation()}>
                <button type="button" className={item} onClick={run(() => handlers.onGuide(apiKey))}><BookOpen className="size-4" />{t('guide')}</button>
                <button type="button" className={item} onClick={run(() => handlers.onCopy(apiKey))}><Copy className="size-4" />{t('copy')}</button>
                <button type="button" className={item} onClick={run(() => handlers.onEdit(apiKey, 'usage'))}><ChartColumn className="size-4" />{t('usage')}</button>
                <button type="button" className={item} onClick={run(() => handlers.onViewLogs(apiKey))}><Logs className="size-4" />{t('logs')}</button>
                <button type="button" className={item} onClick={run(() => handlers.onToggle(apiKey))}>
                    {apiKey.enabled ? <Ban className="size-4" /> : <CircleCheck className="size-4" />}
                    {apiKey.enabled ? t('disable') : t('enable')}
                </button>
                <div className="my-1 h-px bg-border" />
                <button type="button" className={cn(item, 'text-destructive')} onClick={run(() => handlers.onDelete(apiKey))}><Trash2 className="size-4" />{t('delete')}</button>
            </PopoverContent>
        </Popover>
    );
}
