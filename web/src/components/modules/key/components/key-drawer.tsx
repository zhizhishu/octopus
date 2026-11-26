'use client';

import { useMemo, useState } from 'react';
import * as DialogPrimitive from '@radix-ui/react-dialog';
import { ArrowRight, BookOpen, Check, Copy, Loader2, Logs, Search, Trash2, X } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { useCopyToClipboard } from '@uidotdev/usehooks';
import { toast } from '@/components/common/Toast';
import {
    useAPIKeyModels,
    useAPIKeyUsage,
    useCreateAPIKey,
    useUpdateAPIKey,
    type APIKey,
    type CreateAPIKeyRequest,
} from '@/api/endpoints/apikey';
import { useAuthStore, type User, type UserUsageRange } from '@/api/endpoints/user';
import type { AccessPlan } from '@/api/endpoints/access-plan';
import { Switch } from '@/components/ui/switch';
import { cn } from '@/lib/utils';
import { formatUSD } from '@/components/modules/user/lib/balance-preview';
import { userPlanIDs } from '@/components/modules/user/lib/user-filters';
import { PlanPicker } from '@/components/modules/user/components/plan-picker';
import { useRelTime } from '@/components/modules/user/components/shared';
import { UsageOverview, UsageRangeSeg } from '@/components/modules/user/components/user-usage-tab';
import { Bar, CheckMark, HelpTip, Pill, Seg, btn, btnDanger, btnPrimary, chip, chipOn, iconBtn, label } from '@/components/modules/user/components/ui-bits';
import { isPastExpire } from '../lib/expire';
import {
    KEY_FAMILIES,
    expiryState,
    expireAfterDays,
    isAllFamilies,
    keyFamilies,
    keyModels,
    keyPlanIDs,
    keyStatus,
    localInputToUnix,
    matchExpirePreset,
    maskKeyLong,
    normalizeMoneyInput,
    spendState,
    unixToLocalInput,
    type KeyFamily,
} from '../lib/key-filters';
import { KeyStatusPill, inputCls, useFamilyLabel } from './bits';

export type KeyDrawerTab = 'settings' | 'usage';
const EXPIRE_PRESETS = [1, 30, 90];

interface Draft {
    name: string;
    userID?: number;
    enabled: boolean;
    expireAt: number;
    cap: string;
    modelMode: 'all' | 'only';
    models: string[];
    families: KeyFamily[];
    planIDs: number[];
    defaultPlanID?: number;
}

function draftFrom(key: APIKey | null, owner: User | undefined, plans: AccessPlan[], nowSec: number): Draft {
    const models = keyModels(key);
    let planIDs: number[] = key ? keyPlanIDs(key) : owner ? userPlanIDs(owner) : [];
    if (planIDs.length === 0 && !key) {
        const enabled = plans.filter((p) => p.enabled);
        const fallback = enabled.find((p) => p.is_default) ?? enabled[0];
        planIDs = fallback ? [fallback.id] : [];
    }
    return {
        name: key?.name ?? '',
        userID: key?.user_id ?? owner?.id,
        enabled: key?.enabled ?? true,
        // 新建默认 30 天（和原来一样）；编辑保持原样。
        expireAt: key ? key.expire_at ?? 0 : expireAfterDays(30, nowSec),
        cap: key?.max_cost ? String(key.max_cost) : '',
        modelMode: models.length > 0 ? 'only' : 'all',
        models,
        families: key ? keyFamilies(key) : [...KEY_FAMILIES],
        planIDs,
        defaultPlanID: key?.default_access_plan_id || (owner && !key ? owner.default_access_plan_id : undefined) || planIDs[0],
    };
}

