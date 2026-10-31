'use client';

import { useState } from 'react';
import { ChevronDown, Fingerprint, Globe, KeyRound, Loader2, Lock, ShieldOff } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { useUserClientTelemetry, type UserClientFingerprint, type UserClientKeyInfo } from '@/api/endpoints/user';
import { SafeText, MonoSafeText } from '@/components/common/SafeText';
import { cn } from '@/lib/utils';
import { filterByKey, fpTitle, hasTLSFingerprint, headerNames, isSecretHeader, notableEntries, shortHash } from '../lib/client-fp';
import { formatCount, nowUnix } from '../lib/format';
import { formatDateShort, useRelTime } from './shared';
import { HelpTip, Pill, chip, chipOn } from './ui-bits';

const NOTABLE_PREVIEW = 8;

function useKeyName() {
    const t = useTranslations('user.access');
    return (id: number, name?: string) => name || (id > 0 ? t('keyDeleted', { id }) : t('noKey'));
}

function Row({ label, children, tip }: { label: string; children: React.ReactNode; tip?: string }) {
    return (
        <>
            <dt className="flex items-start gap-1 pt-0.5 text-xs text-muted-foreground">{label}{tip && <HelpTip content={tip} />}</dt>
            <dd className="mb-2 min-w-0 text-sm sm:mb-0">{children}</dd>
        </>
    );
}

function Dash() {
    return <span className="text-muted-foreground">—</span>;
}

function FpCard({ fp, keyName, nowSec, defaultOpen }: { fp: UserClientFingerprint; keyName: string; nowSec: number; defaultOpen: boolean }) {
    const t = useTranslations('user.client');
    const rel = useRelTime();
    const [open, setOpen] = useState(defaultOpen);
    const [allHeaders, setAllHeaders] = useState(false);
    const names = headerNames(fp);
    const notable = notableEntries(fp);
    const title = fpTitle(fp) ?? t('unknown');
    return (
        <li data-client-fp={fp.fp_hash} className="px-3 py-3 sm:px-4">
            <button type="button" onClick={() => setOpen((v) => !v)} aria-expanded={open} className="flex w-full min-w-0 items-start gap-2 text-left outline-none">
                <Fingerprint className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
                <span className="min-w-0 flex-1">
                    <span className="flex min-w-0 items-center gap-2">
                        <SafeText value={title} title={fp.user_agent} className="min-w-0 text-sm font-semibold" />
                        <Pill className="shrink-0 px-1.5 py-0 font-mono text-[11px] font-normal text-muted-foreground" title={fp.fp_hash}>{shortHash(fp.fp_hash)}</Pill>
                        <span className="ml-auto shrink-0 text-xs tabular-nums text-muted-foreground">{t('times', { n: formatCount(fp.requests) })}</span>
                    </span>
                    <span className="mt-1 flex min-w-0 flex-wrap items-center gap-x-3 gap-y-0.5 text-[11px] text-muted-foreground">
                        <span className="shrink-0">{t('lastSeen', { when: rel(fp.last_seen, nowSec) })}</span>
                        {fp.last_ip && <span className="shrink-0 font-mono">{fp.last_ip}</span>}
                        {fp.country && <span className="inline-flex shrink-0 items-center gap-0.5"><Globe className="size-3" />{fp.country}</span>}
                        <span className="inline-flex min-w-0 items-center gap-1"><KeyRound className="size-3 shrink-0" /><SafeText value={keyName} className="min-w-0" /></span>
                    </span>
                </span>
                <ChevronDown className={cn('mt-0.5 size-4 shrink-0 text-muted-foreground transition-transform', open && 'rotate-180')} />
            </button>
            {open && (
                <dl className="mt-3 grid grid-cols-1 gap-y-1 rounded-xl bg-muted/30 px-3 py-3 sm:grid-cols-[7rem_1fr] sm:gap-x-3 sm:gap-y-2">
                    <Row label={t('ua')}>{fp.user_agent ? <SafeText value={fp.user_agent} mode="wrap" className="block font-mono text-xs" /> : <Dash />}</Row>
                    <Row label={t('http')} tip={fp.via_proxy ? t('httpProxyTip') : undefined}>
                        <span className="font-mono text-xs">{fp.http_version || '—'}</span>
                        {fp.via_proxy && <Pill className="ml-2 px-1.5 py-0 text-[11px] font-normal">{t('viaProxy')}</Pill>}
                    </Row>
                    <Row label={t('order')} tip={fp.header_order ? t('orderTip') : t('orderUnknownTip')}>
                        {!fp.header_order && <span className="mb-1 block text-[11px] text-muted-foreground">{t('orderUnknown')}</span>}
                        <ol className="flex flex-wrap gap-1">
                            {names.map((n, i) => (
                                <li key={`${i}-${n}`} className={cn('inline-flex max-w-full items-center gap-1 rounded-md border border-border/70 bg-background px-1.5 py-0.5 font-mono text-[11px]', isSecretHeader(n) && 'text-muted-foreground')} title={isSecretHeader(n) ? t('secretTip') : undefined}>
                                    {fp.header_order && <span className="text-muted-foreground/70">{i + 1}</span>}
                                    {isSecretHeader(n) && <Lock className="size-2.5 shrink-0" />}
                                    <span className="truncate">{n}</span>
                                </li>
                            ))}
                        </ol>
                    </Row>
                    <Row label={t('headers')} tip={t('headersTip')}>
                        {notable.length === 0 ? <Dash /> : (
                            <div className="space-y-1">
                                {(allHeaders ? notable : notable.slice(0, NOTABLE_PREVIEW)).map(([k, v]) => (
                                    <div key={k} className="grid min-w-0 grid-cols-1 gap-x-2 sm:grid-cols-[minmax(0,13rem)_1fr]">
                                        <MonoSafeText value={k} className="block text-[11px] text-muted-foreground" />
                                        <SafeText value={v} mode="wrap" className="block font-mono text-[11px]" />
                                    </div>
                                ))}
                                {notable.length > NOTABLE_PREVIEW && (
                                    <button type="button" className="text-xs font-medium text-primary hover:underline" onClick={() => setAllHeaders((v) => !v)}>
                                        {allHeaders ? t('less') : t('more', { n: notable.length - NOTABLE_PREVIEW })}
                                    </button>
                                )}
                            </div>
                        )}
                    </Row>
                    <Row label="JA3">{fp.ja3 ? <MonoSafeText value={fp.ja3} mode="wrap" className="block text-xs" /> : <Dash />}</Row>
                    <Row label="JA4">{fp.ja4 ? <MonoSafeText value={fp.ja4} mode="wrap" className="block text-xs" /> : <Dash />}</Row>
                    <Row label="JA4H" tip={t('ja4hTip')}>{fp.ja4h ? <MonoSafeText value={fp.ja4h} mode="wrap" className="block text-xs" /> : <Dash />}</Row>
                    <Row label={t('path')}>{fp.last_path ? <MonoSafeText value={fp.last_path} className="block text-xs" /> : <Dash />}</Row>
                    <Row label={t('seen')}>
                        <span className="text-xs">{t('seenRange', { first: formatDateShort(fp.first_seen), last: formatDateShort(fp.last_seen) })}</span>
                    </Row>
                </dl>
            )}
        </li>
    );
}

