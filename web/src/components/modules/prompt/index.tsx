'use client';

import { createContext, useContext, useMemo, useState, type Dispatch, type ReactNode, type SetStateAction } from 'react';
import {
    ArrowRight,
    ChevronDown,
    CircleHelp,
    GitBranch,
    KeyRound,
    Layers3,
    Loader2,
    MessageSquareText,
    Radio,
    Save,
    Search,
    ShieldCheck,
    WandSparkles,
    X,
} from 'lucide-react';
import type { LucideIcon } from 'lucide-react';
import type { ApiError } from '@/api/types';
import {
    type AccessPlan,
    type PromptAudience,
    type PromptOverrideMode,
    useAccessPlanList,
    useUpdateAccessPlanPrompt,
    useUpdateAccessPlanRoutePrompt,
} from '@/api/endpoints/access-plan';
import { type Channel, useChannelList, useUpdateChannel } from '@/api/endpoints/channel';
import { SettingKey, useSetSetting, useSettingList } from '@/api/endpoints/setting';
import { useAuthStore } from '@/api/endpoints/user';
import { PageWrapper } from '@/components/common/PageWrapper';
import { toast } from '@/components/common/Toast';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Switch } from '@/components/ui/switch';
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover';
import { InfoTip } from '../dashboard/parts';
import { useNavStore } from '../navbar/nav-store';
import { cn } from '@/lib/utils';
import { matchesPromptSearch, planRouteRows, sortChannelsStable, type RoutePromptRow } from './prompt-rows';
import { normalizePromptMode, promptEnabledFromSaved, promptLayerActive, promptStatus } from './prompt-mode';
import { PromptLayerControls } from './PromptLayerControls';
import { PromptIdentityBuilder } from './PromptIdentityBuilder';
import { appendIdentityPreset, buildIdentityPreset, type IdentityPresetMode } from './prompt-identity';
import { acceptPromptSave, channelIDsForPlan, channelPlanNames, editPromptDraft, groupChannelsByType, normalizeAudience, promptValuesEqual, type PromptDraftMap, type PromptValue } from './prompt-ui-state';

// 提示词页是系统提示词唯一的编辑入口：全局、每个方案、方案里的每条路由、每个渠道。
// 「方案」页只管方案和路由，不再出现提示词；保存时各写各的字段，互不覆盖。

function apiErrorMessage(error: unknown): string | undefined {
    return (error as ApiError | undefined)?.message;
}

const IdentityTemplateContext = createContext('');
const PromptDraftsContext = createContext<{ drafts: PromptDraftMap; setDrafts: Dispatch<SetStateAction<PromptDraftMap>> } | null>(null);

function savedValue(enabled: boolean | null | undefined, mode: string | null | undefined, text: string | null | undefined, audience?: string | null): PromptValue {
    return { text: text ?? '', mode: normalizePromptMode(mode), enabled: promptEnabledFromSaved(enabled, mode, text), audience: normalizeAudience(audience) };
}

function PromptStatusBadge({ value }: { value: PromptValue }) {
    const status = promptStatus(value.enabled, value.mode, value.text);
    return (
        <Badge
            variant={status.tone === 'on' ? 'secondary' : 'outline'}
            title={status.hint}
            className={cn('shrink-0', status.tone === 'empty' && 'text-muted-foreground')}
        >
            {status.label}
        </Badge>
    );
}

