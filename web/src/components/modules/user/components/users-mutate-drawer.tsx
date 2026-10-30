'use client';

import { useState } from 'react';
import * as DialogPrimitive from '@radix-ui/react-dialog';
import { Check, Loader2, Trash2, Wallet, X } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { toast } from '@/components/common/Toast';
import {
    useCreateUser,
    usePatchUser,
    useResetUserPassword,
    useUserBalanceLog,
    type PatchUserRequest,
    type User,
    type UserRole,
    type UserStatus,
    type UserUsageRange,
} from '@/api/endpoints/user';
import type { AccessPlan } from '@/api/endpoints/access-plan';
import { Switch } from '@/components/ui/switch';
import { cn } from '@/lib/utils';
import { computeBalancePreview, formatUSD } from '../lib/balance-preview';
import { dateInputToEndOfDayUnix, daysLeft, extendExpire, nowUnix, unixToDateInput } from '../lib/format';
import { cardState, userPlanIDs } from '../lib/user-filters';
import { PlanPicker } from './plan-picker';
import { formatDateShort, useRelTime } from './shared';
import { UserUsageTab, UsageRangeSeg } from './user-usage-tab';
import { Bar, HelpTip, Pill, Seg, btn, btnDanger, btnPrimary, chip, iconBtn, label } from './ui-bits';

export type DrawerTab = 'profile' | 'usage';

const input = 'flex h-9 w-full rounded-xl border border-input bg-background px-3 text-sm outline-none focus:border-ring disabled:opacity-60';

interface Draft {
    username: string;
    note: string;
    role: UserRole;
    status: UserStatus;
    password: string;
    dailyLimit: string;
    expireAt: number;
    planIDs: number[];
    defaultPlanID?: number;
    initialBalance: string;
}

function draftFrom(user: User | null, plans: AccessPlan[]): Draft {
    const enabled = plans.filter((p) => p.enabled);
    const fallback = enabled.find((p) => p.is_default) ?? enabled[0];
    const ids = user ? userPlanIDs(user) : fallback ? [fallback.id] : [];
    return {
        username: user?.username ?? '',
        note: user?.note ?? '',
        role: user?.role ?? 'user',
        status: user?.status ?? 'active',
        password: '',
        dailyLimit: user && user.monthly_limit > 0 ? String(user.monthly_limit) : '',
        expireAt: user?.monthly_expire_at ?? 0,
        planIDs: ids,
        defaultPlanID: user?.default_access_plan_id || ids[0],
        initialBalance: '',
    };
}

function sameIDs(a: number[], b: number[]) {
    return a.length === b.length && [...a].sort().every((v, i) => v === [...b].sort()[i]);
}

// 右侧抽屉（new-api 的 users-mutate-drawer）：「资料」改人，「用量」看这个人用了多少。
// user=null 时是「添加用户」。
export function UsersMutateDrawer({ open, onOpenChange, user, plans, tab, onTabChange, onBalance, onDelete, onViewLogs }: {
    open: boolean;
    onOpenChange: (open: boolean) => void;
    user: User | null;
    plans: AccessPlan[];
    tab: DrawerTab;
    onTabChange: (tab: DrawerTab) => void;
    onBalance: (user: User) => void;
    onDelete: (user: User) => void;
    onViewLogs: (user: User) => void;
}) {
    return (
        <DialogPrimitive.Root open={open} onOpenChange={onOpenChange}>
            <DialogPrimitive.Portal>
                <DialogPrimitive.Overlay className="data-[state=open]:animate-in data-[state=closed]:animate-out data-[state=closed]:fade-out-0 data-[state=open]:fade-in-0 fixed inset-0 z-50 bg-black/40" />
                <DialogPrimitive.Content
                    aria-describedby={undefined}
                    className="data-[state=open]:animate-in data-[state=closed]:animate-out data-[state=closed]:slide-out-to-right data-[state=open]:slide-in-from-right fixed inset-y-0 right-0 z-50 flex w-full flex-col border-l border-border bg-background shadow-2xl duration-200 sm:w-[520px]"
                >
                    {open && (
                        <DrawerBody
                            key={user?.id ?? 'new'}
                            user={user}
                            plans={plans}
                            tab={user ? tab : 'profile'}
                            onTabChange={onTabChange}
                            onClose={() => onOpenChange(false)}
                            onBalance={onBalance}
                            onDelete={onDelete}
                            onViewLogs={onViewLogs}
                        />
                    )}
                </DialogPrimitive.Content>
            </DialogPrimitive.Portal>
        </DialogPrimitive.Root>
    );
}