function KeyChips({ keys, value, onChange, total }: { keys: UserClientKeyInfo[]; value: number; onChange: (id: number) => void; total: number }) {
    const t = useTranslations('user.client');
    const keyName = useKeyName();
    return (
        <div className="-mx-1 flex gap-1.5 overflow-x-auto px-1 pb-1 [scrollbar-width:none]">
            <button type="button" className={cn(chip, value === 0 && chipOn)} onClick={() => onChange(0)}>{t('allKeys')} · {total}</button>
            {keys.map((k) => (
                <button key={k.api_key_id} type="button" className={cn(chip, 'max-w-[14rem]', value === k.api_key_id && chipOn)} onClick={() => onChange(k.api_key_id)}>
                    <KeyRound className="size-3 shrink-0" />
                    <span className="truncate">{keyName(k.api_key_id, k.api_key_name)}</span>
                    <span className="shrink-0 tabular-nums">· {k.fingerprints}</span>
                </button>
            ))}
        </div>
    );
}

/**
 * 「客户端指纹」面板。userId：看一个人（可再按 Key 筛）；只给 apiKeyId：只看这把 Key
 * （比如管理员名下发给别人用的 Key）。
 */
export function ClientTelemetryPanel({ userId, apiKeyId }: { userId?: number; apiKeyId?: number }) {
    const t = useTranslations('user.client');
    const keyName = useKeyName();
    const { data, isLoading } = useUserClientTelemetry({ userId, apiKeyId });
    const [keyFilter, setKeyFilter] = useState(0);
    const nowSec = nowUnix();

    if (isLoading || !data) {
        return <div className="flex justify-center py-16 text-muted-foreground"><Loader2 className="size-5 animate-spin" /></div>;
    }
    const all = data.fingerprints ?? [];
    if (all.length === 0) {
        return (
            <div className="space-y-3 py-10 text-center text-sm text-muted-foreground">
                <p>{t('empty')}</p>
                <p className="text-xs">{t('note', { days: data.keep_days })}</p>
            </div>
        );
    }
    const keys = data.keys ?? [];
    const shown = filterByKey(all, keyFilter);
    const names = new Map(keys.map((k) => [k.api_key_id, keyName(k.api_key_id, k.api_key_name)]));
    return (
        <div className="space-y-4">
            {!hasTLSFingerprint(all) && (
                <div data-client-no-tls className="flex gap-2 rounded-2xl border border-border/70 bg-muted/20 px-3 py-2.5 text-xs leading-5 text-muted-foreground">
                    <ShieldOff className="mt-0.5 size-3.5 shrink-0" />
                    <span>{t('noTLS')}</span>
                </div>
            )}
            {!apiKeyId && keys.length > 1 && <KeyChips keys={keys} value={keyFilter} onChange={setKeyFilter} total={all.length} />}
            <ul className="divide-y divide-border/70 overflow-hidden rounded-2xl border border-border/70">
                {shown.map((fp, i) => (
                    <FpCard key={fp.fp_hash + ':' + fp.api_key_id} fp={fp} nowSec={nowSec} defaultOpen={i === 0}
                        keyName={names.get(fp.api_key_id) ?? keyName(fp.api_key_id)} />
                ))}
            </ul>
            <p className="text-[11px] leading-5 text-muted-foreground">{t('note', { days: data.keep_days })}</p>
        </div>
    );
}
