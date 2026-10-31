'use client';

import { useTranslations } from 'next-intl';
import { Pill, Tip } from '@/components/modules/user/components/ui-bits';
import { cn } from '@/lib/utils';
import { guestModels, timeLeft, type GuestKeyLike, type GuestStatus } from '../lib/guest-key';

export function GuestStatusPill({ status }: { status: GuestStatus }) {
    const t = useTranslations('keys.guest.status');
    const tone = status === 'ok' ? 'ok' : status === 'off' ? 'off' : status === 'expired' ? 'warn' : 'bad';
    return <Tip content={t(`${status}Tip`)}><Pill tone={tone}>{t(status)}</Pill></Tip>;
}

/** 到期：还剩 N 天 / 小时 / 分钟（访客 Key 常常只给几小时，按天显示不够细）。 */
export function GuestExpiry({ expireAt, nowSec, className }: { expireAt?: number; nowSec: number; className?: string }) {
    const t = useTranslations('keys.guest');
    if (!expireAt) return <span className="text-sm text-muted-foreground">—</span>;
    const l = timeLeft(expireAt, nowSec);
    const at = new Date(expireAt * 1000).toLocaleString([], { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' });
    const soon = !l.past && (l.unit !== 'days' || l.n < 1);
    return (
        <Tip content={t('at', { at })}>
            <span tabIndex={0} className={cn('text-sm whitespace-nowrap outline-none', l.past ? 'text-destructive' : soon ? 'text-amber-700 dark:text-amber-400' : '', className)}>
                {t(`${l.past ? 'past' : 'left'}.${l.unit}`, { n: l.n })}
            </span>
        </Tip>
    );
}

export function GuestModels({ apiKey }: { apiKey: GuestKeyLike }) {
    const t = useTranslations('keys.guest');
    const models = guestModels(apiKey);
    if (models.length === 1) return <span className="block max-w-[14rem] truncate font-mono text-xs" title={models[0]}>{models[0]}</span>;
    return (
        <Tip content={<span className="block max-h-48 overflow-hidden font-mono">{models.slice(0, 12).map((m) => <span key={m} className="block">{m}</span>)}{models.length > 12 && <span className="block">+{models.length - 12}</span>}</span>}>
            <span tabIndex={0} className="inline-flex min-w-0 items-center gap-1 outline-none">
                <span className="max-w-[9rem] truncate font-mono text-xs">{models[0]}</span>
                <span className="shrink-0 rounded-md bg-muted px-1.5 text-[11px] leading-5 text-muted-foreground">{t('moreModels', { n: models.length - 1 })}</span>
            </span>
        </Tip>
    );
}
