'use client';

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
    BadgeDollarSign,
    Check,
    GitBranch,
    Loader2,
    PencilLine,
    Plus,
    RefreshCcw,
    Save,
    Search,
    ShieldCheck,
    Trash2,
} from 'lucide-react';
import type { ApiError } from '@/api/types';
import {
    type AccessPlan,
    type AccessPlanBillingRule,
    type AccessPlanRouteTarget,
    useAccessPlanList,
    useCreateAccessPlan,
    useDeleteAccessPlan,
    useUpdateAccessPlan,
    useUpdateAccessPlanBillingRules,
    useUpdateAccessPlanRouteTargets,
} from '@/api/endpoints/access-plan';
import { useChannelList } from '@/api/endpoints/channel';
import { useModelChannelList, useModelList } from '@/api/endpoints/model';
import { useAuthStore } from '@/api/endpoints/user';
import { PageWrapper } from '@/components/common/PageWrapper';
import { toast } from '@/components/common/Toast';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import {
    Dialog,
    DialogClose,
    DialogContent,
    DialogDescription,
    DialogFooter,
    DialogHeader,
    DialogTitle,
    DialogTrigger,
} from '@/components/ui/dialog';
import { Input } from '@/components/ui/input';
import { SearchableSelect } from '@/components/ui/searchable-select';
import { Switch } from '@/components/ui/switch';
import { cn } from '@/lib/utils';
import { cleanOneMillionModelName } from '@/lib/model-aliases';
import {
    ReactFlow,
    ReactFlowProvider,
    Background,
    BackgroundVariant,
    Controls,
    MiniMap,
    Panel,
    Handle,
    Position,
    useNodesState,
    useEdgesState,
    useReactFlow,
    type Node,
    type Edge,
    type NodeProps,
    type NodeTypes,
} from '@xyflow/react';
import '@xyflow/react/dist/style.css';

const DEFAULT_PLAN_TEMPLATES = [
    { slug: 'vip', display_name: 'VIP' },
    { slug: 'svip', display_name: 'SVIP' },
    { slug: 'ssvip', display_name: 'SSVIP' },
    { slug: 'custom', display_name: 'Custom' },
] as const;

const ACCESS_PLAN_TEXT = {
    list: {
        title: '方案管理',
        loadFailed: '加载失败',
        empty: '暂无方案',
        selectOrCreate: '选择或创建一个方案',
    },
    create: {
        title: '新建方案',
        displayName: '显示名称',
        slug: '调用代号（如 vip，接口里用它选择方案）',
        submit: '创建',
    },
    status: {
        default: '默认',
        enabled: '启用',
        disabled: '停用',
    },
    basic: {
        title: '基础信息',
        displayName: '显示名称',
        slug: '调用代号',
        enabled: '启用',
        defaultPlan: '默认方案',
        autoSyncChannels: '跟随渠道自动同步',
        autoSyncChannelsHint: '新启用或同步的渠道自动加入本方案已有模型的路由，不用手动重建；关闭则保持严格白名单',
    },
    billing: {
        title: '计费倍率',
        jsonTitle: 'JSON 批量修改倍率',
        jsonApply: '应用 JSON',
        jsonCopy: '生成当前 JSON',
        jsonPlaceholder: '{\n  "default_multiplier": 1,\n  "rules": [\n    {"model_name": "gpt-4", "multiplier": 1.2, "enabled": true}\n  ]\n}',
        defaultMultiplier: '默认倍率',
        ruleCount: '倍率 {count} 条',
        modelName: '模型名称',
        multiplier: '倍率',
        empty: '暂无单模型倍率',
        defaultSource: '默认倍率',
        modelRuleSource: '单模型倍率',
    },
    routes: {
        title: '模型映射',
        manageTitle: '方案画布',
        manageHint: '方案设置收进弹窗，主视图只看请求、候选渠道、调度和发送模型。',
        tabBasic: '基础信息',
        tabBilling: '倍率',
        tabRoutes: '模型映射',
        jsonTitle: 'JSON 批量修改映射',
        jsonApply: '应用 JSON',
        jsonCopy: '生成当前 JSON',
        jsonPlaceholder: '[\n  {\n    "request_model": "claude-fable-5",\n    "channel_id": 1,\n    "upstream_model": "claude-fable-5",\n    "priority": 1,\n    "weight": 1,\n    "enabled": true\n  }\n]',
        targetCount: '映射 {count} 条',
        requestModel: '原请求模型',
        upstreamModel: '发送模型',
        priority: '优先级',
        weight: '同级轮询权重',
        noChannels: '暂无渠道',
        missingChannel: '渠道 #{id} 已不存在',
        empty: '暂无映射目标',
        canvasTitle: '无限画布视图',
        canvasHint: '每行＝一条替换规则：原请求模型 → 改走某个渠道、用该渠道上的发送模型；横向看完整链路。',
        canvasEmpty: '暂无可视化链路，先重建映射或添加目标。',
        targetSummary: '候选渠道 / 发送模型',
        setupTitle: '调用前先确认模型池 / 方案',
        setupHint: '模型池负责兜底，当前方案负责映射和计费。新渠道同步后用“重建当前方案映射”刷新。',
        groupRouteHint: '用户扣费按“用户计费”列计算，实际请求会发送到下方渠道与模型。流式响应一旦写出，就不会中途硬切。',
        rebuild: '重建映射',
        rebuildGroup: '重建当前方案映射',
        rebuildHint: '按当前渠道模型重建本方案映射，已消失的上游模型会被移除。',
        quickEdit: '编辑',
        quickEditTitle: '编辑模型映射',
        quickEditDescription: '直接编辑当前请求模型要走的渠道、发送模型、计费方式和提示词，不用滑到下方长列表。',
        targetMore: '+{count} 个目标',
        close: '关闭',
        hiddenStale: '已隐藏 {count} 个已消失的发送模型，保存或重建后会清理。',
        fallback: '备用处理',
        billingModel: '用户计费',
        multiplier: '倍率',
        unset: '未设置',
        noRoute: '未配置',
        disabledTarget: '停用目标',
        billingSourceRequest: '请求模型',
        billingSourceUpstream: '发送模型',
        billingSourceOverride: '覆盖模型',
        fallbackNone: '失败后直接报错',
        fallbackFailover: '尝试下一个目标',
        fallbackReturnGroup: '尝试同能力默认模型池',
        billingBy: '用户计费',
        upstreamTarget: '发送模型',
        billingHint: '用户扣费按这里显示的模型计算，和实际发送到哪个渠道分开。',
        routeFailureHint: '一般保持“尝试同能力默认模型池”就行；流式响应一旦开始输出，就不会中途硬切。',
        promptOverride: '提示词',
        promptAppend: '追加 system',
        promptReplace: '替换 system',
        promptPlaceholder: '可选：仅此请求模型映射生效的 system 提示词',
        advancedTitle: '高级（可选）',
        advancedHint: '优先级、同级轮询、失败兜底、系统提示词',
    },
    actions: {
        save: '保存',
        cancel: '取消',
        delete: '删除',
        confirmDelete: '确认删除',
        add: '添加',
    },
    toast: {
        created: '方案已创建',
        createFailed: '创建失败',
        updated: '方案已更新',
        updateFailed: '更新失败',
        deleted: '方案已删除',
        deleteFailed: '删除失败',
        billingUpdated: '计费规则已更新',
        billingUpdateFailed: '计费规则更新失败',
        routesUpdated: '模型映射已更新',
        routesUpdateFailed: '模型映射更新失败',
        routesRebuilt: '模型映射已重建',
        routesRebuildEmpty: '没有可用的渠道模型可重建',
        jsonApplied: 'JSON 已应用，记得保存',
        jsonInvalid: 'JSON 格式不对',
    },
} as const;

function accessPlanText(path: string, values?: Record<string, string | number>) {
    const value = path.split('.').reduce<unknown>((current, segment) => {
        if (current && typeof current === 'object' && segment in current) {
            return (current as Record<string, unknown>)[segment];
        }
        return undefined;
    }, ACCESS_PLAN_TEXT);

    if (typeof value !== 'string') return path;
    return value.replace(/\{(\w+)\}/g, (_, key: string) => String(values?.[key] ?? ''));
}

function apiErrorMessage(error: unknown): string | undefined {
    return (error as ApiError | undefined)?.message;
}

function asNumber(value: string, fallback: number) {
    const next = Number(value);
    return Number.isFinite(next) ? next : fallback;
}

function asPositiveInt(value: string, fallback: number) {
    const next = Math.trunc(Number(value));
    return Number.isFinite(next) && next > 0 ? next : fallback;
}

function normalizeSlug(value: string) {
    return value.trim().toLowerCase().replace(/\s+/g, '-');
}

function sortedPlans(plans: AccessPlan[]) {
    return [...plans].sort((a, b) => {
        const bySort = (a.sort ?? a.id) - (b.sort ?? b.id);
        return bySort !== 0 ? bySort : a.slug.localeCompare(b.slug);
    });
}

function planTitle(plan: AccessPlan) {
    return plan.display_name || plan.slug;
}

function billingSourceLabel(source: AccessPlanRouteTarget['billing_model_source']) {
    const t = accessPlanText;

    switch (source ?? 'request_model') {
        case 'request_model':
            return t('routes.billingSourceRequest');
        case 'override_model':
            return t('routes.billingSourceOverride');
        case 'upstream_model':
        default:
            return t('routes.billingSourceUpstream');
    }
}

function fallbackModeLabel(mode: AccessPlanRouteTarget['fallback_mode']) {
    const t = accessPlanText;

    switch (mode ?? 'failover') {
        case 'none':
            return t('routes.fallbackNone');
        case 'return_group':
            return t('routes.fallbackReturnGroup');
        case 'failover':
        default:
            return t('routes.fallbackFailover');
    }
}

function billingModelName(target: AccessPlanRouteTarget) {
    const source = target.billing_model_source ?? 'request_model';

    if (source === 'request_model') return cleanOneMillionModelName(target.request_model);
    if (source === 'override_model') return cleanOneMillionModelName(target.billing_model_override ?? '');
    return cleanOneMillionModelName(target.upstream_model);
}

function effectiveMultiplier(plan: AccessPlan, billingModel: string) {
    const rule = plan.billing_rules.find((item) => (
        item.enabled !== false && item.model_name.trim() === billingModel
    ));

    return {
        multiplier: rule?.multiplier ?? plan.default_multiplier ?? 1,
        source: rule ? accessPlanText('billing.modelRuleSource') : accessPlanText('billing.defaultSource'),
    };
}

function groupedRouteTargets(targets: AccessPlanRouteTarget[]) {
    const grouped = new Map<string, AccessPlanRouteTarget[]>();

    targets.forEach((target) => {
        const requestModel = cleanOneMillionModelName(target.request_model);
        const requestKey = requestModel.toLowerCase();
        const current = grouped.get(requestKey) ?? [];
        current.push(target);
        grouped.set(requestKey, current);
    });

    return [...grouped.entries()]
        .map(([requestKey, routeTargets]) => ({
            requestKey,
            requestModel: cleanOneMillionModelName(routeTargets[0]?.request_model ?? '') || accessPlanText('routes.unset'),
            targets: [...routeTargets].sort((a, b) => {
                const byPriority = (a.priority || 0) - (b.priority || 0);
                return byPriority !== 0 ? byPriority : (a.channel_id || 0) - (b.channel_id || 0);
            }),
        }))
        .sort((a, b) => a.requestModel.localeCompare(b.requestModel));
}

type RouteTargetGroup = ReturnType<typeof groupedRouteTargets>[number];

