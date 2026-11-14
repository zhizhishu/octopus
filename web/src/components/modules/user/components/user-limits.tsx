'use client';

import { useState } from 'react';
import { useLocale, useTranslations } from 'next-intl';
import { Loader2 } from 'lucide-react';
import {
    useCurrentUserLimits, useSaveUserLimits, useUserLimits,
    type UserLimitPolicy, type UserLimitStatus,
} from '@/api/endpoints/user-limits';
import { useAuthStore } from '@/api/endpoints/user';
import { Switch } from '@/components/ui/switch';
import { toast } from '@/components/common/Toast';
import { formatUSD } from '../lib/balance-preview';
import { btnPrimary, label } from './ui-bits';

const input = 'flex min-w-0 h-9 w-full rounded-xl border border-input bg-background px-3 text-sm outline-none focus:border-ring disabled:opacity-60 max-sm:min-h-11';
const fields = [
    ['five_hour_cost', 'five_hour_requests', 'five_hour'],
    ['weekly_cost', 'weekly_requests', 'week'],
    ['monthly_cost', 'monthly_requests', 'month'],
] as const;
type NumericField = typeof fields[number][0 | 1];

function blankPolicy(userID: number): UserLimitPolicy {
    return {
        user_id: userID, enabled: false, timezone: '', anchor_at: 0,
        five_hour_cost: 0, weekly_cost: 0, monthly_cost: 0,
        five_hour_requests: 0, weekly_requests: 0, monthly_requests: 0,
        allowed_models: '', revision: 0, created_at: 0, updated_at: 0,
    };
}

function numericDraft(policy: UserLimitPolicy): Record<NumericField, string> {
    return {
        five_hour_cost: String(policy.five_hour_cost), weekly_cost: String(policy.weekly_cost),
        monthly_cost: String(policy.monthly_cost), five_hour_requests: String(policy.five_hour_requests),
        weekly_requests: String(policy.weekly_requests), monthly_requests: String(policy.monthly_requests),
    };
}

// Admission-threshold wording, not a billing cap. Rendered for both the ordinary
// user card and the admin editor so neither role reads the limit as a hard cap.
function LimitsExplanation({ policy, data }: { policy: UserLimitPolicy; data: UserLimitStatus }) {
    const t = useTranslations('userLimits');
    return (
        <div className="space-y-2">
            <p className="text-sm font-medium">{t('admissionTitle')}</p>
            <p className="text-xs leading-relaxed">{t('summary')}</p>
            <p className="text-xs leading-relaxed text-muted-foreground">{t('admissionNote')}</p>
            <details className="rounded-xl bg-muted/40 p-3">
                <summary className="cursor-pointer text-xs font-medium max-sm:min-h-11 max-sm:min-w-11">{t('detailTitle')}</summary>
                <ul className="mt-2 list-disc space-y-1 pl-4 text-xs leading-relaxed text-muted-foreground">
                    <li>{t('costNote')}</li>
                    <li>{t('countNote')}</li>
                    <li>{t('fiveHourNote')}</li>
                    <li>{t('calendarNote', { timezone: policy.timezone })}</li>
                    <li>{t('refundNote')}</li>
                    <li>{t('estimateNote')}</li>
                    <li>{t('zeroMeansUnlimited')}</li>
                    <li className="break-all">
                        {policy.allowed_models ? t('modelScope', { models: policy.allowed_models }) : t('modelScopeAll')}
                    </li>
                </ul>
            </details>
        </div>
    );
}

function LimitsUsage({ data }: { data: UserLimitStatus }) {
    const t = useTranslations('userLimits');
    const locale = useLocale();
    const policy = data.policy;
    if (!policy) return <p className="text-sm text-muted-foreground">{t('notConfigured')}</p>;
    const date = (unix: number) => new Intl.DateTimeFormat(locale, {
        dateStyle: 'medium', timeStyle: 'short', timeZone: policy.timezone,
    }).format(new Date(unix * 1000));
    return (
        <section className="space-y-3">
            <div className="flex flex-wrap items-center justify-between gap-2 text-sm">
                <span className="font-medium">{policy.enabled ? t('enabled') : t('disabled')}</span>
                <span className="break-all text-xs text-muted-foreground">{policy.timezone}</span>
            </div>
            {!policy.enabled && <p className="rounded-xl bg-muted p-3 text-xs leading-relaxed">{t('disabledNote')}</p>}
            {data.windows.map((window) => (
                <div key={window.period} className="space-y-2 rounded-2xl border border-border/70 bg-muted/20 p-4">
                    <div className="flex flex-wrap items-baseline justify-between gap-2">
                        <h4 className="text-sm font-semibold">{t(window.period)}</h4>
                        <span className="text-xs text-muted-foreground">{t('resets', { at: date(window.end_at) })}</span>
                    </div>
                    <div className="grid grid-cols-1 gap-3 text-sm sm:grid-cols-2">
                        <div>
                            <div className="text-xs text-muted-foreground">{t('costUsage')}</div>
                            <div className="break-words font-medium">{formatUSD(window.cost_used + window.cost_reserved)} / {window.cost_limit > 0 ? formatUSD(window.cost_limit) : t('unlimited')}</div>
                            <div className="text-xs text-muted-foreground">{t('reserved', { amount: formatUSD(window.cost_reserved) })}</div>
                        </div>
                        <div>
                            <div className="text-xs text-muted-foreground">{t('accepted')}</div>
                            <div className="font-medium">{window.accepted} / {window.request_limit > 0 ? window.request_limit : t('unlimited')}</div>
                            <div className="text-xs text-muted-foreground">{t('failedAccepted', { count: window.failed_accepted })}</div>
                        </div>
                    </div>
                </div>
            ))}
            <p className="text-xs leading-relaxed text-muted-foreground">{t('pending', { count: data.pending_receipts })}</p>
            {!data.realtime_supported && <p className="rounded-xl bg-muted p-3 text-xs leading-relaxed">{t('realtimeNote')}</p>}
            <LimitsExplanation policy={policy} data={data} />
        </section>
    );
}

