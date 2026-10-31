'use client';

import { useState } from 'react';
import { ArrowRight, Loader2, Logs } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { useGuestKeyAccess, useGuestKeyUsage, type GuestKey } from '@/api/endpoints/guest-key';
import type { UserAccessGroup, UserUsageRange } from '@/api/endpoints/user';
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { MonoSafeText, SafeText } from '@/components/common/SafeText';
import { cn } from '@/lib/utils';
import { formatUSD } from '@/components/modules/user/lib/balance-preview';
import { formatCount } from '@/components/modules/user/lib/format';
import { clientLabel, shortList } from '@/components/modules/user/lib/access';
import { formatDateShort, useRelTime } from '@/components/modules/user/components/shared';
import { UsageOverview, UsageRangeSeg } from '@/components/modules/user/components/user-usage-tab';
import { Bar, Seg } from '@/components/modules/user/components/ui-bits';
import { spendState } from '../lib/key-filters';
import { guestStatus } from '../lib/guest-key';
import { GuestExpiry, GuestStatusPill } from './bits';

type Tab = 'usage' | 'access';

// 一把访客 Key 的用量和来源：用量（同 API Key 抽屉）+ IP / 客户端（同用户页，按这把 Key 取）+ 跳到请求日志。
export function GuestKeyDetail({ open, onOpenChange, apiKey, spent, nowSec, onLogs }: {
    open: boolean;
    onOpenChange: (open: boolean) => void;
    apiKey: GuestKey | null;
    spent: number;
    nowSec: number;
    onLogs: (k: GuestKey) => void;
}) {
    const t = useTranslations('keys.guest.detail');
    const [tab, setTab] = useState<Tab>('usage');
    const [range, setRange] = useState<UserUsageRange>('7d');
    return (
        <Dialog open={open} onOpenChange={onOpenChange}>
            <DialogContent className="flex flex-col gap-0 p-0 sm:max-w-[min(52rem,calc(100vw-2rem))] max-sm:inset-0 max-sm:top-0 max-sm:left-0 max-sm:h-[100dvh] max-sm:max-h-[100dvh] max-sm:w-screen max-sm:max-w-none max-sm:translate-x-0 max-sm:translate-y-0 max-sm:rounded-none max-sm:border-0">
                {apiKey && (
                    <>
                        <DialogHeader className="border-b border-border/70 px-5 pt-5 pb-4 text-left sm:px-6">
                            <DialogTitle className="flex min-w-0 items-center gap-2 pr-6">
                                <SafeText value={apiKey.name} className="min-w-0 truncate" />
                                <GuestStatusPill status={guestStatus(apiKey, spent, nowSec)} />
                            </DialogTitle>
                            <DialogDescription className="flex flex-wrap items-center gap-x-2">
                                {apiKey.note && <SafeText value={apiKey.note} className="min-w-0" />}
                                {apiKey.note && <span>·</span>}
                                <GuestExpiry expireAt={apiKey.expire_at} nowSec={nowSec} className="text-xs" />
                            </DialogDescription>
                            <div className="flex flex-wrap items-center justify-between gap-2 pt-2">
                                <Seg size="sm" value={tab} onChange={setTab} options={[{ value: 'usage', label: t('usage') }, { value: 'access', label: t('access') }]} />
                                {tab === 'usage' && <UsageRangeSeg value={range} onChange={setRange} />}
                            </div>
                        </DialogHeader>
                        <div className="min-h-0 flex-1 overflow-y-auto px-5 py-4 sm:px-6">
                            {tab === 'usage' ? <UsagePane apiKey={apiKey} spent={spent} range={range} /> : <AccessPane apiKey={apiKey} nowSec={nowSec} />}
                        </div>
                        <div className="border-t border-border/70 px-5 py-3 max-sm:pb-[max(0.75rem,env(safe-area-inset-bottom))] sm:px-6">
                            <button type="button" onClick={() => onLogs(apiKey)} className="flex w-full items-center justify-between rounded-2xl bg-muted/40 px-3 py-2.5 text-sm hover:bg-muted/70">
                                <span className="flex items-center gap-2"><Logs className="size-4" />{t('logs')}</span>
                                <ArrowRight className="size-4 text-muted-foreground" />
                            </button>
                        </div>
                    </>
                )}
            </DialogContent>
        </Dialog>
    );
}

function UsagePane({ apiKey, spent, range }: { apiKey: GuestKey; spent: number; range: UserUsageRange }) {
    const t = useTranslations('keys.guest.detail');
    const tu = useTranslations('user.usage');
    const { data, isLoading } = useGuestKeyUsage(apiKey.id, range);
    const s = spendState(spent, apiKey.max_cost);
    return (
        <div className="space-y-4">
            <div className="rounded-2xl border border-border/70 bg-muted/20 px-4 py-3">
                <div className="text-xs text-muted-foreground">{t('total')}</div>
                <div className="mt-1 flex items-baseline gap-1">
                    <span className="text-lg font-semibold tabular-nums">{formatUSD(spent)}</span>
                    {s.limited ? <span className="text-xs text-muted-foreground">/ {formatUSD(apiKey.max_cost ?? 0)}</span> : <span className="text-xs text-muted-foreground">{t('noCap')}</span>}
                </div>
                {s.limited && <Bar value={s.pct} tone={s.level === 'over' ? 'danger' : 'primary'} className="mt-1.5" />}
                <div className="mt-1.5 text-[11px] text-muted-foreground">{t('billing')}</div>
            </div>
            <UsageOverview usage={data} loading={isLoading} range={range} costTip={tu('costTip')} wide />
        </div>
    );
}

