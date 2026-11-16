import type { AccessPlan, PromptAudience, PromptOverrideMode } from '@/api/endpoints/access-plan';
import type { Channel } from '@/api/endpoints/channel';

export type PromptValue = {
    text: string;
    mode: PromptOverrideMode;
    enabled: boolean;
    audience: PromptAudience;
};
export type PromptDraft = { value: PromptValue; baseline: PromptValue };
export type PromptDraftMap = Record<string, PromptDraft>;

export function normalizeAudience(value: string | null | undefined): PromptAudience {
    return value === 'admin_only' ? 'admin_only' : 'all';
}

export function promptValuesEqual(a: PromptValue, b: PromptValue): boolean {
    return a.text.trim() === b.text.trim()
        && a.mode === b.mode && a.enabled === b.enabled && a.audience === b.audience;
}

/** Changes are kept above collapsible rows, so unmount/refetch does not discard them. */
export function editPromptDraft(map: PromptDraftMap, key: string, saved: PromptValue, mutate: (value: PromptValue) => PromptValue): PromptDraftMap {
    const existing = map[key] ?? { value: saved, baseline: saved };
    return { ...map, [key]: { ...existing, value: mutate(existing.value) } };
}

/** A save can finish after the user has already typed more text. Keep that newer draft. */
export function acceptPromptSave(map: PromptDraftMap, key: string, submitted: PromptValue): PromptDraftMap {
    const original = map[key];
    const value = original && !promptValuesEqual(original.value, submitted) ? original.value : submitted;
    return { ...map, [key]: { value, baseline: submitted } };
}

/** A channel's prompt applies wherever the channel is used. Plan filtering is navigation, NOT scope. */
export function channelPlanNames(plans: AccessPlan[]): Map<number, string[]> {
    const names = new Map<number, Set<string>>();
    for (const plan of plans) {
        const label = plan.display_name || plan.slug;
        for (const target of plan.route_targets ?? []) {
            const current = names.get(target.channel_id) ?? new Set<string>();
            current.add(label);
            names.set(target.channel_id, current);
        }
    }
    return new Map([...names].map(([id, values]) => [id, [...values]]));
}

export function channelIDsForPlan(plan: AccessPlan | undefined): Set<number> {
    return new Set((plan?.route_targets ?? []).map((target) => target.channel_id));
}

export const CHANNEL_TYPE_LABELS: Record<number, string> = {
    0: 'OpenAI Chat',
    1: 'OpenAI Responses',
    2: 'Anthropic / Claude',
    3: 'Gemini',
    5: 'Embeddings',
    6: '自定义 OpenAI',
    7: 'SystemOne',
};

export type ChannelPromptGroup = { type: number; label: string; channels: Channel[] };
export function groupChannelsByType(channels: Channel[]): ChannelPromptGroup[] {
    const groups = new Map<number, Channel[]>();
    for (const channel of channels) {
        const list = groups.get(channel.type) ?? [];
        list.push(channel);
        groups.set(channel.type, list);
    }
    return [...groups].sort(([a], [b]) => a - b)
        .map(([type, items]) => ({
            type, label: CHANNEL_TYPE_LABELS[type] ?? `其他类型 (${type})`,
            channels: [...items].sort((a, b) => a.id - b.id),
        }));
}
