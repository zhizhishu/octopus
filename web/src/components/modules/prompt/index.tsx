'use client';

import { useMemo, useState, type ReactNode } from 'react';
import {
    ArrowRight,
    ChevronDown,
    GitBranch,
    KeyRound,
    Layers3,
    Loader2,
    MessageSquareText,
    Radio,
    Save,
    Search,
    ShieldCheck,
    X,
} from 'lucide-react';
import type { LucideIcon } from 'lucide-react';
import type { ApiError } from '@/api/types';
import {
    type AccessPlan,
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
import { useNavStore } from '../navbar/nav-store';
import { cn } from '@/lib/utils';
import { matchesPromptSearch, planRouteRows, sortChannelsStable, type RoutePromptRow } from './prompt-rows';
import { normalizePromptMode, promptEnabledFromSaved, promptLayerActive, promptStatus } from './prompt-mode';
import { PromptLayerControls } from './PromptLayerControls';

// 提示词页是系统提示词唯一的编辑入口：全局、每个方案、方案里的每条路由、每个渠道。
// 「方案」页只管方案和路由，不再出现提示词；保存时各写各的字段，互不覆盖。

function apiErrorMessage(error: unknown): string | undefined {
    return (error as ApiError | undefined)?.message;
}

type PromptValue = { text: string; mode: PromptOverrideMode; enabled: boolean };

function savedValue(enabled: boolean | null | undefined, mode: string | null | undefined, text: string | null | undefined): PromptValue {
    return { text: text ?? '', mode: normalizePromptMode(mode), enabled: promptEnabledFromSaved(enabled, mode, text) };
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
    saved,
    onSave,
    saving,
    label,
    placeholder,
    size = 'min-h-24',
}: {
    saved: PromptValue;
    onSave: (next: PromptValue) => Promise<unknown>;
    saving: boolean;
    label: string;
    placeholder: string;
    size?: string;
}) {
    const [value, setValue] = useState<PromptValue>(saved);
    const dirty = value.text.trim() !== saved.text.trim() || value.mode !== saved.mode || value.enabled !== saved.enabled;
    const save = async () => {
        try {
            await onSave({ ...value, text: value.text.trim() });
            toast.success(`${label} 已保存`);
        } catch (error) {
            toast.error(`${label} 保存失败`, { description: apiErrorMessage(error) });
        }
    };
    return (
        <div className="grid gap-2.5">
            <div className="flex flex-wrap items-center justify-between gap-2">
                <PromptLayerControls
                    enabled={value.enabled}
                    onEnabledChange={(enabled) => setValue((v) => ({ ...v, enabled }))}
                    mode={value.mode}
                    onModeChange={(mode) => setValue((v) => ({ ...v, mode }))}
                    label={label}
                />
                <Button type="button" size="sm" className="h-8 rounded-xl" disabled={saving || !dirty} onClick={save}>
                    {saving ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}
                    保存
                </Button>
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

function promptKey(value: PromptValue) {
    return `${value.enabled ? 1 : 0}|${value.mode}|${value.text}`;
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

function SectionTitle({ icon: Icon, title, hint, right }: { icon: LucideIcon; title: string; hint: string; right?: ReactNode }) {
    return (
        <div className="mb-4 flex flex-wrap items-start justify-between gap-3">
            <div className="min-w-0">
                <h3 className="flex items-center gap-2 text-base font-semibold">
                    <Icon className="size-5" />
                    {title}
                </h3>
                <p className="mt-1 text-xs leading-5 text-muted-foreground">{hint}</p>
            </div>
            {right}
        </div>
    );
}

function GlobalSection({ settings }: { settings: Array<{ key: string; value: string }> | undefined }) {
    const setSetting = useSetSetting();
    const get = (key: string) => settings?.find((item) => item.key === key)?.value;
    const saved = savedValue(get(SettingKey.PromptOverrideEnabled) === 'true', get(SettingKey.PromptOverrideMode), get(SettingKey.PromptOverrideSystem));
    const save = async (next: PromptValue) => {
        await setSetting.mutateAsync({ key: SettingKey.PromptOverrideMode, value: next.mode });
        await setSetting.mutateAsync({ key: SettingKey.PromptOverrideSystem, value: next.text });
        await setSetting.mutateAsync({ key: SettingKey.PromptOverrideEnabled, value: next.enabled ? 'true' : 'false' });
    };
    return (
        <section className="rounded-3xl border border-border bg-card p-4 sm:p-5">
            <SectionTitle
                icon={ShieldCheck}
                title="全局"
                hint="所有请求都会带上，离请求自带的提示词最近。"
                right={<PromptStatusBadge value={saved} />}
            />
            <PromptEditor
                key={promptKey(saved)}
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
    const saved: PromptValue = { text: row.text, mode: row.mode, enabled: row.enabled };
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
                key={promptKey(saved)}
                saved={saved}
                onSave={(next) => update.mutateAsync({
                    access_plan_id: row.plan.id,
                    request_model: row.requestModel,
                    system_prompt_override: next.text,
                    prompt_override_mode: next.mode,
                    prompt_override_enabled: next.enabled,
                })}
                saving={update.isPending}
                label={`${row.plan.display_name || row.plan.slug} · ${row.requestModel} 提示词`}
                placeholder="可选：只对这个模型生效的系统提示词"
            />
        </LayerRow>
    );
}

function PlanCard({ plan, routes, totalRoutes, query, onlyFilled }: { plan: AccessPlan; routes: RoutePromptRow[]; totalRoutes: number; query: string; onlyFilled: boolean }) {
    const update = useUpdateAccessPlanPrompt();
    const setActiveItem = useNavStore((state) => state.setActiveItem);
    const saved = savedValue(plan.prompt_override_enabled, plan.prompt_override_mode, plan.system_prompt_override);
    const routeOn = routes.filter((row) => promptLayerActive(row.enabled, row.mode, row.text)).length;
    const name = plan.display_name || plan.slug;
    return (
        <article className="rounded-3xl border border-border bg-card">
            <header className="flex flex-wrap items-center gap-2 border-b border-border/70 px-4 py-3 sm:px-5">
                <Layers3 className="size-4 text-muted-foreground" />
                <h4 className="text-sm font-semibold">{name}</h4>
                <span className="font-mono text-xs text-muted-foreground">{plan.slug}</span>
                {plan.is_default ? <Badge>默认</Badge> : null}
                {!plan.enabled ? <Badge variant="outline">已停用</Badge> : null}
                <span className="ml-auto text-xs text-muted-foreground">{totalRoutes} 条路由 · {routeOn} 条已启用</span>
            </header>
            <div className="grid gap-4 p-4 sm:p-5 xl:grid-cols-[minmax(0,5fr)_minmax(0,7fr)]">
                <div className="min-w-0">
                    <div className="mb-2 flex items-center justify-between gap-2">
                        <span className="text-xs font-medium text-muted-foreground">方案层 · 这个方案的所有请求</span>
                        <PromptStatusBadge value={saved} />
                    </div>
                    <PromptEditor
                        key={promptKey(saved)}
                        saved={saved}
                        onSave={(next) => update.mutateAsync({
                            id: plan.id,
                            system_prompt_override: next.text,
                            prompt_override_mode: next.mode,
                            prompt_override_enabled: next.enabled,
                        })}
                        saving={update.isPending}
                        label={`${name} 方案提示词`}
                        placeholder="可选：这个方案的系统提示词"
                        size="min-h-28"
                    />
                </div>
                <div className="min-w-0">
                    <div className="mb-2 flex items-center justify-between gap-2">
                        <span className="flex items-center gap-1.5 text-xs font-medium text-muted-foreground">
                            <GitBranch className="size-3.5" />
                            路由层 · 只对某个请求模型
                        </span>
                        {query || onlyFilled ? <span className="text-xs text-muted-foreground">显示 {routes.length} / {totalRoutes}</span> : null}
                    </div>
                    {routes.length === 0 ? (
                        <div className="flex flex-wrap items-center justify-between gap-2 rounded-2xl border border-dashed border-border px-3 py-4 text-xs text-muted-foreground">
                            <span>{totalRoutes === 0 ? '这个方案还没有路由。' : '没有符合条件的路由。'}</span>
                            {totalRoutes === 0 ? (
                                <Button type="button" variant="outline" size="sm" className="h-7 rounded-lg" onClick={() => setActiveItem('access-plan')}>
                                    去「方案」页加路由
                                </Button>
                            ) : null}
                        </div>
                    ) : (
                        <div className="grid gap-2">
                            {routes.map((row) => (
                                <RouteRow key={`${plan.id}-${row.requestModel}-${promptKey(row)}`} row={row} defaultOpen={row.text.trim().length > 0} />
                            ))}
                        </div>
                    )}
                </div>
            </div>
        </article>
    );
}

function ChannelRow({ channel }: { channel: Channel }) {
    const update = useUpdateChannel();
    const saved = savedValue(channel.prompt_override_enabled, channel.prompt_override_mode, channel.system_prompt_override);
    return (
        <LayerRow
            saved={saved}
            defaultOpen={saved.text.trim().length > 0}
            title={<>#{channel.id} {channel.name}</>}
            meta={!channel.enabled ? <Badge variant="outline">渠道已停用</Badge> : null}
        >
            <PromptEditor
                key={promptKey(saved)}
                saved={saved}
                onSave={(next) => update.mutateAsync({
                    id: channel.id,
                    system_prompt_override: next.text,
                    prompt_override_mode: next.mode,
                    prompt_override_enabled: next.enabled,
                })}
                saving={update.isPending}
                label={`${channel.name} 渠道提示词`}
                placeholder="可选：转发到这个渠道时加的系统提示词"
            />
        </LayerRow>
    );
}

const ORDER_CHIPS: Array<{ label: string; client?: boolean }> = [
    { label: '渠道' }, { label: '路由' }, { label: '方案' }, { label: '全局' },
    { label: '请求自带', client: true },
    { label: '全局' }, { label: '方案' }, { label: '路由' }, { label: '渠道' },
];

function AdminPromptView() {
    const { data: settings, isLoading: settingsLoading } = useSettingList();
    const { data: plans = [], isLoading: plansLoading } = useAccessPlanList();
    const { data: channelItems = [], isLoading: channelsLoading } = useChannelList();
    const channels = useMemo(() => sortChannelsStable(channelItems.map((item) => item.raw)), [channelItems]);
    const [query, setQuery] = useState('');
    const [layer, setLayer] = useState<LayerFilter>('all');
    const [onlyFilled, setOnlyFilled] = useState(false);

    const get = (key: string) => settings?.find((item) => item.key === key)?.value;
    const globalActive = promptLayerActive(get(SettingKey.PromptOverrideEnabled) === 'true', get(SettingKey.PromptOverrideMode), get(SettingKey.PromptOverrideSystem));

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
    const visibleChannels = channels.filter((channel) =>
        (!onlyFilled || (channel.system_prompt_override ?? '').trim().length > 0)
        && matchesPromptSearch(query, channel.name, `#${channel.id}`, channel.system_prompt_override)
    );
    const globalText = get(SettingKey.PromptOverrideSystem) ?? '';
    const showGlobal = (layer === 'all' || layer === 'global')
        && (!onlyFilled || globalText.trim().length > 0)
        && matchesPromptSearch(query, '全局', 'global', globalText);
    const showPlans = layer === 'all' || layer === 'plan';
    const showChannels = layer === 'all' || layer === 'channel';

    const tabs: Array<{ value: LayerFilter; label: string; count?: string }> = [
        { value: 'all', label: '全部' },
        { value: 'global', label: '全局', count: globalActive ? '1' : '0' },
        { value: 'plan', label: '方案与路由', count: `${planOn}+${routeOn}` },
        { value: 'channel', label: '渠道', count: String(channelOn) },
    ];

    const loading = (
        <div className="flex justify-center rounded-3xl border border-border bg-card py-10 text-muted-foreground">
            <Loader2 className="size-5 animate-spin" />
        </div>
    );
    const empty = (text: string) => (
        <div className="rounded-2xl border border-dashed border-border px-4 py-8 text-center text-sm text-muted-foreground">{text}</div>
    );

    return (
        <PageWrapper className="h-full min-h-0 overflow-y-auto overscroll-contain space-y-4 rounded-t-3xl pb-24 md:pb-4">
            <section className="rounded-3xl border border-border bg-card p-4 sm:p-6">
                <div className="flex flex-wrap items-start justify-between gap-4">
                    <div className="min-w-0 max-w-3xl">
                        <h2 className="flex items-center gap-2 text-2xl font-bold">
                            <MessageSquareText className="size-6" />
                            提示词
                        </h2>
                        <p className="mt-2 text-sm leading-6 text-muted-foreground">
                            所有系统提示词都在这里改。每层可以单独开关，放在请求自带的提示词前面或后面，不会删掉请求自带的；请求没带就直接用这段。
                        </p>
                    </div>
                    <div className="grid w-full gap-1.5 sm:w-auto" title="几层叠加时，越具体的层排得越靠外">
                        <span className="text-xs text-muted-foreground">最终顺序</span>
                        <div className="flex flex-wrap items-center gap-1">
                            {ORDER_CHIPS.map((chip, index) => (
                                <span
                                    key={index}
                                    className={cn(
                                        'rounded-lg border px-1.5 py-0.5 text-[11px] leading-4',
                                        chip.client ? 'border-primary/40 bg-primary/10 font-medium text-primary' : 'border-border bg-muted/30 text-muted-foreground'
                                    )}
                                >
                                    {chip.label}
                                </span>
                            ))}
                        </div>
                    </div>
                </div>
                <div className="mt-4 flex flex-wrap items-center gap-2 sm:gap-3">
                    <label className="relative min-w-0 flex-1 basis-64">
                        <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                        <input
                            value={query}
                            onChange={(event) => setQuery(event.target.value)}
                            placeholder="搜索方案、模型、渠道或提示词内容"
                            aria-label="搜索提示词"
                            className="h-10 w-full rounded-xl border border-input bg-background pl-9 pr-9 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
                        />
                        {query ? (
                            <button type="button" aria-label="清空搜索" onClick={() => setQuery('')} className="absolute right-2 top-1/2 -translate-y-1/2 rounded-md p-1 text-muted-foreground hover:text-foreground">
                                <X className="size-4" />
                            </button>
                        ) : null}
                    </label>
                    <div role="tablist" aria-label="按层筛选" className="flex h-10 max-w-full items-center overflow-x-auto rounded-xl border border-input bg-muted/40 p-0.5 text-sm">
                        {tabs.map((tab) => (
                            <button
                                key={tab.value}
                                type="button"
                                role="tab"
                                aria-selected={layer === tab.value}
                                onClick={() => setLayer(tab.value)}
                                className={cn(
                                    'flex h-9 shrink-0 items-center gap-1.5 rounded-lg px-3 transition-colors',
                                    layer === tab.value ? 'bg-background font-medium text-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground'
                                )}
                            >
                                {tab.label}
                                {tab.count !== undefined ? <span className="text-xs text-muted-foreground" title="已启用的层数">{tab.count}</span> : null}
                            </button>
                        ))}
                    </div>
                    <label className="inline-flex h-10 cursor-pointer items-center gap-2 rounded-xl px-1 text-sm text-muted-foreground">
                        <Switch checked={onlyFilled} onCheckedChange={setOnlyFilled} aria-label="只看已填写" />
                        只看已填写
                    </label>
                </div>
            </section>

            {showGlobal ? (settingsLoading ? loading : <GlobalSection key={`${get(SettingKey.PromptOverrideSystem)}-${get(SettingKey.PromptOverrideMode)}-${get(SettingKey.PromptOverrideEnabled)}`} settings={settings} />) : null}

            {showPlans ? (
                <section className="rounded-3xl border border-border bg-card/40 p-4 sm:p-5">
                    <SectionTitle
                        icon={Layers3}
                        title="方案与路由"
                        hint="方案层对这个方案的所有请求生效；路由层只对方案里的某个请求模型生效，排在方案层外面。路由本身在「方案」页管理。"
                        right={<Badge variant="outline">{visiblePlans.length} 个方案</Badge>}
                    />
                    {plansLoading ? loading : visiblePlans.length === 0 ? empty('没有符合条件的方案') : (
                        <div className="grid gap-4">
                            {visiblePlans.map(({ plan, routes, total }) => (
                                <PlanCard key={plan.id} plan={plan} routes={routes} totalRoutes={total} query={query} onlyFilled={onlyFilled} />
                            ))}
                        </div>
                    )}
                </section>
            ) : null}

            {showChannels ? (
                <section className="rounded-3xl border border-border bg-card p-4 sm:p-5">
                    <SectionTitle
                        icon={Radio}
                        title="渠道"
                        hint="最终转发到这个上游渠道时生效，排在最外面。适合供应商特定的约束。"
                        right={<Badge variant="outline">{visibleChannels.length} / {channels.length}</Badge>}
                    />
                    {channelsLoading ? loading : visibleChannels.length === 0 ? empty('没有符合条件的渠道') : (
                        <div className="grid gap-2 lg:grid-cols-2">
                            {visibleChannels.map((channel) => (
                                <ChannelRow
                                    key={`${channel.id}-${channel.system_prompt_override ?? ''}-${channel.prompt_override_mode ?? ''}-${channel.prompt_override_enabled ? 1 : 0}`}
                                    channel={channel}
                                />
                            ))}
                        </div>
                    )}
                </section>
            ) : null}
        </PageWrapper>
    );
}

export function PromptManagement() {
    const role = useAuthStore((state) => state.user?.role);
    return role === 'admin' ? <AdminPromptView /> : <UserPromptView />;
}