// ── 模型家族分类：把 request_model 名归到一个厂商家族，用于画布泳道分组 ──
type ModelFamilyKey =
    | 'claude' | 'openai' | 'gemini' | 'deepseek'
    | 'glm' | 'qwen' | 'kimi' | 'grok' | 'other';

// 完整静态 class 字符串——严禁改成模板拼接 `bg-${c}-500`，Tailwind 会 purge 掉动态类导致线上掉色。
const FAMILY_STYLES: Record<ModelFamilyKey, { label: string; dot: string; ring: string; soft: string; text: string }> = {
    claude:   { label: 'Claude',      dot: 'bg-orange-500',  ring: 'border-orange-500/30',  soft: 'bg-orange-500/[0.07]',  text: 'text-orange-600 dark:text-orange-300' },
    openai:   { label: 'OpenAI',      dot: 'bg-teal-500',    ring: 'border-teal-500/30',    soft: 'bg-teal-500/[0.07]',    text: 'text-teal-600 dark:text-teal-300' },
    gemini:   { label: 'Gemini',      dot: 'bg-sky-500',     ring: 'border-sky-500/30',     soft: 'bg-sky-500/[0.07]',     text: 'text-sky-600 dark:text-sky-300' },
    deepseek: { label: 'DeepSeek',    dot: 'bg-indigo-500',  ring: 'border-indigo-500/30',  soft: 'bg-indigo-500/[0.07]',  text: 'text-indigo-600 dark:text-indigo-300' },
    glm:      { label: 'GLM · 智谱',  dot: 'bg-violet-500',  ring: 'border-violet-500/30',  soft: 'bg-violet-500/[0.07]',  text: 'text-violet-600 dark:text-violet-300' },
    qwen:     { label: 'Qwen · 通义', dot: 'bg-rose-500',    ring: 'border-rose-500/30',    soft: 'bg-rose-500/[0.07]',    text: 'text-rose-600 dark:text-rose-300' },
    kimi:     { label: 'Kimi',        dot: 'bg-fuchsia-500', ring: 'border-fuchsia-500/30', soft: 'bg-fuchsia-500/[0.07]', text: 'text-fuchsia-600 dark:text-fuchsia-300' },
    grok:     { label: 'Grok',        dot: 'bg-zinc-500',    ring: 'border-zinc-500/30',    soft: 'bg-zinc-500/[0.07]',    text: 'text-zinc-600 dark:text-zinc-300' },
    other:    { label: '其他',        dot: 'bg-slate-500',   ring: 'border-slate-500/30',   soft: 'bg-slate-500/[0.07]',   text: 'text-slate-600 dark:text-slate-300' },
};

function modelFamilyKey(rawName: string): ModelFamilyKey {
    const n = cleanOneMillionModelName(rawName).toLowerCase();
    if (/^(claude|anthropic)/.test(n)) return 'claude';
    if (/^(gpt|o1|o3|o4|chatgpt|openai|text-|davinci)/.test(n)) return 'openai';
    if (/(^gemini|^google|palm|bison)/.test(n)) return 'gemini';
    if (/deepseek/.test(n)) return 'deepseek';
    if (/(glm|zhipu|zai|chatglm|z-ai)/.test(n)) return 'glm';
    if (/(qwen|qwq|tongyi|通义)/.test(n)) return 'qwen';
    if (/(kimi|moonshot)/.test(n)) return 'kimi';
    if (/grok/.test(n)) return 'grok';
    return 'other';
}

type RouteChannelModel = { channel_id: number; name: string; enabled?: boolean };

function cleanRouteTargetModels(target: AccessPlanRouteTarget): AccessPlanRouteTarget {
    return {
        ...target,
        request_model: cleanOneMillionModelName(target.request_model),
        upstream_model: cleanOneMillionModelName(target.upstream_model),
        billing_model_override: cleanOneMillionModelName(target.billing_model_override ?? '') || undefined,
    };
}

function channelModelKey(channelId: number, modelName: string) {
    return `${channelId}|${modelName.trim().toLowerCase()}`;
}

function buildChannelModelIndex(channelModels: RouteChannelModel[], ready = true) {
    const byChannel = new Map<number, Set<string>>();
    const byKey = new Set<string>();

    channelModels.forEach((model) => {
        const name = cleanOneMillionModelName(model.name);
        if (!name || model.enabled === false) return;
        if (!byChannel.has(model.channel_id)) {
            byChannel.set(model.channel_id, new Set<string>());
        }
        byChannel.get(model.channel_id)?.add(name.toLowerCase());
        byKey.add(channelModelKey(model.channel_id, name));
    });

    return { byChannel, byKey, ready };
}

function isPersistedStaleTarget(target: AccessPlanRouteTarget, index: ReturnType<typeof buildChannelModelIndex>) {
    if (!target.id) return false;
    if (!index.ready) return false;
    const channelModels = index.byChannel.get(target.channel_id);
    if (!channelModels) return true;
    const upstreamModel = cleanOneMillionModelName(target.upstream_model).toLowerCase();
    if (!upstreamModel) return false;
    return !channelModels.has(upstreamModel);
}

function activeRouteTargets(targets: AccessPlanRouteTarget[], index: ReturnType<typeof buildChannelModelIndex>) {
    return targets.filter((target) => !isPersistedStaleTarget(target, index));
}

function rebuildRouteTargetsFromChannelModels(channelModels: RouteChannelModel[], currentTargets: AccessPlanRouteTarget[]) {
    const existingByChannelModel = new Map<string, AccessPlanRouteTarget>();
    const existingByRequestModel = new Map<string, AccessPlanRouteTarget>();
    currentTargets.forEach((target) => {
        existingByChannelModel.set(channelModelKey(target.channel_id, cleanOneMillionModelName(target.upstream_model)), target);
        const requestModel = cleanOneMillionModelName(target.request_model).toLowerCase();
        if (requestModel && !existingByRequestModel.has(requestModel)) {
            existingByRequestModel.set(requestModel, target);
        }
    });

    const counters = new Map<string, number>();
    return [...channelModels]
        .filter((model) => model.enabled !== false && model.name.trim().length > 0)
        .sort((a, b) => {
            const byName = a.name.localeCompare(b.name);
            return byName !== 0 ? byName : a.channel_id - b.channel_id;
        })
        .map((model) => {
            const requestModel = cleanOneMillionModelName(model.name);
            const previous = existingByChannelModel.get(channelModelKey(model.channel_id, requestModel))
                ?? existingByRequestModel.get(requestModel.toLowerCase());
            const nextPriority = (counters.get(requestModel.toLowerCase()) ?? 0) + 1;
            counters.set(requestModel.toLowerCase(), nextPriority);

            return {
                request_model: requestModel,
                channel_id: model.channel_id,
                upstream_model: requestModel,
                priority: previous?.priority ?? nextPriority,
                weight: previous?.weight ?? 1,
                enabled: previous?.enabled ?? true,
                billing_model_source: previous?.billing_model_source ?? 'request_model',
                billing_model_override: previous?.billing_model_override,
                fallback_mode: previous?.fallback_mode ?? 'return_group',
                system_prompt_override: previous?.system_prompt_override ?? '',
                prompt_override_mode: previous?.prompt_override_mode ?? 'append_system',
            } satisfies AccessPlanRouteTarget;
        });
}

function CreatePlanPanel({
    plans,
    onCreated,
}: {
    plans: AccessPlan[];
    onCreated: (id: number) => void;
}) {
    const t = accessPlanText;
    const createPlan = useCreateAccessPlan();
    const [open, setOpen] = useState(false);
    const [displayName, setDisplayName] = useState('');
    const [slug, setSlug] = useState('');
    const existingSlugs = useMemo(() => new Set(plans.map((plan) => plan.slug)), [plans]);

    const create = (payload: { slug: string; display_name: string }) => {
        createPlan.mutate(
            {
                ...payload,
                enabled: true,
                is_default: plans.length === 0,
                default_multiplier: 1,
            },
            {
                onSuccess: (data) => {
                    setDisplayName('');
                    setSlug('');
                    onCreated(data.id);
                    setOpen(false);
                    toast.success(t('toast.created'));
                },
                onError: (error) => {
                    toast.error(t('toast.createFailed'), { description: apiErrorMessage(error) });
                },
            }
        );
    };

    const canCreateCustom = displayName.trim().length > 0 && normalizeSlug(slug).length > 0;

    return (
        <Dialog open={open} onOpenChange={setOpen}>
            <DialogTrigger asChild>
                <Button type="button" variant="outline" className="rounded-xl">
                    <Plus className="size-4" />
                    <span className="min-w-0 truncate">{t('create.title')}</span>
                </Button>
            </DialogTrigger>
            <DialogContent className="w-[calc(100vw-1rem)] max-w-full overflow-hidden rounded-3xl border-border bg-card p-0 sm:max-w-xl">
                <DialogHeader className="border-b border-border px-5 py-4 pr-12">
                    <DialogTitle className="flex items-center gap-2">
                        <Plus className="size-5" />
                        {t('create.title')}
                    </DialogTitle>
                    <DialogDescription>{t('list.selectOrCreate')}</DialogDescription>
                </DialogHeader>
                <div className="grid gap-4 px-5 py-4">
                    <div className="grid grid-cols-2 gap-2">
                        {DEFAULT_PLAN_TEMPLATES.map((template) => {
                            const exists = existingSlugs.has(template.slug);
                            return (
                                <Button
                                    key={template.slug}
                                    type="button"
                                    variant={exists ? 'secondary' : 'outline'}
                                    className="rounded-xl"
                                    disabled={exists || createPlan.isPending}
                                    onClick={() => create(template)}
                                >
                                    {exists ? <Check className="size-4" /> : <Plus className="size-4" />}
                                    <span className="min-w-0 truncate">{template.slug}</span>
                                </Button>
                            );
                        })}
                    </div>

                    <div className="grid gap-3">
                        <label className="grid gap-1 text-xs text-muted-foreground">
                            显示名称：给人看的名字，可以写中文
                            <Input
                                value={displayName}
                                onChange={(event) => setDisplayName(event.target.value)}
                                placeholder={t('create.displayName')}
                                className="rounded-xl"
                            />
                        </label>
                        <label className="grid gap-1 text-xs text-muted-foreground">
                            {t('create.slug')}
                            <Input
                                value={slug}
                                onChange={(event) => setSlug(event.target.value)}
                                placeholder="vip / svip / claude-pro"
                                className="rounded-xl font-mono"
                            />
                        </label>
                    </div>
                </div>
                <DialogFooter className="flex-col gap-2 border-t border-border px-5 py-3 sm:flex-row">
                    <DialogClose asChild>
                        <Button type="button" variant="outline" className="rounded-xl">
                            <span className="min-w-0 truncate">{t('actions.cancel')}</span>
                        </Button>
                    </DialogClose>
                    <Button
                        type="button"
                        className="rounded-xl"
                        disabled={!canCreateCustom || createPlan.isPending}
                        onClick={() => create({ display_name: displayName.trim(), slug: normalizeSlug(slug) })}
                    >
                        {createPlan.isPending ? <Loader2 className="size-4 animate-spin" /> : <Plus className="size-4" />}
                        <span className="min-w-0 truncate">{t('create.submit')}</span>
                    </Button>
                </DialogFooter>
            </DialogContent>
        </Dialog>
    );
}

