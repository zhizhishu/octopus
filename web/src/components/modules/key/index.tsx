'use client';

import { useMemo, useState } from 'react';
import { BookOpen, Loader2, Plus, Search, Ticket } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { useQuery } from '@tanstack/react-query';
import { useCopyToClipboard } from '@uidotdev/usehooks';
import { apiClient } from '@/api/client';
import { useAPIKeyList, useBatchAPIKeys, useDeleteAPIKey, type APIKey } from '@/api/endpoints/apikey';
import { useAuthStore, useUserList, type User } from '@/api/endpoints/user';
import { useAccessPlanList } from '@/api/endpoints/access-plan';
import { useStatsAPIKey } from '@/api/endpoints/stats';
import { CopyIconButton } from '@/components/common/CopyButton';
import { toast } from '@/components/common/Toast';
import { useNavStore } from '@/components/modules/navbar/nav-store';
import { useLogPresetStore } from '@/components/modules/log/log-preset';
import { cn } from '@/lib/utils';
import { ConfirmDialog } from '@/components/modules/user/components/confirm-dialog';
import { Pager } from '@/components/modules/user/components/pager';
import { btn, btnPrimary } from '@/components/modules/user/components/ui-bits';
import { formatUSD } from '@/components/modules/user/lib/balance-preview';
import { nowUnix } from '@/components/modules/user/lib/format';
import { cardState, paginate } from '@/components/modules/user/lib/user-filters';
import { EMPTY_KEY_FILTERS, filterKeys, keyPlanIDs, keyStatus, keyStatusCounts, type KeyFilters } from './lib/key-filters';
import { KeyDrawer, type KeyDrawerTab } from './components/key-drawer';
import { KeyGuideDialog, useBaseUrl } from './components/key-guide-dialog';
import type { KeyRowHandlers } from './components/key-row-actions';
import { KeysBulkActions } from './components/keys-bulk-actions';
import { KeysMobileList } from './components/keys-mobile-list';
import { KeysTable } from './components/keys-table';
import { KeysToolbar } from './components/keys-toolbar';
import { RedeemCodeDialog } from './components/redeem-code-dialog';

const PAGE_SIZE = 20;