// 右侧宽抽屉：「设置」改这把 Key，「用量」看它花了多少。key=null 时是「新建 Key」。
// 桌面上两栏：左边是这把 Key 本身（名称、归属、开关、上限、有效期），右边是它能用什么（模型、协议、方案）。
export function KeyDrawer({ open, onOpenChange, apiKey, tab, onTabChange, users, plans, spentOf, onDelete, onGuide, onViewLogs }: {
    open: boolean;
    onOpenChange: (open: boolean) => void;
    apiKey: APIKey | null;
    tab: KeyDrawerTab;
    onTabChange: (tab: KeyDrawerTab) => void;
    users: User[];
    plans: AccessPlan[];
    spentOf: (id: number) => number;
    onDelete: (key: APIKey) => void;
    onGuide: (key: APIKey) => void;
    onViewLogs: (key: APIKey) => void;
}) {
    return (
        <DialogPrimitive.Root open={open} onOpenChange={onOpenChange}>
            <DialogPrimitive.Portal>
                <DialogPrimitive.Overlay className="data-[state=open]:animate-in data-[state=closed]:animate-out data-[state=closed]:fade-out-0 data-[state=open]:fade-in-0 fixed inset-0 z-50 bg-black/40" />
                <DialogPrimitive.Content
                    aria-describedby={undefined}
                    data-key-drawer
                    className="data-[state=open]:animate-in data-[state=closed]:animate-out data-[state=closed]:slide-out-to-right data-[state=open]:slide-in-from-right fixed inset-y-0 right-0 z-50 flex w-full flex-col border-l border-border bg-background shadow-2xl duration-200 sm:w-[600px] lg:w-[min(960px,calc(100vw-12rem))]"
                >
                    {open && (
                        <DrawerBody
                            key={apiKey?.id ?? 'new'}
                            apiKey={apiKey}
                            tab={apiKey ? tab : 'settings'}
                            onTabChange={onTabChange}
                            users={users}
                            plans={plans}
                            spentOf={spentOf}
                            onClose={() => onOpenChange(false)}
                            onDelete={onDelete}
                            onGuide={onGuide}
                            onViewLogs={onViewLogs}
                        />
                    )}
                </DialogPrimitive.Content>
            </DialogPrimitive.Portal>
        </DialogPrimitive.Root>
    );
}

