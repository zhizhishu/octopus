'use client';

import { useTranslations } from 'next-intl';
import type { User } from '@/api/endpoints/user';
import type { AccessPlan } from '@/api/endpoints/access-plan';
import { relativeTime } from '../lib/format';
import { cardState, userPlanIDs } from '../lib/user-filters';
import { formatUSD } from '../lib/balance-preview';
import { Bar, Pill, Tip } from './ui-bits';
import { cn } from '@/lib/utils';

export function planLabel(plan: Pick<AccessPlan, 'id' | 'slug' | 'display_name'>) {
    return plan.display_name?.trim() || plan.slug || `#${plan.id}`;
}

export function userPlans(user: User, plans: AccessPlan[]) {
    return userPlanIDs(user).map((id) => plans.find((p) => p.id === id) ?? user.access_plans?.find((p) => p.id === id) ?? ({ id, slug: `#${id}`, display_name: '' } as AccessPlan));
}

export function useRelTime() {
    const t = useTranslations('user.time');
    return (ts: number | undefined, nowSec: number) => {
        const r = relativeTime(ts, nowSec);
        switch (r.kind) {
            case 'never': return t('never');
            case 'justNow': return t('justNow');
            case 'minutes': return t('minutes', { n: r.n });
            case 'hours': return t('hours', { n: r.n });
            case 'yesterday': return t('yesterday');
            case 'days': return t('days', { n: r.n });
            case 'date': return new Date(r.ts * 1000).toLocaleDateString();
        }
    };
}

export function formatDateShort(ts: number) {
    if (!ts) return '';
    return new Date(ts * 1000).toLocaleDateString();
}

/** 表格「月卡 · 今天」格：$已用 / $每日 + 进度条 + 还剩 N 天；用完变红、快到期变橙；悬停看重置时间和到期日。 */
export function CardCell({ user, nowSec, compact }: { user: User; nowSec: number; compact?: boolean }) {
    const t = useTranslations('user.card');
    if (user.role === 'admin') return <span className="text-muted-foreground">—</span>;
    const state = cardState(user, nowSec);
    if (state === 'none') return <span className="text-muted-foreground">{t('none')}</span>;
    if (state === 'expired') return <Pill tone="off">{t('expired')}</Pill>;
    const limit = Math.max(0, user.monthly_limit);
    const used = Math.max(0, user.monthly_used);
    const remaining = Math.max(0, limit - used);
    const days = user.days_left ?? Math.ceil((user.monthly_expire_at - nowSec) / 86400);
    const resetAt = user.next_reset_at_iso ? new Date(user.next_reset_at_iso) : null;
    const tip = (
        <span className="block">
            <span className="block font-medium text-foreground">{t('tipTitle', { limit: formatUSD(limit) })}</span>
            <span className="block">{t('tipUsed', { used: formatUSD(used), remaining: formatUSD(remaining) })}</span>
            <span className="block">{t('tipReset', {
                reset: resetAt ? resetAt.toLocaleString([], { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' }) : '—',
                expire: formatDateShort(user.monthly_expire_at),
            })}</span>
        </span>
    );
    return (
        <Tip content={tip}>
            <div tabIndex={0} className={cn('outline-none', compact ? 'w-full' : 'w-[8.5rem]')}>
                <div className="text-xs font-medium tabular-nums text-foreground">{formatUSD(used)} / {formatUSD(limit)}</div>
                <Bar value={limit > 0 ? (used / limit) * 100 : 0} tone={state === 'exhausted' ? 'danger' : 'primary'} className="mt-1.5" />
                <div className={cn('mt-1 text-[11px]', state === 'exhausted' ? 'text-destructive' : state === 'soon' ? 'text-amber-700 dark:text-amber-400' : 'text-muted-foreground')}>
                    {state === 'exhausted' ? t('exhausted') : t('daysLeft', { n: days })}
                </div>
            </div>
        </Tip>
    );
}
