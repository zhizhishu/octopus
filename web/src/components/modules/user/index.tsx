'use client';

import { useEffect, useMemo, useState } from 'react';
import { Loader2, Search, Ticket, UserPlus } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { toast } from '@/components/common/Toast';
import { useDeleteUser, usePatchUser, useResetUserPassword, useUserAccessList, useUserList, type User } from '@/api/endpoints/user';
import { useAccessPlanList } from '@/api/endpoints/access-plan';
import { useUserUsageRank } from '@/api/endpoints/stats';
import { useNavStore } from '@/components/modules/navbar/nav-store';
import { useLogPresetStore } from '@/components/modules/log/log-preset';
import { useUserPresetStore } from './user-preset';
import { cn } from '@/lib/utils';
import { nowUnix } from './lib/format';
import { EMPTY_FILTERS, filterUsers, paginate, statusCounts, type UserFilters } from './lib/user-filters';
import { ConfirmDialog } from './components/confirm-dialog';
import { DataTableBulkActions } from './components/data-table-bulk-actions';
import type { UserRowHandlers } from './components/data-table-row-actions';
import { RedeemDialog } from './components/redeem-dialog';
import { UserQuotaDialog } from './components/user-quota-dialog';
import { UsersMobileList } from './components/users-mobile-list';
import { UsersMutateDrawer, type DrawerTab } from './components/users-mutate-drawer';
import { UsersTable } from './components/users-table';
import { UsersToolbar } from './components/users-toolbar';
import { Pager } from './components/pager';
import { btn, btnPrimary } from './components/ui-bits';

const PAGE_SIZE = 20;

