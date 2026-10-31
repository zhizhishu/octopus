'use client';

import { useMemo, useState } from 'react';
import { Loader2, Pencil, Plus, Search, UserRoundPlus } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { useCopyToClipboard } from '@uidotdev/usehooks';
import { useDeleteGuestKey, useGuestKeyList, useRevokeGuestKey, useUpdateGuestKey, type GuestKey } from '@/api/endpoints/guest-key';
import { useStatsAPIKey } from '@/api/endpoints/stats';
import { SafeText } from '@/components/common/SafeText';
import { toast } from '@/components/common/Toast';
import { useNavStore } from '@/components/modules/navbar/nav-store';
import { useLogPresetStore } from '@/components/modules/log/log-preset';
import { ConfirmDialog } from '@/components/modules/user/components/confirm-dialog';
import { btnPrimary, iconBtn } from '@/components/modules/user/components/ui-bits';
import { nowUnix } from '@/components/modules/user/lib/format';
import { cn } from '@/lib/utils';
import { guestModels, guestSearch, guestStatus } from '../lib/guest-key';
import { LastUsedCell, MaskedKey, SpendCell } from '../components/bits';
import { GuestExpiry, GuestModels, GuestStatusPill } from './bits';
import { GuestRowActions, type GuestRowHandlers } from './guest-actions';
import { GuestKeyDialog } from './guest-dialog';
import { GuestKeyDetail } from './guest-detail';

const th = 'h-10 px-2.5 text-left align-middle text-xs font-medium whitespace-nowrap text-muted-foreground';
const td = 'px-2.5 py-3 align-middle';