function PlanBasicsEditor({ plan }: { plan: AccessPlan }) {
    const t = accessPlanText;
    const updatePlan = useUpdateAccessPlan();
    const deletePlan = useDeleteAccessPlan();
    const [open, setOpen] = useState(false);
    const [displayName, setDisplayName] = useState(plan.display_name);
    const [slug, setSlug] = useState(plan.slug);
    const [enabled, setEnabled] = useState(plan.enabled);
    const [isDefault, setIsDefault] = useState(plan.is_default);
    const [autoSyncChannels, setAutoSyncChannels] = useState(plan.auto_sync_channels ?? false);
    const [confirmDelete, setConfirmDelete] = useState(false);

    const save = () => {
        updatePlan.mutate(
            {
                id: plan.id,
                display_name: displayName.trim(),
                slug: normalizeSlug(slug),
                enabled,
                is_default: isDefault,
                auto_sync_channels: autoSyncChannels,
                system_prompt_override: plan.system_prompt_override ?? '',
                prompt_override_mode: plan.prompt_override_mode ?? 'append_system',
            },
            {
                onSuccess: () => {
                    setOpen(false);
                    toast.success(t('toast.updated'));
                },
                onError: (error) => {
                    toast.error(t('toast.updateFailed'), { description: apiErrorMessage(error) });
                },
            }
        );
    };

    const remove = () => {
        if (!confirmDelete) {
            setConfirmDelete(true);
            return;
        }

        deletePlan.mutate(plan.id, {
            onSuccess: () => {
                setOpen(false);
                toast.success(t('toast.deleted'));
            },
            onError: (error) => {
                toast.error(t('toast.deleteFailed'), { description: apiErrorMessage(error) });
            },
        });
    };

    const canSave = displayName.trim().length > 0 && normalizeSlug(slug).length > 0;

    return (
        <Dialog open={open} onOpenChange={setOpen}>
            <div className="rounded-3xl border border-border bg-card p-5">
                <div className="flex flex-wrap items-center justify-between gap-3">
                    <div className="min-w-0">
                        <h3 className="flex items-center gap-2 text-base font-semibold">
                            <ShieldCheck className="size-5" />
                            {t('basic.title')}
                        </h3>
                        <div className="mt-2 flex min-w-0 flex-wrap items-center gap-2 text-xs text-muted-foreground">
                            <span className="max-w-[14rem] truncate font-mono">{plan.slug}</span>
                            <Badge variant={plan.enabled ? 'secondary' : 'outline'}>
                                {plan.enabled ? t('status.enabled') : t('status.disabled')}
                            </Badge>
                            {plan.is_default && <Badge>{t('status.default')}</Badge>}
                        </div>
                    </div>
                    <DialogTrigger asChild>
                        <Button type="button" variant="outline" className="rounded-xl">
                            <PencilLine className="size-4" />
                            <span className="min-w-0 truncate">{t('routes.quickEdit')}</span>
                        </Button>
                    </DialogTrigger>
                </div>
            </div>

            <DialogContent className="w-[calc(100vw-1rem)] max-w-full overflow-hidden rounded-3xl border-border bg-card p-0 sm:max-w-2xl">
                <DialogHeader className="border-b border-border px-5 py-4 pr-12">
                    <DialogTitle className="flex min-w-0 items-center gap-2">
                        <ShieldCheck className="size-5" />
                        <span className="min-w-0 truncate">{planTitle(plan)}</span>
                    </DialogTitle>
                    <DialogDescription>{t('basic.title')}</DialogDescription>
                </DialogHeader>

                <div className="grid gap-3 px-5 py-4 md:grid-cols-2">
                    <label className="grid gap-1 text-xs text-muted-foreground">
                        {t('basic.displayName')}
                        <Input
                            value={displayName}
                            onChange={(event) => setDisplayName(event.target.value)}
                            className="rounded-xl"
                        />
                    </label>
                    <label className="grid gap-1 text-xs text-muted-foreground">
                        {t('basic.slug')}
                        <Input
                            value={slug}
                            onChange={(event) => setSlug(event.target.value)}
                            className="rounded-xl font-mono"
                        />
                        <span>接口调用用这个代号，不是展示名；展示名可以中文，这里建议英文/数字/-/_。</span>
                    </label>
                    <label className="flex items-center justify-between gap-3 rounded-2xl border border-border/70 bg-muted/20 px-3 py-2 text-sm">
                        <span>{t('basic.enabled')}</span>
                        <Switch checked={enabled} onCheckedChange={setEnabled} />
                    </label>
                    <label className="flex items-center justify-between gap-3 rounded-2xl border border-border/70 bg-muted/20 px-3 py-2 text-sm">
                        <span>{t('basic.defaultPlan')}</span>
                        <Switch checked={isDefault} onCheckedChange={setIsDefault} />
                    </label>
                    <label className="flex items-center justify-between gap-3 rounded-2xl border border-border/70 bg-muted/20 px-3 py-2 text-sm md:col-span-2">
                        <span className="grid gap-0.5">
                            <span>{t('basic.autoSyncChannels')}</span>
                            <span className="text-xs text-muted-foreground">{t('basic.autoSyncChannelsHint')}</span>
                        </span>
                        <Switch checked={autoSyncChannels} onCheckedChange={setAutoSyncChannels} />
                    </label>
                </div>

                <DialogFooter className="flex-col gap-2 border-t border-border px-5 py-3 sm:flex-row">
                    <DialogClose asChild>
                        <Button type="button" variant="outline" className="rounded-xl">
                            <span className="min-w-0 truncate">{t('actions.cancel')}</span>
                        </Button>
                    </DialogClose>
                    <Button
                        type="button"
                        variant={confirmDelete ? 'destructive' : 'outline'}
                        className="rounded-xl"
                        disabled={deletePlan.isPending}
                        onClick={remove}
                    >
                        {deletePlan.isPending ? <Loader2 className="size-4 animate-spin" /> : <Trash2 className="size-4" />}
                        <span className="min-w-0 truncate">{confirmDelete ? t('actions.confirmDelete') : t('actions.delete')}</span>
                    </Button>
                    <Button
                        type="button"
                        className="rounded-xl"
                        disabled={!canSave || updatePlan.isPending}
                        onClick={save}
                    >
                        {updatePlan.isPending ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}
                        <span className="min-w-0 truncate">{t('actions.save')}</span>
                    </Button>
                </DialogFooter>
            </DialogContent>
        </Dialog>
    );
}

function BillingRulesEditor({
    plan,
    modelNames,
}: {
    plan: AccessPlan;
    modelNames: string[];
}) {
    const t = accessPlanText;
    const updateBilling = useUpdateAccessPlanBillingRules();
    const [defaultMultiplier, setDefaultMultiplier] = useState(plan.default_multiplier);
    const [rules, setRules] = useState<AccessPlanBillingRule[]>(plan.billing_rules);
    const [jsonText, setJsonText] = useState('');
    const modelListId = `access-plan-billing-models-${plan.id}`;

    const addRule = () => {
        setRules((current) => [...current, { model_name: '', multiplier: 1, enabled: true }]);
    };

    const updateRule = (index: number, patch: Partial<AccessPlanBillingRule>) => {
        setRules((current) => current.map((rule, currentIndex) => (
            currentIndex === index ? { ...rule, ...patch } : rule
        )));
    };

    const removeRule = (index: number) => {
        setRules((current) => current.filter((_, currentIndex) => currentIndex !== index));
    };

    const invalid = rules.some((rule) => rule.model_name.trim().length === 0 || !Number.isFinite(rule.multiplier));

    const save = () => {
        updateBilling.mutate(
            {
                access_plan_id: plan.id,
                default_multiplier: defaultMultiplier,
                rules: rules.map((rule) => ({
                    ...rule,
                    model_name: rule.model_name.trim(),
                    multiplier: rule.multiplier,
                    enabled: rule.enabled ?? true,
                })),
            },
            {
                onSuccess: () => toast.success(t('toast.billingUpdated')),
                onError: (error) => {
                    toast.error(t('toast.billingUpdateFailed'), { description: apiErrorMessage(error) });
                },
            }
        );
    };


    const fillJSON = () => {
        setJsonText(JSON.stringify({
            default_multiplier: defaultMultiplier,
            rules: rules.map((rule) => ({
                model_name: rule.model_name,
                multiplier: rule.multiplier,
                enabled: rule.enabled ?? true,
            })),
        }, null, 2));
    };

    const applyJSON = () => {
        try {
            const parsed = JSON.parse(jsonText) as {
                default_multiplier?: unknown;
                rules?: Array<Partial<AccessPlanBillingRule>>;
            } | Array<Partial<AccessPlanBillingRule>>;
            const nextDefault = Array.isArray(parsed)
                ? defaultMultiplier
                : Number(parsed.default_multiplier ?? defaultMultiplier);
            const rawRules = Array.isArray(parsed) ? parsed : parsed.rules;
            if (!Array.isArray(rawRules)) throw new Error('rules must be an array');
            setDefaultMultiplier(Number.isFinite(nextDefault) ? nextDefault : defaultMultiplier);
            setRules(rawRules.map((rule) => ({
                model_name: String(rule.model_name ?? '').trim(),
                multiplier: Number.isFinite(Number(rule.multiplier)) ? Number(rule.multiplier) : 1,
                enabled: rule.enabled ?? true,
            })));
            toast.success(t('toast.jsonApplied'));
        } catch (error) {
            toast.error(t('toast.jsonInvalid'), { description: error instanceof Error ? error.message : String(error) });
        }
    };

    return (
        <div className="rounded-3xl border border-border bg-card p-5">
            <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
                <h3 className="flex items-center gap-2 text-base font-semibold">
                    <BadgeDollarSign className="size-5" />
                    {t('billing.title')}
                </h3>
                <div className="flex items-center gap-2">
                    <Button type="button" variant="outline" className="rounded-xl" onClick={addRule}>
                        <Plus className="size-4" />
                        <span className="min-w-0 truncate">{t('actions.add')}</span>
                    </Button>
                    <Button
                        type="button"
                        className="rounded-xl"
                        disabled={invalid || updateBilling.isPending}
                        onClick={save}
                    >
                        {updateBilling.isPending ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}
                        <span className="min-w-0 truncate">{t('actions.save')}</span>
                    </Button>
                </div>
            </div>

            <label className="mb-4 grid gap-1 text-xs text-muted-foreground md:max-w-xs">
                {t('billing.defaultMultiplier')}
                <Input
                    type="number"
                    step="0.01"
                    min={0}
                    value={defaultMultiplier}
                    onChange={(event) => setDefaultMultiplier(asNumber(event.target.value, 1))}
                    className="rounded-xl"
                />
            </label>

            <div className="mb-4 grid gap-2 rounded-2xl border border-border/70 bg-muted/20 p-3">
                <div className="flex flex-wrap items-center justify-between gap-2">
                    <div className="text-sm font-medium text-card-foreground">{t('billing.jsonTitle')}</div>
                    <div className="flex flex-wrap gap-2">
                        <Button type="button" variant="outline" size="sm" className="rounded-xl" onClick={fillJSON}>
                            <span className="min-w-0 truncate">{t('billing.jsonCopy')}</span>
                        </Button>
                        <Button type="button" variant="outline" size="sm" className="rounded-xl" onClick={applyJSON} disabled={jsonText.trim().length === 0}>
                            <span className="min-w-0 truncate">{t('billing.jsonApply')}</span>
                        </Button>
                    </div>
                </div>
                <textarea
                    value={jsonText}
                    onChange={(event) => setJsonText(event.target.value)}
                    placeholder={t('billing.jsonPlaceholder')}
                    className="min-h-28 rounded-xl border border-input bg-background px-3 py-2 font-mono text-xs text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                />
            </div>


            <datalist id={modelListId}>
                {modelNames.map((model) => <option key={model} value={model} />)}
            </datalist>

            <div className="space-y-2">
                {rules.length === 0 ? (
                    <div className="rounded-2xl border border-dashed border-border px-4 py-8 text-center text-sm text-muted-foreground">
                        {t('billing.empty')}
                    </div>
                ) : rules.map((rule, index) => (
                    <div
                        key={`${rule.id ?? 'new'}-${index}`}
                        className="grid gap-2 rounded-2xl border border-border bg-muted/20 p-3 md:grid-cols-[1fr_120px_auto_auto] md:items-center"
                    >
                        <Input
                            value={rule.model_name}
                            list={modelListId}
                            onChange={(event) => updateRule(index, { model_name: event.target.value })}
                            placeholder={t('billing.modelName')}
                            className="rounded-xl"
                        />
                        <Input
                            type="number"
                            step="0.01"
                            min={0}
                            value={rule.multiplier}
                            onChange={(event) => updateRule(index, { multiplier: asNumber(event.target.value, 1) })}
                            aria-label={t('billing.multiplier')}
                            placeholder={t('billing.multiplier')}
                            className="rounded-xl"
                        />
                        <label className="flex items-center gap-2 text-xs text-muted-foreground">
                            <Switch
                                checked={rule.enabled ?? true}
                                onCheckedChange={(enabled) => updateRule(index, { enabled })}
                            />
                            {t('status.enabled')}
                        </label>
                        <Button
                            type="button"
                            variant="ghost"
                            size="icon"
                            className="rounded-xl text-muted-foreground hover:text-destructive"
                            onClick={() => removeRule(index)}
                            aria-label={t('actions.delete')}
                        >
                            <Trash2 className="size-4" />
                        </Button>
                    </div>
                ))}
            </div>
        </div>
    );
}