function AccessPane({ apiKey, nowSec }: { apiKey: GuestKey; nowSec: number }) {
    const t = useTranslations('keys.guest.detail');
    const ta = useTranslations('user.access');
    const rel = useRelTime();
    const { data, isLoading } = useGuestKeyAccess(apiKey.id);
    if (isLoading || !data) return <div className="flex justify-center py-16 text-muted-foreground"><Loader2 className="size-5 animate-spin" /></div>;
    if (!data.last) {
        return (
            <div className="space-y-2 py-10 text-center text-sm text-muted-foreground">
                <p>{ta('empty')}</p>
                <p className="text-xs">{ta('note', { days: data.keep_days })}</p>
            </div>
        );
    }
    const client = (ua: string) => clientLabel(ua) ?? ta('noUA');
    const last = data.last;
    return (
        <div className="space-y-4">
            <div className="rounded-2xl border border-border/70 bg-muted/20 px-4 py-3">
                <div className="flex items-center justify-between gap-2 text-xs text-muted-foreground">
                    <span>{ta('last')}</span>
                    <span>{rel(last.at, nowSec)} · {t('totalRequests', { n: formatCount(data.total_requests) })}</span>
                </div>
                <dl className="mt-2 grid grid-cols-[4.5rem_1fr] gap-x-3 gap-y-1.5 text-sm">
                    <dt className="text-muted-foreground">{ta('ip')}</dt>
                    <dd className="min-w-0"><MonoSafeText value={last.ip} className="block" /></dd>
                    <dt className="text-muted-foreground">{ta('client')}</dt>
                    <dd className="min-w-0">
                        <SafeText value={client(last.user_agent)} className="block font-medium" />
                        {last.user_agent && <SafeText value={last.user_agent} mode="wrap" className="block font-mono text-[11px] text-muted-foreground" />}
                    </dd>
                </dl>
            </div>
            <div className="grid gap-4 md:grid-cols-2">
                <GroupList title={ta('ips')} count={data.ips.length} groups={data.ips} mono sub={(g) => (g.values?.length ? ta('clientsOnIP', { list: joinShort(g.values.map(client), ta) }) : '')} nowSec={nowSec} />
                <GroupList title={ta('clients')} count={data.clients.length} groups={data.clients} label={client} sub={(g) => (g.values?.length ? ta('ipsOnClient', { list: joinShort(g.values, ta) }) : '')} nowSec={nowSec} />
            </div>
            <p className="text-xs text-muted-foreground">{ta('note', { days: data.keep_days })}</p>
        </div>
    );
}

function joinShort(values: string[], ta: ReturnType<typeof useTranslations>) {
    const { shown, rest } = shortList(values, 3);
    return rest > 0 ? ta('alert.more', { list: shown.join('、'), n: rest }) : shown.join('、');
}

function GroupList({ title, count, groups, mono, label, sub, nowSec }: {
    title: string;
    count: number;
    groups: UserAccessGroup[];
    mono?: boolean;
    label?: (v: string) => string;
    sub: (g: UserAccessGroup) => string;
    nowSec: number;
}) {
    const ta = useTranslations('user.access');
    const rel = useRelTime();
    return (
        <section className="min-w-0 rounded-2xl border border-border/70">
            <div className="flex items-center justify-between border-b border-border/70 px-3 py-2 text-sm font-semibold">
                <span>{title}</span>
                <span className="text-xs font-normal text-muted-foreground">{ta('count', { n: count })}</span>
            </div>
            <ul className="max-h-80 divide-y divide-border/60 overflow-y-auto">
                {groups.map((g) => (
                    <li key={g.value} className="px-3 py-2.5">
                        <div className="flex min-w-0 items-center gap-2">
                            {mono ? <MonoSafeText value={g.value} className="min-w-0 text-sm" /> : <SafeText value={label ? label(g.value) : g.value} title={g.value} className="min-w-0 text-sm font-medium" />}
                            <span className="ml-auto shrink-0 text-xs tabular-nums text-muted-foreground">{ta('times', { n: formatCount(g.requests) })}</span>
                        </div>
                        <div className="mt-1 flex flex-wrap gap-x-3 text-[11px] text-muted-foreground">
                            <span>{ta('lastSeen', { when: rel(g.last_seen, nowSec) })}</span>
                            <span>{ta('firstSeen', { date: formatDateShort(g.first_seen) })}</span>
                        </div>
                        {sub(g) && <SafeText value={sub(g)} className={cn('mt-0.5 block text-[11px] text-muted-foreground')} />}
                    </li>
                ))}
            </ul>
        </section>
    );
}