function DrawerBody({ apiKey, tab, onTabChange, users, plans, spentOf, onClose, onDelete, onGuide, onViewLogs }: {
    apiKey: APIKey | null;
    tab: KeyDrawerTab;
    onTabChange: (tab: KeyDrawerTab) => void;
    users: User[];
    plans: AccessPlan[];
    spentOf: (id: number) => number;
    onClose: () => void;
    onDelete: (key: APIKey) => void;
    onGuide: (key: APIKey) => void;
    onViewLogs: (key: APIKey) => void;
}) {
    const t = useTranslations('keys.drawer');
    const me = useAuthStore((s) => s.user);
    const isAdmin = me?.role === 'admin';
    const [nowSec] = useState(() => Math.floor(Date.now() / 1000));
    const [draft, setDraft] = useState<Draft>(() => draftFrom(apiKey, users.find((u) => u.id === me?.id) ?? (me as User | undefined), plans, nowSec));
    const [range, setRange] = useState<UserUsageRange>('7d');
    const create = useCreateAPIKey();
    const update = useUpdateAPIKey();
    const pending = create.isPending || update.isPending;
    const set = (p: Partial<Draft>) => setDraft((d) => ({ ...d, ...p }));
    const spent = apiKey ? spentOf(apiKey.id) : 0;

    const capNum = draft.cap.trim() === '' ? 0 : Number(draft.cap);
    const capOk = Number.isFinite(capNum) && capNum >= 0;

    const changeOwner = (id: number) => {
        const owner = users.find((u) => u.id === id);
        const ids = owner ? userPlanIDs(owner) : [];
        // 换了人：方案跟着换成他自己的（他用不了的方案配上也没用）；限定的模型也清掉，因为能选的模型跟着人变。
        set({ userID: id, planIDs: ids.length > 0 ? ids : draft.planIDs, defaultPlanID: owner?.default_access_plan_id || ids[0] || draft.defaultPlanID, models: [], modelMode: 'all' });
    };

    const save = async () => {
        if (!draft.name.trim()) { toast.error(t('needName')); return; }
        if (!capOk) { toast.error(t('badCap')); return; }
        if (draft.modelMode === 'only' && draft.models.length === 0) { toast.error(t('needModel')); return; }
        if (draft.families.length === 0) { toast.error(t('needFamily')); return; }
        const expireChanged = !apiKey || (apiKey.expire_at ?? 0) !== draft.expireAt;
        if (expireChanged && isPastExpire(draft.expireAt)) { toast.error(t('pastExpire')); return; }
        const body: CreateAPIKeyRequest = {
            name: draft.name.trim(),
            user_id: isAdmin ? draft.userID : undefined,
            enabled: draft.enabled,
            expire_at: draft.expireAt,
            max_cost: capNum,
            supported_models: draft.modelMode === 'only' ? draft.models.join(',') : '',
            endpoint_families: draft.families,
        };
        if (isAdmin && draft.planIDs.length > 0) {
            body.access_plan_ids = draft.planIDs;
            body.default_access_plan_id = draft.defaultPlanID && draft.planIDs.includes(draft.defaultPlanID) ? draft.defaultPlanID : draft.planIDs[0];
        }
        try {
            if (apiKey) {
                await update.mutateAsync({ ...body, id: apiKey.id });
                toast.success(t('saved', { name: body.name }));
            } else {
                await create.mutateAsync(body);
                toast.success(t('created', { name: body.name }), { description: t('createdHint') });
            }
            onClose();
        } catch (error) {
            toast.error(t('saveFailed'), { description: (error as { message?: string } | null)?.message });
        }
    };

    return (
        <>
            <DrawerHead apiKey={apiKey} spent={spent} nowSec={nowSec} isAdmin={isAdmin} />
            {apiKey && (
                <div className="flex flex-wrap items-center justify-between gap-2 px-6 pt-4">
                    <Seg value={tab} onChange={onTabChange} options={[{ value: 'settings', label: t('settings') }, { value: 'usage', label: t('usage') }]} />
                    {tab === 'usage'
                        ? <UsageRangeSeg value={range} onChange={setRange} />
                        : <button type="button" className={cn(btn, 'h-8')} onClick={() => onGuide(apiKey)}><BookOpen className="size-4" />{t('guide')}</button>}
                </div>
            )}
            <div className="min-h-0 flex-1 overflow-y-auto px-6 pt-5 pb-6">
                {apiKey && tab === 'usage' ? (
                    <KeyUsageTab apiKey={apiKey} range={range} spent={spent} nowSec={nowSec} onViewLogs={() => onViewLogs(apiKey)} />
                ) : (
                    <div className="grid grid-cols-1 gap-4 lg:grid-cols-2 lg:items-start">
                        <div className="space-y-4">
                            <section className="space-y-3">
                                <div className={cn('grid grid-cols-1 gap-3', isAdmin && 'sm:grid-cols-2')}>
                                    <label className="block">
                                        <div className={label}>{t('name')}</div>
                                        <input className={inputCls} value={draft.name} maxLength={100} placeholder={t('namePlaceholder')} onChange={(e) => set({ name: e.target.value })} autoFocus={!apiKey} />
                                    </label>
                                    {isAdmin && (
                                        <label className="block">
                                            <div className={label}>{t('owner')}<HelpTip content={t('ownerTip')} align="end" /></div>
                                            <select className={inputCls} value={draft.userID ?? ''} onChange={(e) => changeOwner(Number(e.target.value))}>
                                                {users.map((u) => (
                                                    <option key={u.id} value={u.id}>{u.username}{u.role === 'admin' ? `（${t('roleAdmin')}）` : ''}</option>
                                                ))}
                                            </select>
                                        </label>
                                    )}
                                </div>
                                <label className="flex items-center gap-3 rounded-2xl border border-border/70 bg-muted/20 px-4 py-3">
                                    <div className="min-w-0 flex-1">
                                        <div className="text-sm font-medium">{draft.enabled ? t('enabledOn') : t('enabledOff')}</div>
                                        <div className="text-xs text-muted-foreground">{t('enabledHint')}</div>
                                    </div>
                                    <Switch checked={draft.enabled} onCheckedChange={(enabled) => set({ enabled })} />
                                </label>
                                {apiKey && <KeyLine value={apiKey.api_key} />}
                            </section>

                            <section className="space-y-4 rounded-2xl border border-border/70 bg-muted/20 p-4">
                                <CapField cap={draft.cap} capOk={capOk} capNum={capNum} spent={spent} showSpent={!!apiKey} onChange={(cap) => set({ cap })} />
                                <ExpiryField expireAt={draft.expireAt} nowSec={nowSec} onChange={(expireAt) => set({ expireAt })} />
                            </section>
                        </div>

                        <div className="space-y-4">
                            <ModelsField
                                ownerID={isAdmin ? draft.userID : undefined}
                                mode={draft.modelMode}
                                models={draft.models}
                                onMode={(modelMode) => set({ modelMode })}
                                onModels={(models) => set({ models })}
                            />
                            <FamiliesField value={draft.families} onChange={(families) => set({ families })} />
                            <section className="rounded-2xl border border-border/70 p-4">
                                <div className="mb-2 flex items-center justify-between">
                                    <h4 className="flex items-center gap-1 text-sm font-semibold">{t('plans')}<HelpTip content={isAdmin ? t('plansTip') : t('plansTipUser')} /></h4>
                                    {isAdmin && <span className="text-xs text-muted-foreground">{t('plansHint')}</span>}
                                </div>
                                {isAdmin ? (
                                    <PlanPicker
                                        plans={plans.filter((p) => p.enabled || draft.planIDs.includes(p.id))}
                                        value={draft.planIDs}
                                        defaultID={draft.defaultPlanID}
                                        onChange={(planIDs, defaultPlanID) => set({ planIDs, defaultPlanID })}
                                    />
                                ) : (
                                    <div className="flex flex-wrap gap-1.5">
                                        {(apiKey?.access_plans ?? []).length === 0
                                            ? <span className="text-xs text-muted-foreground">{t('plansAuto')}</span>
                                            : apiKey!.access_plans!.map((p) => (
                                                <Pill key={p.id} tone={p.id === (apiKey!.default_access_plan_id || apiKey!.access_plans![0].id) ? 'pri' : 'outline'}>{p.display_name?.trim() || p.slug}</Pill>
                                            ))}
                                    </div>
                                )}
                            </section>
                        </div>
                    </div>
                )}
            </div>
            {(!apiKey || tab === 'settings') && (
                <div className="flex items-center gap-2 border-t border-border bg-background px-6 py-4">
                    {apiKey && <button type="button" className={btnDanger} onClick={() => onDelete(apiKey)}><Trash2 className="size-4" />{t('delete')}</button>}
                    <button type="button" className={cn(btn, 'ml-auto')} onClick={onClose}>{t('cancel')}</button>
                    <button type="button" className={btnPrimary} disabled={pending} onClick={save}>
                        {pending ? <Loader2 className="size-4 animate-spin" /> : <Check className="size-4" />}
                        {apiKey ? t('save') : t('create')}
                    </button>
                </div>
            )}
        </>
    );
}