// 用户页，照 new-api features/users 的结构：标题 + 右上主按钮，下面一张表，编辑走右侧抽屉。
export function UserManagement() {
    const t = useTranslations('user');
    const { data: users = [], isLoading } = useUserList();
    const { data: accessPlans = [] } = useAccessPlanList();
    const { data: rank = [] } = useUserUsageRank();
    const { data: accessList = [] } = useUserAccessList();
    const patch = usePatchUser();
    const deleteUser = useDeleteUser();
    const resetPassword = useResetUserPassword();
    const setActiveItem = useNavStore((s) => s.setActiveItem);

    const [filters, setFiltersState] = useState<UserFilters>(EMPTY_FILTERS);
    const [page, setPage] = useState(1);
    const [selected, setSelected] = useState<Set<number>>(new Set());
    // 从仪表盘点用户名跳过来：直接打开他的抽屉（用户列表加载完就能显示）；读一次就清掉。
    const [drawer, setDrawer] = useState<{ open: boolean; userID: number | null; tab: DrawerTab }>(() => {
        const focus = useUserPresetStore.getState().focusUserID;
        return focus ? { open: true, userID: focus, tab: 'profile' } : { open: false, userID: null, tab: 'profile' };
    });
    useEffect(() => {
        useUserPresetStore.getState().clear();
    }, []);
    const [balanceUserID, setBalanceUserID] = useState<number | null>(null);
    const [redeemOpen, setRedeemOpen] = useState(false);
    const [deleteTarget, setDeleteTarget] = useState<User | null>(null);
    const [passwordTarget, setPasswordTarget] = useState<User | null>(null);
    const [newPassword, setNewPassword] = useState('');

    const nowSec = nowUnix();
    const setFilters = (f: UserFilters) => { setFiltersState(f); setPage(1); };
    const filtered = useMemo(() => filterUsers(users, filters, nowSec), [users, filters, nowSec]);
    const counts = useMemo(() => statusCounts(users, filters, nowSec), [users, filters, nowSec]);
    const paged = paginate(filtered, page, PAGE_SIZE);
    const usage = useMemo(() => new Map(rank.map((r) => [r.user_id, r])), [rank]);
    const access = useMemo(() => new Map(accessList.map((a) => [a.user_id, a])), [accessList]);
    const liveSelected = [...selected].filter((id) => users.some((u) => u.id === id));
    const drawerUser = drawer.userID ? users.find((u) => u.id === drawer.userID) ?? null : null;
    const balanceUser = balanceUserID ? users.find((u) => u.id === balanceUserID) ?? null : null;

    const fail = (title: string) => (error: unknown) => toast.error(title, { description: (error as { message?: string } | null)?.message });
    const handlers: UserRowHandlers = {
        onEdit: (u, tab = 'profile') => setDrawer({ open: true, userID: u.id, tab }),
        onBalance: (u) => setBalanceUserID(u.id),
        onToggleStatus: (u) => patch.mutate({ id: u.id, status: u.status === 'active' ? 'disabled' : 'active' }, {
            onSuccess: () => toast.success(u.status === 'active' ? t('toast.disabled', { name: u.username }) : t('toast.enabled', { name: u.username })),
            onError: fail(t('toast.failed')),
        }),
        onToggleRole: (u) => patch.mutate({ id: u.id, role: u.role === 'admin' ? 'user' : 'admin' }, {
            onSuccess: () => toast.success(u.role === 'admin' ? t('toast.madeUser', { name: u.username }) : t('toast.madeAdmin', { name: u.username })),
            onError: fail(t('toast.failed')),
        }),
        onResetPassword: (u) => { setNewPassword(''); setPasswordTarget(u); },
        onViewLogs: (u) => {
            useLogPresetStore.getState().setUserID(u.id);
            setDrawer((d) => ({ ...d, open: false }));
            setActiveItem('log');
        },
        onDelete: (u) => setDeleteTarget(u),
    };

    const toggleOne = (id: number, on: boolean) => setSelected((s) => { const n = new Set(s); if (on) n.add(id); else n.delete(id); return n; });
    const togglePage = (on: boolean) => setSelected((s) => { const n = new Set(s); for (const u of paged.items) { if (on) n.add(u.id); else n.delete(u.id); } return n; });

    return (
        <div className="h-full min-h-0 space-y-4 overflow-y-auto overscroll-contain rounded-t-3xl pb-24 md:pb-4">
            {/* 桌面：一句话 + 兑换码 / 添加用户 */}
            <div className="hidden flex-wrap items-center justify-between gap-3 px-1 md:flex">
                <p className="text-sm text-muted-foreground">{t('page.lead')}</p>
                <div className="flex items-center gap-2">
                    <button type="button" className={btn} onClick={() => setRedeemOpen(true)}><Ticket className="size-4" />{t('page.redeem')}</button>
                    <button type="button" className={btnPrimary} onClick={() => setDrawer({ open: true, userID: null, tab: 'profile' })}><UserPlus className="size-4" />{t('page.add')}</button>
                </div>
            </div>

            {/* 手机：搜索 + 新增，筛选胶囊横排 */}
            <div className="space-y-3 md:hidden">
                <div className="flex items-center gap-2 px-1">
                    <label className="flex h-10 min-w-0 flex-1 items-center gap-2 rounded-xl border border-input bg-background px-3 text-sm">
                        <Search className="size-4 shrink-0 text-muted-foreground" />
                        <input value={filters.search} onChange={(e) => setFilters({ ...filters, search: e.target.value })} placeholder={t('toolbar.searchShort')} className="min-w-0 flex-1 bg-transparent outline-none placeholder:text-muted-foreground" />
                    </label>
                    <button type="button" aria-label={t('page.redeem')} className={cn(btn, 'size-10 justify-center px-0')} onClick={() => setRedeemOpen(true)}><Ticket className="size-4" /></button>
                    <button type="button" aria-label={t('page.add')} className={cn(btnPrimary, 'size-10 justify-center px-0')} onClick={() => setDrawer({ open: true, userID: null, tab: 'profile' })}><UserPlus className="size-4" /></button>
                </div>
                <UsersToolbar mobile filters={filters} setFilters={setFilters} counts={counts} plans={accessPlans} />
            </div>

            {isLoading ? (
                <div className="flex justify-center py-16 text-muted-foreground"><Loader2 className="size-5 animate-spin" /></div>
            ) : (
                <>
                    <div className="hidden rounded-3xl border border-border bg-card text-card-foreground md:block">
                        {liveSelected.length > 0
                            ? <DataTableBulkActions selected={liveSelected} users={users} plans={accessPlans} onClear={() => setSelected(new Set())} />
                            : <UsersToolbar filters={filters} setFilters={setFilters} counts={counts} plans={accessPlans} />}
                        <div className="w-full overflow-x-auto">
                            <UsersTable users={paged.items} plans={accessPlans} usage={usage} access={access} nowSec={nowSec} selected={selected} onSelect={toggleOne} onSelectPage={togglePage} handlers={handlers} />
                        </div>
                        {paged.total === 0 && <div className="py-12 text-center text-sm text-muted-foreground">{users.length === 0 ? t('page.empty') : t('page.noMatch')}</div>}
                        <Pager size={PAGE_SIZE} page={paged.page} pages={paged.pages} total={paged.total} onPage={setPage} />
                    </div>
                    <div className="md:hidden">
                        <UsersMobileList users={paged.items} plans={accessPlans} access={access} nowSec={nowSec} handlers={handlers} />
                        {paged.total === 0 && <div className="py-12 text-center text-sm text-muted-foreground">{users.length === 0 ? t('page.empty') : t('page.noMatch')}</div>}
                        {paged.pages > 1 && <Pager size={PAGE_SIZE} page={paged.page} pages={paged.pages} total={paged.total} onPage={setPage} />}
                    </div>
                </>
            )}

            <UsersMutateDrawer
                // 带着某个用户打开、但列表还没加载到他时先不弹，免得闪一下「添加用户」的空表单。
                open={drawer.open && (drawer.userID === null || drawerUser !== null)}
                onOpenChange={(open) => setDrawer((d) => ({ ...d, open }))}
                user={drawerUser}
                plans={accessPlans}
                tab={drawer.tab}
                onTabChange={(tab) => setDrawer((d) => ({ ...d, tab }))}
                onBalance={handlers.onBalance}
                onDelete={handlers.onDelete}
                onViewLogs={handlers.onViewLogs}
                alertCount={drawerUser ? access.get(drawerUser.id)?.alerts.length ?? 0 : 0}
            />
            <UserQuotaDialog user={balanceUser} open={balanceUser !== null} onOpenChange={(o) => { if (!o) setBalanceUserID(null); }} />
            <RedeemDialog open={redeemOpen} onOpenChange={setRedeemOpen} />
            <ConfirmDialog
                open={deleteTarget !== null}
                onOpenChange={(o) => { if (!o) setDeleteTarget(null); }}
                title={t('delete.title', { name: deleteTarget?.username ?? '' })}
                description={t('delete.desc')}
                confirmLabel={t('delete.confirm')}
                danger
                pending={deleteUser.isPending}
                onConfirm={() => deleteTarget && deleteUser.mutate(deleteTarget.id, {
                    onSuccess: () => {
                        toast.success(t('toast.deleted', { name: deleteTarget.username }));
                        if (drawer.userID === deleteTarget.id) setDrawer((d) => ({ ...d, open: false }));
                        setDeleteTarget(null);
                    },
                    onError: fail(t('toast.failed')),
                })}
            />
            <ConfirmDialog
                open={passwordTarget !== null}
                onOpenChange={(o) => { if (!o) setPasswordTarget(null); }}
                title={t('password.title', { name: passwordTarget?.username ?? '' })}
                description={t('password.desc')}
                confirmLabel={t('password.confirm')}
                pending={resetPassword.isPending}
                onConfirm={() => passwordTarget && newPassword.trim() && resetPassword.mutate({ id: passwordTarget.id, password: newPassword }, {
                    onSuccess: () => { toast.success(t('toast.passwordReset', { name: passwordTarget.username })); setPasswordTarget(null); },
                    onError: fail(t('toast.failed')),
                })}
            >
                <input autoFocus type="password" autoComplete="new-password" value={newPassword} onChange={(e) => setNewPassword(e.target.value)} placeholder={t('password.placeholder')} className="flex h-9 w-full rounded-xl border border-input bg-background px-3 text-sm outline-none focus:border-ring" />
            </ConfirmDialog>
        </div>
    );
}
