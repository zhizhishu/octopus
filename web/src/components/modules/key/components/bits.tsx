'use client';

import { useTranslations } from 'next-intl';
import type { APIKey } from '@/api/endpoints/apikey';
import { CopyIconButton } from '@/components/common/CopyButton';
import { cn } from '@/lib/utils';
import { formatUSD } from '@/components/modules/user/lib/balance-preview';
import { useRelTime } from '@/components/modules/user/components/shared';
import { Bar, Pill, Tip } from '@/components/modules/user/components/ui-bits';
import { expiryState, formatSpendPct, isAllFamilies, keyFamilies, keyModels, maskKey, spendState, type KeyFamily, type KeyStatus } from '../lib/key-filters';

export const inputCls = 'flex h-9 w-full rounded-xl border border-input bg-background px-3 text-sm outline-none focus:border-ring disabled:opacity-60';

export function useFamilyLabel() {
    const t = useTranslations('keys.family');
    return (f: KeyFamily) => t(f);
}

export function KeyStatusPill({ status }: { status: KeyStatus }) {
    const t = useTranslations('keys.status');
    const tone = status === 'ok' ? 'ok' : status === 'off' ? 'off' : status === 'expired' ? 'warn' : 'bad';
    return <Tip content={t(`${status}Tip`)}><Pill tone={tone}>{t(status)}</Pill></Tip>;
}

/** sk-…Hm4q + 复制（复制的是完整 Key）。 */
export function MaskedKey({ value, className }: { value: string; className?: string }) {
    const t = useTranslations('keys.table');
    return (
        <span className={cn('inline-flex shrink-0 items-center gap-0.5 whitespace-nowrap', className)}>
            <span className="font-mono text-xs text-muted-foreground" title={t('maskedTip')}>{maskKey(value)}</span>
            <CopyIconButton
                text={value}
                className="inline-flex size-6 items-center justify-center rounded-md text-muted-foreground hover:bg-muted hover:text-foreground"
                copyIconClassName="size-3"
                checkIconClassName="size-3"
            />
        </span>
    );
}

/**
 * 已用金额：formatUSD 在 [0.01, 1) 区间只给两位小数，小额花销会读成 $0.00 或丢细节，
 * 这里多留一位（如 $0.135）；其余区间（含 <0.01 的 4 位小数）沿用 formatUSD。
 */
function formatSpentAmount(value: number): string {
    const abs = Math.abs(value);
    if (abs >= 0.01 && abs < 1) return `$${abs.toFixed(3)}`;
    return formatUSD(value);
}

export function SpendCell({ spent, cap, unpriced, className }: { spent: number; cap?: number; unpriced?: boolean; className?: string }) {
    const t = useTranslations('keys.table');
    const s = spendState(spent, cap);
    if (!s.limited) {
        return (
            <div className={className}>
                <div className="text-sm font-medium tabular-nums">{formatSpentAmount(spent)}</div>
                <div className="text-[11px] text-muted-foreground">{t('noCap')}</div>
                {unpriced && <div className="mt-0.5 text-[11px] text-muted-foreground">{t('unpricedNote')}</div>}
            </div>
        );
    }
    // 有花销但占比极小时，进度条至少画 2%，不然看不见条；文字和档位仍用真实 pct。
    const barPct = spent > 0 ? Math.max(s.pct, 2) : s.pct;
    return (
        <Tip content={t('spendTip', { spent: formatUSD(spent), cap: formatUSD(cap ?? 0), left: formatUSD(s.left) })}>
            <div tabIndex={0} className={cn('w-[8.5rem] outline-none', className)}>
                <div className="text-sm tabular-nums"><span className="font-medium">{formatSpentAmount(spent)}</span><span className="text-xs text-muted-foreground"> / {formatUSD(cap ?? 0)}</span><span className="text-xs text-muted-foreground"> · {formatSpendPct(s.pct)}</span></div>
                <Bar value={barPct} tone={s.level === 'over' ? 'danger' : 'primary'} className={cn('mt-1.5', s.level === 'near' && '[&>i]:bg-amber-500')} />
                {unpriced && <div className="mt-0.5 text-[11px] text-muted-foreground">{t('unpricedNote')}</div>}
            </div>
        </Tip>
    );
}

