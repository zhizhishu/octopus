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


