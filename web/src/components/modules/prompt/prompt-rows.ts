import type { AccessPlan, PromptOverrideMode } from '@/api/endpoints/access-plan';

// 提示词页的纯数据整理，单独放这里方便测。

export type RoutePromptRow = {
    plan: AccessPlan;
    requestModel: string;
    upstreamModels: string[];
    targetCount: number;
    mode: PromptOverrideMode;
};

/**
 * 模型路由提示词按「方案 + 请求模型」（即一条路由）汇总成一行。
 * 接口给的 route_targets 是按目标（渠道 × 上游模型）摊平的，每个目标都带着所属规则的提示词；
 * 以前直接按目标出行，一条规则配 2 个渠道就显示 2 行、计数也多算。
 */
export function routePromptRows(plans: AccessPlan[]): RoutePromptRow[] {
    const rows: RoutePromptRow[] = [];
    for (const plan of plans) {
        const byModel = new Map<string, RoutePromptRow>();
        for (const target of plan.route_targets ?? []) {
            if (!(target.system_prompt_override ?? '').trim()) continue;
            let row = byModel.get(target.request_model);
            if (!row) {
                row = {
                    plan,
                    requestModel: target.request_model,
                    upstreamModels: [],
                    targetCount: 0,
                    mode: target.prompt_override_mode ?? 'append_system',
                };
                byModel.set(target.request_model, row);
                rows.push(row);
            }
            row.targetCount += 1;
            if (target.upstream_model && !row.upstreamModels.includes(target.upstream_model)) {
                row.upstreamModels.push(target.upstream_model);
            }
        }
    }
    return rows;
}

/** 渠道按 id 升序（即创建顺序）。接口顺序来自后端内存表，每次可能不同，输入框会跳。 */
export function sortChannelsStable<T extends { id: number }>(channels: T[]): T[] {
    return [...channels].sort((a, b) => a.id - b.id);
}