// == React Flow 无限画布：手动三列定位 + 家族彩色带（复刻旧样式）+ 缩放/平移/小地图，连线绑接口永不飘 ==

const FAMILY_ORDER: ModelFamilyKey[] = [
    'claude', 'openai', 'gemini', 'deepseek', 'glm', 'qwen', 'kimi', 'grok', 'other',
];

// minimap 用家族色（与 FAMILY_STYLES 的 tailwind -500 对应；canvas 不吃 tailwind 类，必须给 hex）
const FAMILY_HEX: Record<ModelFamilyKey, string> = {
    claude: '#f97316', openai: '#14b8a6', gemini: '#0ea5e9', deepseek: '#6366f1',
    glm: '#8b5cf6', qwen: '#f43f5e', kimi: '#d946ef', grok: '#71717a', other: '#64748b',
};

type RouteFamilyGroup = { key: ModelFamilyKey; label: string; rows: RouteTargetGroup[]; targetTotal: number };

function groupRowsByFamily(rows: RouteTargetGroup[]): RouteFamilyGroup[] {
    const map = new Map<ModelFamilyKey, RouteTargetGroup[]>();
    for (const row of rows) {
        const key = modelFamilyKey(row.requestModel);
        const bucket = map.get(key) ?? [];
        bucket.push(row);
        map.set(key, bucket);
    }
    return FAMILY_ORDER
        .filter((key) => map.has(key))
        .map((key) => {
            const groupRows = map.get(key)!;
            return { key, label: FAMILY_STYLES[key].label, rows: groupRows, targetTotal: groupRows.reduce((sum, r) => sum + r.targets.length, 0) };
        });
}

// —— 手动布局尺寸/坐标（复刻旧版三列对齐）——
const PLAN_W = 200, PLAN_H = 92;
const REQ_W = 244, REQ_H = 84;
const TGT_W = 430, TGT_H = 68;
const PLAN_X = 0;
const BAND_X = 300;
const BAND_PAD = 16;
const BAND_HEADER_H = 40;
const REQ_X = BAND_X + BAND_PAD;
const TGT_X = REQ_X + REQ_W + 76;
const BAND_W = (TGT_X + TGT_W + BAND_PAD) - BAND_X;
const ROW_GAP = 16;
const TGT_GAP = 10;
const FAMILY_GAP = 26;
const TOP_PAD = 8;

type PlanNodeData = { slug: string; name: string; multiplier: number };
type RequestNodeData = {
    requestModel: string;
    family: ModelFamilyKey;
    onEdit: () => void;
};
type TargetNodeData = {
    channelName: string;
    channelId: number;
    priority: number;
    showPriority: boolean;
    weight: number;
    upstreamModel: string;
    enabled: boolean;
    fallback: string;
    multiplier: number | string;
    billingSource: string;
};
type FamilyBandData = { family: ModelFamilyKey; label: string; modelCount: number; targetCount: number; width: number; height: number };
// 渠道级 model_mapping：客户端请求 fromModel 这个名，发上游时改成 toModel 这个名（与方案路由无关，只读展示）
type ChannelModelMapping = { channelId: number; channelName: string; fromModel: string; toModel: string };
type MapFromNodeData = { fromModel: string };
type MapToNodeData = { channelName: string; channelId: number; toModel: string };
type ChannelMapBandData = { label: string; count: number; width: number; height: number };

type PlanFlowNode = Node<PlanNodeData, 'plan'>;
type RequestFlowNode = Node<RequestNodeData, 'request'>;
type TargetFlowNode = Node<TargetNodeData, 'target'>;
type FamilyBandFlowNode = Node<FamilyBandData, 'familyBand'>;
type MapFromFlowNode = Node<MapFromNodeData, 'mapFrom'>;
type MapToFlowNode = Node<MapToNodeData, 'mapTo'>;
type ChannelMapBandFlowNode = Node<ChannelMapBandData, 'channelMapBand'>;
type FlowNode =
    | PlanFlowNode | RequestFlowNode | TargetFlowNode | FamilyBandFlowNode
    | MapFromFlowNode | MapToFlowNode | ChannelMapBandFlowNode;

function PlanFlowCard({ data }: NodeProps<PlanFlowNode>) {
    return (
        <div className="rounded-2xl border border-primary/25 bg-primary/10 p-3 shadow-sm" style={{ width: PLAN_W }}>
            <div className="text-[10px] font-bold uppercase tracking-[0.2em] text-primary">{data.slug}</div>
            <div className="mt-1 truncate text-sm font-black text-foreground">{data.name}</div>
            <div className="mt-2 text-xs text-muted-foreground">默认倍率 {data.multiplier}x</div>
            <Handle type="source" position={Position.Right} className="!size-2 !border-2 !border-background !bg-primary" />
        </div>
    );
}

function RequestFlowCard({ data }: NodeProps<RequestFlowNode>) {
    return (
        <div className="rounded-2xl border border-amber-500/25 bg-amber-500/10 px-3 py-2.5 shadow-sm" style={{ width: REQ_W }}>
            <Handle type="target" position={Position.Left} className="!size-2 !border-2 !border-background !bg-amber-500" />
            <div className="flex items-center justify-between gap-2">
                <span className="inline-flex items-center gap-1.5 text-[10px] font-bold uppercase tracking-[0.16em] text-amber-600 dark:text-amber-300">
                    <span className="size-1.5 rounded-full bg-amber-500" />
                    原请求模型
                </span>
                <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    className="nodrag nopan h-6 gap-1 rounded-lg border-amber-500/30 bg-transparent px-2 text-[11px] text-amber-700 hover:bg-amber-500/15 dark:text-amber-200"
                    onClick={data.onEdit}
                    onPointerDown={(event) => event.stopPropagation()}
                >
                    <PencilLine className="size-3" />{accessPlanText('routes.quickEdit')}
                </Button>
            </div>
            <div className="mt-1.5 break-all font-mono text-[13px] font-black leading-snug text-foreground line-clamp-2">{data.requestModel}</div>
            <Handle type="source" position={Position.Right} className="!size-2 !border-2 !border-background !bg-amber-500" />
        </div>
    );
}

function TargetFlowCard({ data }: NodeProps<TargetFlowNode>) {
    return (
        <div
            className={cn('grid grid-cols-[minmax(84px,1.1fr)_minmax(96px,1.4fr)_auto_auto] items-center gap-3 rounded-2xl border bg-card/90 px-3 py-2 shadow-sm', data.enabled ? 'border-emerald-500/25' : 'border-border opacity-65')}
            style={{ width: TGT_W }}
        >
            <Handle type="target" position={Position.Left} className="!size-2 !border-2 !border-background !bg-emerald-500" />
            <div className="min-w-0">
                <div className="flex min-w-0 items-center gap-1.5">
                    <span className={cn('size-2 shrink-0 rounded-full', data.enabled ? 'bg-emerald-500' : 'bg-muted-foreground')} />
                    <span className="truncate text-sm font-bold text-foreground">{data.channelName}</span>
                </div>
                <div className="mt-0.5 text-[10px] text-muted-foreground">#{data.channelId} · {data.showPriority ? `P${data.priority}` : '并列'}</div>
            </div>
            <div className="min-w-0">
                <div className="text-[9px] font-bold uppercase tracking-[0.14em] text-muted-foreground">{accessPlanText('routes.upstreamTarget')}</div>
                <div className="mt-0.5 truncate font-mono text-xs font-semibold text-foreground">
                    {data.upstreamModel}
                </div>
            </div>
            <div className="text-xs text-muted-foreground">
                <div className="font-bold text-foreground">W{data.weight}</div>
                <div className="text-[10px]">{data.fallback}</div>
            </div>
            <div className="text-xs text-muted-foreground">
                <div className="font-bold text-foreground">{data.multiplier}x</div>
                <div className="truncate text-[10px]">{data.billingSource}</div>
            </div>
        </div>
    );
}

function FamilyBandCard({ data }: NodeProps<FamilyBandFlowNode>) {
    const s = FAMILY_STYLES[data.family];
    return (
        <div className={cn('rounded-2xl border', s.ring, s.soft)} style={{ width: data.width, height: data.height }}>
            <div className="flex items-center gap-2.5 px-4 py-2.5">
                <span className={cn('size-2.5 shrink-0 rounded-full', s.dot)} />
                <span className={cn('text-sm font-black', s.text)}>{data.label}</span>
                <Badge variant="secondary" className="rounded-full">{data.modelCount} 模型</Badge>
                <Badge variant="outline" className="rounded-full">{data.targetCount} 映射</Badge>
            </div>
        </div>
    );
}

// 渠道映射：左卡（客户端请求名，复用琥珀请求卡风格，只读无编辑按钮）
function MapFromCard({ data }: NodeProps<MapFromFlowNode>) {
    return (
        <div className="rounded-2xl border border-amber-500/25 bg-amber-500/10 px-3 py-2.5 shadow-sm" style={{ width: REQ_W }}>
            <div className="inline-flex items-center gap-1.5 text-[10px] font-bold uppercase tracking-[0.16em] text-amber-600 dark:text-amber-300">
                <span className="size-1.5 rounded-full bg-amber-500" />
                客户端请求名
            </div>
            <div className="mt-1.5 break-all font-mono text-[13px] font-black leading-snug text-foreground line-clamp-2">{data.fromModel}</div>
            <Handle type="source" position={Position.Right} className="!size-2 !border-2 !border-background !bg-amber-500" />
        </div>
    );
}

