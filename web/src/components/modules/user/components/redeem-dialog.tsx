'use client';

import { useState } from 'react';
import { Loader2, Trash2 } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { Switch } from '@/components/ui/switch';
import { CopyIconButton } from '@/components/common/CopyButton';
import { MonoSafeText } from '@/components/common/SafeText';
import { toast } from '@/components/common/Toast';
import { useDeleteRedeemCode, useGenerateRedeemCode, useRedeemCodeList, useUpdateRedeemCode, type RedeemCodeType } from '@/api/endpoints/redeem';
import { formatUSD } from '../lib/balance-preview';
import { Pill, Seg, btnPrimary, label } from './ui-bits';

const input = 'flex h-9 w-full rounded-xl border border-input bg-background px-3 text-sm outline-none focus:border-ring disabled:opacity-50';

// 兑换码：原来在页面底部的面板，收进顶部按钮打开的弹窗，接口不变（/api/v1/redeem/*）。
export function RedeemDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (open: boolean) => void }) {
    const t = useTranslations('user.redeem');
    const { data: codes = [], isLoading } = useRedeemCodeList();
    const generate = useGenerateRedeemCode();
    const update = useUpdateRedeemCode();
    const remove = useDeleteRedeemCode();
    const [type, setType] = useState<RedeemCodeType>('balance');
    const [count, setCount] = useState(1);
    const [amount, setAmount] = useState(1);
    const [days, setDays] = useState(30);
    const canSubmit = count > 0 && amount > 0 && (type === 'balance' || days > 0);

    const submit = () => {
        if (!canSubmit) return;
        generate.mutate({
            type,
            count: Math.min(500, Math.max(1, Math.floor(count))),
            balance_amount: type === 'balance' ? amount : undefined,
            monthly_limit: type === 'monthly' ? amount : undefined,
            monthly_days: type === 'monthly' ? Math.max(1, Math.floor(days)) : undefined,
        }, {
            onSuccess: () => toast.success(t('generated')),
            onError: (error) => toast.error(t('generateFailed'), { description: (error as { message?: string } | null)?.message }),
        });
    };

    return (
        <Dialog open={open} onOpenChange={onOpenChange}>
            <DialogContent className="sm:max-w-[640px]">
                <DialogHeader>
                    <DialogTitle>{t('title')}</DialogTitle>
                    <DialogDescription>{t('desc')}</DialogDescription>
                </DialogHeader>
                <div className="grid gap-3 sm:grid-cols-[auto_80px_1fr_1fr_auto] sm:items-end">
                    <div>
                        <div className={label}>{t('type')}</div>
                        <Seg value={type} onChange={setType} options={[{ value: 'balance', label: t('typeBalance') }, { value: 'monthly', label: t('typeMonthly') }]} />
                    </div>
                    <label className="block"><div className={label}>{t('count')}</div><input className={input} type="number" min={1} max={500} value={count} onChange={(e) => setCount(Number(e.target.value))} /></label>
                    <label className="block"><div className={label}>{type === 'balance' ? t('amount') : t('dailyLimit')}</div><input className={input} type="number" step="0.01" value={amount} onChange={(e) => setAmount(Number(e.target.value))} /></label>
                    <label className="block"><div className={label}>{t('days')}</div><input className={input} type="number" min={1} value={days} disabled={type !== 'monthly'} onChange={(e) => setDays(Number(e.target.value))} /></label>
                    <button type="button" className={btnPrimary} disabled={generate.isPending || !canSubmit} onClick={submit}>
                        {generate.isPending && <Loader2 className="size-4 animate-spin" />}
                        {t('generate')}
                    </button>
                </div>
                <div className="max-h-80 space-y-2 overflow-y-auto">
                    {isLoading ? (
                        <div className="flex justify-center py-8 text-muted-foreground"><Loader2 className="size-5 animate-spin" /></div>
                    ) : codes.length === 0 ? (
                        <div className="py-8 text-center text-sm text-muted-foreground">{t('empty')}</div>
                    ) : codes.map((code) => (
                        <div key={code.id} className="grid gap-2 rounded-2xl border border-border bg-muted/20 p-3 sm:grid-cols-[1fr_auto_auto_auto] sm:items-center">
                            <div className="min-w-0">
                                <div className="flex items-center gap-2">
                                    <MonoSafeText value={code.code} className="flex-1 text-sm" />
                                    <CopyIconButton text={code.code} className="text-muted-foreground hover:text-foreground" copyIconClassName="size-4" checkIconClassName="size-4" />
                                </div>
                                <div className="mt-1 text-xs text-muted-foreground">
                                    {code.type === 'balance' ? t('lineBalance', { amount: formatUSD(code.balance_amount) }) : t('lineMonthly', { limit: formatUSD(code.monthly_limit), days: code.monthly_days })}
                                    {code.used && ` · ${t('usedBy', { id: code.used_by_user_id })}`}
                                </div>
                            </div>
                            <Pill tone={code.used ? 'off' : code.enabled ? 'ok' : 'bad'}>{code.used ? t('used') : code.enabled ? t('available') : t('off')}</Pill>
                            <Switch checked={code.enabled} disabled={code.used || update.isPending} onCheckedChange={(enabled) => update.mutate({ id: code.id, enabled, note: code.note })} aria-label={t('toggle')} />
                            <button type="button" disabled={code.used || remove.isPending} onClick={() => remove.mutate(code.id)} aria-label={t('delete')} className="inline-flex h-9 items-center justify-center rounded-xl bg-destructive/10 px-3 text-destructive hover:bg-destructive hover:text-destructive-foreground disabled:opacity-40">
                                <Trash2 className="size-4" />
                            </button>
                        </div>
                    ))}
                </div>
            </DialogContent>
        </Dialog>
    );
}