function DrawerBody({ user, plans, tab, onTabChange, onClose, onBalance, onDelete, onViewLogs }: {
    user: User | null;
    plans: AccessPlan[];
    tab: DrawerTab;
    onTabChange: (tab: DrawerTab) => void;
    onClose: () => void;
    onBalance: (user: User) => void;
    onDelete: (user: User) => void;
    onViewLogs: (user: User) => void;
}) {
    const t = useTranslations('user');
    const [draft, setDraft] = useState<Draft>(() => draftFrom(user, plans));
    const [range, setRange] = useState<UserUsageRange>('7d');
    const patch = usePatchUser();
    const create = useCreateUser();
    const resetPassword = useResetUserPassword();
    const enabledPlans = plans.filter((p) => p.enabled || draft.planIDs.includes(p.id));
    const pending = patch.isPending || create.isPending || resetPassword.isPending;
    const set = (p: Partial<Draft>) => setDraft((d) => ({ ...d, ...p }));

    const limitNum = draft.dailyLimit.trim() === '' ? 0 : Number(draft.dailyLimit);
    const limitOk = Number.isFinite(limitNum) && limitNum >= 0;
    const initialPreview = computeBalancePreview(0, 'set', draft.initialBalance || '0');

    const save = async () => {
        if (!draft.username.trim()) { toast.error(t('drawer.needName')); return; }
        if (!limitOk) { toast.error(t('drawer.badLimit')); return; }
        if (draft.planIDs.length === 0) { toast.error(t('plans.pickOne')); return; }
        try {
            if (!user) {
                if (!draft.password.trim()) { toast.error(t('drawer.needPassword')); return; }
                await create.mutateAsync({
                    username: draft.username.trim(),
                    password: draft.password,
                    role: draft.role,
                    status: draft.status,
                    note: draft.note,
                    balance: initialPreview.ok ? initialPreview.amount : 0,
                    monthly_limit: limitNum,
                    monthly_expire_at: draft.expireAt,
                    access_plan_ids: draft.planIDs,
                    default_access_plan_id: draft.defaultPlanID,
                });
                toast.success(t('drawer.created'));
                onClose();
                return;
            }
            // 只发改了的字段：没改的后端保持原值，余额和今天已用永远不随「保存」写回。
            const req: PatchUserRequest = { id: user.id };
            if (draft.username.trim() !== user.username) req.username = draft.username.trim();
            if (draft.note !== (user.note ?? '')) req.note = draft.note;
            if (draft.role !== user.role) req.role = draft.role;
            if (draft.status !== user.status) req.status = draft.status;
            if (limitNum !== user.monthly_limit) req.monthly_limit = limitNum;
            if (draft.expireAt !== user.monthly_expire_at) req.monthly_expire_at = draft.expireAt;
            if (!sameIDs(draft.planIDs, userPlanIDs(user)) || (draft.defaultPlanID && draft.defaultPlanID !== user.default_access_plan_id)) {
                req.access_plan_ids = draft.planIDs;
                if (draft.defaultPlanID) req.default_access_plan_id = draft.defaultPlanID;
            }
            if (Object.keys(req).length > 1) await patch.mutateAsync(req);
            if (draft.password.trim()) {
                await resetPassword.mutateAsync({ id: user.id, password: draft.password });
                set({ password: '' });
            }
            toast.success(t('drawer.saved'));
            onClose();
        } catch (error) {
            toast.error(t('drawer.saveFailed'), { description: (error as { message?: string } | null)?.message });
        }
    };

    const clearToday = () => {
        if (!user) return;
        patch.mutate({ id: user.id, monthly_used: 0 }, {
            onSuccess: () => toast.success(t('drawer.clearedToday')),
            onError: (error) => toast.error(t('drawer.saveFailed'), { description: (error as { message?: string } | null)?.message }),
        });
    };

    const now = nowUnix();
    return (
        <>
            <DrawerHead user={user} />
            {user && (
                <div className="flex items-center justify-between gap-2 px-6 pt-4">
                    <Seg value={tab} onChange={onTabChange} options={[{ value: 'profile', label: t('drawer.profile') }, { value: 'usage', label: t('drawer.usage') }]} />
                    {tab === 'usage' && <UsageRangeSeg value={range} onChange={setRange} />}
                </div>
            )}
            <div className="min-h-0 flex-1 overflow-y-auto px-6 pt-5 pb-6">
                {user && tab === 'usage' ? (
                    <UserUsageTab user={user} range={range} onViewLogs={() => onViewLogs(user)} />
                ) : (
                    <div className="space-y-4">
                        <section>
                            <h4 className="mb-3 text-sm font-semibold">{t('drawer.basic')}</h4>
                            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                                <label className="block"><div className={label}>{t('drawer.username')}</div><input className={input} value={draft.username} onChange={(e) => set({ username: e.target.value })} /></label>
                                <label className="block"><div className={label}>{t('drawer.note')}</div><input className={input} value={draft.note} placeholder={t('drawer.notePlaceholder')} onChange={(e) => set({ note: e.target.value })} /></label>
                                <div>
                                    <div className={label}>{t('drawer.role')}<HelpTip content={t('drawer.roleTip')} /></div>
                                    <Seg value={draft.role} onChange={(role) => set({ role })} className="w-full" itemClassName="flex-1" options={[{ value: 'user', label: t('role.user') }, { value: 'admin', label: t('role.admin') }]} />
                                </div>
                                <div>
                                    <div className={label}>{t('drawer.canUse')}<HelpTip content={t('drawer.canUseTip')} /></div>
                                    <label className="flex h-9 items-center gap-2 text-sm">
                                        <Switch checked={draft.status === 'active'} onCheckedChange={(on) => set({ status: on ? 'active' : 'disabled' })} />
                                        {draft.status === 'active' ? t('status.active') : t('status.disabled')}
                                    </label>
                                </div>
                                <label className="block sm:col-span-2">
                                    <div className={label}>{user ? t('drawer.newPassword') : t('drawer.password')}</div>
                                    <input className={input} type="password" autoComplete="new-password" value={draft.password} placeholder={user ? t('drawer.passwordKeep') : t('drawer.passwordRequired')} onChange={(e) => set({ password: e.target.value })} />
                                </label>
                            </div>
                        </section>

                        {user ? <BalanceSection user={user} onBalance={() => onBalance(user)} /> : (
                            <section className="rounded-2xl border border-border/70 bg-muted/20 px-4 py-3">
                                <div className={label}>{t('drawer.initialBalance')}<HelpTip content={t('drawer.initialBalanceTip')} /></div>
                                <input className={input} inputMode="decimal" placeholder="0.00" value={draft.initialBalance} onChange={(e) => set({ initialBalance: e.target.value })} />
                            </section>
                        )}

                        <section className="rounded-2xl border border-border/70 bg-muted/20 p-4">
                            <div className="flex items-center justify-between gap-2">
                                <h4 className="flex items-center gap-1 text-sm font-semibold">{t('drawer.card')}<HelpTip content={t('drawer.cardTip')} /></h4>
                                <CardStatusPill limit={limitNum} expireAt={draft.expireAt} used={user?.monthly_used ?? 0} role={draft.role} now={now} />
                            </div>
                            <div className="mt-3 grid grid-cols-2 gap-3">
                                <label className="block">
                                    <div className={label}>{t('drawer.dailyLimit')}</div>
                                    <input className={cn(input, !limitOk && 'border-destructive')} inputMode="decimal" placeholder="0" value={draft.dailyLimit} onChange={(e) => set({ dailyLimit: e.target.value })} />
                                </label>
                                <label className="block">
                                    <div className={label}>{t('drawer.expire')}<HelpTip content={t('drawer.expireTip')} align="end" /></div>
                                    <input className={input} type="date" value={unixToDateInput(draft.expireAt)} onChange={(e) => set({ expireAt: dateInputToEndOfDayUnix(e.target.value) })} />
                                </label>
                            </div>
                            {user && (
                                <>
                                    <div className="mt-3 flex items-center justify-between text-xs text-muted-foreground">
                                        <span>{t('drawer.usedToday', { used: formatUSD(user.monthly_used), limit: formatUSD(user.monthly_limit) })}</span>
                                        <HelpClear onClick={clearToday} disabled={patch.isPending || user.monthly_used <= 0} />
                                    </div>
                                    <Bar value={user.monthly_limit > 0 ? (user.monthly_used / user.monthly_limit) * 100 : 0} tone={user.monthly_used >= user.monthly_limit && user.monthly_limit > 0 ? 'danger' : 'primary'} className="mt-1.5" />
                                </>
                            )}
                            <div className="mt-3 flex flex-wrap items-center gap-2">
                                <button type="button" className={chip} onClick={() => set({ expireAt: extendExpire(draft.expireAt, 7, now) })}>{t('drawer.plus7')}</button>
                                <button type="button" className={chip} onClick={() => set({ expireAt: extendExpire(draft.expireAt, 30, now) })}>{t('drawer.plus30')}</button>
                                <button type="button" className={chip} onClick={() => set({ expireAt: 0, dailyLimit: '' })}>{t('drawer.stopCard')}</button>
                                <span className="text-[11px] text-muted-foreground">{t('drawer.applyOnSave')}</span>
                            </div>
                        </section>

                        <section>
                            <div className="mb-2 flex items-center justify-between">
                                <h4 className="flex items-center gap-1 text-sm font-semibold">{t('plans.title')}<HelpTip content={t('plans.tip')} /></h4>
                                <span className="text-xs text-muted-foreground">{t('plans.hint')}</span>
                            </div>
                            <PlanPicker plans={enabledPlans} value={draft.planIDs} defaultID={draft.defaultPlanID} onChange={(planIDs, defaultPlanID) => set({ planIDs, defaultPlanID })} />
                        </section>
                    </div>
                )}
            </div>
            {(!user || tab === 'profile') && (
                <div className="flex items-center gap-2 border-t border-border bg-background px-6 py-4">
                    {user && <button type="button" className={btnDanger} onClick={() => onDelete(user)}><Trash2 className="size-4" />{t('drawer.delete')}</button>}
                    <button type="button" className={cn(btn, 'ml-auto')} onClick={onClose}>{t('common.cancel')}</button>
                    <button type="button" className={btnPrimary} disabled={pending} onClick={save}>
                        {pending ? <Loader2 className="size-4 animate-spin" /> : <Check className="size-4" />}
                        {user ? t('common.save') : t('drawer.create')}
                    </button>
                </div>
            )}
        </>
    );
}

