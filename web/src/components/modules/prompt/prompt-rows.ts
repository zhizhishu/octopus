import type { AccessPlan, PromptOverrideMode } from '@/api/endpoints/access-plan';
import { normalizePromptMode, promptEnabledFromSaved } from './prompt-mode';

// 提示词页的纯数据整理，单独放这里方便测。

export type RoutePromptRow = {
    plan: AccessPlan;
    requestModel: string;
    upstreamModels: string[];
    targetCount: number;
    mode: PromptOverrideMode;
    /** 这条路由的提示词开关（旧数据有文字算开）。 */
    enabled: boolean;
    text: string;
};

/**
 * 一个方案里的所有路由（请求模型），按出现顺序，一条路由一行。
 * 接口给的 route_targets 是按目标（渠道 × 上游模型）摊平的，每个目标都带着所属规则的提示词，
 * 所以按请求模型（不分大小写）合并。
 */
export function planRouteRows(plan: AccessPlan): RoutePromptRow[] {
    const rows: RoutePromptRow[] = [];
    const byModel = new Map<string, RoutePromptRow>();
    for (const target of plan.route_targets ?? []) {
        const model = (target.request_model ?? '').trim();
        if (!model) continue;
        const key = model.toLowerCase();
        let row = byModel.get(key);
        if (!row) {
            row = {
                plan,
                requestModel: model,
                upstreamModels: [],
                targetCount: 0,
                mode: normalizePromptMode(target.prompt_override_mode),
                enabled: promptEnabledFromSaved(target.prompt_override_enabled, target.prompt_override_mode, target.system_prompt_override),
                text: target.system_prompt_override ?? '',
            };
            byModel.set(key, row);
            rows.push(row);
        }
        row.targetCount += 1;
        if (target.upstream_model && !row.upstreamModels.includes(target.upstream_model)) {
            row.upstreamModels.push(target.upstream_model);
        }
    }
    return rows;
}

/** 所有方案里写了提示词的路由（关着的也算，标「已关闭」）。 */
export function routePromptRows(plans: AccessPlan[]): RoutePromptRow[] {
    return plans.flatMap((plan) => planRouteRows(plan).filter((row) => row.text.trim()));
}

/** 搜索：空格分开的每个词都要在某个字段里出现（不分大小写）。 */
export function matchesPromptSearch(query: string, ...fields: Array<string | number | null | undefined>): boolean {
    const words = query.trim().toLowerCase().split(/\s+/).filter(Boolean);
    if (words.length === 0) return true;
    const hay = fields.map((field) => String(field ?? '').toLowerCase());
    return words.every((word) => hay.some((field) => field.includes(word)));
}

/** 渠道按 id 升序（即创建顺序）。接口顺序来自后端内存表，每次可能不同，输入框会跳。 */
export function sortChannelsStable<T extends { id: number }>(channels: T[]): T[] {
    return [...channels].sort((a, b) => a.id - b.id);
}
