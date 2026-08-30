'use client';

import { memo, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
    ArrowUp,
    ArrowUpToLine,
    BadgeDollarSign,
    Check,
    GitBranch,
    Loader2,
    Maximize2,
    Minimize2,
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
import { SettingKey, useSetSetting, useSettingList } from '@/api/endpoints/setting';
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
import { cleanOneMillionModelName, marketModelName } from '@/lib/model-aliases';
import { getModelIcon } from '@/lib/model-icons';
import {
    ReactFlow,
    ReactFlowProvider,
    Background,
    BackgroundVariant,
    Controls,
    ControlButton,
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
        modeSpread: '轮询',
        modeFillFirst: '填充优先',
        upstreamModel: '发送模型',
        priority: '优先级',
        weight: '同级轮询权重',
        noChannels: '暂无渠道',
        missingChannel: '渠道 #{id} 已不存在',
        empty: '暂无映射目标',
        canvasTitle: '模型顺序',
        canvasHint: '谁在上面谁先用；模型池只决定轮询还是优先填充。',
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
        hiddenStale: '发现 {count} 个已失效映射，画布已标出；点“重建”才会清理。',
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
        advancedHint: '失败兜底、系统提示词',
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

function clampRoutePriority(value: number) {
    if (!Number.isFinite(value)) return 0;
    return Math.min(9, Math.max(0, Math.trunc(value)));
}

/**
 * 分流模式（后端契约）：规则对象上的 mode 是 JSON number，
 * 1 = spread 轮询（均摊），3 = fill_first 优先填充（默认）。其他/缺省值一律视为 fill_first。
 */
function isSpreadMode(mode: number | undefined | null): boolean {
    return Number(mode) === 1;
}

/** 把 mode 归一到合法值：1 或 3（默认 3）。 */
function normalizeRouteMode(mode: number | undefined | null): 1 | 3 {
    return isSpreadMode(mode) ? 1 : 3;
}

/** 全局默认分流模式（setting route_mode_override）的可选值。 */
type RouteModeOverrideChoice = '' | 'spread' | 'fill_first';
const ROUTE_MODE_OVERRIDE_CHOICES: Array<{ value: RouteModeOverrideChoice; label: string }> = [
    { value: '', label: '跟随各规则' },
    { value: 'spread', label: '全局轮询' },
    { value: 'fill_first', label: '全局优先填充' },
];

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

type UnroutedChannelModel = RouteChannelModel & { clean_name: string };

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

function moveRouteTargetToTop(targets: AccessPlanRouteTarget[], targetIndex: number) {
    const selectedTarget = targets[targetIndex];
    if (!selectedTarget) return targets;

    const requestKey = cleanOneMillionModelName(selectedTarget.request_model).toLowerCase();
    const peers = targets
        .map((target, index) => ({ target, index }))
        .filter((item) => cleanOneMillionModelName(item.target.request_model).toLowerCase() === requestKey)
        .sort((a, b) => (
            (a.target.priority || 0) - (b.target.priority || 0)
            || (a.target.channel_id || 0) - (b.target.channel_id || 0)
            || a.index - b.index
        ));
    const ordered = [
        { target: selectedTarget, index: targetIndex },
        ...peers.filter((item) => item.index !== targetIndex),
    ];

    return targets.map((target, index) => {
        const position = ordered.findIndex((item) => item.index === index);
        return position >= 0 ? { ...target, priority: clampRoutePriority(position) } : target;
    });
}

// 上移一位：同一请求模型组内，把点中的候选与它前一个互换，其余 priority 顺延。
// 这是新手最直观的「谁在上面谁先用」手势：点一下往上一格，连点几下就到顶。
function moveRouteTargetUpOne(targets: AccessPlanRouteTarget[], targetIndex: number) {
    const selectedTarget = targets[targetIndex];
    if (!selectedTarget) return targets;

    const requestKey = cleanOneMillionModelName(selectedTarget.request_model).toLowerCase();
    const peers = targets
        .map((target, index) => ({ target, index }))
        .filter((item) => cleanOneMillionModelName(item.target.request_model).toLowerCase() === requestKey)
        .sort((a, b) => (
            (a.target.priority || 0) - (b.target.priority || 0)
            || (a.target.channel_id || 0) - (b.target.channel_id || 0)
            || a.index - b.index
        ));
    const position = peers.findIndex((item) => item.index === targetIndex);
    if (position <= 0) return targets;

    const ordered = [...peers];
    [ordered[position - 1], ordered[position]] = [ordered[position], ordered[position - 1]];

    return targets.map((target, index) => {
        const nextPosition = ordered.findIndex((item) => item.index === index);
        return nextPosition >= 0 ? { ...target, priority: clampRoutePriority(nextPosition) } : target;
    });
}

function unroutedChannelModels(channelModels: RouteChannelModel[], targets: AccessPlanRouteTarget[]) {
    const routedKeys = new Set<string>();
    targets.forEach((target) => {
        routedKeys.add(channelModelKey(target.channel_id, cleanOneMillionModelName(target.upstream_model)));
    });

    const seen = new Set<string>();
    return channelModels
        .filter((model) => model.enabled !== false)
        .map((model): UnroutedChannelModel | null => {
            const cleanName = cleanOneMillionModelName(model.name);
            if (!cleanName) return null;
            const key = channelModelKey(model.channel_id, cleanName);
            if (seen.has(key) || routedKeys.has(key)) return null;
            seen.add(key);
            return { ...model, clean_name: cleanName };
        })
        .filter((model): model is UnroutedChannelModel => model !== null)
        .sort((a, b) => {
            const byName = a.clean_name.localeCompare(b.clean_name);
            return byName !== 0 ? byName : a.channel_id - b.channel_id;
        });
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
                weight: 1,
                enabled: previous?.enabled ?? true,
                billing_model_source: previous?.billing_model_source ?? 'request_model',
                billing_model_override: previous?.billing_model_override,
                mode: previous?.mode,
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
                        <div className="grid min-w-0 gap-1">
                            <Input
                                value={rule.model_name}
                                list={modelListId}
                                onChange={(event) => updateRule(index, { model_name: event.target.value })}
                                placeholder={t('billing.modelName')}
                                className="rounded-xl"
                            />
                            {rule.model_name.trim() && marketModelName(rule.model_name) !== rule.model_name.trim() && (
                                <div className="truncate text-[11px] text-muted-foreground" title={rule.model_name.trim()}>
                                    {marketModelName(rule.model_name)}
                                </div>
                            )}
                        </div>
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
    mode?: number;
    /** Stable edit target; shared callback avoids per-node closures that bust memo on pan/zoom. */
    editTarget: string;
    onEditRequest: (requestModel: string) => void;
};
type TargetNodeData = {
    channelName: string;
    channelId: number;
    targetId?: number;
    laneId: string;
    priority: number;
    showPriority: boolean;
    fillFirst: boolean;
    upstreamModel: string;
    enabled: boolean;
    fallback: string;
    multiplier: number | string;
    billingSource: string;
    onPriorityChange?: (targetId: number, priority: number) => void;
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

const PlanFlowCard = memo(function PlanFlowCard({ data }: NodeProps<PlanFlowNode>) {
    return (
        <div className="rounded-2xl border border-primary/25 bg-primary/10 p-3" style={{ width: PLAN_W, height: PLAN_H }}>
            <div className="text-[10px] font-bold uppercase tracking-[0.2em] text-primary">{data.slug}</div>
            <div className="mt-1 truncate text-sm font-black text-foreground">{data.name}</div>
            <div className="mt-2 text-xs text-muted-foreground">默认倍率 {data.multiplier}x</div>
            <Handle type="source" position={Position.Right} className="!size-2 !border-2 !border-background !bg-primary" />
        </div>
    );
});

const RequestFlowCard = memo(function RequestFlowCard({ data }: NodeProps<RequestFlowNode>) {
    const modeLabel = data.mode !== undefined
        ? (isSpreadMode(data.mode)
            ? accessPlanText('routes.modeSpread')
            : accessPlanText('routes.modeFillFirst'))
        : null;
    return (
        <div className="rounded-2xl border border-amber-500/25 bg-amber-500/10 px-3 py-2.5" style={{ width: REQ_W, height: REQ_H }}>
            <Handle type="target" position={Position.Left} className="!size-2 !border-2 !border-background !bg-amber-500" />
            <div className="flex items-center justify-between gap-2">
                <span className="inline-flex items-center gap-1.5 text-[10px] font-bold uppercase tracking-[0.16em] text-amber-600 dark:text-amber-300">
                    <span className="size-1.5 rounded-full bg-amber-500" />
                    原请求模型
                    {modeLabel !== null && (
                        <span className="ml-0.5 rounded-full bg-muted/60 px-1.5 py-px font-normal normal-case tracking-normal text-muted-foreground">
                            {modeLabel}
                        </span>
                    )}
                </span>
                <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    className="nodrag nopan pointer-events-auto h-6 gap-1 rounded-lg border-amber-500/30 bg-transparent px-2 text-[11px] text-amber-700 hover:bg-amber-500/15 dark:text-amber-200"
                    onClick={() => data.onEditRequest(data.editTarget)}
                    onPointerDown={(event) => event.stopPropagation()}
                >
                    <PencilLine className="size-3" />{accessPlanText('routes.quickEdit')}
                </Button>
            </div>
            <div className="mt-1.5 break-all font-mono text-[13px] font-black leading-snug text-foreground line-clamp-2">{data.requestModel}</div>
            <Handle type="source" position={Position.Right} className="!size-2 !border-2 !border-background !bg-amber-500" />
        </div>
    );
});

const TargetFlowCard = memo(function TargetFlowCard({ data }: NodeProps<TargetFlowNode>) {
    const priority = clampRoutePriority(data.priority);
    const bumpPriority = (delta: number) => {
        if (!data.targetId || !data.onPriorityChange) return;
        const next = clampRoutePriority(priority + delta);
        if (next === priority) return;
        data.onPriorityChange(data.targetId, next);
    };
    const priorityTitle = data.fillFirst
        ? accessPlanText('routes.priority')
        : '并列显示不参与路由';
    return (
        <div
            className={cn('grid grid-cols-[minmax(84px,1.1fr)_minmax(96px,1.4fr)_auto] items-center gap-3 rounded-2xl border bg-card/90 px-3 py-2', data.enabled ? 'border-emerald-500/25' : 'border-border opacity-65')}
            style={{ width: TGT_W, height: TGT_H }}
        >
            <Handle type="target" position={Position.Left} className="!size-2 !border-2 !border-background !bg-emerald-500" />
            <div className="min-w-0">
                <div className="flex min-w-0 items-center gap-1.5">
                    <span className={cn('size-2 shrink-0 rounded-full', data.enabled ? 'bg-emerald-500' : 'bg-muted-foreground')} />
                    <span className="truncate text-sm font-bold text-foreground">{data.channelName}</span>
                </div>
                <div className="mt-0.5 flex min-w-0 items-center gap-1 text-[10px] text-muted-foreground" title={priorityTitle}>
                    <span className="shrink-0">#{data.channelId}</span>
                    <span className="shrink-0">·</span>
                    {data.fillFirst ? (
                        <span className="shrink-0">{data.showPriority ? `P${priority}` : accessPlanText('routes.priority')}</span>
                    ) : (
                        <span className="shrink-0">并列</span>
                    )}
                    <div
                        className="nodrag nopan pointer-events-auto ml-0.5 inline-flex shrink-0 items-center gap-0.5"
                        onPointerDown={(event) => event.stopPropagation()}
                    >
                        <button
                            type="button"
                            className="flex size-4 items-center justify-center rounded border border-border/70 bg-background leading-none text-foreground hover:bg-muted disabled:opacity-40"
                            disabled={!data.targetId || priority >= 9}
                            aria-label={`${accessPlanText('routes.priority')} demote`}
                            onClick={(event) => {
                                event.stopPropagation();
                                bumpPriority(1);
                            }}
                        >
                            −
                        </button>
                        <span className="min-w-3.5 text-center font-semibold text-foreground">{priority}</span>
                        <button
                            type="button"
                            className="flex size-4 items-center justify-center rounded border border-border/70 bg-background leading-none text-foreground hover:bg-muted disabled:opacity-40"
                            disabled={!data.targetId || priority <= 0}
                            aria-label={`${accessPlanText('routes.priority')} promote`}
                            onClick={(event) => {
                                event.stopPropagation();
                                bumpPriority(-1);
                            }}
                        >
                            +
                        </button>
                    </div>
                </div>
            </div>
            <div className="min-w-0">
                <div className="text-[9px] font-bold uppercase tracking-[0.14em] text-muted-foreground">{accessPlanText('routes.upstreamTarget')}</div>
                <div className="mt-0.5 truncate font-mono text-xs font-semibold text-foreground">
                    {data.upstreamModel}
                </div>
            </div>
            <div className="text-xs text-muted-foreground">
                <div className="font-bold text-foreground">{data.multiplier}x</div>
                <div className="truncate text-[10px]">{data.billingSource}</div>
                <div className="truncate text-[10px]">{data.fallback}</div>
            </div>
        </div>
    );
});

const FamilyBandCard = memo(function FamilyBandCard({ data }: NodeProps<FamilyBandFlowNode>) {
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
});

// 渠道映射：左卡（客户端请求名，复用琥珀请求卡风格，只读无编辑按钮）
const MapFromCard = memo(function MapFromCard({ data }: NodeProps<MapFromFlowNode>) {
    return (
        <div className="rounded-2xl border border-amber-500/25 bg-amber-500/10 px-3 py-2.5" style={{ width: REQ_W, height: REQ_H }}>
            <div className="inline-flex items-center gap-1.5 text-[10px] font-bold uppercase tracking-[0.16em] text-amber-600 dark:text-amber-300">
                <span className="size-1.5 rounded-full bg-amber-500" />
                客户端请求名
            </div>
            <div className="mt-1.5 break-all font-mono text-[13px] font-black leading-snug text-foreground line-clamp-2">{data.fromModel}</div>
            <Handle type="source" position={Position.Right} className="!size-2 !border-2 !border-background !bg-amber-500" />
        </div>
    );
});

// 渠道映射：右卡（#渠道 · 上游名，复用目标卡风格）
const MapToCard = memo(function MapToCard({ data }: NodeProps<MapToFlowNode>) {
    return (
        <div className="grid grid-cols-[minmax(84px,1.1fr)_minmax(96px,1.6fr)] items-center gap-3 rounded-2xl border border-emerald-500/25 bg-card/90 px-3 py-2" style={{ width: TGT_W, height: TGT_H }}>
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
});

// 渠道映射家族带（独立于方案路由带，青色区分）
const ChannelMapBandCard = memo(function ChannelMapBandCard({ data }: NodeProps<ChannelMapBandFlowNode>) {
    return (
        <div className="rounded-2xl border border-cyan-500/30 bg-cyan-500/[0.07]" style={{ width: data.width, height: data.height }}>
            <div className="flex items-center gap-2.5 px-4 py-2.5">
                <span className="size-2.5 shrink-0 rounded-full bg-cyan-500" />
                <span className="text-sm font-black text-cyan-600 dark:text-cyan-300">{data.label}</span>
                <Badge variant="secondary" className="rounded-full">{data.count} 映射</Badge>
            </div>
        </div>
    );
});

// Stable identity is required: React Flow re-renders visible nodes on pan/zoom.
// If nodeTypes is recreated each render, every custom node remounts and the canvas stutters.
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
    onPriorityChange?: (targetId: number, priority: number) => void,
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
                    mode: row.targets[0]?.mode,
                    // Stable scalar + shared callback: avoid per-row arrow functions so memoized nodes skip re-render on pan/zoom.
                    editTarget: row.requestKey ? row.requestModel : '',
                    onEditRequest,
                },
                width: REQ_W,
                height: REQ_H,
            });
            edges.push({
                id: `edge:${planId}:${reqId}`,
                source: planId,
                target: reqId,
                type: 'default',
                style: { stroke: 'var(--primary)', strokeWidth: 1.5, strokeOpacity: 0.5 },
            });
            // 优先级只在「优先填充(Failover)」模式下才是真正的选路顺序；轮询(Spread)模式里它
            // 只是拖拽序号、不参与路由（见 balancer.go Spread 语义：只有 ChannelPriority 是硬边界），
            // 故一律并列显示，别把死数字画成假分档（存量老渠道序号各异 → 画布误显 P1-P4 的根因）。
            const isFillFirstMode = !isSpreadMode(row.targets[0]?.mode);
            const prioritySet = new Set(row.targets.map((tgt) => tgt.priority || 0));
            const showPriority = isFillFirstMode && prioritySet.size > 1;
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
                    width: TGT_W,
                    height: TGT_H,
                    data: {
                        channelName,
                        channelId: target.channel_id,
                        targetId: target.id,
                        laneId: reqId,
                        priority: clampRoutePriority(target.priority || 0),
                        showPriority,
                        fillFirst: isFillFirstMode,
                        upstreamModel: cleanOneMillionModelName(target.upstream_model || accessPlanText('routes.unset')),
                        enabled: target.enabled,
                        fallback: fallbackModeLabel(target.fallback_mode),
                        multiplier: billing.multiplier,
                        billingSource: billingSourceLabel(target.billing_model_source),
                        onPriorityChange,
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
            width: BAND_W,
            height: bandHeight,
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
        width: PLAN_W,
        height: PLAN_H,
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
                width: REQ_W,
                height: REQ_H,
                data: { fromModel: mapping.fromModel },
            });
            nodes.push({
                id: toId,
                type: 'mapTo',
                position: { x: TGT_X, y: rowY + (rowH - TGT_H) / 2 },
                zIndex: 1,
                width: TGT_W,
                height: TGT_H,
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
            width: BAND_W,
            height: bandHeight,
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
    modelNames: string[];
    channelModels: RouteChannelModel[];
    channelModelsReady: boolean;
    unroutedModels: UnroutedChannelModel[];
    onEditRequest: (requestModel: string) => void;
    onPriorityChange?: (targetId: number, priority: number) => void;
    onMoveToTop?: (targetId: number) => void;
    onMoveUpOne?: (targetId: number) => void;
    onAddUnroutedModel?: (model: UnroutedChannelModel) => void;
    onOpenJson?: () => void;
    onModeChange?: (rowTargets: AccessPlanRouteTarget[], mode: 1 | 3) => void;
};

type RouteViewMode = 'all' | 'channel' | 'mapping';

function RouteFlowCanvasInner({
    plan, rows, channels, channelMappings, modelNames, channelModels, channelModelsReady, unroutedModels, onEditRequest, onPriorityChange, onMoveToTop, onMoveUpOne, onAddUnroutedModel, onOpenJson, onModeChange,
}: RouteFlowCanvasProps) {
    const t = accessPlanText;
    const channelNameByID = useMemo(() => new Map(channels.map((channel) => [channel.id, channel.name])), [channels]);
    const channelModelIndex = useMemo(() => buildChannelModelIndex(channelModels, channelModelsReady), [channelModels, channelModelsReady]);

    const [viewMode, setViewMode] = useState<RouteViewMode>('all');
    const [selectedChannelId, setSelectedChannelId] = useState<string>('');
    const [selectedMappingModel, setSelectedMappingModel] = useState<string>('');
    const [query, setQuery] = useState('');
    const q = query.trim().toLowerCase();

    const mappingModelOptions = useMemo(() => {
        const set = new Set<string>();
        rows.forEach((r) => set.add(r.requestModel));
        channelMappings.forEach((m) => set.add(m.fromModel));
        return Array.from(set).sort((a, b) => a.localeCompare(b));
    }, [rows, channelMappings]);

    const filteredRows = useMemo(() => {
        const channelFilterId = viewMode === 'channel' && selectedChannelId ? Number(selectedChannelId) : null;
        const modelFilter = viewMode === 'mapping' && selectedMappingModel ? selectedMappingModel.toLowerCase() : null;

        return rows
            .map((row): RouteTargetGroup | null => {
                if (modelFilter && row.requestModel.toLowerCase() !== modelFilter) {
                    return null;
                }
                let targets = row.targets;
                if (channelFilterId !== null) {
                    targets = targets.filter((tgt) => tgt.channel_id === channelFilterId);
                }
                if (!targets.length) return null;

                if (!q) {
                    return { ...row, targets };
                }

                if (row.requestModel.toLowerCase().includes(q)) {
                    return { ...row, targets };
                }
                const matchedTargets = targets.filter((tgt) => {
                    const chName = (channelNameByID.get(tgt.channel_id) ?? '').toLowerCase();
                    return chName.includes(q) || (tgt.upstream_model ?? '').toLowerCase().includes(q);
                });
                return matchedTargets.length ? { ...row, targets: matchedTargets } : null;
            })
            .filter((row): row is RouteTargetGroup => row !== null);
    }, [rows, q, channelNameByID, viewMode, selectedChannelId, selectedMappingModel]);

    const filteredMappings = useMemo(() => {
        const channelFilterId = viewMode === 'channel' && selectedChannelId ? Number(selectedChannelId) : null;
        const modelFilter = viewMode === 'mapping' && selectedMappingModel ? selectedMappingModel.toLowerCase() : null;

        return channelMappings.filter((mapping) => {
            if (channelFilterId !== null && mapping.channelId !== channelFilterId) {
                return false;
            }
            if (modelFilter && mapping.fromModel.toLowerCase() !== modelFilter) {
                return false;
            }
            if (!q) return true;
            return (
                mapping.fromModel.toLowerCase().includes(q)
                || mapping.toModel.toLowerCase().includes(q)
                || mapping.channelName.toLowerCase().includes(q)
            );
        });
    }, [channelMappings, q, viewMode, selectedChannelId, selectedMappingModel]);

    // 画布只渲染「已路由」的模型；搜索时把渠道模型池里命中但未路由的模型也捞出来，
    // 免得搜 deepseek 时误以为系统里没有——其实是没路由进本方案。
    const routedModelNames = useMemo(() => {
        const names = new Set<string>();
        rows.forEach((row) => {
            names.add(row.requestModel.toLowerCase());
            row.targets.forEach((target) => {
                if (target.upstream_model) names.add(target.upstream_model.toLowerCase());
            });
        });
        channelMappings.forEach((mapping) => {
            names.add(mapping.fromModel.toLowerCase());
            names.add(mapping.toModel.toLowerCase());
        });
        return names;
    }, [rows, channelMappings]);

    const matchedPoolModels = useMemo(() => {
        if (!q) return [];
        const seen = new Set<string>();
        const hits: string[] = [];
        const candidates = [...modelNames, ...channelModels.map((model) => model.name)];
        for (const raw of candidates) {
            const name = cleanOneMillionModelName(raw);
            const key = name.toLowerCase();
            if (!key || seen.has(key)) continue;
            seen.add(key);
            if (key.includes(q) && !routedModelNames.has(key)) hits.push(name);
        }
        return hits.sort((a, b) => a.localeCompare(b));
    }, [q, modelNames, channelModels, routedModelNames]);

    const filteredUnroutedModels = useMemo(() => {
        const channelFilterId = viewMode === 'channel' && selectedChannelId ? Number(selectedChannelId) : null;
        const modelFilter = viewMode === 'mapping' && selectedMappingModel ? selectedMappingModel.toLowerCase() : null;

        const matches = unroutedModels.filter((model) => {
            if (channelFilterId !== null && model.channel_id !== channelFilterId) {
                return false;
            }
            if (modelFilter && model.clean_name.toLowerCase() !== modelFilter) {
                return false;
            }
            if (!q) return true;
            return (
                model.clean_name.toLowerCase().includes(q)
                || (channelNameByID.get(model.channel_id) ?? '').toLowerCase().includes(q)
            );
        });
        // ponytail: cap beginner canvas overflow at 40; add pagination if admins manage hundreds of unrouted models.
        return matches.slice(0, 40);
    }, [unroutedModels, q, channelNameByID, viewMode, selectedChannelId, selectedMappingModel]);

    const targetCount = rows.reduce((sum, row) => sum + row.targets.length, 0);
    const hasCanvasContent = filteredRows.length > 0 || filteredUnroutedModels.length > 0 || filteredMappings.length > 0;

    return (
        <section className="flex min-h-0 flex-1 flex-col overflow-hidden rounded-2xl border border-border/70 bg-background/70">
            <div className="flex flex-wrap items-center justify-between gap-3 border-b border-border/70 px-4 py-3">
                <div className="min-w-0">
                    <div className="flex items-center gap-2 text-sm font-black">
                        <GitBranch className="size-4 text-primary" />
                        <span>{t('routes.canvasTitle')}</span>
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
                    <p className="mt-1 hidden text-xs text-muted-foreground sm:block">{t('routes.canvasHint')}</p>
                </div>
                <div className="flex w-full flex-wrap items-center gap-2 text-[11px] text-muted-foreground sm:w-auto">
                    <div className="inline-flex items-center rounded-full border border-border/70 bg-background/60 p-0.5 text-xs">
                        <button
                            type="button"
                            className={cn(
                                'h-6 rounded-full px-2.5 text-xs font-medium transition-colors',
                                viewMode === 'all' ? 'bg-primary text-primary-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground'
                            )}
                            onClick={() => setViewMode('all')}
                        >
                            全部
                        </button>
                        <button
                            type="button"
                            className={cn(
                                'h-6 rounded-full px-2.5 text-xs font-medium transition-colors',
                                viewMode === 'channel' ? 'bg-primary text-primary-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground'
                            )}
                            onClick={() => setViewMode('channel')}
                        >
                            按渠道
                        </button>
                        <button
                            type="button"
                            className={cn(
                                'h-6 rounded-full px-2.5 text-xs font-medium transition-colors',
                                viewMode === 'mapping' ? 'bg-primary text-primary-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground'
                            )}
                            onClick={() => setViewMode('mapping')}
                        >
                            按映射
                        </button>
                    </div>

                    {viewMode === 'channel' && (
                        <select
                            value={selectedChannelId}
                            onChange={(e) => setSelectedChannelId(e.target.value)}
                            aria-label="筛选渠道"
                            className="h-7 rounded-full border border-border/70 bg-background/60 px-2.5 text-xs text-foreground outline-none focus:border-primary/50 max-w-[160px]"
                        >
                            <option value="">全部渠道</option>
                            {channels.map((ch) => (
                                <option key={ch.id} value={String(ch.id)}>
                                    #{ch.id} {ch.name}
                                </option>
                            ))}
                        </select>
                    )}

                    {viewMode === 'mapping' && (
                        <select
                            value={selectedMappingModel}
                            onChange={(e) => setSelectedMappingModel(e.target.value)}
                            aria-label="筛选映射模型"
                            className="h-7 rounded-full border border-border/70 bg-background/60 px-2.5 text-xs text-foreground outline-none focus:border-primary/50 max-w-[160px]"
                        >
                            <option value="">全部请求模型</option>
                            {mappingModelOptions.map((model) => (
                                <option key={model} value={model}>
                                    {model}
                                </option>
                            ))}
                        </select>
                    )}

                    <div className="relative min-w-0 flex-1 sm:flex-none">
                        <Search className="pointer-events-none absolute left-2 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
                        <input
                            value={query}
                            onChange={(event) => setQuery(event.target.value)}
                            placeholder="搜模型 / 渠道 / 上游…"
                            className="h-7 w-full rounded-full border border-border/70 bg-background/60 pl-7 pr-2 text-xs text-foreground outline-none focus:border-primary/50 sm:w-48"
                        />
                    </div>
                    <Badge variant="secondary" className="rounded-full">{planTitle(plan)}</Badge>
                    <Badge variant="outline" className="rounded-full">{t('routes.targetCount', { count: targetCount })}</Badge>
                </div>
            </div>

            {!hasCanvasContent ? (
                <div className="px-4 py-8 text-center text-sm text-muted-foreground">
                    {matchedPoolModels.length > 0 ? (
                        <div className="mx-auto max-w-md space-y-3 text-left">
                            <p className="text-center">模型池里有「{query.trim()}」，但还没路由到本方案：</p>
                            <div className="flex flex-wrap justify-center gap-1.5">
                                {matchedPoolModels.map((name) => (
                                    <Badge key={name} variant="outline" className="rounded-full font-mono">{name}</Badge>
                                ))}
                            </div>
                            <p className="text-center text-xs">点上方「重建分组」把它们路由进来，或手动添加路线。</p>
                        </div>
                    ) : rows.length === 0 ? (
                        t('routes.canvasEmpty')
                    ) : (
                        `没有匹配当前筛选条件的模型或渠道`
                    )}
                </div>
            ) : (
                <div className="min-h-0 flex-1 overflow-y-auto p-3 sm:p-4">
                    <div className="space-y-3">
                        {filteredRows.map((row) => {
                            // 规则自身存的 mode：1=轮询/spread，3=优先填充/fill_first；缺省或其它值一律视为优先填充。
                            const isRowFillFirst = !isSpreadMode(row.targets[0]?.mode);
                            const requestModelDisplayName = marketModelName(row.requestModel) || row.requestModel;
                            const { Avatar: RequestAvatar } = getModelIcon(row.requestModel);
                            const showRawRequestModel = requestModelDisplayName !== row.requestModel;

                            return (
                                <div key={row.requestKey || row.requestModel} className="rounded-2xl border border-border/70 bg-card/40">
                                    <div className="flex items-center justify-between gap-3 border-b border-border/60 px-4 py-2.5">
                                        <div className="flex min-w-0 items-center gap-2">
                                            <RequestAvatar size={16} />
                                            <div className="min-w-0">
                                                <div className="truncate text-sm font-bold text-foreground" title={row.requestModel}>{requestModelDisplayName}</div>
                                                {showRawRequestModel && (
                                                    <div className="truncate font-mono text-[11px] text-muted-foreground">{row.requestModel}</div>
                                                )}
                                            </div>
                                            <Badge variant="outline" className="rounded-full text-[11px]">
                                                {isRowFillFirst ? '优先填充' : '轮询'}
                                            </Badge>
                                            {onModeChange && (
                                                <select
                                                    value={isRowFillFirst ? 3 : 1}
                                                    onChange={(event) => onModeChange(row.targets, Number(event.target.value) as 1 | 3)}
                                                    aria-label={`分流模式：${row.requestModel}`}
                                                    title="分流模式：轮询=同一请求模型的候选均摊流量；优先填充=先集中打满高优先级候选"
                                                    className="h-6 shrink-0 rounded-full border border-border/70 bg-background/60 px-2 text-[11px] text-foreground outline-none focus:border-primary/50"
                                                >
                                                    <option value={1}>轮询</option>
                                                    <option value={3}>优先填充</option>
                                                </select>
                                            )}
                                        </div>
                                        <Button
                                            type="button"
                                            variant="outline"
                                            size="sm"
                                            className="h-7 gap-1 rounded-lg text-xs"
                                            onClick={() => onEditRequest(row.requestKey ? row.requestModel : '')}
                                        >
                                            <PencilLine className="size-3" />
                                            <span>{accessPlanText('routes.quickEdit')}</span>
                                        </Button>
                                    </div>
                                    <div className="divide-y divide-border/50">
                                        {row.targets.map((target, targetIndex) => {
                                            const priority = clampRoutePriority(target.priority || 0);
                                            const isStale = isPersistedStaleTarget(target, channelModelIndex);
                                            const upstreamModel = cleanOneMillionModelName(target.upstream_model || accessPlanText('routes.unset'));
                                            const upstreamDisplayName = marketModelName(upstreamModel) || upstreamModel;
                                            const { Avatar: UpstreamAvatar } = getModelIcon(upstreamModel);
                                            return (
                                                <div
                                                    key={target.id || targetIndex}
                                                    className={cn('flex items-center gap-3 px-4 py-2.5', (!target.enabled || isStale) && 'opacity-55')}
                                                >
                                                    <div className="flex min-w-0 flex-1 flex-wrap items-center gap-x-3 gap-y-1 text-xs">
                                                        <div className="flex items-center gap-1.5 font-semibold text-foreground">
                                                            <span className={cn('size-1.5 shrink-0 rounded-full', target.enabled ? 'bg-emerald-500' : 'bg-muted-foreground')} />
                                                            <span className="truncate">
                                                                {channelNameByID.get(target.channel_id) ?? accessPlanText('routes.missingChannel', { id: target.channel_id })}
                                                            </span>
                                                            <span className="text-[11px] font-normal text-muted-foreground">#{target.channel_id}</span>
                                                        </div>
                                                        <div className="flex items-center gap-1.5 text-muted-foreground">
                                                            <span className="text-foreground/40">→</span>
                                                            <UpstreamAvatar size={14} />
                                                            <span className="font-medium text-foreground" title={upstreamModel}>
                                                                {upstreamDisplayName}
                                                            </span>
                                                        </div>
                                                        {isStale && (
                                                            <Badge variant="outline" className="h-5 gap-1 border-amber-500/40 px-1.5 text-[10px] font-normal text-amber-600 dark:text-amber-300">
                                                                已失效
                                                            </Badge>
                                                        )}
                                                    </div>
                                                    <div className="flex shrink-0 items-center gap-1.5">
                                                        <div className="inline-flex items-center gap-0.5">
                                                            <button
                                                                type="button"
                                                                className="flex size-5 items-center justify-center rounded border border-border/70 bg-background text-xs font-semibold leading-none text-foreground hover:bg-muted disabled:opacity-40"
                                                                disabled={!target.id || isStale || priority >= 9}
                                                                aria-label="降低优先级"
                                                                title="降低优先级（+1）"
                                                                onClick={() => target.id && onPriorityChange?.(target.id, clampRoutePriority(priority + 1))}
                                                            >
                                                                −
                                                            </button>
                                                            <span className="min-w-4 text-center font-mono text-xs font-semibold tabular-nums text-foreground">
                                                                {priority + 1}
                                                            </span>
                                                            <button
                                                                type="button"
                                                                className="flex size-5 items-center justify-center rounded border border-border/70 bg-background text-xs font-semibold leading-none text-foreground hover:bg-muted disabled:opacity-40"
                                                                disabled={!target.id || isStale || priority <= 0}
                                                                aria-label="提高优先级"
                                                                title="提高优先级（-1）"
                                                                onClick={() => target.id && onPriorityChange?.(target.id, clampRoutePriority(priority - 1))}
                                                            >
                                                                +
                                                            </button>
                                                        </div>
                                                        <button
                                                            type="button"
                                                            className="flex h-7 items-center gap-1 rounded-lg border border-border/70 bg-background px-2 text-xs text-foreground hover:bg-muted disabled:opacity-40"
                                                            disabled={!target.id || isStale || priority <= 0}
                                                            onClick={() => onMoveUpOne?.(target.id!)}
                                                            aria-label="上移一位"
                                                            title="往上挪一位：点一下，这个模型就排到它上面那个的前面"
                                                        >
                                                            <ArrowUp className="size-3.5" />
                                                            <span>上移</span>
                                                        </button>
                                                        <button
                                                            type="button"
                                                            className="flex h-7 items-center gap-1 rounded-lg border border-border/70 bg-background px-2 text-xs text-foreground hover:bg-muted disabled:opacity-40"
                                                            disabled={!target.id || isStale || priority <= 0}
                                                            onClick={() => onMoveToTop?.(target.id!)}
                                                            aria-label="移到最上（最优先使用）"
                                                            title="谁在上面谁先用：点一下，这个模型就排到最上面"
                                                        >
                                                            <ArrowUpToLine className="size-3.5" />
                                                            <span>到顶</span>
                                                        </button>
                                                    </div>
                                                </div>
                                            );
                                        })}
                                    </div>
                                </div>
                            );
                        })}

                        {filteredUnroutedModels.length > 0 && (
                            <div className="rounded-2xl border border-dashed border-primary/30 bg-primary/[0.03]">
                                <div className="flex flex-wrap items-center justify-between gap-2 border-b border-primary/15 px-4 py-2.5">
                                    <div className="min-w-0">
                                        <div className="text-xs font-bold text-foreground">模型池里还有未加入当前方案的模型</div>
                                        <div className="mt-0.5 text-[11px] text-muted-foreground">新的渠道模型会先出现在这里，点「加入」后才参与当前方案路由。</div>
                                    </div>
                                    <Badge variant="outline" className="rounded-full text-[10px]">{unroutedModels.length}</Badge>
                                </div>
                                <div className="divide-y divide-primary/10">
                                    {filteredUnroutedModels.map((model) => (
                                        <div key={channelModelKey(model.channel_id, model.clean_name)} className="flex items-center justify-between gap-3 px-4 py-2.5 text-xs">
                                            <div className="flex min-w-0 flex-1 flex-wrap items-center gap-x-3 gap-y-1">
                                                <span className="truncate font-semibold text-foreground" title={model.clean_name}>{marketModelName(model.clean_name) || model.clean_name}</span>
                                                <span className="truncate text-muted-foreground">{channelNameByID.get(model.channel_id) ?? accessPlanText('routes.missingChannel', { id: model.channel_id })}</span>
                                                <span className="text-[11px] text-muted-foreground">#{model.channel_id}</span>
                                            </div>
                                            <Button
                                                type="button"
                                                variant="outline"
                                                size="sm"
                                                className="h-7 shrink-0 rounded-lg text-xs"
                                                onClick={() => onAddUnroutedModel?.(model)}
                                                disabled={!onAddUnroutedModel}
                                            >
                                                加入
                                            </Button>
                                        </div>
                                    ))}
                                </div>
                            </div>
                        )}

                        {filteredMappings.length > 0 && (
                            <div className="rounded-2xl border border-border/70 bg-card/40">
                                <div className="flex items-center gap-2 border-b border-border/60 px-4 py-2.5">
                                    <span className="text-xs font-bold text-foreground">渠道级模型映射</span>
                                    <Badge variant="outline" className="rounded-full text-[10px]">{filteredMappings.length}</Badge>
                                </div>
                                <div className="divide-y divide-border/50">
                                    {filteredMappings.map((m, idx) => (
                                        <div key={`${m.channelId}-${m.fromModel}-${m.toModel}-${idx}`} className="flex items-center justify-between gap-3 px-4 py-2 text-xs">
                                            <div className="flex min-w-0 flex-1 flex-wrap items-center gap-2 font-mono">
                                                <span className="font-semibold text-foreground">{m.fromModel}</span>
                                                <span className="text-muted-foreground">→</span>
                                                <span className="text-muted-foreground">{m.channelName} (#{m.channelId})</span>
                                                <span className="text-muted-foreground">→</span>
                                                <span className="font-semibold text-foreground">{m.toModel}</span>
                                            </div>
                                        </div>
                                    ))}
                                </div>
                            </div>
                        )}
                    </div>
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
    moveTargetToTop,
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
    moveTargetToTop?: (index: number) => void;
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
                        variant="outline"
                        size="sm"
                        className="h-8 shrink-0 gap-1 rounded-xl text-xs"
                        disabled={!moveTargetToTop || cleanOneMillionModelName(target.request_model).length === 0 || clampRoutePriority(target.priority) <= 0}
                        onClick={() => moveTargetToTop?.(index)}
                        title="谁在上面谁先用：点一下，这个模型就排到最上面"
                    >
                        <ArrowUpToLine className="size-3.5" />
                        加一到顶
                    </Button>
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

            {/* 高级（可选）：失败兜底、系统提示词覆盖 —— 默认折叠 */}
            <details className="mt-2 min-w-0 rounded-xl border border-border/60 bg-background/40">
                <summary className="flex cursor-pointer list-none items-center justify-between gap-2 px-2.5 py-2 text-xs font-medium text-foreground [&::-webkit-details-marker]:hidden">
                    <span className="min-w-0 truncate">{t('routes.advancedTitle')}</span>
                    <span className="min-w-0 truncate text-[11px] font-normal text-muted-foreground">{t('routes.advancedHint')}</span>
                </summary>
                <div className="grid min-w-0 gap-2 border-t border-border/60 p-2.5">
                    <div className="grid min-w-0 gap-2 sm:grid-cols-[minmax(150px,1fr)]">
                        <label className="grid min-w-0 gap-1 text-xs text-muted-foreground">
                            分流模式
                            <select
                                value={normalizeRouteMode(target.mode)}
                                onChange={(event) => updateTarget(index, { mode: Number(event.target.value) as 1 | 3 })}
                                aria-label="分流模式"
                                className="h-9 w-full min-w-0 rounded-xl border border-input bg-background px-3 text-sm text-foreground"
                            >
                                <option value={1}>轮询（spread / 均摊）</option>
                                <option value={3}>优先填充（fill_first / 默认）</option>
                            </select>
                        </label>
                    </div>
                    <div className="grid min-w-0 gap-2 sm:grid-cols-[minmax(150px,1fr)]">
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
    // 画布展示全部映射（含已失效的），失效项在渲染层打「已失效」标，不再静默隐藏。
    const canvasRows = useMemo(() => groupedRouteTargets(targets), [targets]);
    const unroutedModels = useMemo(() => unroutedChannelModels(channelModels, targets), [channelModels, targets]);
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
                mode: 3,
                fallback_mode: 'return_group',
                system_prompt_override: '',
                prompt_override_mode: 'append_system',
            },
        ]);
        setEditingRequestModel('');
    };

    const updateTarget = (index: number, patch: Partial<AccessPlanRouteTarget>) => {
        setTargets((current) => {
            const next = current.map((target, currentIndex) => (
                currentIndex === index ? { ...target, ...patch } : target
            ));
            // mode 挂在规则（request_model 级）上：改了某条 target 的分流模式，
            // 同一请求模型的其他 target 一起同步，避免保存时规则内 mode 打架。
            if (patch.mode !== undefined) {
                const requestKey = cleanOneMillionModelName(next[index]?.request_model ?? '').toLowerCase();
                if (requestKey) {
                    return next.map((target) => (
                        cleanOneMillionModelName(target.request_model).toLowerCase() === requestKey
                            ? { ...target, mode: patch.mode }
                            : target
                    ));
                }
            }
            return next;
        });
    };

    const removeTarget = (index: number) => {
        setTargets((current) => current.filter((_, currentIndex) => currentIndex !== index));
    };

    // save() submits the full `targets` array, so validation must cover the same
    // set -- editableTargets already filters out stale targets, which would let a
    // stale target with malformed fields (channel_id<=0 / empty request_model) slip
    // past the frontend gate and fail with a backend 400.
    const invalid = targets.some((target) => (
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
        onSuccess?: () => void,
        restoreOnError?: AccessPlanRouteTarget[],
    ) => {
        updateRoutes.mutate(
            {
                access_plan_id: plan.id,
                targets: nextTargets.map((target, index) => ({
                    ...target,
                    request_model: cleanOneMillionModelName(target.request_model),
                    upstream_model: cleanOneMillionModelName(target.upstream_model),
                    priority: Number.isFinite(target.priority) ? clampRoutePriority(target.priority) : clampRoutePriority(index + 1),
                    weight: 1,
                    enabled: target.enabled,
                    billing_model_source: target.billing_model_source ?? 'request_model',
                    billing_model_override: cleanOneMillionModelName(target.billing_model_override ?? '') || undefined,
                    // 分流模式跟随每条规则（同 request_model 的所有 target 带同一个值），后端按规则落库。
                    mode: isSpreadMode(target.mode) ? 1 : 3,
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
                    if (restoreOnError) setTargets(restoreOnError);
                    toast.error(t('toast.routesUpdateFailed'), { description: apiErrorMessage(error) });
                },
            }
        );
    };

    const saveTargetsRef = useRef(saveTargets);
    saveTargetsRef.current = saveTargets;
    const channelModelIndexRef = useRef(channelModelIndex);
    channelModelIndexRef.current = channelModelIndex;
    const targetsRef = useRef(targets);
    targetsRef.current = targets;

    const changeCanvasPriority = useCallback((targetId: number, nextPriorityRaw: number) => {
        const nextPriority = clampRoutePriority(nextPriorityRaw);
        const current = targetsRef.current;
        const index = current.findIndex((target) => target.id === targetId);
        if (index < 0) return;
        if (clampRoutePriority(current[index].priority) === nextPriority) return;
        const next = current.map((target, currentIndex) => (
            currentIndex === index ? { ...target, priority: nextPriority } : target
        ));
        setTargets(next);
        saveTargetsRef.current(
            next,
            t('toast.routesUpdated'),
            undefined,
            current,
        );
    }, [t]);

    // 「加一到顶」：把点中的候选提到同一请求模型的最前（优先级 0），其余顺延。
    // 优先级同时被 failover(优先填充) 与 spread(轮询) 两个 balancer 消费，所以这里
    // 只改顺序，两种模式的实际先后都会跟着变。
    const moveCanvasTargetToTop = useCallback((targetId: number) => {
        const current = targetsRef.current;
        const index = current.findIndex((target) => target.id === targetId);
        if (index < 0) return;
        const next = moveRouteTargetToTop(current, index);
        setTargets(next);
        saveTargetsRef.current(
            next,
            t('toast.routesUpdated'),
            undefined,
            current,
        );
    }, [t]);

    const moveCanvasTargetUpOne = useCallback((targetId: number) => {
        const current = targetsRef.current;
        const index = current.findIndex((target) => target.id === targetId);
        if (index < 0) return;
        const next = moveRouteTargetUpOne(current, index);
        setTargets(next);
        saveTargetsRef.current(next, t('toast.routesUpdated'), undefined, current);
    }, [t]);

    const moveEditorTargetToTop = useCallback((targetIndex: number) => {
        const current = targetsRef.current;
        const next = moveRouteTargetToTop(current, targetIndex);
        setTargets(next);
        saveTargetsRef.current(next, t('toast.routesUpdated'), undefined, current);
    }, [t]);

    const addUnroutedModel = useCallback((model: UnroutedChannelModel) => {
        const current = targetsRef.current;
        const requestKey = model.clean_name.toLowerCase();
        const nextPriority = current
            .filter((target) => cleanOneMillionModelName(target.request_model).toLowerCase() === requestKey)
            .reduce((maxPriority, target) => Math.max(maxPriority, clampRoutePriority(target.priority)), -1) + 1;
        const next = [
            ...current,
            {
                request_model: model.clean_name,
                channel_id: model.channel_id,
                upstream_model: model.clean_name,
                priority: clampRoutePriority(nextPriority),
                weight: 1,
                enabled: true,
                billing_model_source: 'request_model',
                mode: 3,
                fallback_mode: 'return_group',
                system_prompt_override: '',
                prompt_override_mode: 'append_system',
            } satisfies AccessPlanRouteTarget,
        ];
        setTargets(next);
        saveTargetsRef.current(next, t('toast.routesUpdated'), undefined, current);
    }, [t]);

    const save = (onSuccess?: () => void) => saveTargets(targets, t('toast.routesUpdated'), onSuccess);

    // 画布行内「分流模式」下拉：同一条规则（request_model）的所有 target 一起改，
    // 后端按 request_model 聚成规则、取该组的 mode 落库（见 op.AccessPlanUpdateRouteTargets）。
    const updateRowMode = useCallback((rowTargets: AccessPlanRouteTarget[], mode: 1 | 3) => {
        const keys = new Set(rowTargets.map((target) => target.id).filter((id): id is number => typeof id === 'number'));
        const current = targetsRef.current;
        const next = current.map((target) => (
            (typeof target.id === 'number' && keys.has(target.id)) || rowTargets.includes(target)
                ? { ...target, mode }
                : target
        ));
        setTargets(next);
        saveTargetsRef.current(next, t('toast.routesUpdated'), undefined, current);
    }, [t]);

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
        setJsonText(JSON.stringify(targets.map((target) => ({
            request_model: cleanOneMillionModelName(target.request_model),
            channel_id: target.channel_id,
            upstream_model: cleanOneMillionModelName(target.upstream_model),
            priority: clampRoutePriority(target.priority ?? 1),
            weight: 1,
            enabled: target.enabled,
            billing_model_source: target.billing_model_source ?? 'request_model',
            billing_model_override: cleanOneMillionModelName(target.billing_model_override ?? ''),
            mode: isSpreadMode(target.mode) ? 1 : 3,
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
                priority: clampRoutePriority(Number(target.priority ?? index + 1)),
                weight: 1,
                enabled: target.enabled ?? true,
                billing_model_source: target.billing_model_source ?? 'request_model',
                billing_model_override: cleanOneMillionModelName(target.billing_model_override ?? '') || undefined,
                mode: Number(target.mode) === 1 ? 1 : 3,
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
                <div className="hidden min-w-0 items-center gap-2 px-1 text-xs text-muted-foreground sm:flex">
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
                modelNames={modelNames}
                channelModels={channelModels}
                channelModelsReady={channelModelsReady}
                unroutedModels={unroutedModels}
                onEditRequest={setEditingRequestModel}
                onPriorityChange={changeCanvasPriority}
                onMoveToTop={moveCanvasTargetToTop}
                onMoveUpOne={moveCanvasTargetUpOne}
                onAddUnroutedModel={addUnroutedModel}
                onOpenJson={openJson}
                onModeChange={updateRowMode}
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
                                    moveTargetToTop={moveEditorTargetToTop}
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

/**
 * 「全局默认模式」下拉：读写通用 setting route_mode_override。
 * '' = 跟随各规则（无全局覆盖）；'spread' = 全局轮询；'fill_first' = 全局优先填充。
 * 走现有 setting 的 GET（/setting/list）与写接口（/setting/set）。
 */
function GlobalRouteModeSelect({ className }: { className?: string }) {
    const { data: settings } = useSettingList();
    const setSetting = useSetSetting();
    const [value, setValue] = useState<RouteModeOverrideChoice>('');
    const savedRef = useRef<RouteModeOverrideChoice>('');

    useEffect(() => {
        if (!settings) return;
        const found = settings.find((setting) => setting.key === SettingKey.RouteModeOverride);
        const raw = (found?.value ?? '') as RouteModeOverrideChoice;
        const next = ROUTE_MODE_OVERRIDE_CHOICES.some((choice) => choice.value === raw) ? raw : '';
        setValue(next);
        savedRef.current = next;
    }, [settings]);

    return (
        <label className={cn('flex min-w-0 items-center gap-2 text-xs text-muted-foreground', className)}>
            <span className="shrink-0 whitespace-nowrap">全局默认模式</span>
            <select
                value={value}
                onChange={(event) => {
                    const next = event.target.value as RouteModeOverrideChoice;
                    setValue(next);
                    setSetting.mutate(
                        { key: SettingKey.RouteModeOverride, value: next },
                        {
                            onSuccess: () => {
                                savedRef.current = next;
                                toast.success('全局默认模式已保存');
                            },
                            onError: (error) => {
                                setValue(savedRef.current);
                                toast.error('全局默认模式保存失败', { description: apiErrorMessage(error) });
                            },
                        },
                    );
                }}
                aria-label="全局默认模式"
                title="全局默认分流模式：跟随各规则=不覆盖；全局轮询/全局优先填充=强制覆盖所有未锁定的分流规则"
                className="h-8 min-w-0 rounded-full border border-border/70 bg-background/60 px-2.5 text-xs text-foreground outline-none focus:border-primary/50"
            >
                {ROUTE_MODE_OVERRIDE_CHOICES.map((choice) => (
                    <option key={choice.value} value={choice.value}>{choice.label}</option>
                ))}
            </select>
        </label>
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
                                <div className="hidden min-w-0 flex-wrap gap-1.5 sm:flex">
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
                            <GlobalRouteModeSelect />
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
