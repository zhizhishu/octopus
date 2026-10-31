'use client';

import { useState } from 'react';
import { Ban, CircleCheck, Trash2 } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { toast } from '@/components/common/Toast';
import { useBatchAPIKeys, type APIKey, type APIKeyBatchAction } from '@/api/endpoints/apikey';
import type { UserBatchResult } from '@/api/endpoints/user';
import { ConfirmDialog } from '@/components/modules/user/components/confirm-dialog';
import { Checkbox, btn, btnDanger } from '@/components/modules/user/components/ui-bits';

// 勾选后的批量条：启用 / 停用 / 删除，走 POST /apikey/batch（逐把执行，谁没成功、为什么都会报出来）。
export function KeysBulkActions({ selected, keys, onClear }: { selected: number[]; keys: APIKey[]; onClear: () => void }) {
    const t = useTranslations('keys.bulk');
    const batch = useBatchAPIKeys();
    const [confirmDelete, setConfirmDelete] = useState(false);
    const nameOf = (id: number) => keys.find((k) => k.id === id)?.name ?? `#${id}`;

    const report = (res: UserBatchResult) => {
        if (res.failed.length === 0) toast.success(t('doneAll', { n: res.done.length }));
        else toast.warning(t('donePartly', { done: res.done.length, failed: res.failed.length }), {
            description: res.failed.slice(0, 5).map((f) => `${nameOf(f.id)}：${f.error}`).join('\n'),
        });
        if (res.done.length > 0) onClear();
        setConfirmDelete(false);
    };
    const run = (action: APIKeyBatchAction) => batch.mutate({ ids: selected, action }, {
        onSuccess: report,
        onError: (error) => toast.error(t('failed'), { description: (error as { message?: string } | null)?.message }),
    });

    return (
        <>
            <div data-bulk-bar className="flex flex-wrap items-center gap-2 rounded-t-3xl border-b border-border/70 bg-primary/5 p-4">
                <Checkbox checked indeterminate onChange={() => onClear()} label={t('clear')} />
                <span className="text-sm font-medium">{t('selected', { n: selected.length })}</span>
                <span className="mx-1 h-5 w-px bg-border" />
                <button type="button" className={btn} disabled={batch.isPending} onClick={() => run('enable')}><CircleCheck className="size-4" />{t('enable')}</button>
                <button type="button" className={btn} disabled={batch.isPending} onClick={() => run('disable')}><Ban className="size-4" />{t('disable')}</button>
                <button type="button" className={btnDanger} disabled={batch.isPending} onClick={() => setConfirmDelete(true)}><Trash2 className="size-4" />{t('delete')}</button>
                <button type="button" onClick={onClear} className="ml-auto text-sm text-muted-foreground hover:text-foreground">{t('clear')}</button>
            </div>
            <ConfirmDialog
                open={confirmDelete}
                onOpenChange={setConfirmDelete}
                title={t('deleteTitle', { n: selected.length })}
                description={t('deleteDesc')}
                confirmLabel={t('deleteConfirm')}
                danger
                pending={batch.isPending}
                onConfirm={() => run('delete')}
            />
        </>
    );
}
