'use client';

import type { ReactNode } from 'react';
import { KeyRound, Loader2, MonitorSmartphone, TriangleAlert } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { useUserAccessDetail, type User, type UserAccessAlert, type UserAccessGroup, type UserAccessSummary } from '@/api/endpoints/user';
import { SafeText, MonoSafeText } from '@/components/common/SafeText';
import { accessBadge, clientLabel, lastAccess, shortList, windowParts, type AccessBadgeKind } from '../lib/access';
import { formatCount, nowUnix } from '../lib/format';
import { formatDateShort, useRelTime } from './shared';
import { HelpTip, Pill, Tip } from './ui-bits';

function useWindowText() {
    const t = useTranslations('user.access.window');
    return (sec: number) => {
        const w = windowParts(sec);
        return w.unit === 'hours' ? t('hours', { n: w.n }) : t('minutes', { n: w.n });
    };
}

function useClientText() {
    const t = useTranslations('user.access');
    return (ua: string | undefined) => clientLabel(ua) ?? t('noUA');
}

function timeOfDay(ts: number) {
    return new Date(ts * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}

/** 一条提示的一句话（tooltip 和抽屉共用）。 */
function useAlertSentence() {
    const t = useTranslations('user.access.alert');
    const win = useWindowText();
    const client = useClientText();
    return (a: UserAccessAlert) => {
        const join = (vals: string[], map: (v: string) => string) => {
            const { shown, rest } = shortList(vals);
            const text = shown.map(map).join('、');
            return rest > 0 ? t('more', { list: text, n: rest }) : text;
        };
        const others = a.others ?? [];
        switch (a.kind) {
            case 'many_ips':
                return t('manyIPs', { window: win(a.window_sec), n: a.count, list: join(others, (v) => v) });
            case 'new_ip':
                return t('newIP', { time: timeOfDay(a.at), value: a.value ?? '', window: win(a.window_sec), others: join(others, (v) => v) });
            case 'new_client':
                return t('newClient', { time: timeOfDay(a.at), value: client(a.value), window: win(a.window_sec), others: join(others, (v) => client(v)) });
        }
    };
}

function AlertTipBody({ alerts }: { alerts: UserAccessAlert[] }) {
    const t = useTranslations('user.access.alert');
    const sentence = useAlertSentence();
    return (
        <span className="block space-y-1">
            {alerts.map((a, i) => <span key={i} className="block text-foreground">{sentence(a)}</span>)}
            <span className="block">{t('hint')}</span>
        </span>
    );
}

/** 列表里的警告徽章：悬停 / 聚焦看详情；点一下（手机上）打开抽屉的「IP 与客户端」。 */
export function AccessAlertBadge({ alerts, onOpen, align = 'start', className }: {
    alerts: UserAccessAlert[] | undefined;
    onOpen?: () => void;
    align?: 'start' | 'end' | 'center';
    className?: string;
}) {
    const t = useTranslations('user.access.badge');
    const kind = accessBadge(alerts);
    if (!kind || !alerts) return null;
    return (
        <Tip content={<AlertTipBody alerts={alerts} />} align={align} className={className}>
            <button
                type="button"
                data-access-alert={kind}
                onClick={(e) => { e.stopPropagation(); onOpen?.(); }}
                className="inline-flex h-5 shrink-0 items-center gap-1 rounded-full bg-amber-500/15 px-2 text-[11px] font-medium whitespace-nowrap text-amber-700 outline-none focus-visible:ring-2 focus-visible:ring-amber-500/40 dark:text-amber-400"
            >
                <TriangleAlert className="size-3" />
                {t(kind satisfies AccessBadgeKind)}
            </button>
        </Tip>
    );
}

/** 表格「最近调用」格：多久前 + 徽章；IP；客户端。 */
export function AccessCell({ user, summary, nowSec, onOpen }: { user: User; summary?: UserAccessSummary; nowSec: number; onOpen: () => void }) {
    const t = useTranslations('user');
    const rel = useRelTime();
    const client = useClientText();
    const last = lastAccess(user, summary);
    if (!last) return <span className="text-muted-foreground">{t('table.neverUsed')}</span>;
    return (
        <div className="min-w-0 max-w-[13rem]">
            <div className="flex items-center gap-1.5">
                <span className="whitespace-nowrap">{rel(last.at, nowSec)}</span>
                <AccessAlertBadge alerts={summary?.alerts} onOpen={onOpen} align="end" />
            </div>
            {last.ip && <MonoSafeText value={last.ip} className="mt-0.5 block text-[11px] text-muted-foreground" />}
            {last.userAgent !== undefined && (
                <SafeText value={client(last.userAgent)} title={last.userAgent} className="mt-0.5 block text-[11px] text-muted-foreground" />
            )}
        </div>
    );
}

/** 手机卡片底下那一行：IP · 客户端，有异常时右边一个徽章。 */
export function AccessLine({ user, summary, onOpen }: { user: User; summary?: UserAccessSummary; onOpen: () => void }) {
    const client = useClientText();
    const last = lastAccess(user, summary);
    if (!last || (!last.ip && last.userAgent === undefined)) return null;
    return (
        <div className="mt-1.5 flex min-w-0 items-center gap-2 text-xs text-muted-foreground">
            <MonitorSmartphone className="size-3.5 shrink-0" />
            <span className="flex min-w-0 flex-1 items-center gap-1.5">
                {last.ip && <span className="shrink-0 font-mono text-[11px]">{last.ip}</span>}
                {last.ip && last.userAgent !== undefined && <span aria-hidden>·</span>}
                {last.userAgent !== undefined && <SafeText value={client(last.userAgent)} title={last.userAgent} className="min-w-0" />}
            </span>
            <AccessAlertBadge alerts={summary?.alerts} onOpen={onOpen} align="end" className="shrink-0" />
        </div>
    );
}

function Section({ title, tip, extra, children }: { title: ReactNode; tip?: string; extra?: ReactNode; children: ReactNode }) {
    return (
        <section>
            <div className="mb-2 flex items-center justify-between gap-2">
                <h4 className="flex items-center gap-1 text-sm font-semibold">{title}{tip && <HelpTip content={tip} />}</h4>
                {extra && <span className="text-xs text-muted-foreground">{extra}</span>}
            </div>
            {children}
        </section>
    );
}

function KeysLine({ group }: { group: UserAccessGroup }) {
    const t = useTranslations('user.access');
    const names = group.keys.map((k) => k.api_key_name || (k.api_key_id > 0 ? t('keyDeleted', { id: k.api_key_id }) : t('noKey')));
    const { shown, rest } = shortList(names, 2);
    return (
        <span className="inline-flex min-w-0 items-center gap-1">
            <KeyRound className="size-3 shrink-0" />
            <SafeText value={rest > 0 ? t('alert.more', { list: shown.join('、'), n: rest }) : shown.join('、')} title={names.join('、')} className="min-w-0" />
        </span>
    );
}

function GroupRow({ main, mono, sub, group, isNew, nowSec }: { main: string; mono?: boolean; sub: string; group: UserAccessGroup; isNew: boolean; nowSec: number }) {
    const t = useTranslations('user.access');
    const rel = useRelTime();
    return (
        <li className="px-3 py-2.5">
            <div className="flex min-w-0 items-center gap-2">
                {mono ? <MonoSafeText value={main} className="min-w-0 text-sm" /> : <SafeText value={main} title={group.value} className="min-w-0 text-sm font-medium" />}
                {isNew && <Pill tone="warn" className="px-1.5 py-0 text-[11px]" title={t('newPillTip')}>{t('newPill')}</Pill>}
                <span className="ml-auto shrink-0 text-xs tabular-nums text-muted-foreground">{t('times', { n: formatCount(group.requests) })}</span>
            </div>
            <div className="mt-1 flex min-w-0 flex-wrap items-center gap-x-3 gap-y-0.5 text-[11px] text-muted-foreground">
                <span className="shrink-0">{t('lastSeen', { when: rel(group.last_seen, nowSec) })}</span>
                <span className="shrink-0">{t('firstSeen', { date: formatDateShort(group.first_seen) })}</span>
                <KeysLine group={group} />
            </div>
            {sub && <SafeText value={sub} className="mt-0.5 block text-[11px] text-muted-foreground" />}
        </li>
    );
}

/** 抽屉「IP 与客户端」页。 */
export function UserAccessTab({ user }: { user: User }) {
    const t = useTranslations('user.access');
    const { data, isLoading } = useUserAccessDetail(user.id);
    const sentence = useAlertSentence();
    const win = useWindowText();
    const client = useClientText();
    const rel = useRelTime();
    const nowSec = nowUnix();

    if (isLoading || !data) {
        return <div className="flex justify-center py-16 text-muted-foreground"><Loader2 className="size-5 animate-spin" /></div>;
    }
    const ruleTip = t('ruleTip', { window: win(data.window_sec), limit: data.many_ips_limit });
    if (!data.last) {
        return (
            <div className="space-y-3 py-10 text-center text-sm text-muted-foreground">
                <p>{t('empty')}</p>
                <p className="text-xs">{t('note', { days: data.keep_days })}</p>
            </div>
        );
    }
    const last = data.last;
    // 「新」只标提示里点名的那个 IP / 客户端，与上面的警告一一对应。
    const flagged = new Set(data.alerts.filter((a) => a.kind !== 'many_ips' && a.value !== undefined).map((a) => `${a.kind}:${a.value}`));
    const joinList = (vals: string[] | undefined, map: (v: string) => string) => {
        const { shown, rest } = shortList((vals ?? []).map(map), 3);
        return rest > 0 ? t('alert.more', { list: shown.join('、'), n: rest }) : shown.join('、');
    };

    return (
        <div className="space-y-5">
            {data.alerts.length > 0 && (
                <div data-access-alerts className="rounded-2xl border border-amber-500/30 bg-amber-500/10 px-4 py-3 text-sm">
                    <div className="flex items-center gap-1.5 font-semibold text-amber-700 dark:text-amber-400">
                        <TriangleAlert className="size-4" />{t('alertTitle')}<HelpTip content={ruleTip} />
                    </div>
                    <ul className="mt-1.5 space-y-1 text-foreground">
                        {data.alerts.map((a, i) => <li key={i} className="break-words [overflow-wrap:anywhere]">{sentence(a)}</li>)}
                    </ul>
                    <p className="mt-1.5 text-xs text-muted-foreground">{t('alert.hint')}</p>
                </div>
            )}

            <div className="rounded-2xl border border-border/70 bg-muted/20 px-4 py-3">
                <div className="flex items-center justify-between gap-2 text-xs text-muted-foreground">
                    <span>{t('last')}</span>
                    <span>{rel(last.at, nowSec)}</span>
                </div>
                <dl className="mt-2 grid grid-cols-[4.5rem_1fr] gap-x-3 gap-y-1.5 text-sm">
                    <dt className="text-muted-foreground">{t('ip')}</dt>
                    <dd className="min-w-0"><MonoSafeText value={last.ip} className="block" /></dd>
                    <dt className="text-muted-foreground">{t('client')}</dt>
                    <dd className="min-w-0">
                        <SafeText value={client(last.user_agent)} className="block font-medium" />
                        {last.user_agent && <SafeText value={last.user_agent} mode="wrap" className="block font-mono text-[11px] text-muted-foreground" />}
                    </dd>
                    <dt className="text-muted-foreground">{t('key')}</dt>
                    <dd className="min-w-0"><SafeText value={last.api_key_name || (last.api_key_id > 0 ? t('keyDeleted', { id: last.api_key_id }) : t('noKey'))} className="block" /></dd>
                </dl>
            </div>

            <Section title={t('ips')} tip={ruleTip} extra={t('count', { n: data.ips.length })}>
                <ul className="divide-y divide-border/70 overflow-hidden rounded-2xl border border-border/70">
                    {data.ips.map((g) => (
                        <GroupRow key={g.value} main={g.value} mono group={g} nowSec={nowSec}
                            isNew={flagged.has(`new_ip:${g.value}`)}
                            sub={t('clientsOnIP', { list: joinList(g.values, (v) => client(v)) })} />
                    ))}
                </ul>
            </Section>

            <Section title={t('clients')} extra={t('count', { n: data.clients.length })}>
                <ul className="divide-y divide-border/70 overflow-hidden rounded-2xl border border-border/70">
                    {data.clients.map((g) => (
                        <GroupRow key={g.value} main={client(g.value)} group={g} nowSec={nowSec}
                            isNew={flagged.has(`new_client:${g.value}`)}
                            sub={t('ipsOnClient', { list: joinList(g.values, (v) => v) })} />
                    ))}
                </ul>
            </Section>

            <p className="text-[11px] leading-5 text-muted-foreground">{t('note', { days: data.keep_days })}</p>
        </div>
    );
}