function LimitsEditor({ userID, initial, data }: { userID: number; initial: UserLimitPolicy; data: UserLimitStatus }) {
    const t = useTranslations('userLimits');
    const [draft, setDraft] = useState(initial);
    const [numbers, setNumbers] = useState(() => numericDraft(initial));
    const save = useSaveUserLimits(userID);
    const savePolicy = async () => {
        const next = { ...draft };
        for (const [cost, count] of fields) {
            const money = Number(numbers[cost]);
            const requests = Number(numbers[count]);
            if (!Number.isFinite(money) || money < 0 || !Number.isSafeInteger(requests) || requests < 0) {
                toast.error(t('invalidLimits'));
                return;
            }
            next[cost] = money;
            next[count] = requests;
        }
        try {
            const result = await save.mutateAsync(next);
            if (result.policy) {
                setDraft(result.policy);
                setNumbers(numericDraft(result.policy));
            }
            toast.success(t('saved'));
        } catch (error) {
            toast.error(t('saveFailed'), { description: (error as { message?: string })?.message });
        }
    };
    return (
        <div className="space-y-5">
            <LimitsUsage data={data} />
            <section className="space-y-4 border-t border-border pt-5">
                <label className="flex items-center gap-3 text-sm font-medium">
                    <Switch checked={draft.enabled} disabled={save.isPending} onCheckedChange={(enabled) => setDraft((value) => ({ ...value, enabled }))} />
                    {t('enable')}
                </label>
                <p className="text-xs leading-relaxed text-muted-foreground">{t('preserveNote')}</p>
                <label className="block">
                    <div className={label}>{t('timezone')}</div>
                    <input className={input} value={draft.timezone} placeholder={t('timezoneDefault')} disabled={save.isPending || data.windows.some((window) => window.accepted > 0)} onChange={(event) => setDraft((value) => ({ ...value, timezone: event.target.value }))} />
                </label>
                {fields.map(([cost, count, period]) => (
                    <div key={period} className="space-y-2">
                        <h4 className="text-sm font-semibold">{t(period)}</h4>
                        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                            <label className="block"><div className={label}>{t('costLimit')}</div><input className={input} type="number" min="0" step="any" value={numbers[cost]} disabled={save.isPending} onChange={(event) => setNumbers((value) => ({ ...value, [cost]: event.target.value }))} /></label>
                            <label className="block"><div className={label}>{t('requestLimit')}</div><input className={input} type="number" min="0" step="1" value={numbers[count]} disabled={save.isPending} onChange={(event) => setNumbers((value) => ({ ...value, [count]: event.target.value }))} /></label>
                        </div>
                    </div>
                ))}
                <label className="block">
                    <div className={label}>{t('models')}</div>
                    <textarea className="min-h-20 w-full resize-y rounded-xl border border-input bg-background p-3 text-sm outline-none focus:border-ring max-sm:min-h-11" value={draft.allowed_models} maxLength={16384} disabled={save.isPending} onChange={(event) => setDraft((value) => ({ ...value, allowed_models: event.target.value }))} />
                    <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{t('modelsNote')}</p>
                </label>
                <p className="text-xs leading-relaxed text-muted-foreground">{t('zeroNote')}</p>
                <button type="button" className={`${btnPrimary} max-sm:min-h-11 max-sm:min-w-11`} disabled={save.isPending} onClick={savePolicy}>
                    {save.isPending && <Loader2 className="size-4 animate-spin" />}{t('save')}
                </button>
            </section>
        </div>
    );
}

export function UserLimitsPanel({ userID }: { userID: number }) {
    const t = useTranslations('userLimits');
    const query = useUserLimits(userID);
    if (query.isLoading) return <div className="flex items-center gap-2 text-sm text-muted-foreground"><Loader2 className="size-4 animate-spin" />{t('loading')}</div>;
    if (query.isError || !query.data) return <p className="text-sm text-destructive">{t('loadFailed')}</p>;
    // Background refetches update usage only. A dirty draft keeps its revision
    // and receives a conflict instead of silently overwriting another edit.
    return <LimitsEditor key={userID} userID={userID} initial={query.data.policy ?? blankPolicy(userID)} data={query.data} />;
}

export function CurrentUserLimits() {
    const t = useTranslations('userLimits');
    const user = useAuthStore((state) => state.user);
    const keyAuth = useAuthStore((state) => state.isAPIKeyAuth);
    const query = useCurrentUserLimits(!!user && !keyAuth);
    if (!user || user.role === 'admin' || !query.data?.policy) return null;
    return (
        <section className="space-y-4 rounded-3xl border border-border bg-card p-5">
            <h3 className="text-base font-semibold">{t('title')}</h3>
            <LimitsUsage data={query.data} />
        </section>
    );
}