// 渠道映射：右卡（#渠道 · 上游名，复用目标卡风格）
function MapToCard({ data }: NodeProps<MapToFlowNode>) {
    return (
        <div className="grid grid-cols-[minmax(84px,1.1fr)_minmax(96px,1.6fr)] items-center gap-3 rounded-2xl border border-emerald-500/25 bg-card/90 px-3 py-2 shadow-sm" style={{ width: TGT_W }}>
            <Handle type="target" position={Position.Left} className="!size-2 !border-2 !border-background !bg-emerald-500" />
            <div className="min-w-0">
                <div className="flex min-w-0 items-center gap-1.5">
                    <span className="size-2 shrink-0 rounded-full bg-emerald-500" />
                    <span className="truncate text-sm font-bold text-foreground">{data.channelName}</span>
                </div>
                <div className="mt-0.5 text-[10px] text-muted-foreground">#{data.channelId}</div>
            </div>
            <div className="min-w-0">
                <div className="text-[9px] font-bold uppercase tracking-[0.14em] text-muted-foreground">发送上游名</div>
                <div className="mt-0.5 truncate font-mono text-xs font-semibold text-foreground">{data.toModel}</div>
            </div>
        </div>
    );
}

// 渠道映射家族带（独立于方案路由带，青色区分）
function ChannelMapBandCard({ data }: NodeProps<ChannelMapBandFlowNode>) {
    return (
        <div className="rounded-2xl border border-cyan-500/30 bg-cyan-500/[0.07]" style={{ width: data.width, height: data.height }}>
            <div className="flex items-center gap-2.5 px-4 py-2.5">
                <span className="size-2.5 shrink-0 rounded-full bg-cyan-500" />
                <span className="text-sm font-black text-cyan-600 dark:text-cyan-300">{data.label}</span>
                <Badge variant="secondary" className="rounded-full">{data.count} 映射</Badge>
            </div>
        </div>
    );
}

const flowNodeTypes: NodeTypes = {
    plan: PlanFlowCard,
    request: RequestFlowCard,
    target: TargetFlowCard,
    familyBand: FamilyBandCard,
    mapFrom: MapFromCard,
    mapTo: MapToCard,
    channelMapBand: ChannelMapBandCard,
};

function miniMapNodeColor(node: Node): string {
    if (node.type === 'familyBand' || node.type === 'channelMapBand') return 'transparent';
    if (node.type === 'plan') return '#3b82f6';
    if (node.type === 'request' || node.type === 'mapFrom') {
        const fam = (node.data as Partial<RequestNodeData>).family;
        return fam ? FAMILY_HEX[fam] : '#f59e0b';
    }
    if (node.type === 'mapTo') return '#10b981';
    const enabled = (node.data as Partial<TargetNodeData>).enabled;
    return enabled ? '#10b981' : '#94a3b8';
}

function flowNodeSize(node: FlowNode): { w: number; h: number } {
    if (node.type === 'plan') return { w: PLAN_W, h: PLAN_H };
    if (node.type === 'request' || node.type === 'mapFrom') return { w: REQ_W, h: REQ_H };
    if (node.type === 'target' || node.type === 'mapTo') return { w: TGT_W, h: TGT_H };
    return { w: node.data.width, h: node.data.height };
}

// 手动三列布局：方案(左) → 家族彩色带(内含 请求列 + 目标列对齐)；连线由 React Flow 绑接口自动跟随
function buildRouteFlow(
    plan: AccessPlan,
    rows: RouteTargetGroup[],
    channelNameByID: Map<number, string>,
    onEditRequest: (requestModel: string) => void,
    channelMappings: ChannelModelMapping[] = [],
): { nodes: FlowNode[]; edges: Edge[] } {
    const nodes: FlowNode[] = [];
    const edges: Edge[] = [];
    const planId = 'plan';
    const groups = groupRowsByFamily(rows);
    let y = TOP_PAD;

    for (const group of groups) {
        const bandTop = y;
        let rowY = bandTop + BAND_HEADER_H + BAND_PAD;
        for (const row of group.rows) {
            const reqId = `req:${row.requestKey || row.requestModel}`;
            const nT = row.targets.length;
            const targetsH = nT > 0 ? nT * TGT_H + (nT - 1) * TGT_GAP : TGT_H;
            const rowH = Math.max(REQ_H, targetsH);
            nodes.push({
                id: reqId,
                type: 'request',
                position: { x: REQ_X, y: rowY + (rowH - REQ_H) / 2 },
                zIndex: 1,
                data: {
                    requestModel: cleanOneMillionModelName(row.requestModel),
                    family: modelFamilyKey(row.requestModel),
                    onEdit: () => onEditRequest(row.requestKey ? row.requestModel : ''),
                },
            });
            edges.push({
                id: `edge:${planId}:${reqId}`,
                source: planId,
                target: reqId,
                type: 'default',
                style: { stroke: 'var(--primary)', strokeWidth: 1.5, strokeOpacity: 0.5 },
            });
            const prioritySet = new Set(row.targets.map((tgt) => tgt.priority || 0));
            const showPriority = prioritySet.size > 1;
            const startY = rowY + (rowH - targetsH) / 2;
            row.targets.forEach((target, targetIndex) => {
                const tgtId = `tgt:${reqId}:${target.id ?? targetIndex}`;
                const channelName = channelNameByID.get(target.channel_id) ?? accessPlanText('routes.missingChannel', { id: target.channel_id });
                const billing = effectiveMultiplier(plan, billingModelName(target));
                nodes.push({
                    id: tgtId,
                    type: 'target',
                    position: { x: TGT_X, y: startY + targetIndex * (TGT_H + TGT_GAP) },
                    zIndex: 1,
                    data: {
                        channelName,
                        channelId: target.channel_id,
                        priority: target.priority || 0,
                        showPriority,
                        weight: target.weight || 1,
                        upstreamModel: cleanOneMillionModelName(target.upstream_model || accessPlanText('routes.unset')),
                        enabled: target.enabled,
                        fallback: fallbackModeLabel(target.fallback_mode),
                        multiplier: billing.multiplier,
                        billingSource: billingSourceLabel(target.billing_model_source),
                    },
                });
                edges.push({
                    id: `edge:${reqId}:${tgtId}`,
                    source: reqId,
                    target: tgtId,
                    type: 'default',
                    style: target.enabled
                        ? { stroke: '#f59e0b', strokeWidth: 1.5, strokeOpacity: 0.5 }
                        : { stroke: '#f59e0b', strokeWidth: 1.5, strokeOpacity: 0.25, strokeDasharray: '4 4' },
                });
            });
            rowY += rowH + ROW_GAP;
        }
        const bandHeight = (rowY - ROW_GAP) - bandTop + BAND_PAD;
        nodes.push({
            id: `band:${group.key}`,
            type: 'familyBand',
            position: { x: BAND_X, y: bandTop },
            zIndex: 0,
            selectable: false,
            draggable: false,
            data: { family: group.key, label: group.label, modelCount: group.rows.length, targetCount: group.targetTotal, width: BAND_W, height: bandHeight },
        });
        y = (rowY - ROW_GAP) + FAMILY_GAP;
    }

    const totalHeight = Math.max(PLAN_H, (y - FAMILY_GAP) - TOP_PAD);
    nodes.push({
        id: planId,
        type: 'plan',
        position: { x: PLAN_X, y: TOP_PAD + (totalHeight - PLAN_H) / 2 },
        zIndex: 1,
        data: { slug: plan.slug, name: plan.display_name || plan.slug, multiplier: plan.default_multiplier ?? 1 },
    });

    // ── 渠道模型映射带：追加在方案路由家族带之后，独立不连方案节点（只读展示）──
    // 上面的 totalHeight / 方案节点定位只依赖路由家族带，本段纯追加、不回改任何路由布局。
    if (channelMappings.length > 0) {
        const bandTop = y; // y 已在最后一条家族带底部下方留了 FAMILY_GAP（无家族带时为 TOP_PAD）
        let rowY = bandTop + BAND_HEADER_H + BAND_PAD;
        const rowH = Math.max(REQ_H, TGT_H);
        for (const mapping of channelMappings) {
            const fromId = `map-from:${mapping.channelId}:${mapping.fromModel}`;
            const toId = `map-to:${mapping.channelId}:${mapping.fromModel}`;
            nodes.push({
                id: fromId,
                type: 'mapFrom',
                position: { x: REQ_X, y: rowY + (rowH - REQ_H) / 2 },
                zIndex: 1,
                data: { fromModel: mapping.fromModel },
            });
            nodes.push({
                id: toId,
                type: 'mapTo',
                position: { x: TGT_X, y: rowY + (rowH - TGT_H) / 2 },
                zIndex: 1,
                data: { channelName: mapping.channelName, channelId: mapping.channelId, toModel: mapping.toModel },
            });
            edges.push({
                id: `edge:${fromId}:${toId}`,
                source: fromId,
                target: toId,
                type: 'default',
                style: { stroke: '#06b6d4', strokeWidth: 1.5, strokeOpacity: 0.5 },
            });
            rowY += rowH + ROW_GAP;
        }
        const bandHeight = (rowY - ROW_GAP) - bandTop + BAND_PAD;
        nodes.push({
            id: 'band:channel-mapping',
            type: 'channelMapBand',
            position: { x: BAND_X, y: bandTop },
            zIndex: 0,
            selectable: false,
            draggable: false,
            data: { label: '渠道模型映射（发送改名）', count: channelMappings.length, width: BAND_W, height: bandHeight },
        });
    }

    return { nodes, edges };
}

type RouteFlowCanvasProps = {
    plan: AccessPlan;
    rows: RouteTargetGroup[];
    channels: Array<{ id: number; name: string; enabled: boolean }>;
    channelMappings: ChannelModelMapping[];
    onEditRequest: (requestModel: string) => void;
    onOpenJson?: () => void;
};