// API Key 页，照用户页的结构：一句话 + 右上按钮，一张表（手机是卡片），编辑走右侧宽抽屉。
// 管理员看所有人的 Key；普通用户只看自己的，上面多一条「接入地址 + 余额」。
export function Key() {
    const t = useTranslations('keys');
    const me = useAuthStore((s) => s.user);
    const isAdmin = me?.role === 'admin';
    const { data: keys = [], isLoading } = useAPIKeyList();
    const { data: keyStats = [] } = useStatsAPIKey();
    const { data: users = [] } = useUserList({ enabled: isAdmin });
    const { data: accessPlans = [] } = useAccessPlanList({ enabled: isAdmin });
    const batch = useBatchAPIKeys();
    const deleteKey = useDeleteAPIKey();
    const setActiveItem = useNavStore((s) => s.setActiveItem);
    const [, copy] = useCopyToClipboard();

    const [filters, setFiltersState] = useState<KeyFilters>(EMPTY_KEY_FILTERS);
    const [page, setPage] = useState(1);
    const [selected, setSelected] = useState<Set<number>>(new Set());
    const [drawer, setDrawer] = useState<{ open: boolean; keyID: number | null; tab: KeyDrawerTab }>({ open: false, keyID: null, tab: 'settings' });
    const [guide, setGuide] = useState<{ open: boolean; keyID: number | null; n: number }>({ open: false, keyID: null, n: 0 });
    const [deleteTarget, setDeleteTarget] = useState<APIKey | null>(null);
    const [redeemOpen, setRedeemOpen] = useState(false);
    const [nowSec] = useState(nowUnix);

    const setFilters = (f: KeyFilters) => { setFiltersState(f); setPage(1); };
    const spent = useMemo(() => new Map(keyStats.map((s) => [s.api_key_id, s.total_cost.raw])), [keyStats]);
    const spentOf = (id: number) => spent.get(id) ?? 0;
    const userByID = useMemo(() => new Map(users.map((u) => [u.id, u])), [users]);
    const ownerName = (id?: number) => (id ? userByID.get(id)?.username ?? keys.find((k) => k.user_id === id)?.user_name ?? `#${id}` : '');
    const ownerOf = isAdmin ? (k: APIKey) => ownerName(k.user_id) : undefined;

    // 新的在前（id 大的在前），和原来一样。
    const sorted = useMemo(() => [...keys].sort((a, b) => b.id - a.id), [keys]);
    const filtered = filterKeys(sorted, filters, spentOf, nowSec, ownerName);
    const counts = keyStatusCounts(sorted, filters, spentOf, nowSec, ownerName);
    const paged = paginate(filtered, page, PAGE_SIZE);
    const liveSelected = [...selected].filter((id) => keys.some((k) => k.id === id));
    const drawerKey = drawer.keyID ? keys.find((k) => k.id === drawer.keyID) ?? null : null;

    const ownerOptions = useMemo(() => {
        if (!isAdmin) return undefined;
        const ids = [...new Set(keys.map((k) => k.user_id ?? 0).filter(Boolean))];
        return ids.map((id) => ({ value: id, label: userByID.get(id)?.username ?? keys.find((k) => k.user_id === id)?.user_name ?? `#${id}` }))
            .sort((a, b) => a.label.localeCompare(b.label));
    }, [isAdmin, keys, userByID]);
    const planOptions = useMemo(() => {
        if (!isAdmin) return undefined;
        const ids = new Set(keys.flatMap((k) => keyPlanIDs(k)));
        return accessPlans.filter((p) => ids.has(p.id)).map((p) => ({ value: p.id, label: p.display_name?.trim() || p.slug }));
    }, [isAdmin, keys, accessPlans]);

    const fail = (title: string) => (error: unknown) => toast.error(title, { description: (error as { message?: string } | null)?.message });
    // 没指定哪把时，先挑一把现在能用的。
    const openGuide = (k?: APIKey | null) => {
        const pick = k ?? sorted.find((x) => keyStatus(x, spentOf(x.id), nowSec) === 'ok') ?? sorted[0];
        setGuide((g) => ({ open: true, keyID: pick?.id ?? null, n: g.n + 1 }));
    };
    const handlers: KeyRowHandlers = {
        onEdit: (k, tab = 'settings') => setDrawer({ open: true, keyID: k.id, tab }),
        onGuide: (k) => openGuide(k),
        onToggle: (k) => batch.mutate({ ids: [k.id], action: k.enabled ? 'disable' : 'enable' }, {
            onSuccess: (res) => res.failed.length > 0
                ? toast.error(t('toast.failed'), { description: res.failed[0].error })
                : toast.success(k.enabled ? t('toast.disabled', { name: k.name }) : t('toast.enabled', { name: k.name })),
            onError: fail(t('toast.failed')),
        }),
        onCopy: async (k) => { await copy(k.api_key); toast.success(t('toast.copied', { name: k.name })); },
        onViewLogs: (k) => {
            useLogPresetStore.getState().setPreset({ apiKeyId: k.id });
            setDrawer((d) => ({ ...d, open: false }));
            setActiveItem('log');
        },
        onDelete: (k) => setDeleteTarget(k),
    };

    const toggleOne = (id: number, on: boolean) => setSelected((s) => { const n = new Set(s); if (on) n.add(id); else n.delete(id); return n; });
    const togglePage = (on: boolean) => setSelected((s) => { const n = new Set(s); for (const k of paged.items) { if (on) n.add(k.id); else n.delete(k.id); } return n; });
    const openCreate = () => setDrawer({ open: true, keyID: null, tab: 'settings' });
    const empty = <div className="py-12 text-center text-sm text-muted-foreground">{keys.length === 0 ? t('page.empty') : t('page.noMatch')}</div>;
    const summary = t('page.pager', { total: paged.total, size: PAGE_SIZE });

    return (
        <div className="h-full min-h-0 space-y-4 overflow-y-auto overscroll-contain rounded-t-3xl pb-24 md:pb-4">
            {/* 桌面：一句话 + 接入说明 / 兑换码 / 新建 */}
            <div className="hidden flex-wrap items-center justify-between gap-3 px-1 md:flex">
                <p className="text-sm text-muted-foreground">{isAdmin ? t('page.leadAdmin') : t('page.leadUser')}</p>
                <div className="flex items-center gap-2">
                    {isAdmin && <button type="button" className={btn} onClick={() => openGuide(null)}><BookOpen className="size-4" />{t('page.guide')}</button>}
                    {!isAdmin && <button type="button" className={btn} onClick={() => setRedeemOpen(true)}><Ticket className="size-4" />{t('page.redeem')}</button>}
                    <button type="button" className={btnPrimary} onClick={openCreate}><Plus className="size-4" />{t('page.add')}</button>
                </div>
            </div>

            {!isAdmin && <UserStrip onGuide={() => openGuide(null)} onRedeem={() => setRedeemOpen(true)} />}

            {/* 手机：搜索 + 新建，状态页签横排 */}
            <div className="space-y-3 md:hidden">
                <div className="flex items-center gap-2 px-1">
                    <label className="flex h-10 min-w-0 flex-1 items-center gap-2 rounded-xl border border-input bg-background px-3 text-sm">
                        <Search className="size-4 shrink-0 text-muted-foreground" />
                        <input value={filters.search} onChange={(e) => setFilters({ ...filters, search: e.target.value })} placeholder={t('toolbar.searchShort')} className="min-w-0 flex-1 bg-transparent outline-none placeholder:text-muted-foreground" />
                    </label>
                    <button type="button" aria-label={t('page.guide')} className={cn(btn, 'size-10 justify-center px-0')} onClick={() => openGuide(null)}><BookOpen className="size-4" /></button>
                    <button type="button" aria-label={t('page.add')} className={cn(btnPrimary, 'size-10 justify-center px-0')} onClick={openCreate}><Plus className="size-4" /></button>
                </div>
                <KeysToolbar mobile filters={filters} setFilters={setFilters} counts={counts} />
            </div>

            {isLoading ? (
                <div className="flex justify-center py-16 text-muted-foreground"><Loader2 className="size-5 animate-spin" /></div>
            ) : (
                <>
                    <div className="hidden rounded-3xl border border-border bg-card text-card-foreground md:block">
                        {liveSelected.length > 0
                            ? <KeysBulkActions selected={liveSelected} keys={keys} onClear={() => setSelected(new Set())} />
                            : <KeysToolbar filters={filters} setFilters={setFilters} counts={counts} owners={ownerOptions} plans={planOptions} />}
                        <div className="w-full overflow-x-auto">
                            <KeysTable keys={paged.items} spentOf={spentOf} ownerOf={ownerOf} nowSec={nowSec} selected={selected} onSelect={toggleOne} onSelectPage={togglePage} handlers={handlers} />
                        </div>
                        {paged.total === 0 && empty}
                        <Pager size={PAGE_SIZE} page={paged.page} pages={paged.pages} total={paged.total} onPage={setPage} summary={summary} />
                    </div>
                    <div className="md:hidden">
                        <KeysMobileList keys={paged.items} spentOf={spentOf} ownerOf={ownerOf} nowSec={nowSec} handlers={handlers} />
                        {paged.total === 0 && empty}
                        {paged.pages > 1 && <Pager size={PAGE_SIZE} page={paged.page} pages={paged.pages} total={paged.total} onPage={setPage} summary={summary} />}
                    </div>
                </>
            )}

            <KeyDrawer
                open={drawer.open && (drawer.keyID === null || drawerKey !== null)}
                onOpenChange={(open) => setDrawer((d) => ({ ...d, open }))}
                apiKey={drawerKey}
                tab={drawer.tab}
                onTabChange={(tab) => setDrawer((d) => ({ ...d, tab }))}
                users={users}
                plans={accessPlans}
                spentOf={spentOf}
                onDelete={handlers.onDelete}
                onGuide={handlers.onGuide}
                onViewLogs={handlers.onViewLogs}
            />
            <KeyGuideDialog
                key={`${guide.keyID ?? 'none'}-${guide.n}`}
                open={guide.open}
                onOpenChange={(open) => setGuide((g) => ({ ...g, open }))}
                keys={sorted}
                keyID={guide.keyID}
                spentOf={spentOf}
            />
            <RedeemCodeDialog open={redeemOpen} onOpenChange={setRedeemOpen} />
            <ConfirmDialog
                open={deleteTarget !== null}
                onOpenChange={(o) => { if (!o) setDeleteTarget(null); }}
                title={t('delete.title', { name: deleteTarget?.name ?? '' })}
                description={t('delete.desc')}
                confirmLabel={t('delete.confirm')}
                danger
                pending={deleteKey.isPending}
                onConfirm={() => deleteTarget && deleteKey.mutate(deleteTarget.id, {
                    onSuccess: () => {
                        toast.success(t('toast.deleted', { name: deleteTarget.name }));
                        if (drawer.keyID === deleteTarget.id) setDrawer((d) => ({ ...d, open: false }));
                        setSelected((s) => { const n = new Set(s); n.delete(deleteTarget.id); return n; });
                        setDeleteTarget(null);
                    },
                    onError: fail(t('toast.failed')),
                })}
            />
        </div>
    );
}