/** 一层提示词的编辑区：启用 + 前面/后面 + 文本 + 保存。关掉时文本框变灰、文字保留。 */
function PromptEditor({
    scopeKey,
    saved,
    onSave,
    saving,
    label,
    placeholder,
    size = 'min-h-24',
}: {
    scopeKey: string;
    saved: PromptValue;
    onSave: (next: PromptValue) => Promise<unknown>;
    saving: boolean;
    label: string;
    placeholder: string;
    size?: string;
}) {
    const identityPreset = useContext(IdentityTemplateContext);
    const draftsStore = useContext(PromptDraftsContext);
    const [localValue, setLocalValue] = useState<PromptValue>(saved);
    const draft = draftsStore?.drafts[scopeKey];
    const value = draftsStore ? (draft?.value ?? saved) : localValue;
    const baseline = draft?.baseline ?? saved;
    const dirty = !promptValuesEqual(value, baseline);
    const remoteConflict = Boolean(draft && dirty && !promptValuesEqual(saved, draft.baseline) && !promptValuesEqual(saved, value));
    const setValue = (update: (current: PromptValue) => PromptValue) => {
        if (draftsStore) {
            draftsStore.setDrafts((current) => editPromptDraft(current, scopeKey, saved, update));
        } else {
            setLocalValue(update);
        }
    };
    const save = async () => {
        const submitted = { ...value, text: value.text.trim() };
        try {
            await onSave(submitted);
            draftsStore?.setDrafts((current) => acceptPromptSave(current, scopeKey, submitted));
            toast.success(`${label} 已保存`);
        } catch (error) {
            toast.error(`${label} 保存失败`, { description: apiErrorMessage(error) });
        }
    };
    return (
        <div className="grid gap-2.5">
            <div className="flex flex-wrap items-center gap-2">
                <PromptLayerControls
                    enabled={value.enabled}
                    onEnabledChange={(enabled) => setValue((v) => ({ ...v, enabled }))}
                    mode={value.mode}
                    onModeChange={(mode) => setValue((v) => ({ ...v, mode }))}
                    label={label}
                />
                <Button
                    type="button"
                    size="sm"
                    variant="outline"
                    className="min-h-11 shrink-0 rounded-xl px-2.5 sm:min-h-8"
                    disabled={saving || !identityPreset}
                    onClick={() => setValue((current) => ({ ...current, text: appendIdentityPreset(current.text, identityPreset) }))}
                    title="只填入这一层的草稿，不自动保存、启用或删除原提示词"
                >
                    <WandSparkles className="size-4" />
                    插入模板
                </Button>
                <Button
                    type="button"
                    size="sm"
                    variant={dirty ? 'default' : 'outline'}
                    className={cn('ml-auto h-8 w-8 shrink-0 rounded-xl px-0 sm:w-auto sm:px-3', !dirty && 'disabled:opacity-60')}
                    disabled={saving || !dirty}
                    onClick={save}
                    aria-label={dirty ? '保存' : '已保存'}
                    title={dirty ? '保存这一层的改动' : '没有改动'}
                >
                    {saving ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}
                    <span className="hidden sm:inline">{dirty ? '保存' : '已保存'}</span>
                </Button>
            </div>
            <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
                <label htmlFor={`audience-${scopeKey.replace(/[^a-zA-Z0-9_-]/g, '-')}`}>影响对象</label>
                <select
                    id={`audience-${scopeKey.replace(/[^a-zA-Z0-9_-]/g, '-')}`}
                    value={value.audience}
                    disabled={saving}
                    onChange={(event) => setValue((current) => ({ ...current, audience: event.target.value as PromptAudience }))}
                    className="min-h-11 max-w-full rounded-xl border border-input bg-background px-3 text-sm text-foreground sm:min-h-9"
                >
                    <option value="all">全部用户（含管理员）</option>
                    <option value="admin_only">仅管理员 API Key 请求</option>
                </select>
                {dirty ? <span className="font-medium text-amber-600">有未保存草稿</span> : null}
                {remoteConflict ? <span className="text-destructive">服务端已更新，请核对后保存</span> : null}
            </div>
            <textarea
                value={value.text}
                onChange={(event) => setValue((v) => ({ ...v, text: event.target.value }))}
                placeholder={placeholder}
                aria-label={label}
                disabled={!value.enabled}
                title={value.enabled ? undefined : '已关闭，文字保留；打开「启用」后才能改、才会生效。'}
                className={cn(
                    size,
                    'w-full rounded-xl border border-border bg-background px-3 py-2 text-sm leading-6 text-foreground transition-opacity focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring',
                    !value.enabled && 'bg-muted/40 text-muted-foreground opacity-60'
                )}
            />
        </div>
    );
}

/** 可折叠的一行（路由 / 渠道）：收起时显示状态和文字预览，展开是编辑区。 */
function LayerRow({
    title,
    meta,
    saved,
    defaultOpen,
    children,
}: {
    title: ReactNode;
    meta?: ReactNode;
    saved: PromptValue;
    defaultOpen: boolean;
    children: ReactNode;
}) {
    const [open, setOpen] = useState(defaultOpen);
    const preview = saved.text.trim();
    return (
        <div className={cn('rounded-2xl border bg-background/60 transition-colors', open ? 'border-border' : 'border-border/60 hover:border-border')}>
            <button
                type="button"
                onClick={() => setOpen((v) => !v)}
                aria-expanded={open}
                className="flex w-full min-w-0 items-center gap-3 px-3 py-2.5 text-left"
            >
                <div className="min-w-0 flex-1">
                    <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
                        <span className="min-w-0 truncate text-sm font-medium">{title}</span>
                        {meta}
                    </div>
                    {!open && preview ? (
                        <p className={cn('mt-0.5 truncate text-xs', saved.enabled ? 'text-muted-foreground' : 'text-muted-foreground/60 line-through decoration-muted-foreground/40')}>{preview}</p>
                    ) : null}
                </div>
                <PromptStatusBadge value={saved} />
                <ChevronDown className={cn('size-4 shrink-0 text-muted-foreground transition-transform', open && 'rotate-180')} />
            </button>
            {open ? <div className="border-t border-border/60 p-3">{children}</div> : null}
        </div>
    );
}

function LayerCard({
    icon: Icon,
    title,
    description,
    enabled,
}: {
    icon: LucideIcon;
    title: string;
    description: string;
    enabled: boolean;
}) {
    return (
        <div className="rounded-3xl border border-border bg-card p-4">
            <div className="flex items-start gap-3">
                <div className={cn(
                    'flex size-10 items-center justify-center rounded-2xl border',
                    enabled ? 'border-primary/30 bg-primary/10 text-primary' : 'border-border bg-muted/30 text-muted-foreground'
                )}>
                    <Icon className="size-5" />
                </div>
                <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-2">
                        <h3 className="text-sm font-semibold">{title}</h3>
                        <Badge variant={enabled ? 'default' : 'outline'}>{enabled ? '已启用' : '未启用'}</Badge>
                    </div>
                    <p className="mt-1 text-xs leading-5 text-muted-foreground">{description}</p>
                </div>
            </div>
        </div>
    );
}