function DrawerHead({ apiKey, spent, nowSec, isAdmin }: { apiKey: APIKey | null; spent: number; nowSec: number; isAdmin: boolean }) {
    const t = useTranslations('keys.drawer');
    const rel = useRelTime();
    return (
        <div className="flex items-start justify-between gap-3 border-b border-border px-6 py-5">
            <div className="min-w-0">
                <DialogPrimitive.Title className="flex min-w-0 items-center gap-2">
                    <span className="truncate text-lg font-semibold">{apiKey ? apiKey.name : t('createTitle')}</span>
                    {apiKey && <KeyStatusPill status={keyStatus(apiKey, spent, nowSec)} />}
                </DialogPrimitive.Title>
                <div className="mt-1 truncate text-xs text-muted-foreground">
                    {apiKey ? (
                        <>
                            {isAdmin && apiKey.user_name ? `${t('ownedBy', { name: apiKey.user_name })} · ` : ''}#{apiKey.id}
                            {' · '}
                            {apiKey.last_used_at ? t('lastUsed', { when: rel(apiKey.last_used_at, nowSec), ip: apiKey.last_used_ip || '—' }) : t('neverUsed')}
                        </>
                    ) : t('createHint')}
                </div>
            </div>
            <DialogPrimitive.Close className={iconBtn} aria-label={t('close')}><X className="size-4" /></DialogPrimitive.Close>
        </div>
    );
}