/** 普通用户：接入地址（复制）+ 接入说明，右边余额和月卡今天用了多少。 */
function UserStrip({ onGuide, onRedeem }: { onGuide: () => void; onRedeem: () => void }) {
    const t = useTranslations('keys.strip');
    const baseUrl = useBaseUrl();
    // 余额会随请求变，进页面拉一次最新的（登录时存的那份可能是旧的）。
    const { data: me } = useQuery({
        queryKey: ['user', 'status'],
        queryFn: () => apiClient.get<User>('/api/v1/user/status'),
        refetchInterval: 60000,
    });
    const stored = useAuthStore((s) => s.user);
    const user = me ?? stored;
    const nowSec = nowUnix();
    const card = user ? cardState(user, nowSec) : 'none';
    return (
        <div data-user-strip className="flex flex-wrap items-center gap-x-4 gap-y-2 rounded-3xl border border-border bg-card px-4 py-3 text-sm">
            <div className="flex min-w-0 items-center gap-1.5">
                <span className="shrink-0 text-muted-foreground">{t('endpoint')}</span>
                <code className="min-w-0 truncate font-mono text-[13px]">{baseUrl}</code>
                <CopyIconButton text={baseUrl} className="inline-flex size-7 shrink-0 items-center justify-center rounded-md text-muted-foreground hover:bg-muted hover:text-foreground" copyIconClassName="size-3.5" checkIconClassName="size-3.5" />
                <button type="button" onClick={onGuide} className="inline-flex shrink-0 items-center gap-1 text-primary hover:underline"><BookOpen className="size-4" />{t('guide')}</button>
            </div>
            {user && (
                <div className="ml-auto flex flex-wrap items-center gap-x-4 gap-y-1">
                    <span className="text-muted-foreground">{t('balance')} <b className="font-semibold text-foreground tabular-nums">{formatUSD(user.balance)}</b></span>
                    {(card === 'active' || card === 'soon' || card === 'exhausted') && (
                        <span className="text-muted-foreground">{t('cardToday')} <b className={cn('font-semibold tabular-nums', card === 'exhausted' ? 'text-destructive' : 'text-foreground')}>{formatUSD(user.monthly_used)} / {formatUSD(user.monthly_limit)}</b></span>
                    )}
                    <button type="button" onClick={onRedeem} className="inline-flex items-center gap-1 text-primary hover:underline md:hidden"><Ticket className="size-4" />{t('redeem')}</button>
                </div>
            )}
        </div>
    );
}
