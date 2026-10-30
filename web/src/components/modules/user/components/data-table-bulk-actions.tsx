'use client';

import { useState } from 'react';
import { Ban, CircleCheck, GitBranch, Trash2, Wallet } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { toast } from '@/components/common/Toast';
import { useBatchUsers, type User, type UserBatchAction, type UserBatchResult } from '@/api/endpoints/user';
import type { AccessPlan } from '@/api/endpoints/access-plan';
import { computeBalancePreview } from '../lib/balance-preview';
import { ConfirmDialog } from './confirm-dialog';
import { PlanPicker } from './plan-picker';
import { Checkbox, btn, btnDanger, label } from './ui-bits';

// 勾选后的批量条（new-api 只有空壳，这里补上）：启用 / 停用 / 加余额 / 改方案 / 删除。
// 全部走 POST /user/batch：后端逐个执行，单人的规矩照样生效（最后一个管理员、不能停用自己、有 Key 不能删），
// 谁没成功、为什么，都会报出来。
export function DataTableBulkActions({ selected, users, plans, onClear }: {
    selected: number[];
    users: User[];
    plans: AccessPlan[];
    onClear: () => void;
}) {
    const t = useTranslations('user.bulk');
    const batch = useBatchUsers();
    const [dialog, setDialog] = useState<null | 'balance' | 'plans' | 'delete'>(null);
    const [amount, setAmount] = useState('');
    const [reason, setReason] = useState('');
    const enabled = plans.filter((p) => p.enabled);
    const [planIDs, setPlanIDs] = useState<number[]>([]);
    const [defaultPlanID, setDefaultPlanID] = useState<number | undefined>();
    const nameOf = (id: number) => users.find((u) => u.id === id)?.username ?? `#${id}`;

    const report = (res: UserBatchResult) => {
        if (res.failed.length === 0) toast.success(t('doneAll', { n: res.done.length }));
        else toast.warning(t('donePartly', { done: res.done.length, failed: res.failed.length }), {
            description: res.failed.slice(0, 5).map((f) => `${nameOf(f.id)}：${f.error}`).join('\n'),
        });
        if (res.done.length > 0) onClear();
        setDialog(null);
    };
    const run = (action: UserBatchAction, extra: Record<string, unknown> = {}) => {
        batch.mutate({ ids: selected, action, ...extra }, {
            onSuccess: report,
            onError: (error) => toast.error(t('failed'), { description: (error as { message?: string } | null)?.message }),
        });
    };
    const amountPreview = computeBalancePreview(0, 'add', amount);

    return (
        <>
            <div data-bulk-bar className="flex flex-wrap items-center gap-2 rounded-t-3xl border-b border-border/70 bg-primary/5 p-4">
                <Checkbox checked indeterminate onChange={() => onClear()} label={t('clear')} />
                <span className="text-sm font-medium">{t('selected', { n: selected.length })}</span>
                <span className="mx-1 h-5 w-px bg-border" />
                <button type="button" className={btn} disabled={batch.isPending} onClick={() => run('enable')}><CircleCheck className="size-4" />{t('enable')}</button>
                <button type="button" className={btn} disabled={batch.isPending} onClick={() => run('disable')}><Ban className="size-4" />{t('disable')}</button>
                <button type="button" className={btn} disabled={batch.isPending} onClick={() => setDialog('balance')}><Wallet className="size-4" />{t('addBalance')}</button>
                <button type="button" className={btn} disabled={batch.isPending} onClick={() => setDialog('plans')}><GitBranch className="size-4" />{t('setPlans')}</button>
                <button type="button" className={btnDanger} disabled={batch.isPending} onClick={() => setDialog('delete')}><Trash2 className="size-4" />{t('delete')}</button>
                <button type="button" onClick={onClear} className="ml-auto text-sm text-muted-foreground hover:text-foreground">{t('clear')}</button>
            </div>

            <ConfirmDialog
                open={dialog === 'balance'}
                onOpenChange={(o) => setDialog(o ? 'balance' : null)}
                title={t('addBalanceTitle', { n: selected.length })}
                description={t('addBalanceDesc')}
                confirmLabel={t('confirm')}
                pending={batch.isPending}
                onConfirm={() => amountPreview.ok && run('add_balance', { amount: amountPreview.amount, reason: reason.trim() || undefined })}
            >
                <div className="space-y-3">
                    <div>
                        <div className={label}>{t('amount')}</div>
                        <input autoFocus inputMode="decimal" value={amount} onChange={(e) => setAmount(e.target.value)} placeholder="0.00" className="flex h-9 w-full rounded-xl border border-input bg-background px-3 text-sm outline-none focus:border-ring" />
                        {amount !== '' && !amountPreview.ok && <div className="mt-1 text-xs text-destructive">{t('amountInvalid')}</div>}
                    </div>
                    <div>
                        <div className={label}>{t('reason')}</div>
                        <input value={reason} maxLength={255} onChange={(e) => setReason(e.target.value)} placeholder={t('reasonPlaceholder')} className="flex h-9 w-full rounded-xl border border-input bg-background px-3 text-sm outline-none focus:border-ring" />
                    </div>
                </div>
            </ConfirmDialog>

            <ConfirmDialog
                open={dialog === 'plans'}
                onOpenChange={(o) => setDialog(o ? 'plans' : null)}
                title={t('setPlansTitle', { n: selected.length })}
                description={t('setPlansDesc')}
                confirmLabel={t('confirm')}
                pending={batch.isPending}
                onConfirm={() => planIDs.length > 0 && run('set_plans', { access_plan_ids: planIDs, default_access_plan_id: defaultPlanID })}
            >
                <PlanPicker plans={enabled} value={planIDs} defaultID={defaultPlanID} onChange={(ids, def) => { setPlanIDs(ids); setDefaultPlanID(def); }} />
            </ConfirmDialog>

            <ConfirmDialog
                open={dialog === 'delete'}
                onOpenChange={(o) => setDialog(o ? 'delete' : null)}
                title={t('deleteTitle', { n: selected.length })}
                description={t('deleteDesc')}
                confirmLabel={t('delete')}
                danger
                pending={batch.isPending}
                onConfirm={() => run('delete')}
            />
        </>
    );
}