function UserPromptView() {
    const setActiveItem = useNavStore((state) => state.setActiveItem);

    return (
        <PageWrapper className="h-full min-h-0 overflow-y-auto overscroll-contain space-y-5 rounded-t-3xl pb-24 md:pb-4">
            <section className="rounded-3xl border border-border bg-card p-6">
                <div className="flex flex-wrap items-start justify-between gap-4">
                    <div className="max-w-2xl">
                        <div className="mb-3 inline-flex items-center gap-2 rounded-full border border-border bg-muted/30 px-3 py-1 text-xs text-muted-foreground">
                            <MessageSquareText className="size-3.5" />
                            个人侧提示词
                        </div>
                        <h2 className="text-2xl font-bold">提示词由管理员统一管理</h2>
                        <p className="mt-2 text-sm leading-6 text-muted-foreground">
                            你的请求仍然可以自带 system / developer 提示词；管理员可以在全局、方案、模型路由、渠道四层把提示词放在你的提示词前面或后面，你自带的不会被删。为避免泄露管理策略，普通用户不展示完整覆盖内容。
                        </p>
                    </div>
                    <Button type="button" className="rounded-xl" onClick={() => setActiveItem('key')}>
                        <KeyRound className="size-4" />
                        查看 API Key
                    </Button>
                </div>
            </section>

            <div className="grid gap-4 md:grid-cols-3">
                <LayerCard
                    icon={ShieldCheck}
                    title="管理员策略"
                    description="管理员可在全局、方案、模型路由、渠道四层给请求加系统提示词，不会删掉你自带的。"
                    enabled
                />
                <LayerCard
                    icon={GitBranch}
                    title="模型路由"
                    description="如果你的 API Key 绑定了不同方案，同一个请求模型可能应用不同的提示词策略。"
                    enabled
                />
                <LayerCard
                    icon={Radio}
                    title="渠道约束"
                    description="最终转发到不同上游渠道时，渠道级提示词排在最外面。"
                    enabled
                />
            </div>
        </PageWrapper>
    );
}


type LayerFilter = 'all' | 'global' | 'plan' | 'channel';

const SECTION = 'rounded-3xl border border-border bg-card p-4 sm:p-5';

/** 区块标题一行：图标 + 标题 + ?，右边是状态或数量；手机上也不换行。 */
function SectionHeader({ icon: Icon, title, tip, right }: { icon: LucideIcon; title: string; tip: ReactNode; right?: ReactNode }) {
    return (
        <div className="mb-3 flex min-w-0 items-center justify-between gap-2">
            <h3 className="flex min-w-0 items-center gap-1.5 text-base font-semibold">
                <Icon className="size-[18px] shrink-0 text-muted-foreground" />
                <span className="truncate">{title}</span>
                <InfoTip label={`${title}说明`}>{tip}</InfoTip>
            </h3>
            {right}
        </div>
    );
}

function GlobalSection({ settings }: { settings: Array<{ key: string; value: string }> | undefined }) {
    const setSetting = useSetSetting();
    const get = (key: string) => settings?.find((item) => item.key === key)?.value;
    const saved = savedValue(get(SettingKey.PromptOverrideEnabled) === 'true', get(SettingKey.PromptOverrideMode), get(SettingKey.PromptOverrideSystem), get(SettingKey.PromptAudience));
    const save = async (next: PromptValue) => {
        if (next.audience === 'admin_only') {
            await setSetting.mutateAsync({ key: SettingKey.PromptOverrideEnabled, value: 'false' });
        }
        await setSetting.mutateAsync({ key: SettingKey.PromptAudience, value: next.audience });
        await setSetting.mutateAsync({ key: SettingKey.PromptOverrideMode, value: next.mode });
        await setSetting.mutateAsync({ key: SettingKey.PromptOverrideSystem, value: next.text });
        await setSetting.mutateAsync({ key: SettingKey.PromptOverrideEnabled, value: next.enabled ? 'true' : 'false' });
    };
    return (
        <section className={SECTION}>
            <SectionHeader
                icon={ShieldCheck}
                title="全局"
                tip="所有请求都会带上。几层叠加时，全局离请求自带的提示词最近（最里层）。"
                right={<PromptStatusBadge value={saved} />}
            />
            <PromptEditor
                scopeKey="global"
                saved={saved}
                onSave={save}
                saving={setSetting.isPending}
                label="全局提示词"
                placeholder="可选：全站默认的系统提示词"
                size="min-h-28"
            />
        </section>
    );
}

function RouteRow({ row, defaultOpen }: { row: RoutePromptRow; defaultOpen: boolean }) {
    const update = useUpdateAccessPlanRoutePrompt();
    const saved: PromptValue = { text: row.text, mode: row.mode, enabled: row.enabled, audience: row.audience };
    const upstream = row.upstreamModels.join(' / ');
    return (
        <LayerRow
            saved={saved}
            defaultOpen={defaultOpen}
            title={<span className="font-mono">{row.requestModel}</span>}
            meta={
                <span className="flex min-w-0 items-center gap-1 text-xs text-muted-foreground">
                    <ArrowRight className="size-3 shrink-0" />
                    <span className="truncate font-mono">{upstream || '-'}</span>
                    <span className="shrink-0">· {row.targetCount} 个渠道</span>
                </span>
            }
        >
            <PromptEditor
                scopeKey={`route:${row.plan.id}:${row.requestModel.toLowerCase()}`}
                saved={saved}
                onSave={(next) => update.mutateAsync({
                    access_plan_id: row.plan.id,
                    request_model: row.requestModel,
                    system_prompt_override: next.text,
                    prompt_override_mode: next.mode,
                    prompt_override_enabled: next.enabled,
                    prompt_audience: next.audience,
                })}
                saving={update.isPending}
                label={`${row.plan.display_name || row.plan.slug} · ${row.requestModel} 提示词`}
                placeholder="可选：只对这个模型生效的系统提示词"
            />
        </LayerRow>
    );
}

