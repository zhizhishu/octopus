import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiClient } from '../client';
import { logger } from '@/lib/logger';
import type { PromptAudience } from './access-plan';

// 提示词规则的 CRUD 客户端。字段与 internal/server/handlers/prompt_rule.go、
// internal/model/prompt_rule.go 的 JSON tag 对齐；仅管理员路由（Auth+AdminOnly）。
// 合并预览走 /preview：后端在本地算命中与合并文本（不发上游请求），前端只展示结果。

/** 位置：before = 放在请求自带的系统提示词前面，after = 放在后面；空值后端拒绝写入。 */
export type PromptRulePosition = 'before' | 'after';
/** 方案作用域：all = 所有 key（自动含没选方案的）；selected = 只看 plan_ids。 */
export type PromptRulePlanMode = 'all' | 'selected';
/** 渠道作用域：all / include(只在这些) / exclude(除了这些)。 */
export type PromptRuleChannelMode = 'all' | 'include' | 'exclude';
/** 模型作用域：all / selected（pattern 支持 claude-* 尾星通配）。 */
export type PromptRuleModelMode = 'all' | 'selected';
/** 受众，沿用旧四层的取值；全部用户 / 仅管理员 Key。 */
export type { PromptAudience } from './access-plan';

/** 一条规则（与后端 model.PromptRule 的 JSON tag 对齐；slice 列后端可能发 null）。 */
export type PromptRuleServer = {
    id: number;
    name: string;
    enabled: boolean;
    text: string;
    position?: PromptRulePosition | string | null;
    plan_mode?: PromptRulePlanMode | string | null;
    plan_ids?: number[] | null;
    include_no_plan?: boolean;
    channel_mode?: PromptRuleChannelMode | string | null;
    channel_ids?: number[] | null;
    model_mode?: PromptRuleModelMode | string | null;
    models?: string[] | null;
    audience?: PromptAudience | string | null;
    sort?: number;
    /** Go time.Time 的 JSON 形状（RFC3339 字符串）。 */
    created_at?: string | null;
    updated_at?: string | null;
};

/** 前端使用的完整规则：slice 列保证为数组、枚举收敛到确定值（空值按 all/before 兼容）。 */
export interface PromptRule {
    id: number;
    name: string;
    enabled: boolean;
    text: string;
    position: PromptRulePosition;
    plan_mode: PromptRulePlanMode;
    plan_ids: number[];
    include_no_plan: boolean;
    channel_mode: PromptRuleChannelMode;
    channel_ids: number[];
    model_mode: PromptRuleModelMode;
    models: string[];
    audience: PromptAudience;
    sort: number;
    created_at: string;
    updated_at: string;
}

/** create/update 的 body：可编辑列整行发。create 忽略 id 与时间列（服务端分配/推进）。 */
export type PromptRulePayload = {
    id?: number;
    name: string;
    enabled: boolean;
    text: string;
    position: PromptRulePosition;
    plan_mode: PromptRulePlanMode;
    plan_ids: number[];
    include_no_plan: boolean;
    channel_mode: PromptRuleChannelMode;
    channel_ids: number[];
    model_mode: PromptRuleModelMode;
    models: string[];
    audience: PromptAudience;
    sort: number;
};

/** 一次模拟请求的作用域，与后端 prompt.Scope 对齐；plan_id=0 表示没选方案的 key。 */
export type PromptRuleScope = {
    plan_id: number;
    request_model: string;
    channel_id: number;
    is_admin: boolean;
};

/** 未命中/命中的原因，取值与后端 prompt.Match.Reason 一致。 */
export type PromptRuleMatchReason = 'matched' | 'disabled' | 'audience' | 'plan_scope' | 'channel_scope' | 'model_scope';

/** 一条规则在某个模拟请求下的命中结论：Evaluate 恒为每条规则给一行。 */
export type PromptRuleMatch = {
    rule: PromptRule;
    matched: boolean;
    reason: PromptRuleMatchReason;
};