function RouteFlowCanvasInner({
    plan, rows, channels, channelMappings, onEditRequest, onOpenJson,
}: RouteFlowCanvasProps) {
    const t = accessPlanText;
    const channelNameByID = useMemo(() => new Map(channels.map((channel) => [channel.id, channel.name])), [channels]);
    const { fitView, setCenter } = useReactFlow();

    const [query, setQuery] = useState('');
    const q = query.trim().toLowerCase();
    const filteredRows = useMemo(() => {
        if (!q) return rows;
        return rows
            .map((row): RouteTargetGroup | null => {
                if (row.requestModel.toLowerCase().includes(q)) return row;
                const targets = row.targets.filter((tgt) => {
                    const chName = (channelNameByID.get(tgt.channel_id) ?? '').toLowerCase();
                    return chName.includes(q) || (tgt.upstream_model ?? '').toLowerCase().includes(q);
                });
                return targets.length ? { ...row, targets } : null;
            })
            .filter((row): row is RouteTargetGroup => row !== null);
    }, [rows, q, channelNameByID]);

    const filteredMappings = useMemo(() => {
        if (!q) return channelMappings;
        return channelMappings.filter((mapping) => (
            mapping.fromModel.toLowerCase().includes(q)
            || mapping.toModel.toLowerCase().includes(q)
            || mapping.channelName.toLowerCase().includes(q)
        ));
    }, [channelMappings, q]);

    const flow = useMemo(
        () => buildRouteFlow(plan, filteredRows, channelNameByID, onEditRequest, filteredMappings),
        [plan, filteredRows, channelNameByID, onEditRequest, filteredMappings],
    );

    const [nodes, setNodes, onNodesChange] = useNodesState<FlowNode>(flow.nodes);
    const [edges, setEdges, onEdgesChange] = useEdgesState<Edge>(flow.edges);

    useEffect(() => {
        setNodes(flow.nodes);
        setEdges(flow.edges);
        const raf = requestAnimationFrame(() => fitView({ padding: 0.14, maxZoom: 1, duration: 220 }));
        return () => cancelAnimationFrame(raf);
    }, [flow, setNodes, setEdges, fitView]);

    const showAll = useCallback(() => fitView({ padding: 0.12, duration: 400 }), [fitView]);

    const focusReadable = useCallback(() => {
        if (nodes.length === 0) return;
        let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
        for (const node of nodes) {
            const { w, h } = flowNodeSize(node);
            minX = Math.min(minX, node.position.x);
            minY = Math.min(minY, node.position.y);
            maxX = Math.max(maxX, node.position.x + w);
            maxY = Math.max(maxY, node.position.y + h);
        }
        setCenter((minX + maxX) / 2, (minY + maxY) / 2, { zoom: 0.85, duration: 400 });
    }, [nodes, setCenter]);

    const targetCount = filteredRows.reduce((sum, row) => sum + row.targets.length, 0);

    return (
        <section className="flex min-h-0 flex-1 flex-col overflow-hidden rounded-2xl border border-border/70 bg-background/70">
            <div className="flex flex-wrap items-center justify-between gap-3 border-b border-border/70 px-4 py-3">
                <div className="min-w-0">
                    <div className="flex items-center gap-2 text-sm font-black">
                        <GitBranch className="size-4 text-primary" />
                        <span>{t('routes.canvasTitle')}</span>
                        <Badge variant="outline" className="rounded-full">∞ canvas</Badge>
                        {onOpenJson ? (
                            <Button
                                type="button"
                                variant="outline"
                                size="sm"
                                className="h-6 rounded-full px-2.5 text-xs font-medium"
                                onClick={onOpenJson}
                                title="管理当前画布 JSON（展开高级编辑）"
                            >
                                JSON
                            </Button>
                        ) : null}
                    </div>
                    <p className="mt-1 text-xs text-muted-foreground">{t('routes.canvasHint')}</p>
                </div>
                <div className="flex flex-wrap items-center gap-2 text-[11px] text-muted-foreground">
                    <div className="relative">
                        <Search className="pointer-events-none absolute left-2 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
                        <input
                            value={query}
                            onChange={(event) => setQuery(event.target.value)}
                            placeholder="搜模型 / 渠道 / 上游…"
                            className="h-7 w-40 rounded-full border border-border/70 bg-background/60 pl-7 pr-2 text-xs text-foreground outline-none focus:border-primary/50 sm:w-48"
                        />
                    </div>
                    <Badge variant="secondary" className="rounded-full">{planTitle(plan)}</Badge>
                    <Badge variant="outline" className="rounded-full">{t('routes.targetCount', { count: targetCount })}</Badge>
                </div>
            </div>

            {filteredRows.length === 0 ? (
                <div className="px-4 py-8 text-center text-sm text-muted-foreground">
                    {rows.length === 0 ? t('routes.canvasEmpty') : `没有匹配「${query.trim()}」的模型或渠道`}
                </div>
            ) : (
                <div className="min-h-0 flex-1 h-[clamp(300px,calc(100dvh-26rem),820px)] sm:h-[clamp(340px,calc(100dvh-22rem),860px)]">
                    <ReactFlow<FlowNode, Edge>
                        nodes={nodes}
                        edges={edges}
                        onNodesChange={onNodesChange}
                        onEdgesChange={onEdgesChange}
                        nodeTypes={flowNodeTypes}
                        colorMode="system"
                        fitView
                        fitViewOptions={{ padding: 0.14, maxZoom: 1 }}
                        minZoom={0.15}
                        maxZoom={1.8}
                        nodesDraggable={false}
                        nodesConnectable={false}
                        elementsSelectable={false}
                        edgesFocusable={false}
                        zoomOnDoubleClick={false}
                        proOptions={{ hideAttribution: true }}
                    >
                        <Background variant={BackgroundVariant.Dots} gap={32} size={1} />
                        <MiniMap pannable zoomable onClick={(_event, position) => setCenter(position.x, position.y, { zoom: 1, duration: 400 })} nodeColor={miniMapNodeColor} nodeStrokeWidth={2} className="!hidden sm:!block" />
                        <Controls showInteractive={false} />
                        <Panel position="top-right" className="flex gap-1.5">
                            <button
                                type="button"
                                onClick={showAll}
                                className="rounded-full border border-border/70 bg-background/85 px-2.5 py-1 text-[11px] font-medium text-foreground shadow-sm backdrop-blur hover:bg-muted"
                            >
                                显示全部
                            </button>
                            <button
                                type="button"
                                onClick={focusReadable}
                                className="rounded-full border border-border/70 bg-background/85 px-2.5 py-1 text-[11px] font-medium text-foreground shadow-sm backdrop-blur hover:bg-muted"
                            >
                                看得清
                            </button>
                        </Panel>
                    </ReactFlow>
                </div>
            )}
        </section>
    );
}

function RouteFlowCanvas(props: RouteFlowCanvasProps) {
    return (
        <ReactFlowProvider>
            <RouteFlowCanvasInner {...props} />
        </ReactFlowProvider>
    );
}

function RouteTargetEditorCard({
    planId,
    target,
    index,
    channels,
    requestModelListId,
    upstreamModels,
    modelsForChannel,
    updateTarget,
    removeTarget,
    compact = false,
    idPrefix = 'access-plan-upstream-models',
    lockRequestModel = false,
}: {
    planId: number;
    target: AccessPlanRouteTarget;
    index: number;
    channels: Array<{ id: number; name: string; enabled: boolean }>;
    requestModelListId: string;
    upstreamModels: string[];
    modelsForChannel: (channelId: number) => string[];
    updateTarget: (index: number, patch: Partial<AccessPlanRouteTarget>) => void;
    removeTarget: (index: number) => void;
    compact?: boolean;
    idPrefix?: string;
    lockRequestModel?: boolean;
}) {
    const t = accessPlanText;
    const upstreamListId = `${idPrefix}-${planId}-${index}`;
    const selectedChannelKnown = target.channel_id <= 0 || channels.some((channel) => channel.id === target.channel_id);

    return (
        <div className={cn('min-w-0 rounded-xl border border-border/80 bg-muted/15', compact ? 'p-2.5' : 'p-3')}>
            <div className={cn('grid min-w-0 gap-2 xl:items-end', lockRequestModel ? 'xl:grid-cols-[minmax(120px,0.9fr)_minmax(0,1fr)_auto]' : 'xl:grid-cols-[minmax(0,1fr)_minmax(120px,0.8fr)_minmax(0,1fr)_auto]')}>
                {/* In the per-model quick-edit modal the request model is fixed (shown in
                    the title), so the redundant per-target request-model input is hidden;
                    it stays editable only in the full list where a new mapping is added. */}
                {!lockRequestModel && (
                    <label className="grid min-w-0 gap-1 text-xs text-muted-foreground">
                        {t('routes.requestModel')}
                        <Input
                            value={target.request_model}
                            list={requestModelListId}
                            onChange={(event) => updateTarget(index, { request_model: cleanOneMillionModelName(event.target.value) })}
                            placeholder={t('routes.requestModel')}
                            disabled={lockRequestModel}
                            className="h-9 min-w-0 rounded-xl"
                        />
                    </label>
                )}
                <label className="grid min-w-0 gap-1 text-xs text-muted-foreground">
                    {t('routes.targetSummary')}
                    <SearchableSelect
                        value={String(target.channel_id)}
                        onValueChange={(value) => {
                            const channelId = Number(value);
                            updateTarget(index, {
                                channel_id: channelId,
                                upstream_model: modelsForChannel(channelId)[0] ?? '',
                            });
                        }}
                        options={[
                            ...(!selectedChannelKnown
                                ? [{ value: String(target.channel_id), label: t('routes.missingChannel', { id: target.channel_id }) }]
                                : []),
                            ...(channels.length === 0
                                ? [{ value: '0', label: t('routes.noChannels'), disabled: true }]
                                : []),
                            ...channels
                                .filter((channel) => channel.enabled !== false || channel.id === target.channel_id)
                                .map((channel) => ({
                                    value: String(channel.id),
                                    label: `#${channel.id} ${channel.name}`,
                                    keywords: channel.name,
                                })),
                        ]}
                        placeholder={t('routes.targetSummary')}
                        searchPlaceholder="搜索渠道名或 #ID…"
                        emptyText="没有匹配的渠道"
                        className="h-9 w-full"
                    />
                </label>
                <datalist id={upstreamListId}>
                    {upstreamModels.map((model) => <option key={model} value={model} />)}
                </datalist>
                <label className="grid min-w-0 gap-1 text-xs text-muted-foreground">
                    {t('routes.upstreamModel')}
                    <Input
                        value={target.upstream_model}
                        list={upstreamListId}
                        onChange={(event) => updateTarget(index, { upstream_model: cleanOneMillionModelName(event.target.value) })}
                        placeholder={t('routes.upstreamModel')}
                        className="h-9 min-w-0 rounded-xl"
                    />
                </label>
                <div className="flex min-w-0 flex-wrap items-center gap-2 xl:justify-end">
                    <label className="flex shrink-0 items-center gap-2 text-xs text-muted-foreground">
                        <Switch
                            checked={target.enabled}
                            onCheckedChange={(enabled) => updateTarget(index, { enabled })}
                        />
                        {t('status.enabled')}
                    </label>
                    <Button
                        type="button"
                        variant="ghost"
                        size="icon"
                        className="shrink-0 rounded-xl text-muted-foreground hover:text-destructive"
                        onClick={() => removeTarget(index)}
                        aria-label={t('actions.delete')}
                    >
                        <Trash2 className="size-4" />
                    </Button>
                </div>
            </div>

            {/* 高级（可选）：同优先级轮询、失败兜底、系统提示词覆盖 —— 默认折叠，主行只保留"请求模型→渠道·发送模型" */}
            <details className="mt-2 min-w-0 rounded-xl border border-border/60 bg-background/40">
                <summary className="flex cursor-pointer list-none items-center justify-between gap-2 px-2.5 py-2 text-xs font-medium text-foreground [&::-webkit-details-marker]:hidden">
                    <span className="min-w-0 truncate">{t('routes.advancedTitle')}</span>
                    <span className="min-w-0 truncate text-[11px] font-normal text-muted-foreground">{t('routes.advancedHint')}</span>
                </summary>
                <div className="grid min-w-0 gap-2 border-t border-border/60 p-2.5">
                    <div className="grid min-w-0 gap-2 sm:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_minmax(150px,1fr)]">
                        <label className="grid min-w-0 gap-1 text-xs text-muted-foreground">
                            {t('routes.priority')}
                            <Input
                                type="number"
                                min={1}
                                step={1}
                                value={target.priority}
                                onChange={(event) => updateTarget(index, { priority: asPositiveInt(event.target.value, 1) })}
                                aria-label={t('routes.priority')}
                                placeholder={t('routes.priority')}
                                className="h-9 rounded-xl"
                            />
                        </label>
                        <label className="grid min-w-0 gap-1 text-xs text-muted-foreground">
                            {t('routes.weight')}
                            <Input
                                type="number"
                                min={1}
                                step={1}
                                value={target.weight}
                                onChange={(event) => updateTarget(index, { weight: asPositiveInt(event.target.value, 1) })}
                                aria-label={t('routes.weight')}
                                placeholder={t('routes.weight')}
                                className="h-9 rounded-xl"
                            />
                        </label>
                        <label className="grid min-w-0 gap-1 text-xs text-muted-foreground">
                            {t('routes.fallback')}
                            <select
                                value={target.fallback_mode === 'failover' ? 'return_group' : target.fallback_mode ?? 'return_group'}
                                onChange={(event) => updateTarget(index, {
                                    fallback_mode: event.target.value as AccessPlanRouteTarget['fallback_mode'],
                                })}
                                aria-label={t('routes.fallback')}
                                className="h-9 w-full min-w-0 rounded-xl border border-input bg-background px-3 text-sm text-foreground"
                            >
                                <option value="none">{t('routes.fallbackNone')}</option>
                                <option value="return_group">{t('routes.fallbackReturnGroup')}</option>
                            </select>
                        </label>
                    </div>
                    <div className="grid min-w-0 gap-2 lg:grid-cols-[170px_minmax(0,1fr)]">
                        <select
                            value={target.prompt_override_mode ?? 'append_system'}
                            onChange={(event) => updateTarget(index, {
                                prompt_override_mode: event.target.value as AccessPlanRouteTarget['prompt_override_mode'],
                            })}
                            aria-label={t('routes.promptOverride')}
                            className="h-9 w-full min-w-0 rounded-xl border border-input bg-background px-3 text-sm text-foreground"
                        >
                            <option value="append_system">{t('routes.promptAppend')}</option>
                            <option value="replace_system">{t('routes.promptReplace')}</option>
                        </select>
                        <textarea
                            value={target.system_prompt_override ?? ''}
                            onChange={(event) => updateTarget(index, { system_prompt_override: event.target.value })}
                            placeholder={t('routes.promptPlaceholder')}
                            className="min-h-14 w-full min-w-0 rounded-xl border border-input bg-background px-3 py-2 text-sm text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                        />
                    </div>
                </div>
            </details>
        </div>
    );
}