function SubLabel({ icon: Icon, label, tip, right }: { icon: LucideIcon; label: string; tip: ReactNode; right?: ReactNode }) {
    return (
        <div className="mb-2 flex min-h-6 items-center justify-between gap-2">
            <span className="flex items-center gap-1 text-xs font-medium text-muted-foreground">
                <Icon className="size-3.5" />
                {label}
                <InfoTip label={`${label}层说明`}>{tip}</InfoTip>
            </span>
            {right}
        </div>
    );
}

function PlanCard({ plan, routes, totalRoutes, query, onlyFilled }: { plan: AccessPlan; routes: RoutePromptRow[]; totalRoutes: number; query: string; onlyFilled: boolean }) {
    const update = useUpdateAccessPlanPrompt();
    const [open, setOpen] = useState(false);
    const setActiveItem = useNavStore((state) => state.setActiveItem);
    const saved = savedValue(plan.prompt_override_enabled, plan.prompt_override_mode, plan.system_prompt_override, plan.prompt_audience);
    const routeOn = routes.filter((row) => promptLayerActive(row.enabled, row.mode, row.text)).length;
    const name = plan.display_name || plan.slug;
    return (
        <article className="rounded-2xl border border-border bg-background/50">
            <header className="border-b border-border/70 px-3 sm:px-4">
                <button type="button" aria-expanded={open} aria-label={`展开或收起 ${name} 方案`} onClick={() => setOpen((current) => !current)} className="flex min-h-11 w-full min-w-0 items-center gap-2 py-2.5 text-left">
                <ChevronDown className={cn('size-4 shrink-0 text-muted-foreground transition-transform', open && 'rotate-180')} />
                <span className="truncate text-sm font-semibold">{name}</span>
                <span className="hidden font-mono text-xs text-muted-foreground sm:inline">{plan.slug}</span>
                {plan.is_default ? <Badge className="shrink-0">默认</Badge> : null}
                {!plan.enabled ? <Badge variant="outline" className="shrink-0">已停用</Badge> : null}
                <span className="ml-auto shrink-0 text-xs text-muted-foreground" title={`${totalRoutes} 条路由，其中 ${routeOn} 条的提示词已启用`}>
                    路由 {routeOn}/{totalRoutes}
                </span>
                </button>
            </header>
            {open ? <div className="grid gap-4 p-3 sm:p-4 xl:grid-cols-[minmax(0,5fr)_minmax(0,7fr)]">
                <div className="min-w-0">
                    <SubLabel
                        icon={Layers3}
                        label="方案"
                        tip="对这个方案的所有请求生效。排在全局外面、路由里面。"
                        right={<PromptStatusBadge value={saved} />}
                    />
                    <PromptEditor
                        scopeKey={`plan:${plan.id}`}
                        saved={saved}
                        onSave={(next) => update.mutateAsync({
                            id: plan.id,
                            system_prompt_override: next.text,
                            prompt_override_mode: next.mode,
                            prompt_override_enabled: next.enabled,
                            prompt_audience: next.audience,
                        })}
                        saving={update.isPending}
                        label={`${name} 方案提示词`}
                        placeholder="可选：这个方案的系统提示词"
                        size="min-h-28"
                    />
                </div>
                <div className="min-w-0">
                    <SubLabel
                        icon={GitBranch}
                        label="路由"
                        tip="只对这个方案里的某个请求模型生效，排在方案外面。路由本身（走哪些渠道）在「方案」页管理。"
                        right={query || onlyFilled ? <span className="text-xs text-muted-foreground">{routes.length}/{totalRoutes}</span> : null}
                    />
                    {routes.length === 0 ? (
                        <div className="flex flex-wrap items-center justify-between gap-2 rounded-2xl border border-dashed border-border px-3 py-3 text-xs text-muted-foreground">
                            <span>{totalRoutes === 0 ? '还没有路由' : '没有符合条件的路由'}</span>
                            {totalRoutes === 0 ? (
                                <Button type="button" variant="outline" size="sm" className="h-7 rounded-lg" onClick={() => setActiveItem('access-plan')}>
                                    去「方案」页添加
                                </Button>
                            ) : null}
                        </div>
                    ) : (
                        <div className="grid gap-2">
                            {routes.map((row) => (
                                <RouteRow key={`${plan.id}-${row.requestModel}`} row={row} defaultOpen={false} />
                            ))}
                        </div>
                    )}
                </div>
            </div> : null}
        </article>
    );
}

