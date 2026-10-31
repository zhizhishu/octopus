'use client';

import { Pencil, Wallet } from 'lucide-react';
import { useTranslations } from 'next-intl';
import type { User, UserAccessSummary } from '@/api/endpoints/user';
import type { AccessPlan } from '@/api/endpoints/access-plan';
import type { UserUsageSummary } from '@/api/endpoints/stats';
import { SafeText } from '@/components/common/SafeText';
import { cn } from '@/lib/utils';
import { formatUSD } from '../lib/balance-preview';
import { formatCount } from '../lib/format';
import { CardCell, planLabel, userPlans } from './shared';
import { DataTableRowActions, type UserRowHandlers } from './data-table-row-actions';
import { Checkbox, HelpTip, Pill, iconBtn } from './ui-bits';
import { AccessCell } from './user-access';

const th = 'h-10 px-3 text-left align-middle text-xs font-medium whitespace-nowrap text-muted-foreground';
const td = 'px-3 py-3 align-middle';

export function UsersTable({ users, plans, usage, access, nowSec, selected, onSelect, onSelectPage, handlers }: {
    users: User[];
    plans: AccessPlan[];
    usage: Map<number, UserUsageSummary>;
    access: Map<number, UserAccessSummary>;
    nowSec: number;
    selected: Set<number>;
    onSelect: (id: number, on: boolean) => void;
    onSelectPage: (on: boolean) => void;
    handlers: UserRowHandlers;
}) {
    const t = useTranslations('user');
    const enabledPlanCount = plans.filter((p) => p.enabled).length;
    const pageSelected = users.filter((u) => selected.has(u.id)).length;
    return (
        <table className="w-full text-sm">
            <thead>
                <tr className="border-b border-border/70">
                    <th className={cn(th, 'w-10 pr-0')}>
                        <Checkbox
                            checked={users.length > 0 && pageSelected === users.length}
                            indeterminate={pageSelected > 0 && pageSelected < users.length}
                            onChange={(on) => onSelectPage(on)}
                            label={t('table.selectPage')}
                        />
                    </th>
                    <th className={th}>{t('table.user')}</th>
                    <th className={th}>{t('table.status')}</th>
                    <th className={th}><span className="inline-flex items-center gap-1">{t('table.balance')}<HelpTip content={t('table.balanceTip')} /></span></th>
                    <th className={th}><span className="inline-flex items-center gap-1">{t('table.card')}<HelpTip content={t('table.cardTip')} /></span></th>
                    <th className={th}>{t('table.plans')}</th>
                    <th className={th}><span className="inline-flex items-center gap-1">{t('table.usage')}<HelpTip content={t('table.usageTip')} /></span></th>
                    <th className={th}><span className="inline-flex items-center gap-1">{t('table.lastRelay')}<HelpTip content={t('table.lastRelayTip')} align="end" /></span></th>
                    <th className={cn(th, 'w-28')}><span className="sr-only">{t('table.actions')}</span></th>
                </tr>
            </thead>
            <tbody>
                {users.map((u) => {
                    const dim = u.status !== 'active' ? 'opacity-60' : '';
                    const isSel = selected.has(u.id);
                    const used = usage.get(u.id);
                    const ups = userPlans(u, plans);
                    return (
                        <tr key={u.id} data-user-row={u.id} className={cn('border-b border-border/70 transition-colors last:border-b-0 hover:bg-muted/30', isSel && 'bg-primary/5')}>
                            <td className={cn(td, 'w-10 pr-0')}>
                                <Checkbox checked={isSel} onChange={(on) => onSelect(u.id, on)} label={t('table.selectRow', { name: u.username })} />
                            </td>
                            <td className={cn(td, dim, 'max-w-[15rem]')}>
                                <div className="flex min-w-0 items-center gap-1.5">
                                    <button type="button" onClick={() => handlers.onEdit(u)} className="min-w-0 truncate font-medium hover:underline">
                                        <SafeText value={u.username} />
                                    </button>
                                    {u.role === 'admin' && <Pill tone="pri">{t('role.admin')}</Pill>}
                                    {u.note?.trim() && <Pill className="max-w-[5rem] font-normal text-muted-foreground" title={u.note}><span className="truncate">{u.note}</span></Pill>}
                                </div>
                                <div className="mt-0.5 truncate text-xs text-muted-foreground">{u.email?.trim() || t('table.noEmail')}</div>
                            </td>
                            <td className={cn(td, dim)}>
                                {u.status === 'active' ? <Pill tone="ok">{t('status.active')}</Pill> : <Pill tone="off">{t('status.disabled')}</Pill>}
                            </td>
                            <td className={cn(td, dim, 'tabular-nums whitespace-nowrap')}>
                                {u.role === 'admin'
                                    ? <span className="text-muted-foreground">{t('table.notCharged')}</span>
                                    : <span className="font-medium">{formatUSD(u.balance)}</span>}
                            </td>
                            <td className={cn(td, dim)}><CardCell user={u} nowSec={nowSec} /></td>
                            <td className={cn(td, dim)}>
                                <div className="flex max-w-[9rem] flex-wrap gap-1">
                                    {ups.length === 0 ? <span className="text-xs text-muted-foreground">—</span> : ups.length > 1 && ups.length >= enabledPlanCount ? (
                                        <Pill className="font-normal" title={ups.map(planLabel).join(' · ')}>{t('table.allPlans')}</Pill>
                                    ) : ups.map((p, i) => (
                                        <Pill key={p.id} className={cn('font-normal', i > 0 && 'text-muted-foreground')} title={p.slug}>
                                            <span className="truncate">{planLabel(p)}</span>
                                        </Pill>
                                    ))}
                                </div>
                            </td>
                            <td className={cn(td, dim, 'text-xs tabular-nums whitespace-nowrap')}>
                                <div className="font-medium text-foreground">{formatUSD(used?.total_cost ?? 0)}</div>
                                <div className="mt-0.5 text-muted-foreground">{t('table.times', { n: formatCount((used?.request_success ?? 0) + (used?.request_failed ?? 0)) })}</div>
                            </td>
                            <td className={cn(td, dim, 'text-xs')}>
                                <AccessCell user={u} summary={access.get(u.id)} nowSec={nowSec} onOpen={() => handlers.onEdit(u, 'access')} />
                            </td>
                            <td className={cn(td, 'w-28 pl-0')}>
                                <div className="flex items-center justify-end gap-0.5">
                                    <button type="button" className={iconBtn} aria-label={t('table.edit')} title={t('table.edit')} onClick={() => handlers.onEdit(u)}>
                                        <Pencil className="size-4" />
                                    </button>
                                    <button type="button" className={iconBtn} aria-label={t('table.adjustBalance')} title={t('table.adjustBalance')} onClick={() => handlers.onBalance(u)}>
                                        <Wallet className="size-4" />
                                    </button>
                                    <DataTableRowActions user={u} handlers={handlers} />
                                </div>
                            </td>
                        </tr>
                    );
                })}
            </tbody>
        </table>
    );
}