/** 编辑时显示这把 Key（遮住中间）+ 复制完整 Key。 */
function KeyLine({ value }: { value: string }) {
    const t = useTranslations('keys.drawer');
    const [, copy] = useCopyToClipboard();
    return (
        <div className="flex items-center gap-2 rounded-2xl border border-border/70 px-4 py-2.5">
            <div className="min-w-0 flex-1">
                <div className="text-[11px] text-muted-foreground">{t('keyLabel')}</div>
                <code className="block truncate font-mono text-sm">{maskKeyLong(value)}</code>
            </div>
            <button type="button" className={cn(btn, 'h-8')} onClick={async () => { await copy(value); toast.success(t('copied')); }}>
                <Copy className="size-4" />{t('copy')}
            </button>
        </div>
    );
}

function CapField({ cap, capOk, capNum, spent, showSpent, onChange }: { cap: string; capOk: boolean; capNum: number; spent: number; showSpent: boolean; onChange: (v: string) => void }) {
    const t = useTranslations('keys.drawer');
    const s = spendState(spent, capOk ? capNum : 0);
    const unlimited = cap.trim() === '' || capNum === 0;
    return (
        <div>
            <div className={label}>{t('cap')}<HelpTip content={t('capTip')} /></div>
            <div className="flex items-center gap-2">
                <label className={cn('flex h-9 min-w-0 flex-1 items-center gap-1 rounded-xl border border-input bg-background px-3 text-sm focus-within:border-ring', !capOk && 'border-destructive')}>
                    <span className="text-muted-foreground">$</span>
                    <input inputMode="decimal" className="min-w-0 flex-1 bg-transparent outline-none placeholder:text-muted-foreground" value={cap} placeholder={t('capPlaceholder')} onChange={(e) => onChange(normalizeMoneyInput(e.target.value))} />
                </label>
                <button type="button" className={cn(chip, 'h-9', unlimited && chipOn)} onClick={() => onChange('')}>{t('noCap')}</button>
            </div>
            {showSpent && (
                unlimited ? (
                    <div className="mt-2 text-xs text-muted-foreground">{t('spentOnly', { spent: formatUSD(spent) })}</div>
                ) : (
                    <>
                        <div className="mt-2 flex items-center justify-between text-xs">
                            <span className="text-muted-foreground">{t('spentLeft', { spent: formatUSD(spent), left: formatUSD(s.left) })}</span>
                            {s.level === 'over' && <span className="text-destructive">{t('usedUp')}</span>}
                            {s.level === 'near' && <span className="text-amber-700 dark:text-amber-400">{t('nearCap')}</span>}
                        </div>
                        <Bar value={s.pct} tone={s.level === 'over' ? 'danger' : 'primary'} className={cn('mt-1.5', s.level === 'near' && '[&>i]:bg-amber-500')} />
                    </>
                )
            )}
        </div>
    );
}

function ExpiryField({ expireAt, nowSec, onChange }: { expireAt: number; nowSec: number; onChange: (v: number) => void }) {
    const t = useTranslations('keys.drawer');
    const te = useTranslations('keys.expiry');
    const preset = matchExpirePreset(expireAt, EXPIRE_PRESETS, nowSec);
    const e = expiryState(expireAt, nowSec);
    const tz = Intl.DateTimeFormat().resolvedOptions().timeZone;
    return (
        <div>
            <div className={label}>{t('expire')}<HelpTip content={t('expireTip')} /></div>
            <div className="flex flex-wrap items-center gap-2">
                <input
                    type="datetime-local"
                    className={cn(inputCls, 'w-auto min-w-[13rem] flex-1', e.kind === 'past' && 'border-destructive')}
                    value={unixToLocalInput(expireAt)}
                    min={unixToLocalInput(nowSec)}
                    onChange={(ev) => onChange(localInputToUnix(ev.target.value))}
                />
                <div className="flex flex-wrap gap-1.5">
                    <button type="button" className={cn(chip, preset === 0 && chipOn)} onClick={() => onChange(0)}>{te('forever')}</button>
                    {EXPIRE_PRESETS.map((d) => (
                        <button key={d} type="button" className={cn(chip, preset === d && chipOn)} onClick={() => onChange(expireAfterDays(d, nowSec))}>{t('plusDays', { n: d })}</button>
                    ))}
                </div>
            </div>
            <div className={cn('mt-1.5 text-xs', e.kind === 'past' ? 'text-destructive' : 'text-muted-foreground')}>
                {e.kind === 'forever' ? t('foreverHint') : e.kind === 'past' ? t('pastHint') : t('expireHint', { tz, n: e.days })}
            </div>
        </div>
    );
}