function HelpClear({ onClick, disabled }: { onClick: () => void; disabled?: boolean }) {
    const t = useTranslations('user.drawer');
    return (
        <span className="inline-flex items-center gap-1">
            <button type="button" onClick={onClick} disabled={disabled} className="text-primary hover:underline disabled:text-muted-foreground disabled:no-underline">{t('clearToday')}</button>
            <HelpTip content={t('clearTodayTip')} align="end" />
        </span>
    );
}

function CardStatusPill({ limit, expireAt, used, role, now }: { limit: number; expireAt: number; used: number; role: UserRole; now: number }) {
    const t = useTranslations('user.card');
    if (role === 'admin') return <Pill tone="off">{t('adminFree')}</Pill>;
    const state = cardState({ id: 0, username: '', role, status: 'active', monthly_limit: limit, monthly_used: used, monthly_expire_at: expireAt }, now);
    if (state === 'none') return <Pill tone="off">{t('none')}</Pill>;
    if (state === 'expired') return <Pill tone="off">{t('expired')}</Pill>;
    if (state === 'exhausted') return <Pill tone="bad">{t('exhaustedPill', { n: daysLeft(expireAt, now) })}</Pill>;
    return <Pill tone={state === 'soon' ? 'warn' : 'ok'}>{t('activePill', { n: daysLeft(expireAt, now) })}</Pill>;
}