function RouteTargetsEditor({
    plan,
    channels,
    channelMappings,
    modelNames,
    channelModels,
    channelModelsReady,
}: {
    plan: AccessPlan;
    channels: Array<{ id: number; name: string; enabled: boolean }>;
    channelMappings: ChannelModelMapping[];
    modelNames: string[];
    channelModels: RouteChannelModel[];
    channelModelsReady: boolean;
}) {
    const t = accessPlanText;
    const updateRoutes = useUpdateAccessPlanRouteTargets();
    const [targets, setTargets] = useState<AccessPlanRouteTarget[]>(() => plan.route_targets.map(cleanRouteTargetModels));
    const [jsonText, setJsonText] = useState('');
    const [editingRequestModel, setEditingRequestModel] = useState<string | null>(null);
    const requestModelListId = `access-plan-request-models-${plan.id}`;
    const channelModelIndex = useMemo(
        () => buildChannelModelIndex(channelModels, channelModelsReady),
        [channelModels, channelModelsReady]
    );
    const editableTargets = useMemo(() => activeRouteTargets(targets, channelModelIndex), [targets, channelModelIndex]);
    const canvasRows = useMemo(() => groupedRouteTargets(editableTargets), [editableTargets]);
    const staleTargetCount = targets.length - editableTargets.length;
    const editingRequestKey = editingRequestModel ? cleanOneMillionModelName(editingRequestModel).toLowerCase() : '';
    const editingTargets = useMemo(() => (
        editingRequestModel !== null
            ? editableTargets.filter((target) => cleanOneMillionModelName(target.request_model).toLowerCase() === editingRequestKey)
            : []
    ), [editableTargets, editingRequestKey, editingRequestModel]);

    const modelsForChannel = (channelId: number) => (
        channelModels
            .filter((model) => model.channel_id === channelId && model.enabled !== false)
            .map((model) => cleanOneMillionModelName(model.name))
            .filter(Boolean)
            .sort((a, b) => a.localeCompare(b))
    );

    const addTarget = () => {
        const channelId = channels[0]?.id ?? 0;
        const upstreamModel = modelsForChannel(channelId)[0] ?? '';

        setTargets((current) => [
            ...current,
            {
                request_model: '',
                channel_id: channelId,
                upstream_model: upstreamModel,
                priority: current.length + 1,
                weight: 1,
                enabled: true,
                billing_model_source: 'request_model',
                fallback_mode: 'return_group',
                system_prompt_override: '',
                prompt_override_mode: 'append_system',
            },
        ]);
        setEditingRequestModel('');
    };

    const updateTarget = (index: number, patch: Partial<AccessPlanRouteTarget>) => {
        setTargets((current) => current.map((target, currentIndex) => (
            currentIndex === index ? { ...target, ...patch } : target
        )));
    };

    const removeTarget = (index: number) => {
        setTargets((current) => current.filter((_, currentIndex) => currentIndex !== index));
    };

    const invalid = editableTargets.some((target) => (
        target.request_model.trim().length === 0 ||
        target.channel_id <= 0 ||
        target.upstream_model.trim().length === 0 ||
        (
            target.billing_model_source === 'override_model' &&
            (target.billing_model_override?.trim().length ?? 0) === 0
        )
    ));

    const saveTargets = (
        nextTargets: AccessPlanRouteTarget[],
        successMessage = t('toast.routesUpdated'),
        onSuccess?: () => void
    ) => {
        updateRoutes.mutate(
            {
                access_plan_id: plan.id,
                targets: nextTargets.map((target, index) => ({
                    ...target,
                    request_model: cleanOneMillionModelName(target.request_model),
                    upstream_model: cleanOneMillionModelName(target.upstream_model),
                    priority: target.priority || index + 1,
                    weight: target.weight || 1,
                    enabled: target.enabled,
                    billing_model_source: target.billing_model_source ?? 'request_model',
                    billing_model_override: cleanOneMillionModelName(target.billing_model_override ?? '') || undefined,
                    fallback_mode: target.fallback_mode ?? 'return_group',
                    system_prompt_override: target.system_prompt_override?.trim() || undefined,
                    prompt_override_mode: target.prompt_override_mode ?? 'append_system',
                })),
            },
            {
                onSuccess: (data) => {
                    setTargets((data.route_targets ?? []).map(cleanRouteTargetModels));
                    toast.success(successMessage);
                    onSuccess?.();
                },
                onError: (error) => {
                    toast.error(t('toast.routesUpdateFailed'), { description: apiErrorMessage(error) });
                },
            }
        );
    };

    const save = (onSuccess?: () => void) => saveTargets(editableTargets, t('toast.routesUpdated'), onSuccess);

    const rebuild = () => {
        const rebuilt = rebuildRouteTargetsFromChannelModels(channelModels, editableTargets);
        if (rebuilt.length === 0) {
            toast.error(t('toast.routesRebuildEmpty'));
            return;
        }
        setTargets(rebuilt);
        saveTargets(rebuilt, t('toast.routesRebuilt'));
    };

    const fillJSON = () => {
        setJsonText(JSON.stringify(editableTargets.map((target) => ({
            request_model: cleanOneMillionModelName(target.request_model),
            channel_id: target.channel_id,
            upstream_model: cleanOneMillionModelName(target.upstream_model),
            priority: target.priority || 1,
            weight: target.weight || 1,
            enabled: target.enabled,
            billing_model_source: target.billing_model_source ?? 'request_model',
            billing_model_override: cleanOneMillionModelName(target.billing_model_override ?? ''),
            fallback_mode: target.fallback_mode ?? 'return_group',
            prompt_override_mode: target.prompt_override_mode ?? 'append_system',
            system_prompt_override: target.system_prompt_override ?? '',
        })), null, 2));
    };

    const advancedRef = useRef<HTMLDetailsElement>(null);
    const openJson = () => {
        fillJSON();
        const node = advancedRef.current;
        if (node) {
            node.open = true;
            node.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
        }
    };

    const applyJSON = () => {
        try {
            const parsed = JSON.parse(jsonText) as Array<Partial<AccessPlanRouteTarget>> | { targets?: Array<Partial<AccessPlanRouteTarget>> };
            const rawTargets = Array.isArray(parsed) ? parsed : parsed.targets;
            if (!Array.isArray(rawTargets)) throw new Error('targets must be an array');
            const nextTargets = rawTargets.map((target, index) => ({
                request_model: cleanOneMillionModelName(String(target.request_model ?? '')),
                channel_id: Number(target.channel_id ?? 0),
                upstream_model: cleanOneMillionModelName(String(target.upstream_model ?? '')),
                priority: asPositiveInt(String(target.priority ?? index + 1), index + 1),
                weight: asPositiveInt(String(target.weight ?? 1), 1),
                enabled: target.enabled ?? true,
                billing_model_source: target.billing_model_source ?? 'request_model',
                billing_model_override: cleanOneMillionModelName(target.billing_model_override ?? '') || undefined,
                fallback_mode: target.fallback_mode ?? 'return_group',
                system_prompt_override: target.system_prompt_override?.trim() || undefined,
                prompt_override_mode: target.prompt_override_mode ?? 'append_system',
            }));
            setTargets(nextTargets);
            toast.success(t('toast.jsonApplied'));
        } catch (error) {
            toast.error(t('toast.jsonInvalid'), { description: error instanceof Error ? error.message : String(error) });
        }
    };

    return (
        <div className="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto pr-1 pb-24 md:pb-4">
            <div className="flex shrink-0 flex-col gap-2 rounded-2xl border border-border/70 bg-card/70 p-2 sm:flex-row sm:items-center sm:justify-between">
                <div className="flex min-w-0 items-center gap-2 px-1 text-xs text-muted-foreground">
                    <GitBranch className="size-4 shrink-0 text-primary" />
                    <span className="min-w-0 truncate">{t('routes.canvasTitle')}</span>
                    <Badge variant="outline" className="rounded-full">{t('routes.targetCount', { count: editableTargets.length })}</Badge>
                    {staleTargetCount > 0 ? (
                        <Badge variant="secondary" className="rounded-full text-amber-600 dark:text-amber-300">
                            {t('routes.hiddenStale', { count: staleTargetCount })}
                        </Badge>
                    ) : null}
                </div>
                <div className="flex min-w-0 flex-wrap items-center gap-2 sm:justify-end">
                    <Button
                        type="button"
                        variant="outline"
                        className="rounded-xl"
                        disabled={channelModels.length === 0 || updateRoutes.isPending}
                        onClick={rebuild}
                        title={t('routes.rebuildHint')}
                    >
                        <RefreshCcw className="size-4" />
                        <span className="min-w-0 truncate">{t('routes.rebuildGroup')}</span>
                    </Button>
                    <Button type="button" variant="outline" className="rounded-xl" onClick={addTarget}>
                        <Plus className="size-4" />
                        <span className="min-w-0 truncate">{t('actions.add')}</span>
                    </Button>
                    <Button
                        type="button"
                        className="rounded-xl"
                        disabled={invalid || updateRoutes.isPending}
                        onClick={() => save()}
                    >
                        {updateRoutes.isPending ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}
                        <span className="min-w-0 truncate">{t('actions.save')}</span>
                    </Button>
                </div>
            </div>

            <RouteFlowCanvas
                plan={plan}
                rows={canvasRows}
                channels={channels}
                channelMappings={channelMappings}
                onEditRequest={setEditingRequestModel}
                onOpenJson={openJson}
            />

            <details ref={advancedRef} className="shrink-0 rounded-2xl border border-border/70 bg-card/70">
                <summary className="flex cursor-pointer list-none items-center justify-between gap-3 px-3 py-2 text-sm font-medium text-foreground">
                    <span className="min-w-0 truncate">高级编辑</span>
                    <span className="min-w-0 truncate text-xs font-normal text-muted-foreground">JSON 批量导入导出收在这里，不抢主画布</span>
                </summary>
                <div className="grid gap-3 border-t border-border/70 p-3">
                    <div className="grid gap-2 rounded-2xl border border-border/70 bg-muted/15 p-3">
                        <div className="flex flex-wrap items-center justify-between gap-2">
                            <div className="text-sm font-medium text-card-foreground">{t('routes.jsonTitle')}</div>
                            <div className="flex min-w-0 flex-wrap gap-2">
                                <Button type="button" variant="outline" size="sm" className="rounded-xl" onClick={applyJSON} disabled={jsonText.trim().length === 0}>
                                    <span className="min-w-0 truncate">{t('routes.jsonApply')}</span>
                                </Button>
                                <Button type="button" variant="outline" size="sm" className="rounded-xl" onClick={fillJSON}>
                                    <span className="min-w-0 truncate">{t('routes.jsonCopy')}</span>
                                </Button>
                            </div>
                        </div>
                        <textarea
                            value={jsonText}
                            onChange={(event) => setJsonText(event.target.value)}
                            placeholder={t('routes.jsonPlaceholder')}
                            className="min-h-32 rounded-xl border border-input bg-background px-3 py-2 font-mono text-xs text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                        />
                    </div>

                    <div className="rounded-xl border border-border/70 bg-muted/15 px-3 py-2.5 text-xs leading-relaxed text-muted-foreground">
                        <div className="font-medium text-foreground">{t('routes.setupTitle')}</div>
                        <p className="mt-1 break-words">{t('routes.setupHint')}</p>
                        <p className="mt-1 break-words">{t('routes.groupRouteHint')}</p>
                    </div>
                </div>
            </details>

            <Dialog open={editingRequestModel !== null} onOpenChange={(open) => !open && setEditingRequestModel(null)}>
                <DialogContent className="w-[calc(100vw-1rem)] max-w-full overflow-hidden rounded-3xl border-border bg-card p-0 sm:max-w-4xl">
                    <DialogHeader className="border-b border-border px-4 py-4 pr-12 sm:px-5">
                        <DialogTitle className="flex min-w-0 flex-wrap items-center gap-2">
                            {t('routes.quickEditTitle')}
                            {editingRequestModel ? (
                                <span className="min-w-0 break-all font-mono text-sm text-muted-foreground">
                                    {cleanOneMillionModelName(editingRequestModel)}
                                </span>
                            ) : editingRequestModel !== null ? (
                                <span className="min-w-0 break-all font-mono text-sm text-muted-foreground">
                                    {t('routes.unset')}
                                </span>
                            ) : null}
                        </DialogTitle>
                        <DialogDescription>{t('routes.quickEditDescription')}</DialogDescription>
                    </DialogHeader>
                    <div className="max-h-[calc(100dvh-12rem)] min-w-0 space-y-2 overflow-y-auto px-4 py-3 sm:px-5">
                        {editingTargets.length === 0 ? (
                            <div className="rounded-xl border border-dashed border-border px-4 py-6 text-center text-sm text-muted-foreground">
                                {t('routes.empty')}
                            </div>
                        ) : editingTargets.map((target) => {
                            const index = targets.indexOf(target);
                            return (
                                <RouteTargetEditorCard
                                    key={`dialog-${target.id ?? 'new'}-${index}`}
                                    planId={plan.id}
                                    target={target}
                                    index={index}
                                    channels={channels}
                                    requestModelListId={requestModelListId}
                                    upstreamModels={modelsForChannel(target.channel_id)}
                                    modelsForChannel={modelsForChannel}
                                    updateTarget={updateTarget}
                                    removeTarget={removeTarget}
                                    compact
                                    idPrefix="access-plan-upstream-dialog"
                                    lockRequestModel
                                />
                            );
                        })}
                    </div>
                    <DialogFooter className="flex-col gap-2 border-t border-border px-4 py-3 sm:flex-row sm:px-5">
                        <DialogClose asChild>
                            <Button type="button" variant="outline" className="rounded-xl">
                                <span className="min-w-0 truncate">{t('routes.close')}</span>
                            </Button>
                        </DialogClose>
                        <Button
                            type="button"
                            className="rounded-xl"
                            disabled={invalid || updateRoutes.isPending}
                            onClick={() => save(() => setEditingRequestModel(null))}
                        >
                            {updateRoutes.isPending ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}
                            <span className="min-w-0 truncate">{t('actions.save')}</span>
                        </Button>
                    </DialogFooter>
                </DialogContent>
            </Dialog>

            <datalist id={requestModelListId}>
                {modelNames.map((model) => <option key={model} value={model} />)}
            </datalist>
        </div>
    );
}