function ModelsField({ ownerID, mode, models, onMode, onModels }: {
    ownerID?: number;
    mode: 'all' | 'only';
    models: string[];
    onMode: (m: 'all' | 'only') => void;
    onModels: (m: string[]) => void;
}) {
    const t = useTranslations('keys.drawer');
    const { data: available = [], isLoading } = useAPIKeyModels(ownerID, { enabled: mode === 'only' });
    const [q, setQ] = useState('');
    const list = useMemo(() => {
        // 已选但现在列表里没有的（下线了 / 换了人）也列出来，方便取消。
        const extra = models.filter((m) => !available.includes(m));
        return [...extra, ...available].filter((m) => !q.trim() || m.toLowerCase().includes(q.trim().toLowerCase()));
    }, [available, models, q]);
    const toggle = (m: string) => onModels(models.includes(m) ? models.filter((x) => x !== m) : [...models, m]);
    return (
        <section className="rounded-2xl border border-border/70 p-4">
            <div className="mb-2 flex items-center justify-between gap-2">
                <h4 className="flex items-center gap-1 text-sm font-semibold">{t('models')}<HelpTip content={t('modelsTip')} /></h4>
                <Seg size="sm" value={mode} onChange={onMode} options={[{ value: 'all', label: t('modelsAll') }, { value: 'only', label: t('modelsOnly') }]} />
            </div>
            {mode === 'all' ? (
                <div className="text-xs text-muted-foreground">{t('modelsAllHint')}</div>
            ) : (
                <div>
                    <div className="flex items-center gap-2">
                        <label className="flex h-8 min-w-0 flex-1 items-center gap-2 rounded-lg border border-input bg-background px-2.5 text-xs">
                            <Search className="size-3.5 shrink-0 text-muted-foreground" />
                            <input value={q} onChange={(e) => setQ(e.target.value)} placeholder={t('modelsSearch')} className="min-w-0 flex-1 bg-transparent outline-none placeholder:text-muted-foreground" />
                        </label>
                        <span className="shrink-0 text-xs text-muted-foreground">{t('modelsPicked', { n: models.length })}</span>
                        {models.length > 0 && <button type="button" className="shrink-0 text-xs text-primary hover:underline" onClick={() => onModels([])}>{t('modelsClear')}</button>}
                    </div>
                    <div className="mt-2 max-h-64 overflow-y-auto rounded-xl border border-border/70">
                        {isLoading ? (
                            <div className="flex justify-center py-6 text-muted-foreground"><Loader2 className="size-4 animate-spin" /></div>
                        ) : list.length === 0 ? (
                            <div className="px-3 py-6 text-center text-xs text-muted-foreground">{available.length === 0 ? t('modelsNone') : t('modelsNoMatch')}</div>
                        ) : list.map((m) => {
                            const on = models.includes(m);
                            const gone = !available.includes(m);
                            return (
                                <button key={m} type="button" onClick={() => toggle(m)} className={cn('flex w-full items-center gap-2 border-b border-border/50 px-3 py-1.5 text-left text-sm last:border-b-0 hover:bg-muted/50', on && 'bg-primary/5')}>
                                    <CheckMark checked={on} />
                                    <span className="min-w-0 flex-1 truncate font-mono text-xs">{m}</span>
                                    {gone && <span className="shrink-0 text-[11px] text-amber-700 dark:text-amber-400">{t('modelGone')}</span>}
                                </button>
                            );
                        })}
                    </div>
                    <div className="mt-1.5 text-xs text-muted-foreground">{t('modelsOnlyHint')}</div>
                </div>
            )}
        </section>
    );
}

