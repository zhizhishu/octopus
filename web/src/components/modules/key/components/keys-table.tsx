'use client';

import { Pencil } from 'lucide-react';
import { useTranslations } from 'next-intl';
import type { APIKey } from '@/api/endpoints/apikey';
import { SafeText } from '@/components/common/SafeText';
import { cn } from '@/lib/utils';
import { Checkbox, HelpTip, iconBtn } from '@/components/modules/user/components/ui-bits';
import { keyStatus } from '../lib/key-filters';
import { AccessCell, ExpiryCell, KeyStatusPill, LastUsedCell, MaskedKey, PlanCell, SpendCell } from './bits';
import { KeyRowActions, type KeyRowHandlers } from './key-row-actions';

const th = 'h-10 px-3 text-left align-middle text-xs font-medium whitespace-nowrap text-muted-foreground';
const td = 'px-3 py-3 align-middle';

// 桌面表格：名称（归属 · 遮住的 Key + 复制）/ 状态 / 已用·上限 / 有效期 / 能用 / 方案 / 最近使用 / 操作。
export function KeysTable({ keys, spentOf, ownerOf, nowSec, selected, onSelect, onSelectPage, handlers }: {
    keys: APIKey[];
    spentOf: (id: number) => number;
    /** 管理员看到「谁的」；普通用户只有自己的 Key，不显示。 */
    ownerOf?: (key: APIKey) => string;
    nowSec: number;
    selected: Set<number>;
    onSelect: (id: number, on: boolean) => void;
    onSelectPage: (on: boolean) => void;
    handlers: KeyRowHandlers;
}) {
    const t = useTranslations('keys.table');
    const pageSelected = keys.filter((k) => selected.has(k.id)).length;
    return (
        <table className="w-full text-sm">
            <thead>
                <tr className="border-b border-border/70">
                    <th className={cn(th, 'w-10 pr-0')}>
                        <Checkbox
                            checked={keys.length > 0 && pageSelected === keys.length}
                            indeterminate={pageSelected > 0 && pageSelected < keys.length}
                            onChange={onSelectPage}
                            label={t('selectPage')}
                        />
                    </th>
                    <th className={th}>{t('name')}</th>
                    <th className={th}>{t('status')}</th>
                    <th className={th}><span className="inline-flex items-center gap-1">{t('spend')}<HelpTip content={t('spendHelp')} /></span></th>
                    <th className={th}>{t('expiry')}</th>
                    <th className={th}><span className="inline-flex items-center gap-1">{t('access')}<HelpTip content={t('accessHelp')} /></span></th>
                    <th className={th}><span className="inline-flex items-center gap-1">{t('plans')}<HelpTip content={t('plansHelp')} /></span></th>
                    <th className={th}>{t('lastUsed')}</th>
                    <th className={cn(th, 'w-20')}><span className="sr-only">{t('actions')}</span></th>
                </tr>
            </thead>
            <tbody>
                {keys.map((k) => {
                    const spent = spentOf(k.id);
                    const status = keyStatus(k, spent, nowSec);
                    const dim = status === 'off' ? 'opacity-60' : '';
                    const isSel = selected.has(k.id);
                    return (
                        <tr key={k.id} data-key-row={k.id} className={cn('border-b border-border/70 transition-colors last:border-b-0 hover:bg-muted/30', isSel && 'bg-primary/5')}>
                            <td className={cn(td, 'w-10 pr-0')}>
                                <Checkbox checked={isSel} onChange={(on) => onSelect(k.id, on)} label={t('selectRow', { name: k.name })} />
                            </td>
                            <td className={cn(td, dim, 'max-w-[16rem]')}>
                                <button type="button" onClick={() => handlers.onEdit(k)} className="block max-w-full truncate text-left font-medium hover:underline">
                                    <SafeText value={k.name} />
                                </button>
                                <div className="mt-0.5 flex min-w-0 items-center gap-1 text-xs text-muted-foreground">
                                    {ownerOf && <span className="truncate">{ownerOf(k)}</span>}
                                    {ownerOf && <span>·</span>}
                                    <MaskedKey value={k.api_key} />
                                </div>
                            </td>
                            <td className={cn(td, dim)}><KeyStatusPill status={status} /></td>
                            <td className={cn(td, dim)}><SpendCell spent={spent} cap={k.max_cost} /></td>
                            <td className={cn(td, dim)}><ExpiryCell expireAt={k.expire_at} nowSec={nowSec} /></td>
                            <td className={cn(td, dim, 'max-w-[12rem]')}><AccessCell apiKey={k} /></td>
                            <td className={cn(td, dim, 'max-w-[10rem]')}><PlanCell apiKey={k} /></td>
                            <td className={cn(td, dim)}><LastUsedCell apiKey={k} nowSec={nowSec} /></td>
                            <td className={cn(td, 'w-20 pl-0')}>
                                <div className="flex items-center justify-end gap-0.5">
                                    <button type="button" className={iconBtn} aria-label={t('edit')} title={t('edit')} onClick={() => handlers.onEdit(k)}>
                                        <Pencil className="size-4" />
                                    </button>
                                    <KeyRowActions apiKey={k} handlers={handlers} />
                                </div>
                            </td>
                        </tr>
                    );
                })}
            </tbody>
        </table>
    );
}