/** 合并文本的一段来源：客户端自带的段固定 rule_id=0、name=client、position=client。 */
export type PromptRuleSegment = {
    rule_id: number;
    name: string;
    position: PromptRulePosition | 'client';
    text: string;
};

/** 预览输出：全部规则的命中结论、最终文本的来源段与最终文本（后端 /preview 归一后的形状）。 */
export type PromptRulePreviewResult = {
    matches: PromptRuleMatch[];
    segments: PromptRuleSegment[];
    text: string;
};

/**
 * POST /api/v1/prompt-rule/preview 的请求体：字段名与
 * internal/server/handlers/prompt_rule.go 的 previewPromptRules 结构体 tag 逐字对齐
 * （plan_id / model / channel_id / is_admin / client_system）。注意请求模型在
 * 线上叫 `model`，不是本地 Scope 的 `request_model`，发送前用
 * promptRulePreviewRequest() 映射，别把 Scope 直接当 body 发。
 */
export type PromptRulePreviewRequest = {
    plan_id: number;
    /** 原始请求模型名；plan_id=0 表示没选方案的 key。 */
    model: string;
    channel_id: number;
    is_admin: boolean;
    client_system: string;
};

/** /preview 响应里的一条命中：rule 是服务端原始行（slice 列可能为 null）。 */
export type PromptRulePreviewMatch = {
    rule: PromptRuleServer;
    matched: boolean;
    reason: PromptRuleMatchReason;
};

/** /preview 响应：与 internal/prompt.PreviewResult 的 JSON 字段（matches/segments/text）对齐。 */
export type PromptRulePreviewResponse = {
    matches: PromptRulePreviewMatch[];
    segments: PromptRuleSegment[];
    text: string;
};

export function promptRulePositionValid(value?: string | null): boolean {
    return value === 'before' || value === 'after';
}

export function promptRulePlanModeValid(value?: string | null): boolean {
    return value === 'all' || value === 'selected';
}

export function promptRuleChannelModeValid(value?: string | null): boolean {
    return value === 'all' || value === 'include' || value === 'exclude';
}

export function promptRuleModelModeValid(value?: string | null): boolean {
    return value === 'all' || value === 'selected';
}

export function promptRuleAudienceValid(value?: string | null): boolean {
    return value === 'all' || value === 'admin_only';
}

/** 接口给的 slice 列可能是 null，归一成数组；枚举收敛到确定值（未知值按 all/before 兼容显示）。 */
export function normalizePromptRule(rule: PromptRuleServer): PromptRule {
    return {
        ...rule,
        name: rule.name ?? '',
        enabled: Boolean(rule.enabled),
        text: rule.text ?? '',
        position: rule.position === 'after' ? 'after' : 'before',
        plan_mode: rule.plan_mode === 'selected' ? 'selected' : 'all',
        plan_ids: rule.plan_ids ?? [],
        include_no_plan: Boolean(rule.include_no_plan),
        channel_mode: rule.channel_mode === 'include' ? 'include' : rule.channel_mode === 'exclude' ? 'exclude' : 'all',
        channel_ids: rule.channel_ids ?? [],
        model_mode: rule.model_mode === 'selected' ? 'selected' : 'all',
        models: rule.models ?? [],
        audience: rule.audience === 'admin_only' ? 'admin_only' : 'all',
        sort: Number(rule.sort ?? 0) || 0,
        created_at: rule.created_at ?? '',
        updated_at: rule.updated_at ?? '',
    };
}

const PROMPT_RULE_LIST_KEY = ['prompt-rules', 'list'] as const;

function invalidatePromptRules(queryClient: ReturnType<typeof useQueryClient>) {
    queryClient.invalidateQueries({ queryKey: PROMPT_RULE_LIST_KEY });
}

export function usePromptRuleList(options?: { enabled?: boolean }) {
    return useQuery({
        queryKey: PROMPT_RULE_LIST_KEY,
        queryFn: () => apiClient.get<PromptRuleServer[]>('/api/v1/prompt-rule/list'),
        select: (data) => data.map(normalizePromptRule),
        enabled: options?.enabled ?? true,
    });
}

