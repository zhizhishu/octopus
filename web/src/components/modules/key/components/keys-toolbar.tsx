'use client';

import { Search, X } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { cn } from '@/lib/utils';
import { FacetFilter } from '@/components/modules/user/components/facet-filter';
import { Seg } from '@/components/modules/user/components/ui-bits';
import { EMPTY_KEY_FILTERS, KEY_FAMILIES, KEY_STATUS_TABS, type KeyFilters, type KeyStatusTab } from '../lib/key-filters';
import { useFamilyLabel } from './bits';

export interface FacetOption { value: number; label: string }

// 表头上方一行：状态页签（带数字）、搜索、用户 / 方案 / 协议筛选、右边总数。
// 手机上只要状态页签（横向可滑），搜索框在页面顶上。
export function KeysToolbar({ filters, setFilters, counts, owners, plans, mobile }: {
    filters: KeyFilters;
    setFilters: (f: KeyFilters) => void;
    counts: Record<KeyStatusTab, number>;
    owners?: FacetOption[];
    plans?: FacetOption[];
    mobile?: boolean;
}) {
    const t = useTranslations('keys.toolbar');
    const ts = useTranslations('keys.status');
    const famLabel = useFamilyLabel();
    const statusOptions = KEY_STATUS_TABS.map((s) => ({
        value: s,
        label: <span>{s === 'all' ? t('all') : ts(s)}<span className="ml-1 tabular-nums opacity-70">{counts[s]}</span></span>,
    }));
    const dirty = filters.search !== '' || filters.owners.length > 0 || filters.plans.length > 0 || filters.families.length > 0;

    if (mobile) {
        return (
            <div className="-mx-1 overflow-x-auto px-1">
                <Seg value={filters.status} onChange={(status) => setFilters({ ...filters, status })} options={statusOptions} />
            </div>
        );
    }
    return (
        <div className="flex flex-wrap items-center gap-2 border-b border-border px-4 py-3">
            <Seg value={filters.status} onChange={(status) => setFilters({ ...filters, status })} options={statusOptions} />
            <label className="flex h-9 w-60 min-w-0 items-center gap-2 rounded-xl border border-input bg-background px-3 text-sm">
                <Search className="size-4 shrink-0 text-muted-foreground" />
                <input value={filters.search} onChange={(e) => setFilters({ ...filters, search: e.target.value })} placeholder={owners ? t('searchAdmin') : t('search')} className="min-w-0 flex-1 bg-transparent outline-none placeholder:text-muted-foreground" />
            </label>
            {owners && owners.length > 0 && (
                <FacetFilter title={t('owner')} options={owners} selected={filters.owners} onChange={(v) => setFilters({ ...filters, owners: v })} />
            )}
            {plans && plans.length > 0 && (
                <FacetFilter title={t('plan')} options={plans} selected={filters.plans} onChange={(v) => setFilters({ ...filters, plans: v })} />
            )}
            <FacetFilter
                title={t('protocol')}
                options={KEY_FAMILIES.map((f) => ({ value: f, label: famLabel(f) }))}
                selected={filters.families}
                onChange={(v) => setFilters({ ...filters, families: v })}
            />
            {dirty && (
                <button type="button" onClick={() => setFilters({ ...EMPTY_KEY_FILTERS, status: filters.status })} className={cn('inline-flex h-8 items-center gap-1 rounded-lg px-2 text-xs text-muted-foreground hover:bg-muted hover:text-foreground')}>
                    <X className="size-3.5" />{t('reset')}
                </button>
            )}
        </div>
    );
}
