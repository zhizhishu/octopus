'use client';

import { useState, type ReactNode } from 'react';
import { Loader2 } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { toast } from '@/components/common/Toast';
import { useAdjustUserBalance, type User } from '@/api/endpoints/user';
import { cn } from '@/lib/utils';
import { computeBalancePreview, formatUSD, type BalanceMode } from '../lib/balance-preview';
import { HelpTip, Seg, btn, btnPrimary, chip, label } from './ui-bits';

const QUICK = [5, 10, 50, 100];

// 调整余额：加 / 减 / 直接改成 + 金额 + 一行预览 + 原因（记进余额记录）。
// 加和减在后端是一条 balance = balance ± ? 的 UPDATE，不会盖掉同时进行的扣费。
export function UserQuotaDialog({ user, open, onOpenChange }: { user: User | null; open: boolean; onOpenChange: (open: boolean) => void }) {
    const t = useTranslations('user.balanceDialog');
    const adjust = useAdjustUserBalance();
    const [mode, setMode] = useState<BalanceMode>('add');
    const [amount, setAmount] = useState('');
    const [reason, setReason] = useState('');
    const current = user?.balance ?? 0;
    const preview = computeBalancePreview(current, mode, amount);

    const close = (next: boolean) => {
        if (!next) { setAmount(''); setReason(''); setMode('add'); }
        onOpenChange(next);
    };

    const submit = () => {
        if (!user || !preview.ok) return;
        adjust.mutate({ id: user.id, mode, amount: preview.amount, reason: reason.trim() || undefined }, {
            onSuccess: (res) => {
                toast.success(t('done', { balance: formatUSD(res.user.balance) }));
                close(false);
            },
            onError: (error) => toast.error(t('failed'), { description: (error as { message?: string } | null)?.message }),
        });
    };

    const sign = mode === 'add' ? '+' : '-';
    let previewLine: ReactNode;
    if (preview.error === 'not_enough') previewLine = <span className="text-destructive">{t('notEnough', { current: formatUSD(current) })}</span>;
    else if (preview.error === 'invalid' || preview.error === 'not_positive' || preview.error === 'negative') previewLine = <span className="text-destructive">{t('invalid')}</span>;
    else if (!preview.ok) previewLine = <span className="text-muted-foreground">{t('enterAmount')}</span>;
    else if (mode === 'set') previewLine = <><span className="text-muted-foreground">{t('after')}</span> <span className="tabular-nums">{formatUSD(current)} →</span> <b className="font-semibold tabular-nums">{formatUSD(preview.next)}</b></>;
    else previewLine = <><span className="text-muted-foreground">{t('after')}</span> <span className="tabular-nums">{formatUSD(current)} {sign} {formatUSD(preview.amount)} =</span> <b className="font-semibold tabular-nums">{formatUSD(preview.next)}</b></>;

    return (
        <Dialog open={open} onOpenChange={close}>
            <DialogContent className="sm:max-w-[440px]">
                <DialogHeader>
                    <DialogTitle>{t('title')}</DialogTitle>
                    <DialogDescription>{t('subtitle', { name: user?.username ?? '', balance: formatUSD(current) })}</DialogDescription>
                </DialogHeader>
                <div className="space-y-4">
                    <div>
                        <div className={label}>{t('how')}<HelpTip content={t('howTip')} /></div>
                        <Seg
                            value={mode}
                            onChange={setMode}
                            className="w-full"
                            itemClassName="flex-1"
                            options={[
                                { value: 'add', label: t('add') },
                                { value: 'subtract', label: t('subtract') },
                                { value: 'set', label: t('set') },
                            ]}
                        />
                    </div>
                    <div>
                        <div className={label}>{t('amount')}</div>
                        <input
                            autoFocus
                            inputMode="decimal"
                            value={amount}
                            onChange={(e) => setAmount(e.target.value)}
                            onKeyDown={(e) => { if (e.key === 'Enter') submit(); }}
                            placeholder="0.00"
                            className="flex h-9 w-full rounded-xl border border-input bg-background px-3 text-base outline-none focus:border-ring"
                        />
                        {mode !== 'set' && (
                            <div className="mt-2 flex gap-2">
                                {QUICK.map((n) => (
                                    <button key={n} type="button" className={chip} onClick={() => setAmount(String(n))}>{sign}{n}</button>
                                ))}
                            </div>
                        )}
                    </div>
                    <div data-balance-preview className={cn('rounded-xl bg-muted/50 px-3 py-2.5 text-sm')}>{previewLine}</div>
                    <div>
                        <div className={label}>{t('reason')}<HelpTip content={t('reasonTip')} /></div>
                        <input
                            value={reason}
                            maxLength={255}
                            onChange={(e) => setReason(e.target.value)}
                            placeholder={t('reasonPlaceholder')}
                            className="flex h-9 w-full rounded-xl border border-input bg-background px-3 text-sm outline-none focus:border-ring"
                        />
                    </div>
                </div>
                <DialogFooter>
                    <button type="button" className={btn} onClick={() => close(false)}>{t('cancel')}</button>
                    <button type="button" className={btnPrimary} disabled={!preview.ok || adjust.isPending} onClick={submit}>
                        {adjust.isPending && <Loader2 className="size-4 animate-spin" />}
                        {t('confirm')}
                    </button>
                </DialogFooter>
            </DialogContent>
        </Dialog>
    );
}