function FamiliesField({ value, onChange }: { value: KeyFamily[]; onChange: (v: KeyFamily[]) => void }) {
    const t = useTranslations('keys.drawer');
    const famLabel = useFamilyLabel();
    const toggle = (f: KeyFamily) => {
        const next = value.includes(f) ? value.filter((x) => x !== f) : [...value, f];
        if (next.length > 0) onChange(KEY_FAMILIES.filter((x) => next.includes(x)));
    };
    return (
        <section className="rounded-2xl border border-border/70 p-4">
            <div className="mb-2 flex items-center justify-between">
                <h4 className="flex items-center gap-1 text-sm font-semibold">{t('families')}<HelpTip content={t('familiesTip')} /></h4>
                <span className="text-xs text-muted-foreground">{isAllFamilies(value) ? t('familiesAll') : t('familiesSome', { n: value.length })}</span>
            </div>
            <div className="flex flex-wrap gap-2">
                {KEY_FAMILIES.map((f) => {
                    const on = value.includes(f);
                    return (
                        <button key={f} type="button" onClick={() => toggle(f)} disabled={on && value.length === 1} title={t(`familyPath.${f}`)} className={cn(chip, 'h-9 px-3', on && chipOn)}>
                            {on && <Check className="size-3.5" />}{famLabel(f)}
                        </button>
                    );
                })}
            </div>
            <div className="mt-1.5 text-xs text-muted-foreground">{t('familiesHint')}</div>
        </section>
    );
}

function KeyUsageTab({ apiKey, range, spent, nowSec, onViewLogs }: { apiKey: APIKey; range: UserUsageRange; spent: number; nowSec: number; onViewLogs: () => void }) {
    const t = useTranslations('keys.drawer');
    const tu = useTranslations('user.usage');
    const rel = useRelTime();
    const { data: usage, isLoading } = useAPIKeyUsage(apiKey.id, range);
    const s = spendState(spent, apiKey.max_cost);
    return (
        <div className="space-y-5">
            <div className="grid grid-cols-2 gap-2">
                <div className="rounded-2xl border border-border/70 bg-muted/20 px-4 py-3">
                    <div className="flex items-center gap-1 text-xs text-muted-foreground">{t('total')}<HelpTip content={t('totalTip')} /></div>
                    <div className="mt-1 flex items-baseline gap-1">
                        <span className="text-lg font-semibold tabular-nums">{formatUSD(spent)}</span>
                        {s.limited && <span className="text-xs text-muted-foreground">/ {formatUSD(apiKey.max_cost ?? 0)}</span>}
                    </div>
                    {s.limited ? <Bar value={s.pct} tone={s.level === 'over' ? 'danger' : 'primary'} className={cn('mt-1.5', s.level === 'near' && '[&>i]:bg-amber-500')} /> : <div className="text-[11px] text-muted-foreground">{t('noCap')}</div>}
                </div>
                <div className="rounded-2xl border border-border/70 bg-muted/20 px-4 py-3">
                    <div className="text-xs text-muted-foreground">{t('lastUsedTitle')}</div>
                    <div className="mt-1 text-lg font-semibold">{apiKey.last_used_at ? rel(apiKey.last_used_at, nowSec) : t('neverUsed')}</div>
                    <div className="font-mono text-[11px] text-muted-foreground">{apiKey.last_used_ip || '—'}</div>
                </div>
            </div>
            <UsageOverview usage={usage} loading={isLoading} range={range} costTip={tu('costTip')} wide />
            <button type="button" onClick={onViewLogs} className="flex w-full items-center justify-between rounded-2xl bg-muted/40 px-3 py-2.5 text-sm hover:bg-muted/70">
                <span className="flex items-center gap-2"><Logs className="size-4" />{t('viewLogs')}</span>
                <ArrowRight className="size-4 text-muted-foreground" />
            </button>
        </div>
    );
}
