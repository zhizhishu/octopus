'use client';

import { memo, useCallback, useEffect, useMemo, useRef, useState, useSyncExternalStore } from 'react';
import { DragDropContext, Draggable, Droppable, type DropResult } from '@hello-pangea/dnd';
import { useTranslations } from 'next-intl';
import {
    ArrowUpToLine,
    BadgeDollarSign,
    Check,
    GitBranch,
    GripVertical,
    ListOrdered,
    MapIcon,
    Loader2,
    Maximize2,
    Minimize2,
    PencilLine,
    Plus,
    Repeat,
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
import { ROUTE_MODE_OVERRIDE_VALUES, RouteModeOverrideValue, routeModeOverrideLabel, useRouteModeOverrideSetting } from '@/api/endpoints/setting';
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
import {
    canvasViewSignature,
    decideCanvasViewport,
    rememberCanvasViewport,
    routeTargetsServerVersion,
    savedCanvasViewport,
    shouldResyncRouteTargets,
    type CanvasViewKey,
} from './canvas-viewport';
import {
    applyFrozenOrder,
    captureOrder,
    createLatestSaveQueue,
    decideDialogClose,
    FROZEN_ORDER_IDLE_MS,
    sameDraft,
    shouldReleaseFrozenOrder,
    shouldResyncDraft,
    type FrozenOrder,
    type SaveOutcome,
} from './edit-state';
import {
    displayPriority,
    moveItem,
    renumberFirst,
    rotatePeers,
    setRotatePriority,
    simpleModeOf,
    sortByTryOrder,
    switchToFirst,
    switchToRotate,
    tryLevels,
    type SimpleRouteMode,
} from './route-mode';
import { normalizePromptMode, promptEnabledFromSaved } from '../prompt/prompt-mode';
import { PromptLayerControls } from '../prompt/PromptLayerControls';

type UpdateRoutesPayload = Parameters<ReturnType<typeof useUpdateAccessPlanRouteTargets>['mutateAsync']>[0];
type AccessPlanRouteSaveResult = Awaited<ReturnType<ReturnType<typeof useUpdateAccessPlanRouteTargets>['mutateAsync']>>;
type RouteSaveJob = {
    payload: UpdateRoutesPayload;
    /** 这次整表是从哪份本地 targets 来的：回包时本地还是它，才拿回包覆盖本地。 */
    sourceTargets: AccessPlanRouteTarget[];
    successMessage: string;
    onSuccess?: () => void;
    restoreOnError: boolean;
};

/**
 * 关编辑弹窗时有没保存的改动：问一句再关。文案走 next-intl（accessPlan.unsaved）。
 * 叠在原弹窗上面的小弹窗：点外面 / Esc = 继续编辑，只有点「不保存」才真关。
 */
function UnsavedChangesConfirm({ open, onKeepEditing, onDiscard }: { open: boolean; onKeepEditing: () => void; onDiscard: () => void }) {
    const t = useTranslations('accessPlan.unsaved');
    return (
        <Dialog open={open} onOpenChange={(next) => { if (!next) onKeepEditing(); }}>
            <DialogContent
                showCloseButton={false}
                data-testid="unsaved-confirm"
                className="w-[calc(100vw-2rem)] max-w-sm gap-3 rounded-2xl border-border bg-card p-5 sm:max-w-sm"
            >
                <DialogHeader className="text-left">
                    <DialogTitle className="text-base">{t('title')}</DialogTitle>
                    <DialogDescription>{t('description')}</DialogDescription>
                </DialogHeader>
                <DialogFooter className="flex-row justify-end gap-2">
                    <Button type="button" variant="outline" className="rounded-xl" onClick={onKeepEditing} autoFocus>
                        {t('keepEditing')}
                    </Button>
                    <Button type="button" variant="destructive" className="rounded-xl" onClick={onDiscard}>
                        {t('discard')}
                    </Button>
                </DialogFooter>
            </DialogContent>
        </Dialog>
    );
}

const DEFAULT_PLAN_TEMPLATES = [
    { slug: 'vip', display_name: 'VIP' },
    { slug: 'svip', display_name: 'SVIP' },
    { slug: 'ssvip', display_name: 'SSVIP' },
    { slug: 'custom', display_name: 'Custom' },
] as const;

/**
 * 方案页文案（用户 2026-10-30：「候选渠道 / 发送模型」不够产品化）。统一词表，跟设置页 / 渠道页 / 日志一致：
 * - 请求模型：用户调用时写的模型名（日志同名）。
 * - 实际模型：转发给渠道时用的模型名（日志同名；可与请求模型不同）。
 * - 模型路由：一条 = 一个请求模型 → 它能用的渠道 + 顺序 + 都失败时怎么办；一个渠道一条「线路」。
 * - 分配方式：按顺序（fill_first, 3）/ 轮流（spread, 1）/ 分层轮流（6）。
 * - 模型重定向：渠道自己的 model_mapping（请求名 → 实际名），与方案路由无关，画布只读展示。
 * 主屏一句话，细节放 title 悬停提示。每条文案都要和后端真实效果一致（见 internal/relay、balancer）。
 */
const ACCESS_PLAN_TEXT = {
    list: {
        title: '正在加载方案…',
        loadFailed: '加载失败',
        empty: '还没有方案',
        selectOrCreate: '选一个方案，或新建一个',
    },
    create: {
        title: '新建方案',
        displayName: '名称',
        slug: '调用代号（如 vip，调用时用它选方案）',
        submit: '创建',
    },
    status: {
        default: '默认',
        enabled: '启用',
        disabled: '停用',
    },
    basic: {
        title: '基本信息',
        displayName: '名称',
        slug: '调用代号',
        enabled: '启用',
        defaultPlan: '设为默认方案',
    },
    billing: {
        title: '计费倍率',
        jsonTitle: '用 JSON 批量改倍率',
        jsonApply: '导入',
        jsonCopy: '填入当前配置',
        jsonPlaceholder: '{\n  "default_multiplier": 1,\n  "rules": [\n    {"model_name": "gpt-4", "multiplier": 1.2, "enabled": true}\n  ]\n}',
        defaultMultiplier: '默认倍率',
        ruleCount: '{count} 个单独倍率',
        modelName: '模型名',
        multiplier: '倍率',
        empty: '还没有单独设倍率的模型',
        defaultSource: '默认倍率',
        modelRuleSource: '单独倍率',
    },
    routes: {
        title: '模型路由',
        manageTitle: '方案设置',
        manageHint: '名称、调用代号和计费倍率。模型走哪些渠道在主画布上改。',
        jsonTitle: '用 JSON 批量改路由',
        jsonApply: '导入到画布',
        jsonCopy: '填入当前配置',
        jsonPlaceholder: '[\n  {\n    "request_model": "claude-fable-5",\n    "channel_id": 1,\n    "upstream_model": "claude-fable-5",\n    "priority": 1,\n    "weight": 1,\n    "enabled": true\n  }\n]',
        targetCount: '{count} 条线路',
        requestModel: '请求模型',
        requestModelHint: '用户调用时写的模型名',
        modeSpread: '轮询',
        modeFillFirst: '优先',
        modeLabel: '分配方式',
        modeFillFirstShort: '从上往下依次用，出错才换',
        modeSpreadShort: '数字大的先用，一样的轮流',
        modeSpreadHint: '轮询：优先级数字大的先用，数字一样的轮流；这一级都不能用，才换数字小的。',
        modeFillFirstHint: '优先：从上往下依次用，出错、限流或冷却才换下一个（上游缓存命中最好）。',
        tryOrder: '实际顺序',
        tryOrderThen: '都不能用时',
        tryOrderRotate: '轮流',
        tryOrderNone: '没有开着的渠道',
        priorityLabel: '优先级',
        dragHint: '拖 ⠿ 调整先后',
        lineSwitchHint: '不想用这个渠道就关掉（渠道和实际模型跟着渠道页自动同步，这里不改）',
        dragHandle: '拖动调整先后',
        priorityInputHint: '优先级：数字大的先用，一样的轮流',
        upstreamModel: '实际模型',
        upstreamModelHint: '转发给这个渠道时用的模型名，可以和请求模型不同',
        orderLabel: '第 {order} 个',
        noChannels: '还没有渠道',
        missingChannel: '渠道 #{id} 已删除',
        empty: '这个模型还没有渠道，用上方「添加渠道」加一个',
        canvasTitle: '模型路由',
        canvasHint: '左边是用户请求的模型，右边是它会用到的渠道；点「编辑」调整。',
        canvasEmpty: '还没有路由。渠道启用模型后会自动出现在这里。',
        filterEmpty: '没有符合筛选的模型或渠道',
        targetSummary: '渠道',
        setupTitle: '怎么生效',
        setupHint: '用户调用某个模型时，按这里的路由挑渠道；这里没有的模型，直接用所有提供它的渠道。渠道或模型有变化会自动同步。',
        groupRouteHint: '扣费按每条线路显示的计费模型和倍率算。流式回复一旦开始输出，就不会再换渠道。',
        quickEdit: '编辑',
        quickEditTitle: '编辑路由',
        quickEditDescription: '这个模型用哪些渠道、谁先用、转发时用什么模型名。',
        addChannel: '添加渠道：',
        addChannelPlaceholder: '选择渠道…',
        close: '关闭',
        hiddenStale: '{count} 条线路已失效（渠道已删除或不再提供该模型）',
        fallback: '都不能用时',
        multiplier: '倍率',
        unset: '未设置',
        billingSourceRequest: '按请求模型计费',
        billingSourceUpstream: '按实际模型计费',
        billingSourceOverride: '按指定模型计费',
        fallbackNone: '直接报错',
        fallbackNoneHint: '只在这里列出的渠道里试，全部失败就报错',
        fallbackReturnGroup: '再试其他有此模型的渠道',
        fallbackReturnGroupHint: '这里的渠道都失败后，再试其他提供同名模型、还没试过的渠道',
        fallbackCardNone: '不换其他渠道',
        fallbackCardReturnGroup: '可换其他渠道',
        promptOverride: '提示词',
        promptPlaceholder: '可选：只对这个模型生效的系统提示词',
        advancedTitle: '更多设置',
        advancedHint: '都不能用时、提示词',
        moveToTopFillFirst: '排到第一',
        moveToTopTiered: '调到最高',
        moveToTopFillFirstHint: '把这个渠道排到第 1 个，最先用',
        moveToTopTieredHint: '把它的优先级调到比其他都大，最先用（其他渠道不变）',
        moveToTopAtTop: '已经是最高',
        orderFillFirstHint: '第 {order} 个用：从上往下依次用，出错才换。＋ 往前，− 往后',
        orderTieredHint: '优先级 {tier}：数字大的先用，一样的轮流。＋ 调高，− 调低',
        orderTieredPeersHint: '优先级 {tier}：和 {peers} 轮流。数字大的先用。＋ 调高，− 调低',
        moveUp: '往前',
        moveDown: '往后',
        tierUp: '调高（更先用）',
        tierDown: '调低',
        batchTitle: '批量编辑（JSON）',
        batchHint: '一次导入或导出整个方案的路由',
        batchButtonHint: '用 JSON 批量导入 / 导出路由',
        viewAll: '全部',
        viewByChannel: '按渠道',
        viewByModel: '按模型',
        filterChannel: '筛选渠道',
        filterChannelAll: '全部渠道',
        filterModel: '筛选模型',
        filterModelAll: '全部模型',
        searchPlaceholder: '搜模型或渠道…',
        fullscreen: '全屏',
        exitFullscreen: '退出全屏（Esc）',
        minimapShow: '显示小地图',
        minimapHide: '隐藏小地图',
        minimap: '小地图',
        fitAll: '看全部',
        zoomReadable: '放大',
        familyModels: '{count} 个模型',
        familyTargets: '{count} 条线路',
        redirectBand: '渠道里的模型重定向（只读，去渠道页修改）',
        redirectCount: '{count} 条',
        redirectFrom: '请求模型',
        redirectTo: '实际模型',
        planDefaultMultiplier: '默认倍率 {multiplier}x',
        channelSearch: '搜渠道名或 #ID…',
        channelSearchEmpty: '没有匹配的渠道',
        planLabel: '方案',
        planSearch: '搜方案名或代号…',
        planSearchEmpty: '没有匹配的方案',
        settingsButton: '方案设置',
        defaultModeLabel: '默认分配方式',
        defaultModeUnavailable: '默认分配方式暂时读不到',
        defaultModeHint: '新出现的模型路由、以及没配路由的模型，用哪种方式挑渠道；已经单独设过的路由不受影响',
        displayNameHint: '名称：给人看的，可以写中文',
        slugHint: '调用时用这个代号选方案（请求头 X-Octopus-Plan）；建议英文、数字、-、_。',
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
        updated: '方案已保存',
        updateFailed: '保存失败',
        deleted: '方案已删除',
        deleteFailed: '删除失败',
        billingUpdated: '计费倍率已保存',
        billingUpdateFailed: '计费倍率保存失败',
        routesUpdated: '路由已保存',
        routesUpdateFailed: '路由保存失败',
        jsonApplied: '已导入，记得保存',
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
    if (!Number.isFinite(value)) return 1;
    return Math.min(Number.MAX_SAFE_INTEGER, Math.max(1, Math.trunc(value)));
}

/**
 * 分流模式（后端契约）：规则对象上的 mode 是 JSON number，
 * 1 = 纯轮询 spread（所有目标并列均摊），6 = 分层轮询（目标上的数字越大越先尝试，
 * 同一个数字内继续轮询），3 = fill_first 优先填充（默认）。其他/缺省值一律视为 fill_first。
 *
 * 6 是用户 2026-09-30 定的「B 方案」：画布节点上的数字必须真的影响选路（刻意不用旧版退役过的
 * 2/4/5，避免历史库里的旧模式值被重新赋义）。纯轮询(1) 故意
 * 不看数字（拖拽序号天然唯一，当硬边界会退化成固定顺序），所以分层语义单独一档，
 * 旧规则不动、只有在管理员显式改数字或重选模式时才升级为 6。
 */
const SPREAD_TIERED_MODE = 6;

function isSpreadMode(mode: number | undefined | null): boolean {
    const value = Number(mode);
    return value === 1 || value === SPREAD_TIERED_MODE;
}

/** 轮询家族内部保持原本那一档：分层轮询不能因为一次保存被悄悄降级回纯轮询。 */
function spreadModeValue(mode: number | undefined | null): 1 | 6 {
    return Number(mode) === SPREAD_TIERED_MODE ? SPREAD_TIERED_MODE : 1;
}

/** 把 mode 归一到合法值：1 / 6 / 3（默认 3）。 */
function normalizeRouteMode(mode: number | undefined | null): 1 | 3 | 6 {
    return isSpreadMode(mode) ? spreadModeValue(mode) : 3;
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

/**
 * 「都失败时」：后端 failover（旧默认）与 return_group 现在效果一样 —— 方案里的渠道都失败后，
 * 再去试其他提供同名模型、还没试过的渠道（relay.shouldReturnToOriginalGroup）；只有 none 不外溢、直接报错。
 * 所以两者显示同一句，不再出现「尝试下一个目标」这种和实际不符的说法。
 */
function fallbackModeLabel(mode: AccessPlanRouteTarget['fallback_mode'], variant: 'select' | 'card' | 'hint' = 'select') {
    const t = accessPlanText;
    const none = mode === 'none';
    if (variant === 'card') return t(none ? 'routes.fallbackCardNone' : 'routes.fallbackCardReturnGroup');
    if (variant === 'hint') return t(none ? 'routes.fallbackNoneHint' : 'routes.fallbackReturnGroupHint');
    return t(none ? 'routes.fallbackNone' : 'routes.fallbackReturnGroup');
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
        .map(([requestKey, routeTargets]) => {
            // 数字在两档里的含义相反：优先填充 = 顺位（越小越先），分层轮询 = 层级（越大越先）。
            // 画布必须按「谁先被尝试」从上往下画，否则同一个 + 会让节点往下掉、看起来被贬到队尾，
            // 而选路其实先打它 —— 这正是审查抓到的「画布和选路对穿」。
            const rowMode = normalizeRouteMode(routeTargets.find((target) => target.mode !== undefined)?.mode);
            return {
                requestKey,
                requestModel: cleanOneMillionModelName(routeTargets[0]?.request_model ?? '') || accessPlanText('routes.unset'),
                mode: rowMode,
                // 冻结排序期间每张卡的真实顺位（见 edit-state.ts 的 applyFrozenOrder）；平时为空 = 按下标。
                ranks: undefined as number[] | undefined,
                targets: sortByTryOrder(routeTargets, rowMode),
            };
        })
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

/** 同一条规则（同请求模型）的候选在 targets 里的下标。 */
function ruleIndexes(targets: AccessPlanRouteTarget[], requestModel: string): number[] {
    const key = cleanOneMillionModelName(requestModel).toLowerCase();
    return targets.flatMap((target, index) => (cleanOneMillionModelName(target.request_model).toLowerCase() === key ? [index] : []));
}

/** 用 fn 改写一整条规则（fn 收到这条规则的候选、返回同样多个），其他规则原样不动。 */
function mapRule(
    targets: AccessPlanRouteTarget[],
    requestModel: string,
    fn: (group: AccessPlanRouteTarget[]) => AccessPlanRouteTarget[],
): AccessPlanRouteTarget[] {
    const indexes = ruleIndexes(targets, requestModel);
    if (indexes.length === 0) return targets;
    const group = indexes.map((index) => targets[index]);
    const changed = fn(group);
    const byOld = new Map(group.map((target, i) => [target, changed[i]]));
    // fn 可能重新排过序：按「旧对象 → 新对象」对回去，保持 targets 里的位置不变。
    const sortedChanged = new Map<AccessPlanRouteTarget, AccessPlanRouteTarget>();
    changed.forEach((next) => {
        const old = group.find((target) => target.id !== undefined ? target.id === next.id : target.channel_id === next.channel_id && target.upstream_model === next.upstream_model);
        if (old) sortedChanged.set(old, next);
    });
    return targets.map((target, index) => (
        indexes.includes(index) ? (sortedChanged.get(target) ?? byOld.get(target) ?? target) : target
    ));
}

/**
 * 「到顶」。两种方式语义相反，分开处理：
 * - 优先：按现在的顺序把被点项挪到第一个，重编 1..N。
 * - 轮询：只把被点项的数字调到「比别的都大 1」，其余候选的数字一个都不动（同级轮流关系保留）。
 */
function moveRouteTargetToTop(targets: AccessPlanRouteTarget[], targetIndex: number) {
    const selectedTarget = targets[targetIndex];
    if (!selectedTarget) return targets;
    return mapRule(targets, selectedTarget.request_model, (group) => {
        const mode = normalizeRouteMode(group.find((target) => target.mode !== undefined)?.mode);
        const own = group.indexOf(selectedTarget);
        if (simpleModeOf(mode) === 'rotate') {
            const maxPriority = group.reduce((max, target) => Math.max(max, displayPriority(target, mode)), 1);
            if (maxPriority >= Number.MAX_SAFE_INTEGER) return group;
            return setRotatePriority(group, own, maxPriority + 1);
        }
        const ordered = sortByTryOrder(group, mode);
        return renumberFirst([selectedTarget, ...ordered.filter((target) => target !== selectedTarget)]);
    });
}

/**
 * 分层轮询下「到顶」是否已无意义：被点项已独占最高层，或全场数字都顶到数值上限。
 * 优先填充恒为 false（它的「到顶」总能通过重编号达成）。
 */
function isRouteTargetAtTopTier(target: AccessPlanRouteTarget, peers: AccessPlanRouteTarget[]) {
    const mode = normalizeRouteMode(peers[0]?.mode ?? target.mode);
    if (simpleModeOf(mode) !== 'rotate') return false;
    const maxPriority = peers.reduce(
        (max, item) => Math.max(max, displayPriority(item, mode)),
        1,
    );
    if (maxPriority >= Number.MAX_SAFE_INTEGER) return true;
    const own = displayPriority(target, mode);
    const sameTier = peers.filter((item) => displayPriority(item, mode) === own);
    return own === maxPriority && sameTier.length === 1;
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
                            {t('routes.displayNameHint')}
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
    const [confirmDelete, setConfirmDelete] = useState(false);
    const [confirmCloseOpen, setConfirmCloseOpen] = useState(false);
    // 打开弹窗那一刻的值：草稿从它起步，也拿它判断改没改（列表回刷不会改到正在编辑的草稿）。
    const draftOf = (p: AccessPlan) => ({ displayName: p.display_name, slug: p.slug, enabled: p.enabled, isDefault: p.is_default });
    const [openedWith, setOpenedWith] = useState(() => draftOf(plan));
    const dirty = open && !sameDraft({ displayName, slug, enabled, isDefault }, openedWith);
    const openEditor = () => {
        const base = draftOf(plan);
        setOpenedWith(base);
        setDisplayName(base.displayName);
        setSlug(base.slug);
        setEnabled(base.enabled);
        setIsDefault(base.isDefault);
        setConfirmDelete(false);
        setOpen(true);
    };
    const onOpenChange = (next: boolean) => {
        if (next) {
            openEditor();
            return;
        }
        if (decideDialogClose(dirty) === 'confirm') {
            setConfirmCloseOpen(true);
            return;
        }
        setOpen(false);
    };
    const closeEditor = () => {
        setConfirmCloseOpen(false);
        setOpen(false);
    };

    const save = () => {
        updatePlan.mutate(
            {
                id: plan.id,
                display_name: displayName.trim(),
                slug: normalizeSlug(slug),
                enabled,
                is_default: isDefault,
                system_prompt_override: plan.system_prompt_override ?? '',
                prompt_override_mode: normalizePromptMode(plan.prompt_override_mode),
                prompt_override_enabled: promptEnabledFromSaved(plan.prompt_override_enabled, plan.prompt_override_mode, plan.system_prompt_override),
            },
            {
                onSuccess: () => {
                    closeEditor();
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
                closeEditor();
                toast.success(t('toast.deleted'));
            },
            onError: (error) => {
                toast.error(t('toast.deleteFailed'), { description: apiErrorMessage(error) });
            },
        });
    };

    const canSave = displayName.trim().length > 0 && normalizeSlug(slug).length > 0;

    return (
        <Dialog open={open} onOpenChange={onOpenChange}>
            <UnsavedChangesConfirm open={confirmCloseOpen} onKeepEditing={() => setConfirmCloseOpen(false)} onDiscard={closeEditor} />
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
                        <span>{t('routes.slugHint')}</span>
                    </label>
                    <label className="flex items-center justify-between gap-3 rounded-2xl border border-border/70 bg-muted/20 px-3 py-2 text-sm">
                        <span>{t('basic.enabled')}</span>
                        <Switch checked={enabled} onCheckedChange={setEnabled} />
                    </label>
                    <label className="flex items-center justify-between gap-3 rounded-2xl border border-border/70 bg-muted/20 px-3 py-2 text-sm">
                        <span>{t('basic.defaultPlan')}</span>
                        <Switch checked={isDefault} onCheckedChange={setIsDefault} />
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

function billingServerVersion(plan: Pick<AccessPlan, 'id' | 'default_multiplier' | 'billing_rules'>): string {
    return `${plan.id}:${JSON.stringify({ d: plan.default_multiplier, r: plan.billing_rules ?? [] })}`;
}

function BillingRulesEditor({
    plan,
    modelNames,
    onDirtyChange,
}: {
    plan: AccessPlan;
    modelNames: string[];
    onDirtyChange?: (dirty: boolean) => void;
}) {
    const t = accessPlanText;
    const updateBilling = useUpdateAccessPlanBillingRules();
    const [defaultMultiplier, setDefaultMultiplier] = useState(plan.default_multiplier);
    const [rules, setRules] = useState<AccessPlanBillingRule[]>(plan.billing_rules);
    // baseline = 最近一次和服务器对齐的值；草稿和它不同 = 有没保存的改动。
    const [baseline, setBaseline] = useState(() => ({ defaultMultiplier: plan.default_multiplier, rules: plan.billing_rules }));
    const dirty = !sameDraft({ defaultMultiplier, rules }, baseline);
    // 服务器数据变了（别处保存 / 列表回刷）：只在没改动、没在保存时跟上，绝不冲掉正在改的草稿。
    const serverVersion = billingServerVersion(plan);
    const [syncedVersion, setSyncedVersion] = useState(serverVersion);
    if (shouldResyncDraft(syncedVersion, serverVersion, updateBilling.isPending, dirty)) {
        setSyncedVersion(serverVersion);
        setDefaultMultiplier(plan.default_multiplier);
        setRules(plan.billing_rules);
        setBaseline({ defaultMultiplier: plan.default_multiplier, rules: plan.billing_rules });
    }
    useEffect(() => {
        onDirtyChange?.(dirty);
    }, [dirty, onDirtyChange]);
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
                onSuccess: (data) => {
                    const savedRules = data.billing_rules ?? [];
                    const savedDefault = data.default_multiplier ?? defaultMultiplier;
                    setDefaultMultiplier(savedDefault);
                    setRules(savedRules);
                    setBaseline({ defaultMultiplier: savedDefault, rules: savedRules });
                    setSyncedVersion(billingServerVersion({ id: plan.id, default_multiplier: savedDefault, billing_rules: savedRules }));
                    toast.success(t('toast.billingUpdated'));
                },
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
    orderIndex: number;
    showPriority: boolean;
    fillFirst: boolean;
    // 轮询家族（纯轮询或分层轮询）：显示数字 + −/+ 步进器，数字越大越先尝试。
    spreadFamily: boolean;
    /** 轮询里和它数字一样、会轮流的其他渠道名（悬停提示用）；没有就是空串。 */
    rotatePeers: string;
    upstreamModel: string;
    enabled: boolean;
    fallback: string;
    fallbackHint: string;
    multiplier: number | string;
    billingSource: string;
    onPriorityChange?: (targetId: number, priority: number) => void;
    /** 鼠标停在 +/− 上：冻结的排序先不放开（触屏没有悬停，松手即算离开）。 */
    onPriorityHover?: (hovering: boolean) => void;
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
            <div className="mt-2 text-xs text-muted-foreground">{accessPlanText('routes.planDefaultMultiplier', { multiplier: data.multiplier })}</div>
            <Handle type="source" position={Position.Right} className="!size-2 !border-2 !border-background !bg-primary" />
        </div>
    );
});

const RequestFlowCard = memo(function RequestFlowCard({ data }: NodeProps<RequestFlowNode>) {
    const modeKey = data.mode !== undefined
        ? (isSpreadMode(data.mode) ? 'routes.modeSpread' : 'routes.modeFillFirst')
        : null;
    const modeLabel = modeKey ? accessPlanText(modeKey) : null;
    const modeHint = modeKey ? accessPlanText(`${modeKey}Hint`) : undefined;
    return (
        <div className="rounded-2xl border border-amber-500/25 bg-amber-500/10 px-3 py-2.5" style={{ width: REQ_W, height: REQ_H }}>
            <Handle type="target" position={Position.Left} className="!size-2 !border-2 !border-background !bg-amber-500" />
            <div className="flex items-center justify-between gap-2">
                <span className="inline-flex items-center gap-1.5 text-[10px] font-bold uppercase tracking-[0.16em] text-amber-600 dark:text-amber-300">
                    <span className="size-1.5 rounded-full bg-amber-500" />
                    <span title={accessPlanText('routes.requestModelHint')}>{accessPlanText('routes.requestModel')}</span>
                    {modeLabel !== null && (
                        <span title={modeHint} className="ml-0.5 cursor-help rounded-full bg-muted/60 px-1.5 py-px font-normal normal-case tracking-normal text-muted-foreground">
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
        ? accessPlanText('routes.orderFillFirstHint', { order: data.orderIndex })
        : data.rotatePeers
            ? accessPlanText('routes.orderTieredPeersHint', { tier: priority, peers: data.rotatePeers })
            : accessPlanText('routes.orderTieredHint', { tier: priority });
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
                        <>
                            <span className="shrink-0 font-medium text-foreground/80">{accessPlanText('routes.orderLabel', { order: data.orderIndex })}</span>
                            <div
                                className="nodrag nopan pointer-events-auto ml-0.5 inline-flex shrink-0 items-center gap-0.5"
                                onPointerDown={(event) => event.stopPropagation()}
                                onPointerEnter={(event) => { if (event.pointerType === 'mouse') data.onPriorityHover?.(true); }}
                                onPointerLeave={() => data.onPriorityHover?.(false)}
                            >
                                <button
                                    type="button"
                                    className="flex size-4 items-center justify-center rounded border border-border/70 bg-background leading-none text-foreground hover:bg-muted disabled:opacity-40"
                                    disabled={!data.targetId || priority >= Number.MAX_SAFE_INTEGER}
                                    aria-label={accessPlanText('routes.moveDown')}
                                    title={accessPlanText('routes.moveDown')}
                                    onClick={(event) => {
                                        event.stopPropagation();
                                        bumpPriority(1);
                                    }}
                                >
                                    −
                                </button>
                                <button
                                    type="button"
                                    className="flex size-4 items-center justify-center rounded border border-border/70 bg-background leading-none text-foreground hover:bg-muted disabled:opacity-40"
                                    disabled={!data.targetId || priority <= 1}
                                    aria-label={accessPlanText('routes.moveUp')}
                                    title={accessPlanText('routes.moveUp')}
                                    onClick={(event) => {
                                        event.stopPropagation();
                                        bumpPriority(-1);
                                    }}
                                >
                                    +
                                </button>
                            </div>
                        </>
                    ) : data.spreadFamily ? (
                        // 轮询：数字就是优先级，越大越先被尝试；数字一样的轮流。
                        // 这里 + 是「抬数字」= 更优先，与上面「优先」的顺位语义相反（顺位越小越先）。
                        <>
                            <span className="shrink-0 font-medium text-foreground/80">{accessPlanText('routes.priorityLabel')}</span>
                            <div
                                className="nodrag nopan pointer-events-auto ml-0.5 inline-flex shrink-0 items-center gap-0.5"
                                onPointerDown={(event) => event.stopPropagation()}
                                onPointerEnter={(event) => { if (event.pointerType === 'mouse') data.onPriorityHover?.(true); }}
                                onPointerLeave={() => data.onPriorityHover?.(false)}
                            >
                                <button
                                    type="button"
                                    className="flex size-4 items-center justify-center rounded border border-border/70 bg-background leading-none text-foreground hover:bg-muted disabled:opacity-40"
                                    disabled={!data.targetId || priority <= 1}
                                    aria-label={accessPlanText('routes.tierDown')}
                                    title={accessPlanText('routes.tierDown')}
                                    onClick={(event) => {
                                        event.stopPropagation();
                                        bumpPriority(-1);
                                    }}
                                >
                                    −
                                </button>
                                <span className="min-w-3.5 text-center font-semibold text-foreground">{priority}</span>
                                <button
                                    type="button"
                                    className="flex size-4 items-center justify-center rounded border border-border/70 bg-background leading-none text-foreground hover:bg-muted disabled:opacity-40"
                                    disabled={!data.targetId || priority >= Number.MAX_SAFE_INTEGER}
                                    aria-label={accessPlanText('routes.tierUp')}
                                    title={accessPlanText('routes.tierUp')}
                                    onClick={(event) => {
                                        event.stopPropagation();
                                        bumpPriority(1);
                                    }}
                                >
                                    +
                                </button>
                            </div>
                            {data.rotatePeers ? (
                                <span className="inline-flex shrink-0 items-center gap-0.5 font-medium text-emerald-600 dark:text-emerald-400">
                                    <Repeat className="size-2.5" />{accessPlanText('routes.tryOrderRotate')}
                                </span>
                            ) : null}
                        </>
                    ) : (
                        // 兜底：spreadFamily = !fillFirst，正常构造下走不到这里。留着只是为了
                        // 万一有别的构造点忘了传 spreadFamily 时不至于整行空白。
                        <span className="shrink-0 font-medium text-foreground/80">{accessPlanText('routes.modeSpread')}</span>
                    )}
                </div>
            </div>
            <div className="min-w-0">
                <div className="text-[9px] font-bold uppercase tracking-[0.14em] text-muted-foreground" title={accessPlanText('routes.upstreamModelHint')}>{accessPlanText('routes.upstreamModel')}</div>
                <div className="mt-0.5 truncate font-mono text-xs font-semibold text-foreground">
                    {data.upstreamModel}
                </div>
            </div>
            <div className="text-xs text-muted-foreground">
                <div className="font-bold text-foreground">{data.multiplier}x</div>
                <div className="truncate text-[10px]" title={data.billingSource}>{data.billingSource}</div>
                <div className="truncate text-[10px]" title={data.fallbackHint}>{data.fallback}</div>
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
                <Badge variant="secondary" className="rounded-full">{accessPlanText('routes.familyModels', { count: data.modelCount })}</Badge>
                <Badge variant="outline" className="rounded-full">{accessPlanText('routes.familyTargets', { count: data.targetCount })}</Badge>
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
                {accessPlanText('routes.redirectFrom')}
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
                <div className="text-[9px] font-bold uppercase tracking-[0.14em] text-muted-foreground">{accessPlanText('routes.redirectTo')}</div>
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
                <Badge variant="secondary" className="rounded-full">{accessPlanText('routes.redirectCount', { count: data.count })}</Badge>
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
    onPriorityHover?: (hovering: boolean) => void,
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
            // 优先填充：数字是选路顺位（越小越先）。轮询家族（纯轮询/分层轮询）：数字是分层，
            // 越大越先；同数字内继续轮询。
            // showPriority 只给优先填充那一路用（数字不全相同时才显示 P 值）；轮询家族的徽标
            // 走 spreadTiered = 模式本身，避免「数字调成一样」被误读成模式变了。
            // 用分组时算好的模式，不要回头看 row.targets[0] —— 分层行的顺序已被改成降序，
            // 取首项会变成「看哪个渠道恰好排第一」，模式的判定不该跟着排序跑。
            const rowMode = row.mode;
            const isFillFirstMode = !isSpreadMode(rowMode);
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
                    width: TGT_W,
                    height: TGT_H,
                    data: {
                        channelName,
                        channelId: target.channel_id,
                        targetId: target.id,
                        laneId: reqId,
                        priority: displayPriority(target, rowMode),
                        orderIndex: row.ranks?.[targetIndex] ?? targetIndex + 1,
                        showPriority,
                        fillFirst: isFillFirstMode,
                        spreadFamily: !isFillFirstMode,
                        rotatePeers: rotatePeers(target, row.targets, rowMode)
                            .map((peer) => channelNameByID.get(peer.channel_id) ?? `#${peer.channel_id}`)
                            .join('、'),
                        upstreamModel: cleanOneMillionModelName(target.upstream_model || accessPlanText('routes.unset')),
                        enabled: target.enabled,
                        fallback: fallbackModeLabel(target.fallback_mode, 'card'),
                        fallbackHint: fallbackModeLabel(target.fallback_mode, 'hint'),
                        multiplier: billing.multiplier,
                        billingSource: billingSourceLabel(target.billing_model_source),
                        onPriorityChange,
                        onPriorityHover,
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
            data: { label: accessPlanText('routes.redirectBand'), count: channelMappings.length, width: BAND_W, height: bandHeight },
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
    onEditRequest: (requestModel: string) => void;
    onPriorityChange?: (targetId: number, priority: number) => void;
    onPriorityHover?: (hovering: boolean) => void;
    onOpenJson?: () => void;
};

type RouteViewMode = 'all' | 'channel' | 'mapping';

// 宽屏（≥ sm）判定：小地图默认开关跟它走。服务端快照 = 窄屏（不画小地图），水合后按真实宽度。
const WIDE_QUERY = '(min-width: 640px)';
function subscribeWide(onChange: () => void) {
    const mq = window.matchMedia(WIDE_QUERY);
    mq.addEventListener('change', onChange);
    return () => mq.removeEventListener('change', onChange);
}
function useIsWideScreen(): boolean {
    return useSyncExternalStore(subscribeWide, () => window.matchMedia(WIDE_QUERY).matches, () => false);
}

function RouteFlowCanvasInner({
    plan, rows, channels, channelMappings, modelNames, channelModels, channelModelsReady, onEditRequest, onPriorityChange, onPriorityHover, onOpenJson,
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

    const targetCount = rows.reduce((sum, row) => sum + row.targets.length, 0);

    // ── ReactFlow 无限画布 ──
    const { fitView, setCenter, setViewport, getViewport } = useReactFlow();

    // 全屏/清屏切换后容器尺寸变了要重新自适应。ReactFlow 靠 ResizeObserver 异步拿新尺寸，
    // 单个 rAF 会读到旧尺寸导致 fitView 对不齐，故用双 rAF 等布局与观察器都落定后再 fit（807e22b）。
    const fitSoon = useCallback(() => {
        requestAnimationFrame(() => requestAnimationFrame(() => {
            fitView({ padding: 0.14, maxZoom: 1, duration: 220 });
        }));
    }, [fitView]);

    // ── 全屏清屏切换（f0ba588 · 手机端拥挤优化）──
    const [canvasFullscreen, setCanvasFullscreen] = useState(false);
    const toggleFullscreen = useCallback((next?: boolean) => {
        setCanvasFullscreen((v) => (next === undefined ? !v : next));
        fitSoon();
    }, [fitSoon]);

    useEffect(() => {
        if (!canvasFullscreen) return;
        const handler = (event: KeyboardEvent) => {
            if (event.key === 'Escape') toggleFullscreen(false);
        };
        window.addEventListener('keydown', handler);
        return () => window.removeEventListener('keydown', handler);
    }, [canvasFullscreen, toggleFullscreen]);

    const flow = useMemo(
        () => buildRouteFlow(plan, filteredRows, channelNameByID, onEditRequest, filteredMappings, onPriorityChange, onPriorityHover),
        [plan, filteredRows, channelNameByID, onEditRequest, filteredMappings, onPriorityChange, onPriorityHover],
    );

    const [nodes, setNodes, onNodesChange] = useNodesState<FlowNode>(flow.nodes);
    const [edges, setEdges, onEdgesChange] = useEdgesState<Edge>(flow.edges);

    // 视口只在「换视图」时自动 fit（首次挂载 / 换方案 / 换筛选 / 由空变有），
    // 数据编辑（+1/−1、弹窗保存、增删映射、保存后回刷）只原地换节点，平移和缩放保持不动。
    // 判定见 canvas-viewport.ts 的 decideCanvasViewport（有单测）。
    const viewSignature = canvasViewSignature(viewMode, selectedChannelId, selectedMappingModel, query);
    const flowEmpty = flow.nodes.length === 0;
    const lastViewKeyRef = useRef<CanvasViewKey | null>(null);
    // 同方案重挂载时直接以已存视口起步，避免先闪一帧默认视口再跳。
    const [initialViewport] = useState(() => savedCanvasViewport(plan.id) ?? undefined);
    useEffect(() => {
        setNodes(flow.nodes);
        setEdges(flow.edges);
        const nextKey: CanvasViewKey = { planId: plan.id, view: viewSignature, empty: flowEmpty };
        const saved = savedCanvasViewport(plan.id);
        const action = decideCanvasViewport(lastViewKeyRef.current, nextKey, saved !== null);
        if (action === 'keep') {
            lastViewKeyRef.current = nextKey;
            return;
        }
        // 视图键在真正执行时才落账：被取消（StrictMode 双跑 / 紧接着又一次变化）时下次仍会重新判定。
        const raf = requestAnimationFrame(() => {
            lastViewKeyRef.current = nextKey;
            if (action === 'restore' && saved) {
                void setViewport(saved);
            } else {
                void fitView({ padding: 0.14, maxZoom: 1, duration: 220 });
            }
        });
        return () => cancelAnimationFrame(raf);
    }, [flow, flowEmpty, plan.id, viewSignature, setNodes, setEdges, fitView, setViewport]);

    // 视口兜底：平移/缩放结束和卸载时记下当前方案的视口，同方案重挂载时原位恢复。
    const planIdRef = useRef(plan.id);
    planIdRef.current = plan.id;
    const rememberViewport = useCallback((_event: unknown, viewport: { x: number; y: number; zoom: number }) => {
        rememberCanvasViewport(planIdRef.current, viewport);
    }, []);
    useEffect(() => () => {
        if (lastViewKeyRef.current && !lastViewKeyRef.current.empty) {
            rememberCanvasViewport(lastViewKeyRef.current.planId, getViewport());
        }
    }, [getViewport]);

    const showAll = useCallback(() => fitView({ padding: 0.12, duration: 400 }), [fitView]);

    // 小地图：照 React Flow 的惯例放右下角，但缩小一圈，并且可以收起（控制栏里的地图按钮）。
    // 宽屏默认展开，手机默认收起——390 宽的屏上它会正好压住右边那一列目标卡片。
    const isWide = useIsWideScreen();
    const [minimapPref, setMinimapPref] = useState<boolean | null>(null);
    const showMinimap = minimapPref ?? isWide;

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

    return (
        <section className="flex shrink-0 flex-col overflow-hidden rounded-2xl border border-border/70 bg-background/70 sm:min-h-0 sm:flex-1 sm:shrink">
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
                                title={t('routes.batchButtonHint')}
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
                            {t('routes.viewAll')}
                        </button>
                        <button
                            type="button"
                            className={cn(
                                'h-6 rounded-full px-2.5 text-xs font-medium transition-colors',
                                viewMode === 'channel' ? 'bg-primary text-primary-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground'
                            )}
                            onClick={() => setViewMode('channel')}
                        >
                            {t('routes.viewByChannel')}
                        </button>
                        <button
                            type="button"
                            className={cn(
                                'h-6 rounded-full px-2.5 text-xs font-medium transition-colors',
                                viewMode === 'mapping' ? 'bg-primary text-primary-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground'
                            )}
                            onClick={() => setViewMode('mapping')}
                        >
                            {t('routes.viewByModel')}
                        </button>
                    </div>

                    {viewMode === 'channel' && (
                        <select
                            value={selectedChannelId}
                            onChange={(e) => setSelectedChannelId(e.target.value)}
                            aria-label={t('routes.filterChannel')}
                            className="h-7 rounded-full border border-border/70 bg-background/60 px-2.5 text-xs text-foreground outline-none focus:border-primary/50 max-w-[160px]"
                        >
                            <option value="">{t('routes.filterChannelAll')}</option>
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
                            aria-label={t('routes.filterModel')}
                            className="h-7 rounded-full border border-border/70 bg-background/60 px-2.5 text-xs text-foreground outline-none focus:border-primary/50 max-w-[160px]"
                        >
                            <option value="">{t('routes.filterModelAll')}</option>
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
                            placeholder={t('routes.searchPlaceholder')}
                            className="h-7 w-full rounded-full border border-border/70 bg-background/60 pl-7 pr-2 text-xs text-foreground outline-none focus:border-primary/50 sm:w-48"
                        />
                    </div>
                    <Badge variant="secondary" className="rounded-full">{planTitle(plan)}</Badge>
                    <Badge variant="outline" className="rounded-full">{t('routes.targetCount', { count: targetCount })}</Badge>
                </div>
            </div>

            {filteredRows.length === 0 && filteredMappings.length === 0 ? (
                <div className="px-4 py-8 text-center text-sm text-muted-foreground">
                    {rows.length === 0 ? (
                        t('routes.canvasEmpty')
                    ) : (
                        t('routes.filterEmpty')
                    )}
                </div>
            ) : (
                <div className={cn(
                    canvasFullscreen
                        ? 'fixed inset-0 z-50 h-[100dvh] w-screen bg-background p-2 sm:p-4'
                        // 手机：整页一起滚，画布本身拿到「视口减去顶栏和底部导航」那么高（390×844 上约 590px），
                        // 往上一滑画布就占满屏；不再挤在上下两块之间只剩 300px。宽屏照旧。
                        : 'h-[max(360px,calc(100dvh-16rem))] shrink-0 sm:min-h-0 sm:flex-1 sm:shrink sm:h-[clamp(340px,calc(100dvh-22rem),860px)]',
                    'transition-[height]',
                )}>
                    <ReactFlow<FlowNode, Edge>
                        nodes={nodes}
                        edges={edges}
                        onNodesChange={onNodesChange}
                        onEdgesChange={onEdgesChange}
                        nodeTypes={flowNodeTypes}
                        colorMode="system"
                        onMoveEnd={rememberViewport}
                        defaultViewport={initialViewport}
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
                        <Controls showInteractive={false} showFitView={false}>
                            <ControlButton
                                onClick={() => toggleFullscreen()}
                                title={canvasFullscreen ? t('routes.exitFullscreen') : t('routes.fullscreen')}
                                aria-label={canvasFullscreen ? t('routes.exitFullscreen') : t('routes.fullscreen')}
                            >
                                {canvasFullscreen
                                    ? <Minimize2 style={{ width: 14, height: 14 }} />
                                    : <Maximize2 style={{ width: 14, height: 14 }} />}
                            </ControlButton>
                            <ControlButton
                                onClick={() => setMinimapPref(!showMinimap)}
                                title={showMinimap ? t('routes.minimapHide') : t('routes.minimapShow')}
                                aria-label={showMinimap ? t('routes.minimapHide') : t('routes.minimapShow')}
                                aria-pressed={showMinimap}
                                data-testid="minimap-toggle"
                            >
                                <MapIcon style={{ width: 14, height: 14 }} />
                            </ControlButton>
                        </Controls>
                        {showMinimap ? (
                            <MiniMap
                                pannable
                                zoomable
                                position="bottom-right"
                                onClick={(_event, position) => setCenter(position.x, position.y, { zoom: 1, duration: 400 })}
                                nodeColor={miniMapNodeColor}
                                nodeStrokeWidth={2}
                                ariaLabel={t('routes.minimap')}
                                // 默认 200×150 太占地方；缩到 150×100（手机 120×80），贴边留 8px。
                                style={isWide ? { width: 150, height: 100, margin: 8 } : { width: 120, height: 80, margin: 6 }}
                                className="!rounded-lg !border !border-border/70 !bg-background/80 opacity-90 hover:opacity-100"
                            />
                        ) : null}
                        <Panel position="top-right" className="flex gap-1.5">
                            <button
                                type="button"
                                onClick={showAll}
                                className="rounded-full border border-border/70 bg-background/85 px-2.5 py-1 text-[11px] font-medium text-foreground shadow-sm backdrop-blur hover:bg-muted"
                            >
                                {t('routes.fitAll')}
                            </button>
                            <button
                                type="button"
                                onClick={focusReadable}
                                className="rounded-full border border-border/70 bg-background/85 px-2.5 py-1 text-[11px] font-medium text-foreground shadow-sm backdrop-blur hover:bg-muted"
                            >
                                {t('routes.zoomReadable')}
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

/**
 * 编辑弹窗里的一行。渠道、实际模型只读：渠道一有变化（启用 / 停用 / 改模型）后端就会自动同步
 * （op.AccessPlanSyncEnabledChannels），在这里改渠道、改实际模型、删掉一行，下次同步都会被改回来
 * （删掉的还会以「开着」的状态加回来），所以只留真正存得住的：先后（拖动 / 数字）、开关、到顶。
 */
function RouteTargetEditorCard({
    target,
    index,
    peerTargets,
    channelName,
    updateTarget,
    moveTargetToTop,
    leading,
    dragging = false,
}: {
    target: AccessPlanRouteTarget;
    index: number;
    /** 同一条规则（同 request_model）的全部候选，用来判断是否已经最先用。 */
    peerTargets?: AccessPlanRouteTarget[];
    channelName: string;
    updateTarget: (index: number, patch: Partial<AccessPlanRouteTarget>) => void;
    moveTargetToTop?: (index: number) => void;
    /** 行首：优先 = 拖动手柄 + 第 N 个；轮询 = 优先级步进器。 */
    leading?: React.ReactNode;
    dragging?: boolean;
}) {
    const t = accessPlanText;
    const rowMode = normalizeRouteMode(target.mode);
    const tieredMode = simpleModeOf(rowMode) === 'rotate';
    const tierPeers = peerTargets && peerTargets.length > 0 ? peerTargets : [target];
    const atTopTier = tieredMode && isRouteTargetAtTopTier(target, tierPeers);
    const firstInOrder = !tieredMode && sortByTryOrder(tierPeers, rowMode)[0] === target;
    const moveToTopDisabled = !moveTargetToTop
        || cleanOneMillionModelName(target.request_model).length === 0
        || atTopTier
        || firstInOrder;
    const moveToTopTitle = tieredMode
        ? (atTopTier ? t('routes.moveToTopAtTop') : t('routes.moveToTopTieredHint'))
        : t('routes.moveToTopFillFirstHint');
    const moveToTopLabel = tieredMode ? t('routes.moveToTopTiered') : t('routes.moveToTopFillFirst');
    const upstream = cleanOneMillionModelName(target.upstream_model);

    return (
        <div className={cn(
            'grid min-w-0 grid-cols-[minmax(0,1fr)_auto] items-center gap-x-3 gap-y-2 rounded-xl border border-border/80 bg-muted/15 p-2.5 sm:grid-cols-[auto_minmax(0,1fr)_auto]',
            !target.enabled && 'opacity-70',
            dragging && 'border-primary/50 bg-card shadow-lg',
        )}>
            {leading ? <div className="col-span-2 flex min-w-0 items-center gap-2 sm:col-span-1">{leading}</div> : null}
            <div className="min-w-0">
                <div className="flex min-w-0 items-center gap-1.5">
                    <span className={cn('size-2 shrink-0 rounded-full', target.enabled ? 'bg-emerald-500' : 'bg-muted-foreground')} />
                    <span className="truncate text-sm font-bold text-foreground">{channelName}</span>
                    <span className="shrink-0 text-[11px] text-muted-foreground">#{target.channel_id}</span>
                </div>
                <div className="mt-0.5 truncate text-[11px] text-muted-foreground" title={t('routes.upstreamModelHint')}>
                    {t('routes.upstreamModel')} <span className="font-mono text-foreground/80">{upstream || t('routes.unset')}</span>
                </div>
            </div>
            <div className="flex shrink-0 items-center gap-2">
                <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    className="h-8 shrink-0 gap-1 rounded-xl px-2 text-xs"
                    disabled={moveToTopDisabled}
                    onClick={() => moveTargetToTop?.(index)}
                    title={moveToTopTitle}
                    aria-label={moveToTopLabel}
                >
                    <ArrowUpToLine className="size-3.5" />
                    <span className="hidden md:inline">{moveToTopLabel}</span>
                </Button>
                <label className="flex shrink-0 items-center gap-1.5 text-xs text-muted-foreground" title={t('routes.lineSwitchHint')}>
                    <Switch
                        checked={target.enabled}
                        onCheckedChange={(enabled) => updateTarget(index, { enabled })}
                    />
                    {t('status.enabled')}
                </label>
            </div>
        </div>
    );
}

function RouteModeCards({ value, onChange }: { value: SimpleRouteMode; onChange: (next: SimpleRouteMode) => void }) {
    const t = accessPlanText;
    const options = [
        { key: 'first' as const, icon: ListOrdered, title: t('routes.modeFillFirst'), short: t('routes.modeFillFirstShort'), hint: t('routes.modeFillFirstHint') },
        { key: 'rotate' as const, icon: Repeat, title: t('routes.modeSpread'), short: t('routes.modeSpreadShort'), hint: t('routes.modeSpreadHint') },
    ];
    return (
        <div role="radiogroup" aria-label={t('routes.modeLabel')} className="grid grid-cols-2 gap-2">
            {options.map((option) => {
                const active = value === option.key;
                const Icon = option.icon;
                return (
                    <button
                        key={option.key}
                        type="button"
                        role="radio"
                        aria-checked={active}
                        title={option.hint}
                        onClick={() => onChange(option.key)}
                        className={cn(
                            'flex min-w-0 flex-col items-start gap-1 rounded-2xl border px-3 py-2.5 text-left transition-colors',
                            active
                                ? 'border-primary/60 bg-primary/10 text-foreground ring-1 ring-primary/30'
                                : 'border-border/80 bg-background/60 text-foreground hover:bg-muted/40',
                        )}
                    >
                        <span className="flex items-center gap-1.5 text-sm font-bold">
                            <span className={cn('flex size-4 items-center justify-center rounded-full border', active ? 'border-primary bg-primary text-primary-foreground' : 'border-muted-foreground/40')}>
                                {active ? <Check className="size-3" /> : null}
                            </span>
                            <Icon className={cn('size-4', active ? 'text-primary' : 'text-muted-foreground')} />
                            {option.title}
                        </span>
                        <span className="text-xs leading-snug text-muted-foreground">{option.short}</span>
                    </button>
                );
            })}
        </div>
    );
}

/** 「实际顺序」：一级一级写出来，比如 官方直连 → 都不能用时 → 中转 A、中转 B 轮流。 */
function TryOrderSummary({ levels, nameOf }: { levels: AccessPlanRouteTarget[][]; nameOf: (channelId: number) => string }) {
    const t = accessPlanText;
    return (
        <div className="rounded-2xl border border-border/70 bg-background/40 px-3 py-2.5">
            <div className="text-xs font-medium text-muted-foreground">{t('routes.tryOrder')}</div>
            {levels.length === 0 ? (
                <div className="mt-1 text-sm text-muted-foreground">{t('routes.tryOrderNone')}</div>
            ) : (
                <ol className="mt-1.5 grid gap-1">
                    {levels.map((level, index) => (
                        <li key={index} className="grid gap-1">
                            {index > 0 ? <span className="pl-7 text-[11px] text-muted-foreground">↓ {t('routes.tryOrderThen')}</span> : null}
                            <span className="flex min-w-0 items-center gap-2 text-sm">
                                <span className={cn('flex size-5 shrink-0 items-center justify-center rounded-full text-[11px] font-bold', level.length > 1 ? 'bg-emerald-500/15 text-emerald-700 dark:text-emerald-300' : 'bg-primary/12 text-primary')}>{index + 1}</span>
                                <span className="min-w-0 truncate font-semibold text-foreground">{level.map((target) => nameOf(target.channel_id)).join('、')}</span>
                                {level.length > 1 ? (
                                    <span className="inline-flex shrink-0 items-center gap-0.5 rounded-full bg-emerald-500/12 px-1.5 py-0.5 text-[11px] font-semibold text-emerald-700 dark:text-emerald-300">
                                        <Repeat className="size-3" />{t('routes.tryOrderRotate')}
                                    </span>
                                ) : null}
                            </span>
                        </li>
                    ))}
                </ol>
            )}
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
    // baseline = 最近一次和服务器对齐的 targets（列表回刷或保存回包）。本地和它不一样 = 有没保存的改动。
    const [baseline, setBaseline] = useState<AccessPlanRouteTarget[]>(() => plan.route_targets.map(cleanRouteTargetModels));
    // 保存排队里还有活（在飞或排着）。比 updateRoutes.isPending 准：那个只看最后一次 mutate。
    const [saveBusy, setSaveBusy] = useState(false);
    const localDirty = useMemo(() => !sameDraft(targets, baseline), [targets, baseline]);
    // 服务器数据变了就以服务器为准重置本地 targets（见 routeTargetsServerVersion 的说明：
    // 父级 key 里的 updated_at 后端从不返回，原来那套刷新从未生效）。原地同步，画布节点按同 id 更新、视口不动。
    // 保存在路上或本地有没保存的改动时不重置，免得列表回刷把正在改的东西冲掉。
    const serverVersion = useMemo(() => routeTargetsServerVersion(plan), [plan]);
    const [syncedServerVersion, setSyncedServerVersion] = useState(serverVersion);
    if (shouldResyncRouteTargets(syncedServerVersion, serverVersion, saveBusy, localDirty)) {
        const fresh = plan.route_targets.map(cleanRouteTargetModels);
        setSyncedServerVersion(serverVersion);
        setTargets(fresh);
        setBaseline(fresh);
    }
    const [jsonText, setJsonText] = useState('');
    const [editingRequestModel, setEditingRequestModel] = useState<string | null>(null);
    // 弹窗打开那一刻的 targets：关窗时拿它判断改没改，选「不保存」时退回它。
    const [dialogSnapshot, setDialogSnapshot] = useState<AccessPlanRouteTarget[] | null>(null);
    const [confirmCloseOpen, setConfirmCloseOpen] = useState(false);
    // 轮询里改数字时行不跟着跳：第一次改数字时记下顺序，之后按它排（新加的放最后）；重新打开 / 切换方式时清掉。
    const [dialogOrder, setDialogOrder] = useState<string[] | null>(null);
    const [draggingLine, setDraggingLine] = useState(false);
    const requestModelListId = `access-plan-request-models-${plan.id}`;
    const channelModelIndex = useMemo(
        () => buildChannelModelIndex(channelModels, channelModelsReady),
        [channelModels, channelModelsReady]
    );
    const editableTargets = useMemo(() => activeRouteTargets(targets, channelModelIndex), [targets, channelModelIndex]);
    // 画布展示全部映射（含已失效的），失效项在渲染层打「已失效」标，不再静默隐藏。
    const sortedCanvasRows = useMemo(() => groupedRouteTargets(targets), [targets]);
    // 点 +/− 期间卡片先不换位（见 edit-state.ts）：松手、停手一会儿、保存落定后再按新数字重排。
    const [frozenOrder, setFrozenOrder] = useState<FrozenOrder | null>(null);
    const [lastPriorityChangeAt, setLastPriorityChangeAt] = useState(0);
    const [priorityHover, setPriorityHover] = useState(false);
    const canvasRows = useMemo(() => applyFrozenOrder(sortedCanvasRows, frozenOrder), [sortedCanvasRows, frozenOrder]);
    const canvasRowsRef = useRef(canvasRows);
    canvasRowsRef.current = canvasRows;
    useEffect(() => {
        if (!frozenOrder || saveBusy || priorityHover) return;
        const wait = Math.max(0, FROZEN_ORDER_IDLE_MS - (Date.now() - lastPriorityChangeAt));
        const timer = setTimeout(() => {
            if (shouldReleaseFrozenOrder({ now: Date.now(), lastChangeAt: lastPriorityChangeAt, saving: false, hovering: false })) {
                setFrozenOrder(null);
            }
        }, wait + 16);
        return () => clearTimeout(timer);
    }, [frozenOrder, saveBusy, priorityHover, lastPriorityChangeAt]);
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

    const openRequestEditor = useCallback((requestModel: string) => {
        setDialogSnapshot(targetsRef.current);
        setDialogOrder(null);
        setEditingRequestModel(requestModel);
    }, []);

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
        restoreOnError = false,
    ) => {
        // 按 request_model 分组归一化：
        // 1. 同一 request_model 下 targets 统一 mode（以组内首个明确设置的 mode 为准，默认 fill_first 即 mode=3）
        // 2. 纯轮询 (mode=1)：全部 priority=1, priority_overridden=false（选路本来就不看数字）
        // 3. 分层轮询 (mode=6)：保留每个 target 的数字（≥1）并标 priority_overridden=true，
        //    数字越大越先尝试；保存时绝不清零，否则画布上的 +/− 就成了假按钮。
        // 4. fill_first (mode=3 优先填充)：这是人工排序保存，必须同 rule 所有 target 无条件 priority_overridden=true，按分配的 priority (1..N) 保存
        const groups = new Map<string, AccessPlanRouteTarget[]>();
        nextTargets.forEach((target) => {
            const key = cleanOneMillionModelName(target.request_model).toLowerCase();
            const group = groups.get(key) ?? [];
            group.push(target);
            groups.set(key, group);
        });

        const normalizedTargets: AccessPlanRouteTarget[] = [];
        groups.forEach((groupTargets) => {
            const rowMode = normalizeRouteMode(groupTargets.find((t) => t.mode !== undefined)?.mode);
            if (rowMode === 1 || rowMode === SPREAD_TIERED_MODE) {
                const tiered = rowMode === SPREAD_TIERED_MODE;
                groupTargets.forEach((target) => {
                    normalizedTargets.push({
                        ...target,
                        mode: rowMode,
                        priority: tiered ? clampRoutePriority(target.priority) : 1,
                        priority_overridden: tiered,
                    });
                });
            } else {
                // 优先填充：人工排序保存，无条件 priority_overridden=true
                groupTargets.forEach((target, i) => {
                    const assignedPriority = Number.isFinite(target.priority)
                        ? clampRoutePriority(target.priority)
                        : clampRoutePriority(i + 1);
                    normalizedTargets.push({
                        ...target,
                        mode: 3,
                        priority: assignedPriority,
                        priority_overridden: true,
                    });
                });
            }
        });

        saveQueue.submit({
            sourceTargets: nextTargets,
            successMessage,
            onSuccess,
            restoreOnError,
            payload: {
                access_plan_id: plan.id,
                targets: normalizedTargets.map((target, index) => ({
                    ...target,
                    request_model: cleanOneMillionModelName(target.request_model),
                    upstream_model: cleanOneMillionModelName(target.upstream_model),
                    priority: Number.isFinite(target.priority) ? clampRoutePriority(target.priority) : clampRoutePriority(index + 1),
                    weight: 1,
                    enabled: target.enabled,
                    billing_model_source: target.billing_model_source ?? 'request_model',
                    billing_model_override: cleanOneMillionModelName(target.billing_model_override ?? '') || undefined,
                    mode: isSpreadMode(target.mode) ? spreadModeValue(target.mode) : 3,
                    priority_overridden: target.priority_overridden,
                    fallback_mode: target.fallback_mode ?? 'return_group',
                    system_prompt_override: target.system_prompt_override?.trim() || undefined,
                    prompt_override_mode: normalizePromptMode(target.prompt_override_mode),
                    prompt_override_enabled: promptEnabledFromSaved(target.prompt_override_enabled, target.prompt_override_mode, target.system_prompt_override),
                })),
            },
        });
    };

    // 保存排队：同一时间只发一个，排着的只留最新整表；只有最后一次的结果覆盖本地（见 edit-state.ts）。
    const mutateRoutesRef = useRef(updateRoutes.mutateAsync);
    mutateRoutesRef.current = updateRoutes.mutateAsync;
    const settleSaveRef = useRef<(outcome: SaveOutcome<RouteSaveJob, AccessPlanRouteSaveResult>) => void>(() => {});
    const [saveQueue] = useState(() => createLatestSaveQueue<RouteSaveJob, AccessPlanRouteSaveResult>(
        (job) => mutateRoutesRef.current(job.payload),
        (outcome) => settleSaveRef.current(outcome),
        setSaveBusy,
    ));
    settleSaveRef.current = (outcome) => {
        // 后面还有更新的整表在排队：这次的结果不覆盖本地，也不提示，等最后一次。
        if (outcome.superseded) return;
        const job = outcome.payload;
        // 发出去之后本地又改了（比如保存在路上时打开弹窗改了东西）：只更新 baseline，不冲掉本地。
        const untouchedSince = sameDraft(targetsRef.current, job.sourceTargets);
        if (outcome.ok) {
            const fresh = (outcome.value.route_targets ?? []).map(cleanRouteTargetModels);
            setBaseline(fresh);
            setSyncedServerVersion(routeTargetsServerVersion({ id: plan.id, route_targets: outcome.value.route_targets ?? [] }));
            if (untouchedSince) {
                setTargets(fresh);
                setDialogSnapshot((snapshot) => (snapshot ? fresh : snapshot));
            }
            toast.success(job.successMessage);
            job.onSuccess?.();
            return;
        }
        if (job.restoreOnError && untouchedSince) setTargets(baselineRef.current);
        toast.error(t('toast.routesUpdateFailed'), { description: apiErrorMessage(outcome.error) });
    };
    const baselineRef = useRef(baseline);
    baselineRef.current = baseline;

    const saveTargetsRef = useRef(saveTargets);
    saveTargetsRef.current = saveTargets;
    const channelModelIndexRef = useRef(channelModelIndex);
    channelModelIndexRef.current = channelModelIndex;
    const targetsRef = useRef(targets);
    targetsRef.current = targets;

    // 画布节点上的 +/−：
    // - 轮询：改这个渠道的优先级数字；整条规则升级成 mode 6（数字真正起作用，见 route-mode.ts）。
    // - 优先：和上一个 / 下一个换位置，重编 1..N（原版是 ±1 改顺位值，会和邻居撞成同一个数字，
    //   撞了以后按渠道 ID 定先后，点了 ＋ 经常看不出变化）。
    const changeCanvasPriority = useCallback((targetId: number, nextPriorityRaw: number) => {
        const current = targetsRef.current;
        const index = current.findIndex((target) => target.id === targetId);
        if (index < 0) return;
        const selected = current[index];
        const next = mapRule(current, selected.request_model, (group) => {
            const mode = normalizeRouteMode(group.find((target) => target.mode !== undefined)?.mode);
            const own = group.indexOf(selected);
            if (simpleModeOf(mode) === 'rotate') {
                const nextPriority = clampRoutePriority(nextPriorityRaw);
                if (displayPriority(selected, mode) === nextPriority) return group;
                return setRotatePriority(group, own, nextPriority);
            }
            const ordered = sortByTryOrder(group, mode);
            const at = ordered.indexOf(selected);
            const delta = nextPriorityRaw < clampRoutePriority(selected.priority) ? -1 : 1;
            const to = at + delta;
            if (to < 0 || to >= ordered.length) return group;
            return renumberFirst(moveItem(ordered, at, to));
        });
        if (next === current || sameDraft(next, current)) return;
        setFrozenOrder((frozen) => frozen ?? captureOrder(canvasRowsRef.current));
        setLastPriorityChangeAt(Date.now());
        setTargets(next);
        saveTargetsRef.current(
            next,
            t('toast.routesUpdated'),
            undefined,
            true,
        );
    }, [t]);

    const moveEditorTargetToTop = useCallback((targetIndex: number) => {
        const current = targetsRef.current;
        const next = moveRouteTargetToTop(current, targetIndex);
        setTargets(next);
        saveTargetsRef.current(next, t('toast.routesUpdated'), undefined, true);
    }, [t]);

    const save = (onSuccess?: () => void) => saveTargets(targets, t('toast.routesUpdated'), onSuccess);

    const closeRequestEditor = () => {
        setConfirmCloseOpen(false);
        setEditingRequestModel(null);
        setDialogSnapshot(null);
        setDialogOrder(null);
        setDraggingLine(false);
    };

    // ── 编辑弹窗：分配方式 / 顺序 / 数字 —— 都是规则级的，改的是整条规则 ──
    const editingMode = normalizeRouteMode(editingTargets.find((target) => target.mode !== undefined)?.mode);
    const editingSimpleMode = simpleModeOf(editingMode);
    const lineKey = (target: AccessPlanRouteTarget) => (target.id !== undefined ? `id:${target.id}` : `ch:${target.channel_id}:${target.upstream_model}`);
    const editingLines = (() => {
        const ordered = sortByTryOrder(editingTargets, editingMode);
        if (editingSimpleMode === 'first' || !dialogOrder) return ordered;
        const rank = new Map(dialogOrder.map((key, index) => [key, index]));
        return [...ordered].sort((a, b) => (rank.get(lineKey(a)) ?? Infinity) - (rank.get(lineKey(b)) ?? Infinity));
    })();
    const editingLinesRef = useRef(editingLines);
    editingLinesRef.current = editingLines;
    const updateEditingRule = (fn: (group: AccessPlanRouteTarget[]) => AccessPlanRouteTarget[]) => {
        if (editingRequestModel === null) return;
        setTargets((current) => mapRule(current, editingRequestModel, fn));
    };
    const setEditingMode = (next: SimpleRouteMode) => {
        if (next === editingSimpleMode) return;
        setDialogOrder(null);
        updateEditingRule((group) => (next === 'first' ? switchToFirst(group) : switchToRotate(group)));
    };
    const setEditingPriority = (target: AccessPlanRouteTarget, priority: number) => {
        if (editingSimpleMode !== 'rotate') return;
        setDialogOrder((order) => order ?? editingLinesRef.current.map(lineKey));
        updateEditingRule((group) => {
            const own = group.findIndex((item) => lineKey(item) === lineKey(target));
            return own < 0 ? group : setRotatePriority(group, own, priority);
        });
    };
    const onLineDragEnd = (result: DropResult) => {
        setDraggingLine(false);
        if (!result.destination || result.destination.index === result.source.index) return;
        const orderedKeys = editingLinesRef.current.map(lineKey);
        const movedKeys = moveItem(orderedKeys, result.source.index, result.destination.index);
        updateEditingRule((group) => {
            const byKey = new Map(group.map((target) => [lineKey(target), target]));
            const ordered = movedKeys.map((key) => byKey.get(key)).filter((target): target is AccessPlanRouteTarget => Boolean(target));
            const renumbered = renumberFirst(ordered);
            const next = new Map(renumbered.map((target) => [lineKey(target), target]));
            return group.map((target) => next.get(lineKey(target)) ?? target);
        });
    };
    const editingRuleHead = editingTargets[0];
    const editingRulePromptEnabled = editingRuleHead
        ? promptEnabledFromSaved(editingRuleHead.prompt_override_enabled, editingRuleHead.prompt_override_mode, editingRuleHead.system_prompt_override)
        : false;
    const setEditingRuleField = (patch: Partial<AccessPlanRouteTarget>) => updateEditingRule((group) => group.map((target) => ({ ...target, ...patch })));
    const channelNameOf = (channelId: number) => channels.find((channel) => channel.id === channelId)?.name ?? t('routes.missingChannel', { id: channelId });

    const dialogDirty = editingRequestModel !== null && dialogSnapshot !== null && !sameDraft(targets, dialogSnapshot);
    const requestCloseEditor = () => {
        if (decideDialogClose(dialogDirty) === 'confirm') {
            setConfirmCloseOpen(true);
            return;
        }
        closeRequestEditor();
    };
    const discardRequestEditor = () => {
        if (dialogSnapshot) setTargets(dialogSnapshot);
        closeRequestEditor();
    };

    // 分流模式只在「高级编辑」的分流模式下拉里改（updateTarget 会把同一条 request_model 的
    // 所有 target 一起同步，因为 mode 是规则级字段）。旧的画布行内下拉是死代码，已删除。
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
            mode: isSpreadMode(target.mode) ? spreadModeValue(target.mode) : 3,
            fallback_mode: target.fallback_mode ?? 'return_group',
            prompt_override_enabled: promptEnabledFromSaved(target.prompt_override_enabled, target.prompt_override_mode, target.system_prompt_override),
            prompt_override_mode: normalizePromptMode(target.prompt_override_mode),
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
                mode: Number(target.mode) === 1 ? 1 : (Number(target.mode) === SPREAD_TIERED_MODE ? SPREAD_TIERED_MODE : 3),
                fallback_mode: target.fallback_mode ?? 'return_group',
                system_prompt_override: target.system_prompt_override?.trim() || undefined,
                prompt_override_mode: normalizePromptMode(target.prompt_override_mode),
                prompt_override_enabled: promptEnabledFromSaved(target.prompt_override_enabled, target.prompt_override_mode, target.system_prompt_override),
            }));
            setTargets(nextTargets);
            toast.success(t('toast.jsonApplied'));
        } catch (error) {
            toast.error(t('toast.jsonInvalid'), { description: error instanceof Error ? error.message : String(error) });
        }
    };

    return (
        <div className="flex flex-none flex-col gap-3 pr-1 pb-24 sm:min-h-0 sm:flex-1 sm:overflow-y-auto md:pb-4">
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
                        className="rounded-xl"
                        disabled={invalid || saveBusy}
                        onClick={() => save()}
                    >
                        {saveBusy ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}
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
                onEditRequest={openRequestEditor}
                onPriorityChange={changeCanvasPriority}
                onPriorityHover={setPriorityHover}
                onOpenJson={openJson}
            />

            <details ref={advancedRef} className="shrink-0 rounded-2xl border border-border/70 bg-card/70">
                <summary className="flex cursor-pointer list-none items-center justify-between gap-3 px-3 py-2 text-sm font-medium text-foreground">
                    <span className="min-w-0 truncate">{t('routes.batchTitle')}</span>
                    <span className="min-w-0 truncate text-xs font-normal text-muted-foreground">{t('routes.batchHint')}</span>
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

            <Dialog open={editingRequestModel !== null} onOpenChange={(open) => { if (!open) requestCloseEditor(); }}>
                {/* 弹窗不用 translate 居中（inset-0 + m-auto + h-fit）：@hello-pangea/dnd 拖动时用 position:fixed，
                    祖先有 transform 会让拖着的行偏到别处；也不用 renderClone —— 它会卸载原节点，触屏就拖不动了。 */}
                <DialogContent className="top-0 right-0 bottom-0 left-0 m-auto flex h-fit max-h-[calc(100dvh-1rem)] w-[calc(100vw-1rem)] max-w-full translate-none flex-col gap-0 overflow-hidden rounded-3xl border-border bg-card p-0 sm:max-h-[calc(100dvh-3rem)] sm:max-w-[min(72rem,calc(100vw-3rem))]">
                    <DialogHeader className="shrink-0 border-b border-border px-4 py-4 pr-12 sm:px-5">
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
                    <div className="grid min-h-0 flex-1 gap-4 overflow-y-auto px-4 py-3 sm:px-5 lg:grid-cols-[minmax(17rem,20rem)_minmax(0,1fr)]">
                        <div className="grid min-w-0 content-start gap-3">
                            <div className="grid gap-1.5">
                                <div className="text-xs font-medium text-muted-foreground">{t('routes.modeLabel')}</div>
                                <RouteModeCards value={editingSimpleMode} onChange={setEditingMode} />
                            </div>
                            <TryOrderSummary levels={tryLevels(editingTargets, editingMode)} nameOf={channelNameOf} />
                            {editingRuleHead ? (
                                <details
                                    className="rounded-2xl border border-border/70 bg-background/40"
                                    open={(editingRuleHead.system_prompt_override ?? '').trim().length > 0 || editingRuleHead.fallback_mode === 'none' ? true : undefined}
                                >
                                    <summary className="flex cursor-pointer list-none items-center justify-between gap-2 px-3 py-2.5 text-sm font-medium [&::-webkit-details-marker]:hidden">
                                        <span>{t('routes.advancedTitle')}</span>
                                        <span className="truncate text-xs font-normal text-muted-foreground">{t('routes.advancedHint')}</span>
                                    </summary>
                                    <div className="grid gap-2 border-t border-border/60 p-3">
                                        <label className="grid min-w-0 gap-1 text-xs text-muted-foreground">
                                            {t('routes.fallback')}
                                            <select
                                                value={editingRuleHead.fallback_mode === 'none' ? 'none' : 'return_group'}
                                                onChange={(event) => setEditingRuleField({ fallback_mode: event.target.value as AccessPlanRouteTarget['fallback_mode'] })}
                                                aria-label={t('routes.fallback')}
                                                title={t(editingRuleHead.fallback_mode === 'none' ? 'routes.fallbackNoneHint' : 'routes.fallbackReturnGroupHint')}
                                                className="h-9 w-full min-w-0 rounded-xl border border-input bg-background px-3 text-sm text-foreground"
                                            >
                                                <option value="return_group">{t('routes.fallbackReturnGroup')}</option>
                                                <option value="none">{t('routes.fallbackNone')}</option>
                                            </select>
                                        </label>
                                        <div className="grid min-w-0 gap-1 text-xs text-muted-foreground">
                                            {t('routes.promptOverride')}
                                            <PromptLayerControls
                                                enabled={editingRulePromptEnabled}
                                                onEnabledChange={(value) => setEditingRuleField({ prompt_override_enabled: value, prompt_override_mode: normalizePromptMode(editingRuleHead.prompt_override_mode) })}
                                                mode={editingRuleHead.prompt_override_mode}
                                                onModeChange={(value) => setEditingRuleField({ prompt_override_mode: value, prompt_override_enabled: editingRulePromptEnabled })}
                                                label={t('routes.promptOverride')}
                                            />
                                        </div>
                                        <textarea
                                            value={editingRuleHead.system_prompt_override ?? ''}
                                            onChange={(event) => setEditingRuleField({ system_prompt_override: event.target.value })}
                                            placeholder={t('routes.promptPlaceholder')}
                                            disabled={!editingRulePromptEnabled}
                                            title={editingRulePromptEnabled ? undefined : '已关闭，文字保留；打开「启用」后才能改、才会生效。'}
                                            className={cn(
                                                'min-h-16 w-full min-w-0 rounded-xl border border-input bg-background px-3 py-2 text-sm text-foreground transition-opacity focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring',
                                                !editingRulePromptEnabled && 'bg-muted/40 text-muted-foreground opacity-60'
                                            )}
                                        />
                                    </div>
                                </details>
                            ) : null}
                        </div>
                        <div className="grid min-w-0 content-start gap-2">
                            <div className="flex items-center justify-between gap-2 text-xs text-muted-foreground">
                                <span className="font-medium">{t('routes.targetSummary')}</span>
                                <span className="truncate">{editingSimpleMode === 'first' ? t('routes.dragHint') : t('routes.priorityInputHint')}</span>
                            </div>
                        {editingRequestModel && (() => {
                            const currentChannelIds = new Set(editingTargets.map((t) => t.channel_id));
                            const reqKey = cleanOneMillionModelName(editingRequestModel).toLowerCase();

                            // 预先建立每个渠道在当前 request_model 下的上游模型解析
                            // 规则：
                            // 1. 优先查 channelMappings (key=request model, value=upstream model)，
                            //    若该渠道 mapping 的 fromModel 匹配 reqKey，且该渠道 selected_models 包含 mapped upstream，
                            //    则解析为 mapping value（保留清洗后的显示名）
                            // 2. 否则同名支持：若渠道 selected_models 包含与 reqKey 相同的模型名
                            // 3. 不匹配的渠道不展示
                            const availableChannelsWithUpstream = channels.flatMap((ch) => {
                                if (ch.enabled === false || currentChannelIds.has(ch.id)) return [];
                                const chModels = modelsForChannel(ch.id);

                                // 检查是否有 mapping 匹配
                                const matchedMapping = channelMappings.find(
                                    (m) => m.channelId === ch.id && cleanOneMillionModelName(m.fromModel).toLowerCase() === reqKey
                                );

                                if (matchedMapping) {
                                    const mappedUpstream = cleanOneMillionModelName(matchedMapping.toModel);
                                    const mappedKey = mappedUpstream.toLowerCase();
                                    const foundModel = chModels.find((m) => m.toLowerCase() === mappedKey);
                                    if (foundModel) {
                                        return [{ channel: ch, upstreamModel: foundModel }];
                                    }
                                }

                                // identity 同名匹配
                                const identityModel = chModels.find((m) => m.toLowerCase() === reqKey);
                                if (identityModel) {
                                    return [{ channel: ch, upstreamModel: identityModel }];
                                }
                                return [];
                            });

                            if (availableChannelsWithUpstream.length === 0) return null;
                            return (
                                <div className="flex items-center gap-2 rounded-xl border border-dashed border-border/80 bg-muted/20 p-2 text-xs">
                                    <span className="shrink-0 text-muted-foreground">{t('routes.addChannel')}</span>
                                    <select
                                        value=""
                                        onChange={(e) => {
                                            const channelId = Number(e.target.value);
                                            if (!channelId) return;
                                            const match = availableChannelsWithUpstream.find((item) => item.channel.id === channelId);
                                            if (!match) return;
                                            const groupMode = editingMode;
                                            const maxPriority = editingTargets.reduce((max, t) => Math.max(max, t.priority ?? 0), 0);
                                            // 轮询家族新渠道进「最低层」（数字 1）：不抢现有分层；分层轮询保持 6，
                                            // 否则列表里加个渠道就把这条规则悄悄降级回纯轮询。
                                            const spreadRow = isSpreadMode(groupMode);
                                            const rowMode = spreadRow ? spreadModeValue(groupMode) : 3;
                                            setTargets((current) => [
                                                ...current,
                                                {
                                                    request_model: editingRequestModel,
                                                    channel_id: channelId,
                                                    upstream_model: match.upstreamModel,
                                                    priority: spreadRow ? 1 : Math.max(1, maxPriority + 1),
                                                    weight: 1,
                                                    enabled: true,
                                                    billing_model_source: 'request_model',
                                                    mode: rowMode,
                                                    priority_overridden: rowMode !== 1,
                                                    fallback_mode: 'return_group',
                                                    system_prompt_override: '',
                                                    prompt_override_mode: 'before',
                                                    prompt_override_enabled: false,
                                                },
                                            ]);
                                        }}
                                        className="h-7 min-w-36 rounded-lg border border-border/70 bg-background px-2 text-xs text-foreground outline-none focus:border-primary/50"
                                    >
                                        <option value="">{t('routes.addChannelPlaceholder')}</option>
                                        {availableChannelsWithUpstream.map(({ channel: ch }) => (
                                            <option key={ch.id} value={ch.id}>
                                                {ch.name} (#{ch.id})
                                            </option>
                                        ))}
                                    </select>
                                </div>
                            );
                        })()}
                        {editingLines.length === 0 ? (
                            <div className="rounded-xl border border-dashed border-border px-4 py-6 text-center text-sm text-muted-foreground">
                                {t('routes.empty')}
                            </div>
                        ) : (
                            <DragDropContext onBeforeCapture={() => setDraggingLine(true)} onDragEnd={onLineDragEnd}>
                                <Droppable droppableId="lines" isDropDisabled={editingSimpleMode !== 'first'}>
                                    {(dropProvided) => (
                                        <div ref={dropProvided.innerRef} {...dropProvided.droppableProps} className="grid gap-2">
                                            {editingLines.map((target, lineIndex) => {
                                                const index = targets.indexOf(target);
                                                const shown = displayPriority(target, editingMode);
                                                return (
                                                    <Draggable key={lineKey(target)} draggableId={lineKey(target)} index={lineIndex} isDragDisabled={editingSimpleMode !== 'first'}>
                                                        {(dragProvided, dragSnapshot) => (
                                                            <div ref={dragProvided.innerRef} {...dragProvided.draggableProps}>
                                                                <RouteTargetEditorCard
                                                                    target={target}
                                                                    index={index}
                                                                    peerTargets={editingTargets}
                                                                    channelName={channelNameOf(target.channel_id)}
                                                                    updateTarget={updateTarget}
                                                                    moveTargetToTop={moveEditorTargetToTop}
                                                                    dragging={dragSnapshot.isDragging}
                                                                    leading={editingSimpleMode === 'first' ? (
                                                                        <>
                                                                            <span
                                                                                {...dragProvided.dragHandleProps}
                                                                                aria-label={t('routes.dragHandle')}
                                                                                title={t('routes.dragHandle')}
                                                                                className="flex size-8 shrink-0 cursor-grab touch-none items-center justify-center rounded-lg text-muted-foreground hover:bg-muted active:cursor-grabbing"
                                                                            >
                                                                                <GripVertical className="size-4" />
                                                                            </span>
                                                                            <span className="shrink-0 whitespace-nowrap text-xs font-semibold text-foreground" title={t('routes.orderFillFirstHint', { order: lineIndex + 1 })}>
                                                                                {t('routes.orderLabel', { order: lineIndex + 1 })}
                                                                            </span>
                                                                        </>
                                                                    ) : (
                                                                        <span className="inline-flex shrink-0 items-center gap-1" title={t('routes.priorityInputHint')}>
                                                                            <span className="whitespace-nowrap text-xs font-medium text-muted-foreground">{t('routes.priorityLabel')}</span>
                                                                            <button
                                                                                type="button"
                                                                                className="flex size-7 items-center justify-center rounded-lg border border-border/70 bg-background text-sm hover:bg-muted disabled:opacity-40"
                                                                                disabled={shown <= 1}
                                                                                aria-label={t('routes.tierDown')}
                                                                                title={t('routes.tierDown')}
                                                                                onClick={() => setEditingPriority(target, shown - 1)}
                                                                            >−</button>
                                                                            <Input
                                                                                type="number"
                                                                                inputMode="numeric"
                                                                                min={1}
                                                                                value={shown}
                                                                                aria-label={t('routes.priorityLabel')}
                                                                                onChange={(event) => setEditingPriority(target, Number(event.target.value))}
                                                                                className="h-7 w-12 rounded-lg px-1 text-center text-sm font-semibold [appearance:textfield] [&::-webkit-inner-spin-button]:appearance-none [&::-webkit-outer-spin-button]:appearance-none"
                                                                            />
                                                                            <button
                                                                                type="button"
                                                                                className="flex size-7 items-center justify-center rounded-lg border border-border/70 bg-background text-sm hover:bg-muted"
                                                                                aria-label={t('routes.tierUp')}
                                                                                title={t('routes.tierUp')}
                                                                                onClick={() => setEditingPriority(target, shown + 1)}
                                                                            >+</button>
                                                                        </span>
                                                                    )}
                                                                />
                                                            </div>
                                                        )}
                                                    </Draggable>
                                                );
                                            })}
                                            {dropProvided.placeholder}
                                        </div>
                                    )}
                                </Droppable>
                            </DragDropContext>
                        )}
                        </div>
                    </div>
                    <DialogFooter className="shrink-0 flex-col gap-2 border-t border-border px-4 py-3 sm:flex-row sm:px-5">
                        <DialogClose asChild>
                            <Button type="button" variant="outline" className="rounded-xl">
                                <span className="min-w-0 truncate">{t('routes.close')}</span>
                            </Button>
                        </DialogClose>
                        <Button
                            type="button"
                            className="rounded-xl"
                            disabled={invalid || saveBusy || draggingLine}
                            onClick={() => save(closeRequestEditor)}
                        >
                            {saveBusy ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}
                            <span className="min-w-0 truncate">{t('actions.save')}</span>
                        </Button>
                    </DialogFooter>
                </DialogContent>
            </Dialog>

            <UnsavedChangesConfirm
                open={confirmCloseOpen}
                onKeepEditing={() => setConfirmCloseOpen(false)}
                onDiscard={discardRequestEditor}
            />

            <datalist id={requestModelListId}>
                {modelNames.map((model) => <option key={model} value={model} />)}
            </datalist>
        </div>
    );
}

/** 默认只有轮询/优先填充两档；独立设置过模式的规则保持自己的选择。 */
function GlobalRouteModeSelect({ className }: { className?: string }) {
    const { value, update, isPending, isReady } = useRouteModeOverrideSetting();
    if (!ROUTE_MODE_OVERRIDE_VALUES.includes(value as RouteModeOverrideValue)) {
        return <span className={cn('text-xs text-muted-foreground', className)} role="status">{accessPlanText('routes.defaultModeUnavailable')}</span>;
    }

    return (
        <label className={cn('flex min-w-0 items-center gap-2 text-xs text-muted-foreground', className)}>
            <span className="shrink-0 whitespace-nowrap" title={accessPlanText('routes.defaultModeHint')}>{accessPlanText('routes.defaultModeLabel')}</span>
            <select
                value={value}
                onChange={(event) => update(event.target.value as RouteModeOverrideValue)}
                disabled={isPending || !isReady}
                aria-label={accessPlanText('routes.defaultModeLabel')}
                title={accessPlanText('routes.defaultModeHint')}
                className="h-8 min-w-0 rounded-full border border-border/70 bg-background/60 px-2.5 text-xs text-foreground outline-none focus:border-primary/50"
            >
                {ROUTE_MODE_OVERRIDE_VALUES.map((option) => (
                    <option key={option} value={option}>
                        {routeModeOverrideLabel(option)}
                    </option>
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
    const [open, setOpen] = useState(false);
    const [billingDirty, setBillingDirty] = useState(false);
    const [confirmCloseOpen, setConfirmCloseOpen] = useState(false);
    const close = () => {
        setConfirmCloseOpen(false);
        setBillingDirty(false);
        setOpen(false);
    };
    const onOpenChange = (next: boolean) => {
        if (next) {
            setOpen(true);
            return;
        }
        if (decideDialogClose(billingDirty) === 'confirm') {
            setConfirmCloseOpen(true);
            return;
        }
        close();
    };

    return (
        <Dialog open={open} onOpenChange={onOpenChange}>
            <UnsavedChangesConfirm open={confirmCloseOpen} onKeepEditing={() => setConfirmCloseOpen(false)} onDiscard={close} />
            <DialogTrigger asChild>
                <Button type="button" variant="outline" className="rounded-xl">
                    <PencilLine className="size-4" />
                    <span className="min-w-0 truncate">{t('routes.settingsButton')}</span>
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
                    {/* 只按方案 id 换实例：带 updated_at 的 key 会在保存/回刷时整块重挂载，正在改的草稿当场丢掉。
                        服务器数据由编辑器内部在「没改动、没在保存」时原地跟上。 */}
                    <PlanBasicsEditor key={`basic-${plan.id}`} plan={plan} />
                    <BillingRulesEditor
                        key={`billing-${plan.id}`}
                        plan={plan}
                        modelNames={modelNames}
                        onDirtyChange={setBillingDirty}
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
            {/* 手机上整页一起滚（画布才能拿到接近一屏的高度）；宽屏照旧固定、编辑区内部滚。 */}
            <div className="flex h-full min-h-0 flex-col gap-3 overflow-y-auto overscroll-contain sm:overflow-hidden">
                <div className="shrink-0 rounded-2xl border border-border/70 bg-card/70 p-2 shadow-sm">
                    <div className="flex flex-col gap-2 lg:flex-row lg:items-center lg:justify-between">
                        <div className="flex min-w-0 flex-1 flex-col gap-2 sm:flex-row sm:items-center">
                            <span className="shrink-0 px-1 text-xs font-medium text-muted-foreground">{t('routes.planLabel')}</span>
                            <SearchableSelect
                                value={activeSelectedId != null ? String(activeSelectedId) : ''}
                                onValueChange={(value) => setSelectedId(Number(value))}
                                options={orderedPlans.map((plan) => ({
                                    value: String(plan.id),
                                    label: planTitle(plan),
                                    keywords: `${plan.slug ?? ''} ${plan.display_name ?? ''}`,
                                }))}
                                placeholder={orderedPlans.length === 0 ? t('list.empty') : t('list.selectOrCreate')}
                                searchPlaceholder={t('routes.planSearch')}
                                emptyText={t('routes.planSearchEmpty')}
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
                                    key={`settings-${selectedPlan.id}`}
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
                        // 只按方案 id 换实例，绝不能带随保存变化的字段（会重挂载画布、视口跳回 fit-all）。
                        // 服务器数据刷新由 RouteTargetsEditor 内部按路由内容原地同步。
                        key={`routes-${selectedPlan.id}`}
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