// 「访客」页签：管理员临时发给客人的 Key。只有管理员能看到这个页签（接口也是 AdminOnly）。
// 一张表（手机是卡片），新建 / 编辑走宽对话框，用量和 IP 走另一个对话框。
export function GuestKeys() {
    const t = useTranslations('keys.guest');
    const { data: keys = [], isLoading } = useGuestKeyList();
    const { data: keyStats = [] } = useStatsAPIKey();
    const update = useUpdateGuestKey();
    const revoke = useRevokeGuestKey();
    const del = useDeleteGuestKey();
    const setActiveItem = useNavStore((s) => s.setActiveItem);
    const [, copy] = useCopyToClipboard();
    const [q, setQ] = useState('');
    const [edit, setEdit] = useState<{ open: boolean; id: number | null }>({ open: false, id: null });
    const [detailID, setDetailID] = useState<number | null>(null);
    const [revokeTarget, setRevokeTarget] = useState<GuestKey | null>(null);
    const [deleteTarget, setDeleteTarget] = useState<GuestKey | null>(null);
    const [nowSec] = useState(nowUnix);

    const spent = useMemo(() => new Map(keyStats.map((s) => [s.api_key_id, s.total_cost.raw])), [keyStats]);
    const spentOf = (id: number) => spent.get(id) ?? 0;
    // 能用的在前，作废的沉底；同一类里新的在前。
    const sorted = useMemo(() => [...keys].sort((a, b) => Number((a.revoked_at ?? 0) > 0) - Number((b.revoked_at ?? 0) > 0) || b.id - a.id), [keys]);
    const list = guestSearch(sorted, q);
    const editKey = edit.id ? keys.find((k) => k.id === edit.id) ?? null : null;
    const detailKey = detailID ? keys.find((k) => k.id === detailID) ?? null : null;
    const fail = (e: unknown) => toast.error(t('toast.failed'), { description: (e as { message?: string } | null)?.message });

    const handlers: GuestRowHandlers = {
        onEdit: (k) => setEdit({ open: true, id: k.id }),
        onDetail: (k) => setDetailID(k.id),
        onCopy: async (k) => { await copy(k.api_key); toast.success(t('toast.copied', { name: k.name })); },
        onLogs: (k) => {
            useLogPresetStore.getState().setPreset({ apiKeyId: k.id });
            setDetailID(null);
            setActiveItem('log');
        },
        onToggle: (k) => update.mutate(
            { id: k.id, name: k.name, note: k.note ?? '', models: guestModels(k), expire_at: k.expire_at ?? 0, max_cost: k.max_cost ?? 0, enabled: !k.enabled },
            { onSuccess: () => toast.success(k.enabled ? t('toast.disabled', { name: k.name }) : t('toast.enabled', { name: k.name })), onError: fail },
        ),
        onRevoke: (k) => setRevokeTarget(k),
        onDelete: (k) => setDeleteTarget(k),
    };
    const openCreate = () => setEdit({ open: true, id: null });
    const empty = keys.length === 0 ? (
        <div className="flex flex-col items-center gap-2 py-14 text-center">
            <UserRoundPlus className="size-8 text-muted-foreground/60" />
            <div className="text-sm font-medium">{t('empty')}</div>
            <div className="text-xs text-muted-foreground">{t('emptyHint')}</div>
            <button type="button" className={cn(btnPrimary, 'mt-2')} onClick={openCreate}><Plus className="size-4" />{t('add')}</button>
        </div>
    ) : <div className="py-12 text-center text-sm text-muted-foreground">{t('noMatch')}</div>;

    return (
        <div className="space-y-4" data-guest-keys>
            <div className="flex flex-wrap items-center gap-2 px-1">
                <p className="hidden min-w-0 flex-1 text-sm text-muted-foreground md:block">{t('lead')}</p>
                <label className="flex h-10 min-w-0 flex-1 items-center gap-2 rounded-xl border border-input bg-background px-3 text-sm md:h-9 md:w-72 md:flex-none">
                    <Search className="size-4 shrink-0 text-muted-foreground" />
                    <input value={q} onChange={(e) => setQ(e.target.value)} placeholder={t('search')} className="min-w-0 flex-1 bg-transparent outline-none placeholder:text-muted-foreground" />
                </label>
                <button type="button" className={cn(btnPrimary, 'max-md:size-10 max-md:justify-center max-md:px-0')} onClick={openCreate} aria-label={t('add')}>
                    <Plus className="size-4" /><span className="max-md:hidden">{t('add')}</span>
                </button>
            </div>
            <p className="px-1 text-xs leading-5 text-muted-foreground md:hidden">{t('lead')}</p>

            {isLoading ? (
                <div className="flex justify-center py-16 text-muted-foreground"><Loader2 className="size-5 animate-spin" /></div>
            ) : (
                <>
                    <div className="hidden rounded-3xl border border-border bg-card text-card-foreground md:block">
                        <div className="w-full overflow-x-auto">
                            <table className="w-full text-sm">
                                <thead>
                                    <tr className="border-b border-border/70">
                                        <th className={cn(th, 'pl-4')}>{t('col.name')}</th>
                                        <th className={th}>{t('col.status')}</th>
                                        <th className={th}>{t('col.models')}</th>
                                        <th className={th}>{t('col.expiry')}</th>
                                        <th className={th}>{t('col.spend')}</th>
                                        <th className={th}>{t('col.lastUsed')}</th>
                                        <th className={cn(th, 'w-px')}><span className="sr-only">{t('col.actions')}</span></th>
                                    </tr>
                                </thead>
                                <tbody>
                                    {list.map((k) => {
                                        const status = guestStatus(k, spentOf(k.id), nowSec);
                                        const dim = status === 'off' || status === 'revoked' ? 'opacity-60' : '';
                                        return (
                                            <tr key={k.id} data-guest-row={k.id} className="border-b border-border/70 transition-colors last:border-b-0 hover:bg-muted/30">
                                                <td className={cn(td, dim, 'max-w-[15rem] pl-4')}>
                                                    <button type="button" onClick={() => handlers.onDetail(k)} className="block max-w-full truncate text-left font-medium hover:underline"><SafeText value={k.name} /></button>
                                                    <div className="mt-0.5 flex min-w-0 items-center gap-1 text-xs text-muted-foreground">
                                                        {k.note && <><SafeText value={k.note} className="min-w-0 truncate" /><span>·</span></>}
                                                        {status === 'revoked' ? <span className="font-mono">{t('revokedKey')}</span> : <MaskedKey value={k.api_key} />}
                                                    </div>
                                                </td>
                                                <td className={td}><GuestStatusPill status={status} /></td>
                                                <td className={cn(td, dim)}><GuestModels apiKey={k} /></td>
                                                <td className={cn(td, dim)}><GuestExpiry expireAt={k.expire_at} nowSec={nowSec} /></td>
                                                <td className={cn(td, dim)}><SpendCell spent={spentOf(k.id)} cap={k.max_cost} /></td>
                                                <td className={cn(td, dim)}><LastUsedCell apiKey={k} nowSec={nowSec} /></td>
                                                <td className={cn(td, 'w-px pl-0 pr-3')}>
                                                    <div className="flex items-center justify-end gap-0.5">
                                                        {status !== 'revoked' && <button type="button" className={iconBtn} aria-label={t('menu.edit')} title={t('menu.edit')} onClick={() => handlers.onEdit(k)}><Pencil className="size-4" /></button>}
                                                        <GuestRowActions apiKey={k} handlers={handlers} />
                                                    </div>
                                                </td>
                                            </tr>
                                        );
                                    })}
                                </tbody>
                            </table>
                        </div>
                        {list.length === 0 && empty}
                    </div>
                    <div className="space-y-3 md:hidden">
                        {list.map((k) => {
                            const status = guestStatus(k, spentOf(k.id), nowSec);
                            const cap = 'mb-1 text-[11px] text-muted-foreground';
                            return (
                                <div key={k.id} data-guest-card={k.id} className={cn('rounded-3xl border border-border bg-card p-4', (status === 'off' || status === 'revoked') && 'opacity-70')}>
                                    <div className="flex items-start gap-2">
                                        <button type="button" onClick={() => handlers.onDetail(k)} className="min-w-0 flex-1 text-left">
                                            <div className="flex min-w-0 items-center gap-2">
                                                <SafeText value={k.name} className="min-w-0 truncate font-medium" />
                                                <GuestStatusPill status={status} />
                                            </div>
                                            {k.note && <SafeText value={k.note} className="mt-0.5 block truncate text-xs text-muted-foreground" />}
                                        </button>
                                        {status !== 'revoked' && <button type="button" className={iconBtn} aria-label={t('menu.edit')} onClick={() => handlers.onEdit(k)}><Pencil className="size-4" /></button>}
                                        <GuestRowActions apiKey={k} handlers={handlers} />
                                    </div>
                                    <div className="mt-1 text-xs text-muted-foreground">{status === 'revoked' ? t('revokedKey') : <MaskedKey value={k.api_key} />}</div>
                                    <div className="mt-3 grid grid-cols-2 gap-x-4 gap-y-3">
                                        <div className="min-w-0"><div className={cap}>{t('col.models')}</div><GuestModels apiKey={k} /></div>
                                        <div className="min-w-0"><div className={cap}>{t('col.expiry')}</div><GuestExpiry expireAt={k.expire_at} nowSec={nowSec} /></div>
                                        <div className="min-w-0"><div className={cap}>{t('col.spend')}</div><SpendCell spent={spentOf(k.id)} cap={k.max_cost} className="w-full" /></div>
                                        <div className="min-w-0"><div className={cap}>{t('col.lastUsed')}</div><LastUsedCell apiKey={k} nowSec={nowSec} /></div>
                                    </div>
                                </div>
                            );
                        })}
                        {list.length === 0 && <div className="rounded-3xl border border-border bg-card">{empty}</div>}
                    </div>
                </>
            )}

            <GuestKeyDialog open={edit.open && (edit.id === null || editKey !== null)} onOpenChange={(open) => setEdit((e) => ({ ...e, open }))} apiKey={editKey} nowSec={nowSec} />
            <GuestKeyDetail open={detailKey !== null} onOpenChange={(o) => { if (!o) setDetailID(null); }} apiKey={detailKey} spent={detailKey ? spentOf(detailKey.id) : 0} nowSec={nowSec} onLogs={handlers.onLogs} />
            <ConfirmDialog
                open={revokeTarget !== null}
                onOpenChange={(o) => { if (!o) setRevokeTarget(null); }}
                title={t('revoke.title', { name: revokeTarget?.name ?? '' })}
                description={t('revoke.desc')}
                confirmLabel={t('revoke.confirm')}
                danger
                pending={revoke.isPending}
                onConfirm={() => revokeTarget && revoke.mutate(revokeTarget.id, {
                    onSuccess: () => { toast.success(t('toast.revoked', { name: revokeTarget.name })); setRevokeTarget(null); },
                    onError: fail,
                })}
            />
            <ConfirmDialog
                open={deleteTarget !== null}
                onOpenChange={(o) => { if (!o) setDeleteTarget(null); }}
                title={t('delete.title', { name: deleteTarget?.name ?? '' })}
                description={t('delete.desc')}
                confirmLabel={t('delete.confirm')}
                danger
                pending={del.isPending}
                onConfirm={() => deleteTarget && del.mutate(deleteTarget.id, {
                    onSuccess: () => { toast.success(t('toast.deleted', { name: deleteTarget.name })); setDeleteTarget(null); },
                    onError: fail,
                })}
            />
        </div>
    );
}
