'use client';

import { useState } from 'react';
import { Ban, CircleCheck, Ellipsis, KeyRound, Logs, ShieldCheck, ShieldOff, Trash2 } from 'lucide-react';
import { useTranslations } from 'next-intl';
import type { User } from '@/api/endpoints/user';
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover';
import { cn } from '@/lib/utils';
import { iconBtn } from './ui-bits';

export interface UserRowHandlers {
    onEdit: (user: User, tab?: 'profile' | 'usage' | 'access' | 'client') => void;
    onBalance: (user: User) => void;
    onToggleStatus: (user: User) => void;
    onToggleRole: (user: User) => void;
    onResetPassword: (user: User) => void;
    onViewLogs: (user: User) => void;
    onDelete: (user: User) => void;
}

// 行尾「⋯」菜单（new-api 的 data-table-row-actions）：停用/启用、设为管理员/普通用户、重置密码、看他的日志、删除。
export function DataTableRowActions({ user, handlers, className }: { user: User; handlers: UserRowHandlers; className?: string }) {
    const t = useTranslations('user.menu');
    const [open, setOpen] = useState(false);
    const run = (fn: (u: User) => void) => () => { setOpen(false); fn(user); };
    const item = 'flex w-full items-center gap-2 rounded-lg px-2 py-1.5 text-left text-sm hover:bg-muted';
    return (
        <Popover open={open} onOpenChange={setOpen}>
            <PopoverTrigger asChild>
                <button type="button" aria-label={t('more')} className={cn(iconBtn, open && 'bg-muted text-foreground', className)}>
                    <Ellipsis className="size-4" />
                </button>
            </PopoverTrigger>
            <PopoverContent align="end" className="w-44 p-1" onClick={(e) => e.stopPropagation()}>
                <button type="button" className={item} onClick={run(handlers.onToggleStatus)}>
                    {user.status === 'active' ? <Ban className="size-4" /> : <CircleCheck className="size-4" />}
                    {user.status === 'active' ? t('disable') : t('enable')}
                </button>
                <button type="button" className={item} onClick={run(handlers.onToggleRole)}>
                    {user.role === 'admin' ? <ShieldOff className="size-4" /> : <ShieldCheck className="size-4" />}
                    {user.role === 'admin' ? t('makeUser') : t('makeAdmin')}
                </button>
                <button type="button" className={item} onClick={run(handlers.onResetPassword)}>
                    <KeyRound className="size-4" />
                    {t('resetPassword')}
                </button>
                <button type="button" className={item} onClick={run(handlers.onViewLogs)}>
                    <Logs className="size-4" />
                    {t('viewLogs')}
                </button>
                <div className="my-1 h-px bg-border" />
                <button type="button" className={cn(item, 'text-destructive')} onClick={run(handlers.onDelete)}>
                    <Trash2 className="size-4" />
                    {t('delete')}
                </button>
            </PopoverContent>
        </Popover>
    );
}
