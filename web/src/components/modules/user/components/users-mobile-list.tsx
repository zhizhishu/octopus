'use client';

import { useTranslations } from 'next-intl';
import type { User, UserAccessSummary } from '@/api/endpoints/user';
import type { AccessPlan } from '@/api/endpoints/access-plan';
import { SafeText } from '@/components/common/SafeText';
import { cn } from '@/lib/utils';
import { formatUSD } from '../lib/balance-preview';
import { cardState } from '../lib/user-filters';
import { planLabel, userPlans, useRelTime } from './shared';
import { DataTableRowActions, type UserRowHandlers } from './data-table-row-actions';
import { Bar, Pill } from './ui-bits';
import { AccessLine } from './user-access';

// 手机：每人一张卡（名字 / 状态 / ⋯，余额 + 月卡一行，进度条，方案 + 最近用过）。点卡片打开编辑。
export function UsersMobileList({ users, plans, access, nowSec, handlers }: {
    users: User[];
    plans: AccessPlan[];
    access: Map<number, UserAccessSummary>;
    nowSec: number;
    handlers: UserRowHandlers;
}) {
    const t = useTranslations('user');
    const rel = useRelTime();
    return (
        <div className="space-y-2.5">
            {users.map((u) => {
                const state = cardState(u, nowSec);
                const summary = access.get(u.id);
                const lastAt = Math.max(u.last_relay_at || 0, summary?.last.at ?? 0);
                const limit = Math.max(0, u.monthly_limit);
                const used = Math.max(0, u.monthly_used);
                let cardLine: string;
                if (u.role === 'admin') cardLine = t('mobile.adminFree');
                else if (state === 'none') cardLine = t('mobile.cardNone');
                else if (state === 'expired') cardLine = t('mobile.cardExpired');
                else cardLine = t('mobile.cardToday', { used: formatUSD(used), limit: formatUSD(limit) });
                return (
                    <div
                        key={u.id}
                        data-user-card={u.id}
                        role="button"
                        tabIndex={0}
                        onClick={() => handlers.onEdit(u)}
                        onKeyDown={(e) => { if (e.key === 'Enter') handlers.onEdit(u); }}
                        className={cn('rounded-3xl border border-border bg-card px-4 py-3 text-card-foreground', u.status !== 'active' && 'opacity-60')}
                    >
                        <div className="flex min-w-0 items-center gap-2">
                            <SafeText value={u.username} className="min-w-0 font-semibold" />
                            {u.role === 'admin' && <Pill tone="pri">{t('role.admin')}</Pill>}
                            <span className="ml-auto shrink-0">
                                {u.status === 'active' ? <Pill tone="ok">{t('status.active')}</Pill> : <Pill tone="off">{t('status.disabled')}</Pill>}
                            </span>
                            <span onClick={(e) => e.stopPropagation()} className="-mr-2 shrink-0">
                                <DataTableRowActions user={u} handlers={handlers} />
                            </span>
                        </div>
                        <div className="mt-1.5 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
                            <span>{t('mobile.balance')} <b className="font-medium text-foreground">{u.role === 'admin' ? '—' : formatUSD(u.balance)}</b></span>
                            <span>{cardLine}</span>
                        </div>
                        {u.role !== 'admin' && (state === 'active' || state === 'soon' || state === 'exhausted') && (
                            <Bar value={limit > 0 ? (used / limit) * 100 : 0} tone={state === 'exhausted' ? 'danger' : 'primary'} className="mt-2" />
                        )}
                        <div className="mt-2 flex items-center justify-between gap-2 text-xs text-muted-foreground">
                            <span className="min-w-0 truncate">{userPlans(u, plans).map(planLabel).join(' · ') || '—'}</span>
                            <span className="shrink-0">{lastAt > 0 ? t('mobile.usedAt', { when: rel(lastAt, nowSec) }) : t('table.neverUsed')}</span>
                        </div>
                        <AccessLine user={u} summary={summary} onOpen={() => handlers.onEdit(u, 'access')} />
                    </div>
                );
            })}
        </div>
    );
}