export function ExpiryCell({ expireAt, nowSec }: { expireAt?: number; nowSec: number }) {
    const t = useTranslations('keys.expiry');
    const e = expiryState(expireAt, nowSec);
    const at = expireAt ? new Date(expireAt * 1000).toLocaleString([], { year: 'numeric', month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' }) : '';
    if (e.kind === 'forever') return <span className="text-sm text-muted-foreground">{t('forever')}</span>;
    return (
        <Tip content={t('at', { at })}>
            <span tabIndex={0} className={cn('text-sm whitespace-nowrap outline-none', e.kind === 'past' ? 'text-destructive' : e.soon ? 'text-amber-700 dark:text-amber-400' : '')}>
                {e.kind === 'past' ? t('pastDays', { n: e.days }) : t('leftDays', { n: e.days })}
            </span>
        </Tip>
    );
}

/** 能用：模型（全部 / N 个）+ 协议（全部 / 列出来）。 */
export function AccessCell({ apiKey }: { apiKey: APIKey }) {
    const t = useTranslations('keys.table');
    const famLabel = useFamilyLabel();
    const models = keyModels(apiKey);
    const fams = keyFamilies(apiKey);
    const all = isAllFamilies(fams);
    return (
        <div className="min-w-0">
            {models.length === 0 ? (
                <Tip content={t('allModelsTip')}><span tabIndex={0} className="text-sm outline-none">{t('allModels')}</span></Tip>
            ) : (
                <Tip content={<span className="block max-h-48 overflow-hidden font-mono">{models.slice(0, 12).map((m) => <span key={m} className="block">{m}</span>)}{models.length > 12 && <span className="block">+{models.length - 12}</span>}</span>}>
                    <span tabIndex={0} className="text-sm font-medium outline-none">{t('nModels', { n: models.length })}</span>
                </Tip>
            )}
            <div className="mt-0.5 flex flex-wrap gap-1">
                {all ? <span className="text-[11px] text-muted-foreground">{t('allProtocols')}</span> : fams.map((f) => (
                    <span key={f} className="rounded-md bg-muted px-1.5 text-[11px] leading-5 text-muted-foreground">{famLabel(f)}</span>
                ))}
            </div>
        </div>
    );
}

export function PlanCell({ apiKey }: { apiKey: APIKey }) {
    const plans = apiKey.access_plans ?? [];
    if (plans.length === 0) return <span className="text-muted-foreground">—</span>;
    const def = apiKey.default_access_plan_id || plans[0]?.id;
    return (
        <div className="flex flex-wrap gap-1">
            {plans.map((p) => (
                <Pill key={p.id} tone={p.id === def ? 'pri' : 'outline'} className={cn(!p.enabled && 'opacity-50 line-through')}>
                    {p.display_name?.trim() || p.slug || `#${p.id}`}
                </Pill>
            ))}
        </div>
    );
}

export function LastUsedCell({ apiKey, nowSec }: { apiKey: APIKey; nowSec: number }) {
    const rel = useRelTime();
    const t = useTranslations('keys.table');
    if (!apiKey.last_used_at) return <span className="text-sm text-muted-foreground">{t('neverUsed')}</span>;
    return (
        <Tip content={new Date(apiKey.last_used_at * 1000).toLocaleString()} align="end">
            <div tabIndex={0} className="outline-none">
                <div className="text-sm whitespace-nowrap">{rel(apiKey.last_used_at, nowSec)}</div>
                {apiKey.last_used_ip && <div className="font-mono text-[11px] text-muted-foreground">{apiKey.last_used_ip}</div>}
            </div>
        </Tip>
    );
}
