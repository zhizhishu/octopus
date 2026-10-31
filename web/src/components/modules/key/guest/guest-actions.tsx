'use client';

import { useState } from 'react';
import { Ban, ChartColumn, CircleCheck, Copy, Ellipsis, Logs, ShieldOff, Trash2 } from 'lucide-react';
import { useTranslations } from 'next-intl';
import type { GuestKey } from '@/api/endpoints/guest-key';
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover';
import { cn } from '@/lib/utils';
import { iconBtn } from '@/components/modules/user/components/ui-bits';

export interface GuestRowHandlers {
    onEdit: (k: GuestKey) => void;
    onDetail: (k: GuestKey) => void;
    onCopy: (k: GuestKey) => void;
    onLogs: (k: GuestKey) => void;
    onToggle: (k: GuestKey) => void;
    onRevoke: (k: GuestKey) => void;
    onDelete: (k: GuestKey) => void;
}

// 行尾「⋯」：用量和 IP、请求日志、复制、停用/启用、作废、删除。已作废的只剩看和删。
export function GuestRowActions({ apiKey, handlers }: { apiKey: GuestKey; handlers: GuestRowHandlers }) {
    const t = useTranslations('keys.guest.menu');
    const [open, setOpen] = useState(false);
    const run = (fn: () => void) => () => { setOpen(false); fn(); };
    const item = 'flex w-full items-center gap-2 rounded-lg px-2 py-1.5 text-left text-sm hover:bg-muted';
    const revoked = (apiKey.revoked_at ?? 0) > 0;
    return (
        <Popover open={open} onOpenChange={setOpen}>
            <PopoverTrigger asChild>
                <button type="button" aria-label={t('more')} className={cn(iconBtn, open && 'bg-muted text-foreground')}>
                    <Ellipsis className="size-4" />
                </button>
            </PopoverTrigger>
            <PopoverContent align="end" className="w-44 p-1" onClick={(e) => e.stopPropagation()}>
                <button type="button" className={item} onClick={run(() => handlers.onDetail(apiKey))}><ChartColumn className="size-4" />{t('detail')}</button>
                <button type="button" className={item} onClick={run(() => handlers.onLogs(apiKey))}><Logs className="size-4" />{t('logs')}</button>
                {!revoked && <button type="button" className={item} onClick={run(() => handlers.onCopy(apiKey))}><Copy className="size-4" />{t('copy')}</button>}
                {!revoked && (
                    <button type="button" className={item} onClick={run(() => handlers.onToggle(apiKey))}>
                        {apiKey.enabled ? <Ban className="size-4" /> : <CircleCheck className="size-4" />}
                        {apiKey.enabled ? t('disable') : t('enable')}
                    </button>
                )}
                <div className="my-1 h-px bg-border" />
                {!revoked && <button type="button" className={cn(item, 'text-destructive')} onClick={run(() => handlers.onRevoke(apiKey))}><ShieldOff className="size-4" />{t('revoke')}</button>}
                <button type="button" className={cn(item, 'text-destructive')} onClick={run(() => handlers.onDelete(apiKey))}><Trash2 className="size-4" />{t('delete')}</button>
            </PopoverContent>
        </Popover>
    );
}
