'use client';

import { CheckCircle2, Gift, Sparkles } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { Button } from '@/components/ui/button';
import { toast } from '@/components/common/Toast';
import { useAuth, useCheckIn, useCheckInStatus } from '@/api/endpoints/user';

function formatAmount(value?: number) {
    if (typeof value !== 'number' || Number.isNaN(value)) return '0';
    return new Intl.NumberFormat(undefined, {
        maximumFractionDigits: 6,
    }).format(value);
}

function errorMessage(error: unknown) {
    if (error && typeof error === 'object' && 'message' in error && typeof error.message === 'string') {
        return error.message;
    }
    return undefined;
}

export function CheckInCard() {
    const t = useTranslations('home.checkIn');
    const { isAuthenticated, isAPIKeyAuth, user } = useAuth();
    const canQuery = isAuthenticated && !isAPIKeyAuth && !!user;
    const { data: status } = useCheckInStatus({ enabled: canQuery });
    const checkIn = useCheckIn();

    if (!canQuery || !status?.enabled) return null;

    const rewardText = status.reward_mode === 'random'
        ? `${formatAmount(status.reward_min)}-${formatAmount(status.reward_max)}`
        : formatAmount(status.reward_amount);

    const handleCheckIn = () => {
        checkIn.mutate(undefined, {
            onSuccess: (data) => {
                toast.success(t('success'), {
                    description: t('successDescription', { amount: formatAmount(data.reward) }),
                });
            },
            onError: (error) => {
                toast.error(t('failed'), {
                    description: errorMessage(error),
                });
            },
        });
    };

    return (
        <section className="rounded-3xl border border-border bg-card p-5 text-card-foreground">
            <div className="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
                <div className="flex min-w-0 items-center gap-4">
                    <div className="flex size-12 shrink-0 items-center justify-center rounded-2xl bg-primary/10 text-primary">
                        {status.checked_today ? <CheckCircle2 className="size-5" /> : <Gift className="size-5" />}
                    </div>
                    <div className="min-w-0 space-y-1">
                        <div className="flex items-center gap-2">
                            <h2 className="text-base font-semibold">{t('title')}</h2>
                            <span className="inline-flex items-center gap-1 rounded-full bg-muted px-2 py-0.5 text-xs text-muted-foreground">
                                <Sparkles className="size-3" />
                                {t('reward', { amount: rewardText })}
                            </span>
                        </div>
                        <p className="text-sm text-muted-foreground">
                            {status.checked_today
                                ? t('checked', { amount: formatAmount(status.last_amount) })
                                : t('available')}
                        </p>
                    </div>
                </div>
                <div className="flex items-center justify-between gap-3 sm:justify-end">
                    <span className="text-sm text-muted-foreground">
                        {t('balance', { amount: formatAmount(status.balance) })}
                    </span>
                    <Button
                        type="button"
                        size="sm"
                        className="rounded-xl"
                        disabled={status.checked_today || checkIn.isPending}
                        onClick={handleCheckIn}
                    >
                        {status.checked_today ? t('done') : (checkIn.isPending ? t('checking') : t('button'))}
                    </Button>
                </div>
            </div>
        </section>
    );
}