function DrawerHead({ user }: { user: User | null }) {
    const t = useTranslations('user.drawer');
    return (
        <div className="flex items-start justify-between gap-3 border-b border-border px-6 py-5">
            <div className="min-w-0">
                <DialogPrimitive.Title className="flex min-w-0 items-center gap-2">
                    <span className="truncate text-lg font-semibold">{user ? user.username : t('createTitle')}</span>
                    {user && (user.status === 'active' ? <Pill tone="ok">{t('active')}</Pill> : <Pill tone="off">{t('disabled')}</Pill>)}
                    {user?.role === 'admin' && <Pill tone="pri">{t('admin')}</Pill>}
                    {user?.note?.trim() && <Pill className="max-w-[8rem] font-normal text-muted-foreground"><span className="truncate">{user.note}</span></Pill>}
                </DialogPrimitive.Title>
                {user && (
                    <div className="mt-1 truncate text-xs text-muted-foreground" title={user.register_ip ? t('registerIP', { ip: user.register_ip }) : undefined}>
                        #{user.id}
                        {user.email ? ` · ${user.email}${user.email_verified ? t('verified') : ''}` : ''}
                        {user.created_at ? ` · ${t('registered', { date: formatDateShort(user.created_at) })}` : ''}
                    </div>
                )}
            </div>
            <DialogPrimitive.Close className={iconBtn} aria-label={t('close')}><X className="size-4" /></DialogPrimitive.Close>
        </div>
    );
}

