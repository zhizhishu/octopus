'use client';

import { Pencil } from 'lucide-react';
import { useTranslations } from 'next-intl';
import type { APIKey } from '@/api/endpoints/apikey';
import { SafeText } from '@/components/common/SafeText';
import { cn } from '@/lib/utils';
import { iconBtn } from '@/components/modules/user/components/ui-bits';
import { keyStatus } from '../lib/key-filters';
import { AccessCell, ExpiryCell, KeyStatusPill, LastUsedCell, MaskedKey, PlanCell, SpendCell } from './bits';
import { KeyRowActions, type KeyRowHandlers } from './key-row-actions';

// 手机：一把 Key 一张卡。上面名称 + 状态 + 操作，下面两列小格子。
export function KeysMobileList({ keys, spentOf, ownerOf, nowSec, handlers }: {
    keys: APIKey[];
    spentOf: (id: number) => number;
    ownerOf?: (key: APIKey) => string;
    nowSec: number;
    handlers: KeyRowHandlers;
}) {
    const t = useTranslations('keys.table');
    const cell = 'min-w-0';
    const cap = 'mb-1 text-[11px] text-muted-foreground';
    return (
        <div className="space-y-3">
            {keys.map((k) => {
                const spent = spentOf(k.id);
                const status = keyStatus(k, spent, nowSec);
                return (
                    <div key={k.id} data-key-card={k.id} className={cn('rounded-3xl border border-border bg-card p-4', status === 'off' && 'opacity-70')}>
                        <div className="flex items-start gap-2">
                            <button type="button" onClick={() => handlers.onEdit(k)} className="min-w-0 flex-1 text-left">
                                <div className="flex min-w-0 items-center gap-2">
                                    <SafeText value={k.name} className="min-w-0 truncate font-medium" />
                                    <KeyStatusPill status={status} />
                                </div>
                            </button>
                            <button type="button" className={iconBtn} aria-label={t('edit')} onClick={() => handlers.onEdit(k)}><Pencil className="size-4" /></button>
                            <KeyRowActions apiKey={k} handlers={handlers} />
                        </div>
                        <div className="mt-0.5 flex min-w-0 items-center gap-1 text-xs text-muted-foreground">
                            {ownerOf && <><span className="truncate">{ownerOf(k)}</span><span>·</span></>}
                            <MaskedKey value={k.api_key} />
                        </div>
                        <div className="mt-3 grid grid-cols-2 gap-x-4 gap-y-3">
                            <div className={cell}><div className={cap}>{t('spend')}</div><SpendCell spent={spent} cap={k.max_cost} className="w-full" /></div>
                            <div className={cell}><div className={cap}>{t('expiry')}</div><ExpiryCell expireAt={k.expire_at} nowSec={nowSec} /></div>
                            <div className={cell}><div className={cap}>{t('access')}</div><AccessCell apiKey={k} /></div>
                            <div className={cell}><div className={cap}>{t('lastUsed')}</div><LastUsedCell apiKey={k} nowSec={nowSec} /></div>
                            {(k.access_plans?.length ?? 0) > 0 && <div className={cn(cell, 'col-span-2')}><div className={cap}>{t('plans')}</div><PlanCell apiKey={k} /></div>}
                        </div>
                    </div>
                );
            })}
        </div>
    );
}