export function useCreatePromptRule() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: (data: PromptRulePayload) => (
            apiClient.post<PromptRuleServer>('/api/v1/prompt-rule/create', data)
        ),
        onSuccess: (data) => {
            logger.log('Prompt rule created:', data);
            invalidatePromptRules(queryClient);
        },
        onError: (error) => {
            logger.error('Prompt rule create failed:', error);
        },
    });
}

/** 完整覆盖式更新：定位只认 body 里的 id；created_at 保留，updated_at 服务端推进。 */
export function useUpdatePromptRule() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: (data: PromptRulePayload & { id: number }) => (
            apiClient.post<PromptRuleServer>('/api/v1/prompt-rule/update', data)
        ),
        onSuccess: (data) => {
            logger.log('Prompt rule updated:', data);
            invalidatePromptRules(queryClient);
        },
        onError: (error) => {
            logger.error('Prompt rule update failed:', error);
        },
    });
}

export function useDeletePromptRule() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: (id: number) => apiClient.post<null>('/api/v1/prompt-rule/delete', { id }),
        onSuccess: () => {
            logger.log('Prompt rule deleted');
            invalidatePromptRules(queryClient);
        },
        onError: (error) => {
            logger.error('Prompt rule delete failed:', error);
        },
    });
}

export function useTogglePromptRule() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: (data: { id: number; enabled: boolean }) => (
            apiClient.post<{ id: number; enabled: boolean }>('/api/v1/prompt-rule/toggle', data)
        ),
        onSuccess: () => invalidatePromptRules(queryClient),
        onError: (error) => {
            logger.error('Prompt rule toggle failed:', error);
        },
    });
}

/** 按 ids 全量精确排列服务端 sort；重复/遗漏/未知项后端整失败，不静默修正。 */
export function useReorderPromptRules() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: (data: { ids: number[] }) => (
            apiClient.post<{ ids: number[] }>('/api/v1/prompt-rule/reorder', data)
        ),
        onSuccess: () => invalidatePromptRules(queryClient),
        onError: (error) => {
            logger.error('Prompt rule reorder failed:', error);
        },
    });
}

/**
 * 合并预览用的接口：作用域输入进去，命中结论与合并文本一次返回，后端纯本地计算。
 * 请求体经 promptRulePreviewRequest() 映射成后端认的字段名（模型字段线上叫 `model`）；
 * client_system 原样发，没填就发空串，不往正文里塞任何占位文字。
 */
export function usePreviewPromptRules() {
    return useMutation({
        mutationFn: (data: PromptRuleScope & { client_system?: string }) => (
            apiClient.post<PromptRulePreviewResponse>(
                '/api/v1/prompt-rule/preview',
                promptRulePreviewRequest(data, data.client_system),
            )
        ),
        onError: (error) => {
            logger.error('Prompt rule preview failed:', error);
        },
    });
}

/**
 * 本地 Scope → 后端 /preview 请求体。唯一一处字段改名（request_model → model），
 * 其余同名字段原样直传；client_system 缺省发空串（后端纯本地拼接，空串即无客户端段内容）。
 */
export function promptRulePreviewRequest(scope: PromptRuleScope, clientSystem?: string): PromptRulePreviewRequest {
    return {
        plan_id: scope.plan_id,
        model: scope.request_model,
        channel_id: scope.channel_id,
        is_admin: scope.is_admin,
        client_system: clientSystem ?? '',
    };
}

/** 服务端响应 → 组件侧离线形状：命中里的规则行先按列表同款规则归一（slice 列 null → []）。 */
export function normalizePromptRulePreview(response: PromptRulePreviewResponse): PromptRulePreviewResult {
    return {
        matches: response.matches.map((match) => ({
            rule: normalizePromptRule(match.rule),
            matched: match.matched,
            reason: match.reason,
        })),
        segments: response.segments,
        text: response.text,
    };
}