function PlanSettingsDialog({
    plan,
    modelNames,
}: {
    plan: AccessPlan;
    modelNames: string[];
}) {
    const t = accessPlanText;

    return (
        <Dialog>
            <DialogTrigger asChild>
                <Button type="button" variant="outline" className="rounded-xl">
                    <PencilLine className="size-4" />
                    <span className="min-w-0 truncate">方案设置</span>
                </Button>
            </DialogTrigger>
            <DialogContent className="w-[calc(100vw-1rem)] max-w-full overflow-hidden rounded-3xl border-border bg-card p-0 sm:max-w-5xl">
                <DialogHeader className="border-b border-border px-4 py-4 pr-12 sm:px-5">
                    <DialogTitle className="flex min-w-0 flex-wrap items-center gap-2">
                        {t('routes.manageTitle')}
                        <span className="min-w-0 truncate text-sm font-normal text-muted-foreground">
                            {plan.display_name || plan.slug}
                        </span>
                    </DialogTitle>
                    <DialogDescription>{t('routes.manageHint')}</DialogDescription>
                </DialogHeader>
                <div className="grid max-h-[calc(100dvh-10rem)] gap-3 overflow-y-auto px-4 py-4 sm:px-5 lg:grid-cols-2">
                    <PlanBasicsEditor key={`basic-${plan.id}-${plan.updated_at ?? 0}`} plan={plan} />
                    <BillingRulesEditor
                        key={`billing-${plan.id}-${plan.updated_at ?? 0}`}
                        plan={plan}
                        modelNames={modelNames}
                    />
                </div>
            </DialogContent>
        </Dialog>
    );
}

export function AccessPlan() {
    const t = accessPlanText;
    const isAdmin = useAuthStore((state) => state.user?.role === 'admin');
    const { data: plans = [], isLoading, error } = useAccessPlanList();
    const { data: channelData = [] } = useChannelList();
    const { data: modelData = [] } = useModelList();
    const {
        data: channelModels = [],
        isFetched: channelModelsFetched,
        error: channelModelsError,
    } = useModelChannelList();
    const channelModelsReady = channelModelsFetched && !channelModelsError;
    const orderedPlans = useMemo(() => sortedPlans(plans), [plans]);
    const [selectedId, setSelectedId] = useState<number | null>(null);

    const selectedPlan = orderedPlans.find((plan) => plan.id === selectedId) ?? orderedPlans[0] ?? null;
    const activeSelectedId = selectedPlan?.id ?? null;
    const channels = useMemo(() => (
        channelData.map((channel) => ({ id: channel.raw.id, name: channel.raw.name, enabled: channel.raw.enabled }))
    ), [channelData]);
    // 从各渠道非空 model_mapping 合成"客户端名→上游名"的只读映射行（供画布底部映射带展示）
    const channelMappings = useMemo<ChannelModelMapping[]>(() => {
        const out: ChannelModelMapping[] = [];
        for (const channel of channelData) {
            const mapping = channel.raw.model_mapping;
            if (!mapping || typeof mapping !== 'object' || Array.isArray(mapping)) continue;
            for (const [fromModel, toModel] of Object.entries(mapping)) {
                const from = typeof fromModel === 'string' ? fromModel.trim() : '';
                const to = typeof toModel === 'string' ? toModel.trim() : '';
                if (!from || !to) continue;
                out.push({ channelId: channel.raw.id, channelName: channel.raw.name, fromModel: from, toModel: to });
            }
        }
        return out.sort((a, b) => {
            const byFrom = a.fromModel.localeCompare(b.fromModel);
            return byFrom !== 0 ? byFrom : a.channelId - b.channelId;
        });
    }, [channelData]);
    const modelNames = useMemo(() => (
        Array.from(new Set(modelData.map((model) => cleanOneMillionModelName(model.name)).filter(Boolean)))
            .sort((a, b) => a.localeCompare(b))
    ), [modelData]);

    if (!isAdmin) return null;

    return (
        <PageWrapper className="h-full min-h-0 rounded-t-3xl [&>*]:h-full [&>*]:min-h-0">
            <div className="flex h-full min-h-0 flex-col gap-3 overflow-hidden">
                <div className="shrink-0 rounded-2xl border border-border/70 bg-card/70 p-2 shadow-sm">
                    <div className="flex flex-col gap-2 lg:flex-row lg:items-center lg:justify-between">
                        <div className="flex min-w-0 flex-1 flex-col gap-2 sm:flex-row sm:items-center">
                            <span className="shrink-0 px-1 text-xs font-medium text-muted-foreground">方案</span>
                            <SearchableSelect
                                value={activeSelectedId != null ? String(activeSelectedId) : ''}
                                onValueChange={(value) => setSelectedId(Number(value))}
                                options={orderedPlans.map((plan) => ({
                                    value: String(plan.id),
                                    label: planTitle(plan),
                                    keywords: `${plan.slug ?? ''} ${plan.display_name ?? ''}`,
                                }))}
                                placeholder={orderedPlans.length === 0 ? t('list.empty') : t('list.selectOrCreate')}
                                searchPlaceholder="搜索方案名或代号…"
                                emptyText="没有匹配的方案"
                                disabled={orderedPlans.length === 0}
                                className="h-9 min-w-0 max-w-full flex-1 sm:max-w-[22rem]"
                            />
                            {selectedPlan ? (
                                <div className="flex min-w-0 flex-wrap gap-1.5">
                                    {selectedPlan.is_default && <Badge className="rounded-full">{t('status.default')}</Badge>}
                                    <Badge variant={selectedPlan.enabled ? 'secondary' : 'outline'} className="rounded-full">
                                        {selectedPlan.enabled ? t('status.enabled') : t('status.disabled')}
                                    </Badge>
                                    <Badge variant="outline" className="rounded-full">
                                        {t('routes.targetCount', { count: selectedPlan.route_targets.length })}
                                    </Badge>
                                </div>
                            ) : null}
                        </div>

                        <div className="flex min-w-0 flex-wrap items-center gap-2 lg:justify-end">
                            <CreatePlanPanel plans={orderedPlans} onCreated={setSelectedId} />
                            {selectedPlan ? (
                                <PlanSettingsDialog
                                    key={`settings-${selectedPlan.id}-${selectedPlan.updated_at ?? 0}`}
                                    plan={selectedPlan}
                                    modelNames={modelNames}
                                />
                            ) : null}
                        </div>
                    </div>

                    {error ? (
                        <div className="mt-2 rounded-xl border border-destructive/30 bg-destructive/10 px-3 py-2 text-xs text-destructive">
                            {t('list.loadFailed')}: {apiErrorMessage(error)}
                        </div>
                    ) : null}
                    {isLoading ? (
                        <div className="mt-2 flex items-center gap-2 px-1 text-xs text-muted-foreground">
                            <Loader2 className="size-4 animate-spin" />
                            {t('list.title')}
                        </div>
                    ) : null}
                </div>

                {selectedPlan ? (
                    <RouteTargetsEditor
                        key={`routes-${selectedPlan.id}-${selectedPlan.updated_at ?? 0}`}
                        plan={selectedPlan}
                        channels={channels}
                        channelMappings={channelMappings}
                        modelNames={modelNames}
                        channelModels={channelModels}
                        channelModelsReady={channelModelsReady}
                    />
                ) : (
                    <div className="flex min-h-0 flex-1 items-center justify-center rounded-3xl border border-dashed border-border bg-card p-8 text-center text-sm text-muted-foreground">
                        {t('list.selectOrCreate')}
                    </div>
                )}
            </div>
        </PageWrapper>
    );
}
