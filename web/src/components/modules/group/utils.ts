import type { LLMChannel } from '@/api/endpoints/model';
import { GroupMode } from '@/api/endpoints/group';

// Every legacy mode value folds onto the two product-facing labels so historical
// groups (random/weighted/smart) still render as the spread/round-robin mode.
export const MODE_LABELS: Record<GroupMode, string> = {
    [GroupMode.RoundRobin]: 'spread',
    [GroupMode.Random]: 'spread',
    [GroupMode.Failover]: 'fillFirst',
    [GroupMode.Weighted]: 'spread',
    [GroupMode.Smart]: 'spread',
} as const;

// 真实模式中文名（诊断用）：与 MODE_LABELS 的对外折叠不同，这里保留每种模式的真身，
// 只在界面把 random/weighted/smart 折叠成「轮询」而掩盖真身时用来点出实际调度方式。
export const RAW_MODE_LABELS: Record<GroupMode, string> = {
    [GroupMode.RoundRobin]: '轮询',
    [GroupMode.Random]: '随机',
    [GroupMode.Failover]: '填充优先',
    [GroupMode.Weighted]: '加权',
    [GroupMode.Smart]: '智能',
} as const;

// 折叠模式：对外显示成「轮询」但真身并非 RoundRobin（random/weighted/smart）。
// 命中即说明界面掩盖了真实调度方式，需要额外点名提示。
export function isFoldedMode(mode: GroupMode): boolean {
    return mode !== GroupMode.RoundRobin && mode !== GroupMode.Failover;
}

export function normalizeKey(value: string) {
    return value.trim().toLowerCase();
}

export function modelChannelKey(channelId: number, modelName: string) {
    return `${channelId}-${modelName}`;
}

export function memberKey(member: Pick<LLMChannel, 'channel_id' | 'name'>) {
    return modelChannelKey(member.channel_id, member.name);
}

export function activeModelChannels(modelChannels: LLMChannel[]) {
    const seen = new Set<string>();
    return modelChannels.filter((mc) => {
        if (!mc.enabled) return false;
        const key = memberKey(mc);
        if (seen.has(key)) return false;
        seen.add(key);
        return true;
    });
}

export function activeModelChannelKeySet(modelChannels: LLMChannel[]) {
    return new Set(activeModelChannels(modelChannels).map(memberKey));
}

export function matchesGroupName(modelName: string, groupKey: string) {
    if (!groupKey) return false;
    return modelName.toLowerCase().includes(groupKey);
}

export function parseGroupMatchRegex(input: string): RegExp {
    const inlineMatch = input.match(/^\(\?([ism]+)\)(.+)$/);
    if (inlineMatch) {
        const flagMap: Record<string, string> = { i: 'i', s: 's', m: 'm' };
        const flags = inlineMatch[1].split('').map(f => flagMap[f] || '').join('');
        return new RegExp(inlineMatch[2], flags);
    }

    return new RegExp(input);
}

export function matchGroupModelChannels(
    modelChannels: LLMChannel[],
    groupName: string,
    matchRegex: string
) {
    const regexKey = matchRegex.trim();
    const groupKey = normalizeKey(groupName);

    if (regexKey) {
        try {
            const re = parseGroupMatchRegex(regexKey);
            return { matchedModelChannels: modelChannels.filter((mc) => re.test(mc.name)), regexError: '' };
        } catch (e) {
            return { matchedModelChannels: [], regexError: (e as Error)?.message ?? 'Invalid regex' };
        }
    }

    if (!groupKey) return { matchedModelChannels: [], regexError: '' };
    return { matchedModelChannels: modelChannels.filter((mc) => matchesGroupName(mc.name, groupKey)), regexError: '' };
}

export function buildChannelNameByModelKey(modelChannels: LLMChannel[]) {
    const map = new Map<string, string>();
    modelChannels.forEach((mc) => {
        map.set(modelChannelKey(mc.channel_id, mc.name), mc.channel_name);
    });
    return map;
}