function BalanceSection({ user, onBalance }: { user: User; onBalance: () => void }) {
    const t = useTranslations('user.drawer');
    const rel = useRelTime();
    const { data: logs = [] } = useUserBalanceLog(user.id, 5);
    const now = nowUnix();
    return (
        <section className="rounded-2xl border border-border/70 bg-muted/20 px-4 py-3">
            <div className="flex items-center gap-3">
                <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-1 text-xs text-muted-foreground">{t('balance')}<HelpTip content={user.role === 'admin' ? t('balanceTipAdmin') : t('balanceTip')} /></div>
                    <div className="text-xl font-semibold tabular-nums">{formatUSD(user.balance)}</div>
                </div>
                <button type="button" className={cn(btn, 'h-8')} onClick={onBalance}><Wallet className="size-4" />{t('adjustBalance')}</button>
            </div>
            {logs.length > 0 && (
                <div className="mt-3 border-t border-border/70 pt-2">
                    <div className="mb-1 text-[11px] text-muted-foreground">{t('history')}</div>
                    <ul data-balance-history className="space-y-1 text-xs">
                        {logs.map((l) => (
                            <li key={l.id} className="flex items-center gap-2">
                                <span className={cn('w-16 shrink-0 font-medium tabular-nums', l.delta >= 0 ? 'text-primary' : 'text-destructive')}>{l.delta >= 0 ? '+' : ''}{formatUSD(l.delta)}</span>
                                <span className="min-w-0 flex-1 truncate text-muted-foreground" title={l.reason}>{l.reason || t(`mode.${l.mode}`)}</span>
                                <span className="shrink-0 text-muted-foreground">{l.actor_name || '—'} · {rel(l.created_at, now)}</span>
                            </li>
                        ))}
                    </ul>
                </div>
            )}
        </section>
    );
}
