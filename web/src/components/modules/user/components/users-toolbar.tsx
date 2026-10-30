'use client';

import type { ReactNode } from 'react';
import { Search } from 'lucide-react';
import { useTranslations } from 'next-intl';
import type { AccessPlan } from '@/api/endpoints/access-plan';
import { cn } from '@/lib/utils';
import type { CardState, StatusFilter, UserFilters, UserRoleValue } from '../lib/user-filters';
import { FacetFilter } from './facet-filter';
import { planLabel } from './shared';
import { Seg, chip, chipOn } from './ui-bits';

const CARD_STATES: CardState[] = ['active', 'soon', 'exhausted', 'expired', 'none'];

export function useCardOptions() {
    const t = useTranslations('user.cardFilter');
    return CARD_STATES.map((value) => ({ value, label: t(value) }));
}

// 工具条：全部/启用/停用（带人数）· 搜索 · 角色 / 方案 / 月卡 筛选。
export function UsersToolbar({ filters, setFilters, counts, plans, mobile }: {
    filters: UserFilters;
    setFilters: (f: UserFilters) => void;
    counts: { all: number; active: number; disabled: number };
    plans: AccessPlan[];
    mobile?: boolean;
}) {
    const t = useTranslations('user');
    const cardOptions = useCardOptions();
    const planOptions = plans.map((p) => ({ value: p.id, label: planLabel(p) }));
    const roleOptions: { value: UserRoleValue; label: string }[] = [
        { value: 'user', label: t('role.user') },
        { value: 'admin', label: t('role.admin') },
    ];
    const statusOptions: { value: StatusFilter; label: ReactNode }[] = (['all', 'active', 'disabled'] as const).map((v) => ({
        value: v,
        label: <>{t(`toolbar.${v}`)}<span className="ml-1 text-xs tabular-nums text-muted-foreground">{counts[v]}</span></>,
    }));

    if (mobile) {
        return (
            <div className="-mx-1 flex gap-2 overflow-x-auto px-1 pb-1 [scrollbar-width:none]">
                {(['all', 'active', 'disabled'] as const).map((v) => (
                    <button key={v} type="button" onClick={() => setFilters({ ...filters, status: v })} className={cn(chip, 'h-8 shrink-0 rounded-xl px-2.5 text-[13px]', filters.status === v && chipOn)}>
                        {t(`toolbar.${v}`)} {counts[v]}
                    </button>
                ))}
                <FacetFilter title={t('toolbar.plan')} options={planOptions} selected={filters.planIDs} onChange={(planIDs) => setFilters({ ...filters, planIDs })} className="h-8 shrink-0 rounded-xl px-2.5 text-[13px]" />
                <FacetFilter title={t('toolbar.card')} options={cardOptions} selected={filters.cards} onChange={(cards) => setFilters({ ...filters, cards })} className="h-8 shrink-0 rounded-xl px-2.5 text-[13px]" />
                <FacetFilter title={t('toolbar.role')} options={roleOptions} selected={filters.roles} onChange={(roles) => setFilters({ ...filters, roles })} className="h-8 shrink-0 rounded-xl px-2.5 text-[13px]" />
            </div>
        );
    }

    return (
        <div className="flex flex-wrap items-center gap-2 border-b border-border/70 p-4">
            <Seg value={filters.status} options={statusOptions} onChange={(status) => setFilters({ ...filters, status })} />
            <label className="flex h-9 w-64 items-center gap-2 rounded-xl border border-input bg-background px-3 text-sm focus-within:border-ring">
                <Search className="size-4 shrink-0 text-muted-foreground" />
                <input
                    value={filters.search}
                    onChange={(e) => setFilters({ ...filters, search: e.target.value })}
                    placeholder={t('toolbar.searchPlaceholder')}
                    className="min-w-0 flex-1 bg-transparent outline-none placeholder:text-muted-foreground"
                />
            </label>
            <FacetFilter title={t('toolbar.role')} options={roleOptions} selected={filters.roles} onChange={(roles) => setFilters({ ...filters, roles })} />
            <FacetFilter title={t('toolbar.plan')} options={planOptions} selected={filters.planIDs} onChange={(planIDs) => setFilters({ ...filters, planIDs })} />
            <FacetFilter title={t('toolbar.card')} options={cardOptions} selected={filters.cards} onChange={(cards) => setFilters({ ...filters, cards })} />
        </div>
    );
}