function ChannelRow({ channel, relatedPlans }: { channel: Channel; relatedPlans: string[] }) {
    const update = useUpdateChannel();
    const saved = savedValue(channel.prompt_override_enabled, channel.prompt_override_mode, channel.system_prompt_override, channel.prompt_audience);
    return (
        <LayerRow
            saved={saved}
            defaultOpen={false}
            title={<>#{channel.id} {channel.name}</>}
            meta={
                <span className="flex min-w-0 flex-wrap items-center gap-1 text-xs text-muted-foreground">
                    {!channel.enabled ? <Badge variant="outline">渠道已停用</Badge> : null}
                    {relatedPlans.length ? <span className="truncate" title={relatedPlans.join('、')}>关联 {relatedPlans.join('、')}</span> : null}
                </span>
            }
        >
            {relatedPlans.length > 1 ? (
                <p className="mb-3 rounded-xl border border-amber-500/30 bg-amber-500/5 px-3 py-2 text-xs leading-5">
                    此渠道被 {relatedPlans.join('、')} 共同引用。修改渠道级规则会影响所有命中此渠道的方案，按套餐筛选不代表仅对该套餐生效。
                </p>
            ) : null}
            <PromptEditor
                scopeKey={`channel:${channel.id}`}
                saved={saved}
                onSave={(next) => update.mutateAsync({
                    id: channel.id,
                    system_prompt_override: next.text,
                    prompt_override_mode: next.mode,
                    prompt_override_enabled: next.enabled,
                    prompt_audience: next.audience,
                })}
                saving={update.isPending}
                label={`${channel.name} 渠道提示词`}
                placeholder="可选：转发到这个渠道时加的系统提示词"
            />
        </LayerRow>
    );
}

type OrderLayer = { layer: string; names: string[] };

function orderNames(names: string[]) {
    if (names.length === 0) return null;
    if (names.length <= 2) return names.join('、');
    return `${names.slice(0, 2).join('、')} 等 ${names.length} 个`;
}

function OrderStack({ title, rows }: { title: string; rows: OrderLayer[] }) {
    return (
        <div className="grid gap-1">
            <span className="text-[11px] font-medium text-muted-foreground">{title}</span>
            <ol className="grid gap-1">
                {rows.map((row) => {
                    const names = orderNames(row.names);
                    return (
                        <li key={row.layer} className="flex min-w-0 items-center gap-2 rounded-lg border border-border/70 bg-background px-2 py-1">
                            <span className="w-8 shrink-0 text-xs font-medium text-foreground">{row.layer}</span>
                            <span className={cn('min-w-0 truncate text-xs', names ? 'text-foreground/80' : 'text-muted-foreground/50')}>{names ?? '—'}</span>
                        </li>
                    );
                })}
            </ol>
        </div>
    );
}

/** 「拼接顺序」：图示 + 当前筛选下真实已启用的层。 */
function OrderPopover({ before, after }: { before: OrderLayer[]; after: OrderLayer[] }) {
    const [open, setOpen] = useState(false);
    return (
        <Popover open={open} onOpenChange={setOpen}>
            <PopoverTrigger
                className="inline-flex h-7 shrink-0 items-center gap-1 rounded-lg px-1.5 text-xs text-muted-foreground transition-colors hover:text-foreground"
                onPointerEnter={(e) => { if (e.pointerType === 'mouse') setOpen(true); }}
                onPointerLeave={(e) => { if (e.pointerType === 'mouse') setOpen(false); }}
            >
                拼接顺序
                <CircleHelp className="size-3.5" />
            </PopoverTrigger>
            <PopoverContent align="end" className="w-[min(20rem,calc(100vw-1rem))] rounded-2xl p-3" onOpenAutoFocus={(e) => e.preventDefault()}>
                <div className="grid gap-2">
                    <OrderStack title="放在前面 ↓ 越具体越靠上" rows={before} />
                    <div className="rounded-lg border border-primary/40 bg-primary/10 px-2 py-1.5 text-center text-xs font-medium text-primary">请求自带的系统提示词</div>
                    <OrderStack title="放在后面 ↓ 越具体越靠下" rows={after} />
                    <p className="text-[11px] leading-4 text-muted-foreground">
                        右边是当前筛选下已启用的层。一个请求只会用到它所在的一个方案、一条路由、一个渠道；没启用的层不加。
                    </p>
                </div>
            </PopoverContent>
        </Popover>
    );
}

function AdminPromptView() {
    const { data: settings, isLoading: settingsLoading } = useSettingList();
    const { data: plans = [], isLoading: plansLoading } = useAccessPlanList();
    const { data: channelItems = [], isLoading: channelsLoading } = useChannelList();
    const channels = useMemo(() => sortChannelsStable(channelItems.map((item) => item.raw)), [channelItems]);
    const [query, setQuery] = useState('');
    const [layer, setLayer] = useState<LayerFilter>('all');
    const [onlyFilled, setOnlyFilled] = useState(false);
    const [drafts, setDrafts] = useState<PromptDraftMap>({});
    const draftContext = useMemo(() => ({ drafts, setDrafts }), [drafts]);
    const [focusedPlanId, setFocusedPlanId] = useState('all');
    const [channelStatusFilter, setChannelStatusFilter] = useState<'all' | 'active' | 'configured' | 'empty'>('all');
    const [expandedChannelTypes, setExpandedChannelTypes] = useState<Set<number>>(new Set());
    const [groupPageSize, setGroupPageSize] = useState<Record<number, number>>({});
    const [identityMode, setIdentityMode] = useState<IdentityPresetMode>('brand');
    const [identityName, setIdentityName] = useState('Octopus 助手');
    const [identityStyle, setIdentityStyle] = useState('专业、清晰、简洁');
    const identityPreset = useMemo(
        () => buildIdentityPreset({ mode: identityMode, displayName: identityName, style: identityStyle }),
        [identityMode, identityName, identityStyle]
    );

    const get = (key: string) => settings?.find((item) => item.key === key)?.value;
    const globalSaved = savedValue(get(SettingKey.PromptOverrideEnabled) === 'true', get(SettingKey.PromptOverrideMode), get(SettingKey.PromptOverrideSystem), get(SettingKey.PromptAudience));
    const globalActive = promptLayerActive(globalSaved.enabled, globalSaved.mode, globalSaved.text);

    const planRows = useMemo(() => plans.map((plan) => ({ plan, routes: planRouteRows(plan) })), [plans]);
    const planOn = plans.filter((plan) => promptLayerActive(plan.prompt_override_enabled, plan.prompt_override_mode, plan.system_prompt_override)).length;
    const routeOn = planRows.reduce((sum, { routes }) => sum + routes.filter((row) => promptLayerActive(row.enabled, row.mode, row.text)).length, 0);
    const channelOn = channels.filter((channel) => promptLayerActive(channel.prompt_override_enabled, channel.prompt_override_mode, channel.system_prompt_override)).length;

    const visiblePlans = planRows
        .map(({ plan, routes }) => {
            const planHit = matchesPromptSearch(query, plan.display_name, plan.slug, plan.system_prompt_override);
            const filled = (row: RoutePromptRow) => !onlyFilled || row.text.trim().length > 0;
            const shown = routes.filter((row) => filled(row) && (planHit || matchesPromptSearch(query, row.requestModel, row.upstreamModels.join(' '), row.text)));
            const planFilled = !onlyFilled || (plan.system_prompt_override ?? '').trim().length > 0 || shown.length > 0;
            const visible = planFilled && (planHit || shown.length > 0);
            return { plan, routes: shown, total: routes.length, visible };
        })
        .filter((row) => row.visible);
    const planRelations = useMemo(() => channelPlanNames(plans), [plans]);
    const focusedPlan = useMemo(() => plans.find((plan) => String(plan.id) === focusedPlanId), [plans, focusedPlanId]);
    const focusedChannelIds = useMemo(() => channelIDsForPlan(focusedPlan), [focusedPlan]);
    const visibleChannels = channels.filter((channel) => {
        const hasText = Boolean(channel.system_prompt_override?.trim());
        const active = promptLayerActive(channel.prompt_override_enabled, channel.prompt_override_mode, channel.system_prompt_override);
        return (!onlyFilled || hasText)
            && (focusedPlanId === 'all' || focusedChannelIds.has(channel.id))
            && (channelStatusFilter === 'all' || (channelStatusFilter === 'active' && active)
                || (channelStatusFilter === 'configured' && hasText)
                || (channelStatusFilter === 'empty' && !hasText))
            && matchesPromptSearch(query, channel.name, `#${channel.id}`, channel.system_prompt_override, (planRelations.get(channel.id) ?? []).join(' '));
    });
    const channelGroups = groupChannelsByType(visibleChannels);
    const showGlobal = (layer === 'all' || layer === 'global')
        && (!onlyFilled || globalSaved.text.trim().length > 0)
        && matchesPromptSearch(query, '全局', 'global', globalSaved.text);
    const showPlans = layer === 'all' || layer === 'plan';
    const showChannels = layer === 'all' || layer === 'channel';

    // 拼接顺序里的实时预览：当前筛选下、真正会加东西的层，按位置分两边。
    const order = useMemo(() => {
        const pick = (mode: PromptOverrideMode) => {
            const g = showGlobal && globalActive && globalSaved.mode === mode ? ['全局'] : [];
            const p = showPlans ? visiblePlans.filter(({ plan }) => promptLayerActive(plan.prompt_override_enabled, plan.prompt_override_mode, plan.system_prompt_override) && normalizePromptMode(plan.prompt_override_mode) === mode).map(({ plan }) => plan.display_name || plan.slug) : [];
            const r = showPlans ? visiblePlans.flatMap(({ plan, routes }) => routes.filter((row) => promptLayerActive(row.enabled, row.mode, row.text) && row.mode === mode).map((row) => `${plan.display_name || plan.slug} · ${row.requestModel}`)) : [];
            const c = showChannels ? visibleChannels.filter((ch) => promptLayerActive(ch.prompt_override_enabled, ch.prompt_override_mode, ch.system_prompt_override) && normalizePromptMode(ch.prompt_override_mode) === mode).map((ch) => ch.name) : [];
            return { g, p, r, c };
        };
        const b = pick('before');
        const a = pick('after');
        return {
            before: [{ layer: '渠道', names: b.c }, { layer: '路由', names: b.r }, { layer: '方案', names: b.p }, { layer: '全局', names: b.g }],
            after: [{ layer: '全局', names: a.g }, { layer: '方案', names: a.p }, { layer: '路由', names: a.r }, { layer: '渠道', names: a.c }],
        };
    }, [showGlobal, showPlans, showChannels, globalActive, globalSaved.mode, visiblePlans, visibleChannels]);

    const tabs: Array<{ value: LayerFilter; label: string; short?: string; count?: number }> = [
        { value: 'all', label: '全部' },
        { value: 'global', label: '全局', count: globalActive ? 1 : 0 },
        { value: 'plan', label: '方案与路由', short: '方案', count: planOn + routeOn },
        { value: 'channel', label: '渠道', count: channelOn },
    ];

    const loading = (
        <div className={cn(SECTION, 'flex justify-center py-10 text-muted-foreground')}>
            <Loader2 className="size-5 animate-spin" />
        </div>
    );
    const empty = (text: string) => (
        <div className="rounded-2xl border border-dashed border-border px-4 py-8 text-center text-sm text-muted-foreground">{text}</div>
    );

    return (
        <PromptDraftsContext.Provider value={draftContext}>
        <IdentityTemplateContext.Provider value={identityPreset}>
        <PageWrapper className="h-full min-h-0 overflow-y-auto overscroll-contain space-y-4 rounded-t-3xl pb-[calc(7rem+env(safe-area-inset-bottom))] md:pb-4">
            <details className="rounded-3xl border border-border bg-card">
                <summary className="flex min-h-12 cursor-pointer items-center gap-2 px-4 py-3 text-sm font-medium">
                    <WandSparkles className="size-4 text-muted-foreground" />
                    助手身份与中性模板
                    <span className="ml-auto text-xs text-muted-foreground">按需展开</span>
                </summary>
                <div className="border-t border-border/70 p-2 sm:p-3">
                    <PromptIdentityBuilder
                        mode={identityMode}
                        onModeChange={setIdentityMode}
                        displayName={identityName}
                        onDisplayNameChange={setIdentityName}
                        style={identityStyle}
                        onStyleChange={setIdentityStyle}
                        preview={identityPreset}
                    />
                </div>
            </details>
            {Object.values(drafts).filter((draft) => !promptValuesEqual(draft.value, draft.baseline)).length > 0 ? (
                <p className="rounded-xl border border-amber-500/30 bg-amber-500/5 px-3 py-2 text-sm">
                    有未保存的提示词草稿；折叠和切换筛选不会丢失，保存后才会生效。
                </p>
            ) : null}
            <section className={cn(SECTION, 'grid gap-3 py-3 sm:py-4')}>
                <div className="flex min-w-0 items-center gap-1">
                    <p className="min-w-0 truncate text-sm text-muted-foreground">每层可单独开关，放在请求自带提示词的前面或后面。</p>
                    <InfoTip label="提示词说明">
                        不会删掉请求自带的系统提示词；请求没带时，直接用这段当系统提示词。关掉的层什么都不加，请求原样转发，文字会保留。
                    </InfoTip>
                    <span className="ml-auto" />
                    <OrderPopover before={order.before} after={order.after} />
                </div>
                <div className="flex flex-wrap items-center gap-2">
                    <label className="relative min-w-0 flex-1 basis-56">
                        <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                        <input
                            value={query}
                            onChange={(event) => setQuery(event.target.value)}
                            placeholder="搜索方案、模型、渠道或内容"
                            aria-label="搜索提示词"
                            className="h-9 w-full rounded-xl border border-input bg-background pl-9 pr-9 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
                        />
                        {query ? (
                            <button type="button" aria-label="清空搜索" onClick={() => setQuery('')} className="absolute right-2 top-1/2 -translate-y-1/2 rounded-md p-1 text-muted-foreground hover:text-foreground">
                                <X className="size-4" />
                            </button>
                        ) : null}
                    </label>
                    <div className="flex w-full min-w-0 items-center gap-2 sm:w-auto">
                    <div role="tablist" aria-label="按层筛选" className="flex h-9 min-w-0 items-center overflow-x-auto rounded-xl border border-input bg-muted/40 p-0.5 text-[13px] sm:text-sm">
                        {tabs.map((tab) => (
                            <button
                                key={tab.value}
                                type="button"
                                role="tab"
                                aria-selected={layer === tab.value}
                                onClick={() => setLayer(tab.value)}
                                title={tab.count !== undefined ? `已启用 ${tab.count} 层` : undefined}
                                className={cn(
                                    'flex h-8 shrink-0 items-center gap-1 rounded-lg px-2 transition-colors sm:px-2.5',
                                    layer === tab.value ? 'bg-background font-medium text-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground'
                                )}
                            >
                                {tab.short ? (
                                    <>
                                        <span className="sm:hidden">{tab.short}</span>
                                        <span className="hidden sm:inline">{tab.label}</span>
                                    </>
                                ) : (
                                    tab.label
                                )}
                                {tab.count ? <span className="min-w-4 rounded-full bg-primary/15 px-1 text-center text-[11px] leading-4 text-primary">{tab.count}</span> : null}
                            </button>
                        ))}
                    </div>
                    <label className="ml-auto inline-flex h-9 shrink-0 cursor-pointer items-center gap-1.5 text-[13px] text-muted-foreground sm:ml-0 sm:gap-2 sm:px-1 sm:text-sm" title="只看已填写">
                        <Switch checked={onlyFilled} onCheckedChange={setOnlyFilled} aria-label="只看已填写" />
                        <span className="sm:hidden">已填写</span>
                        <span className="hidden sm:inline">只看已填写</span>
                    </label>
                    </div>
                </div>
            </section>

            {showGlobal ? (settingsLoading ? loading : <GlobalSection settings={settings} />) : null}

            {showPlans ? (
                <section className={SECTION}>
                    <SectionHeader
                        icon={Layers3}
                        title="方案与路由"
                        tip="每个方案一张卡：左边是方案层（这个方案的所有请求），右边是方案里每条路由（某个请求模型）。路由层排在方案层外面。"
                        right={<Badge variant="outline" className="shrink-0">{visiblePlans.length} 个方案</Badge>}
                    />
                    {plansLoading ? loading : visiblePlans.length === 0 ? empty('没有符合条件的方案') : (
                        <div className="grid gap-3">
                            {visiblePlans.map(({ plan, routes, total }) => (
                                <PlanCard key={plan.id} plan={plan} routes={routes} totalRoutes={total} query={query} onlyFilled={onlyFilled} />
                            ))}
                        </div>
                    )}
                </section>
            ) : null}

            {showChannels ? (
                <section className={SECTION}>
                    <SectionHeader
                        icon={Radio}
                        title="渠道级规则"
                        tip="一个渠道可能同时被 VIP / SVIP 多个方案使用。渠道级策略按最终实际转发渠道生效，不随页面筛选变成套餐专属。"
                        right={<Badge variant="outline" className="shrink-0">{visibleChannels.length}/{channels.length}</Badge>}
                    />
                    <div className="mb-3 grid gap-2 sm:grid-cols-2">
                        <label className="grid gap-1 text-xs text-muted-foreground">
                            关联方案（仅筛选，不改变作用域）
                            <select value={focusedPlanId} onChange={(event) => setFocusedPlanId(event.target.value)} className="min-h-11 rounded-xl border border-input bg-background px-3 text-sm text-foreground">
                                <option value="all">全部方案的渠道</option>
                                {plans.map((plan) => <option key={plan.id} value={String(plan.id)}>{plan.display_name || plan.slug}</option>)}
                            </select>
                        </label>
                        <label className="grid gap-1 text-xs text-muted-foreground">
                            提示词状态
                            <select value={channelStatusFilter} onChange={(event) => setChannelStatusFilter(event.target.value as typeof channelStatusFilter)} className="min-h-11 rounded-xl border border-input bg-background px-3 text-sm text-foreground">
                                <option value="all">全部状态</option>
                                <option value="active">已启用</option>
                                <option value="configured">已填写</option>
                                <option value="empty">未配置</option>
                            </select>
                        </label>
                    </div>
                    {channelsLoading ? loading : visibleChannels.length === 0 ? empty('没有符合条件的渠道') : (
                        <div className="grid gap-2">
                            {channelGroups.map((group) => {
                                const open = expandedChannelTypes.has(group.type) || Boolean(query.trim());
                                const count = groupPageSize[group.type] ?? 25;
                                const shown = group.channels.slice(0, count);
                                const active = group.channels.filter((ch) => promptLayerActive(ch.prompt_override_enabled, ch.prompt_override_mode, ch.system_prompt_override)).length;
                                return (
                                    <div key={group.type} className="overflow-hidden rounded-2xl border border-border bg-background/50">
                                        <button
                                            type="button"
                                            aria-expanded={open}
                                            aria-label={`展开或收起 ${group.label} 渠道组`}
                                            onClick={() => setExpandedChannelTypes((old) => {
                                                const next = new Set(old);
                                                if (next.has(group.type)) next.delete(group.type);
                                                else next.add(group.type);
                                                return next;
                                            })}
                                            className="flex min-h-12 w-full min-w-0 items-center gap-2 px-3 py-2 text-left sm:px-4"
                                        >
                                            <ChevronDown className={cn('size-4 shrink-0 text-muted-foreground transition-transform', open && 'rotate-180')} />
                                            <span className="min-w-0 truncate text-sm font-medium">{group.label}</span>
                                            <span className="ml-auto shrink-0 text-xs text-muted-foreground">{active} 已启用 / {group.channels.length} 渠道</span>
                                        </button>
                                        {open ? (
                                            <div className="grid gap-2 border-t border-border/70 p-2 sm:p-3 lg:grid-cols-2">
                                                {shown.map((channel) => (
                                                    <ChannelRow key={channel.id} channel={channel} relatedPlans={planRelations.get(channel.id) ?? []} />
                                                ))}
                                                {count < group.channels.length ? (
                                                    <button
                                                        type="button"
                                                        onClick={() => setGroupPageSize((old) => ({ ...old, [group.type]: count + 25 }))}
                                                        className="min-h-11 rounded-xl border border-dashed border-border px-3 text-sm text-muted-foreground hover:text-foreground"
                                                    >
                                                        显示更多（已显示 {shown.length}/{group.channels.length}）
                                                    </button>
                                                ) : null}
                                            </div>
                                        ) : null}
                                    </div>
                                );
                            })}
                        </div>
                    )}
                </section>
            ) : null}
        </PageWrapper>
        </IdentityTemplateContext.Provider>
        </PromptDraftsContext.Provider>
    );
}

export function PromptManagement() {
    const role = useAuthStore((state) => state.user?.role);
    return role === 'admin' ? <AdminPromptView /> : <UserPromptView />;
}
