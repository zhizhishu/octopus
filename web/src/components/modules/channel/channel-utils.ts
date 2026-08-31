import { ChannelType, type Channel } from '@/api/endpoints/channel';
import { cleanOneMillionModelName } from '@/lib/model-aliases';

export type ChannelEndpointFamilyId =
    | 'openai_chat'
    | 'openai_responses'
    | 'anthropic'
    | 'gemini'
    | 'embedding'
    | 'volcengine'
    | 'custom_openai_chat';

export type ChannelEndpointFilter = ChannelEndpointFamilyId | 'all';

export type ChannelEndpointFamily = {
    id: ChannelEndpointFamilyId;
    label: string;
    shortLabel: string;
    lane: string;
    description: string;
    tone: string;
    accent: string;
    badge: string;
    types: ChannelType[];
};

export const CHANNEL_ENDPOINT_FAMILIES: ChannelEndpointFamily[] = [
    {
        id: 'anthropic',
        label: 'Anthropic / Claude',
        shortLabel: 'Claude',
        lane: 'ANT',
        description: '/v1/messages · Claude-only 客户端',
        tone: 'from-fuchsia-500/10 via-transparent to-transparent',
        accent: 'bg-fuchsia-500',
        badge: 'border-fuchsia-500/25 bg-fuchsia-500/10 text-fuchsia-700 dark:text-fuchsia-300',
        types: [ChannelType.Anthropic],
    },
    {
        id: 'openai_responses',
        label: 'OpenAI Responses',
        shortLabel: 'Responses',
        lane: 'RSP',
        description: '/v1/responses · Codex / GPT-5.5',
        tone: 'from-sky-500/10 via-transparent to-transparent',
        accent: 'bg-sky-500',
        badge: 'border-sky-500/25 bg-sky-500/10 text-sky-700 dark:text-sky-300',
        types: [ChannelType.OpenAIResponse],
    },
    {
        id: 'openai_chat',
        label: 'OpenAI Chat',
        shortLabel: 'Chat',
        lane: 'OAI',
        description: '/v1/chat/completions · 兼容客户端',
        tone: 'from-emerald-500/10 via-transparent to-transparent',
        accent: 'bg-emerald-500',
        badge: 'border-emerald-500/25 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300',
        types: [ChannelType.OpenAIChat],
    },
    {
        id: 'custom_openai_chat',
        label: 'Custom OpenAI Chat',
        shortLabel: 'Custom Chat',
        lane: 'CUS',
        description: '自定义 OpenAI-compatible 路径',
        tone: 'from-amber-500/10 via-transparent to-transparent',
        accent: 'bg-amber-500',
        badge: 'border-amber-500/25 bg-amber-500/10 text-amber-700 dark:text-amber-300',
        types: [ChannelType.CustomOpenAIChat],
    },
    {
        id: 'gemini',
        label: 'Gemini',
        shortLabel: 'Gemini',
        lane: 'GEM',
        description: '/v1beta/models/:model:generateContent',
        tone: 'from-violet-500/10 via-transparent to-transparent',
        accent: 'bg-violet-500',
        badge: 'border-violet-500/25 bg-violet-500/10 text-violet-700 dark:text-violet-300',
        types: [ChannelType.Gemini],
    },
    {
        id: 'embedding',
        label: 'OpenAI Embedding',
        shortLabel: 'Embedding',
        lane: 'EMB',
        description: '/v1/embeddings · 向量端点',
        tone: 'from-teal-500/10 via-transparent to-transparent',
        accent: 'bg-teal-500',
        badge: 'border-teal-500/25 bg-teal-500/10 text-teal-700 dark:text-teal-300',
        types: [ChannelType.OpenAIEmbedding],
    },
    {
        id: 'volcengine',
        label: 'Volcengine',
        shortLabel: 'Volcengine',
        lane: 'VOL',
        description: '火山方舟兼容端点',
        tone: 'from-red-500/10 via-transparent to-transparent',
        accent: 'bg-red-500',
        badge: 'border-red-500/25 bg-red-500/10 text-red-700 dark:text-red-300',
        types: [ChannelType.Volcengine],
    },
];

const FAMILY_BY_TYPE = new Map<ChannelType, ChannelEndpointFamily>(
    CHANNEL_ENDPOINT_FAMILIES.flatMap((family) => family.types.map((type) => [type, family] as const))
);

export function getChannelEndpointFamily(channelOrType: Channel | ChannelType): ChannelEndpointFamily {
    const type = typeof channelOrType === 'number' ? channelOrType : channelOrType.type;
    return FAMILY_BY_TYPE.get(type) ?? CHANNEL_ENDPOINT_FAMILIES[0];
}

export function getChannelTypeLabel(type: ChannelType): string {
    return getChannelEndpointFamily(type).label;
}

export function splitChannelModels(...models: Array<string | undefined | null>): string[] {
    return models
        .flatMap((value) => (value ?? '').split(','))
        .map((item) => cleanOneMillionModelName(item))
        .filter(Boolean);
}

export function getSelectedChannelModels(channel: Channel): string[] {
    if (channel.selected_models?.length) {
        return channel.selected_models.map((item) => cleanOneMillionModelName(item)).filter(Boolean);
    }
    return splitChannelModels(channel.model, channel.custom_model);
}

export function getPrimaryChannelModel(channel: Channel): string {
    return getSelectedChannelModels(channel)[0] ?? '';
}

export function getPrimaryBaseUrl(channel: Channel): string {
    return channel.base_urls?.find((item) => item.url.trim())?.url ?? '';
}

export function getChannelRequestCount(channel: Channel): number {
    const stats = channel.stats;
    return Math.max(0, stats?.request_success ?? 0) + Math.max(0, stats?.request_failed ?? 0);
}

export function filterChannel(channel: Channel, searchTerm: string): boolean {
    const term = searchTerm.toLowerCase().trim();
    if (!term) return true;

    const family = getChannelEndpointFamily(channel);
    const searchFields: string[] = [
        channel.name ?? '',
        String(channel.id ?? ''),
        channel.model ?? '',
        channel.custom_model ?? '',
        ...(channel.selected_models ?? []),
        ...(channel.discovered_models ?? []),
        family.label ?? '',
        family.shortLabel ?? '',
    ];

    // model_mapping alias and upstream
    if (channel.model_mapping && typeof channel.model_mapping === 'object') {
        for (const [alias, upstream] of Object.entries(channel.model_mapping)) {
            if (alias) searchFields.push(alias);
            if (upstream) searchFields.push(upstream);
        }
    }

    // all base URLs
    if (Array.isArray(channel.base_urls)) {
        for (const item of channel.base_urls) {
            if (item?.url) searchFields.push(item.url);
        }
    }

    // Key remarks ONLY (strict: never search key plaintext)
    if (Array.isArray(channel.keys)) {
        for (const key of channel.keys) {
            if (key?.remark) searchFields.push(key.remark);
        }
    }

    return searchFields.some((field) => field.toLowerCase().includes(term));
}
