import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiClient, API_BASE_URL } from '../client';
import { logger } from '@/lib/logger';
import { formatCount, formatMoney, formatTime } from '@/lib/utils';
import { StatsChannel, type StatsMetricsFormatted } from './stats';
import type { PromptOverrideMode } from './access-plan';
import type { ModelTestResponse } from './model';
import { useAuthStore } from './user';

export type { PromptOverrideMode } from './access-plan';
/**
 * 渠道类型枚举
 */
export enum ChannelType {
    OpenAIChat = 0,
    OpenAIResponse = 1,
    Anthropic = 2,
    Gemini = 3,
    Volcengine = 4,
    OpenAIEmbedding = 5,
    CustomOpenAIChat = 6,
}

/**
 * Key 选择策略枚举
 */
export enum KeySelectStrategy {
    CostBalanced = 0, // 成本均衡（默认）
    Sticky = 1,       // 同 key 优先
}

export type BaseUrl = {
    url: string;
    delay: number;
};

export type CustomHeader = {
    header_key: string;
    header_value: string;
};

export type ChannelCloak = {
    mode?: 'auto' | 'always' | 'never' | string;
    // 0 = 跟随全局默认指纹（现状）；>0 = 使用对应的指纹 Profile。
    profile_id?: number;
};

export type ChannelKey = {
    id: number;
    channel_id: number;
    enabled: boolean;
    channel_key: string;
    status_code: number;
    last_use_time_stamp: number;
    total_cost: number;
    remark: string;
};

/**
 * 渠道完整数据（与后端 model.Channel 对齐；数组字段在前端保证为 []）
 */
export type Channel = {
    id: number;
    name: string;
    type: ChannelType;
    enabled: boolean;
    priority: number;
    max_concurrent: number;
    rpm_limit: number;
    key_select_strategy: KeySelectStrategy;
    disable_circuit_breaker: boolean;
    base_urls: BaseUrl[];
    keys: ChannelKey[];
    model: string;
    custom_model: string;
    discovered_models: string[];
    selected_models: string[];
    anthropic_context_1m: boolean;
    thinking_to_content: boolean;
    proxy: boolean;
    auto_sync: boolean;
    custom_header: CustomHeader[];
    cloak: ChannelCloak;
    param_override?: string | null;
    system_prompt_override?: string | null;
    prompt_override_mode?: PromptOverrideMode | null;
    channel_proxy?: string | null;
    openai_chat_path: string;
    openai_models_path: string;
    match_regex?: string | null;
    model_mapping?: Record<string, string>;
    stats: StatsChannel;
    circuit_tripped: boolean;
    circuit_remaining_seconds: number;
    circuit_open_keys: number;
};

// Internal type: backend may return null for slice fields; normalize to [] in select()
type ChannelServer = Omit<Channel, 'base_urls' | 'custom_header' | 'keys' | 'cloak' | 'discovered_models' | 'selected_models'> & {
    base_urls: BaseUrl[] | null;
    custom_header: CustomHeader[] | null;
    cloak?: ChannelCloak | null;
    keys: ChannelKey[] | null;
    discovered_models?: string[] | null;
    selected_models?: string[] | null;
};

/**
 * 创建渠道请求：必填字段 + 可选字段
 */
export type CreateChannelRequest = {
    name: string;
    type: ChannelType;
    enabled?: boolean;
    priority?: number;
    max_concurrent?: number;
    rpm_limit?: number;
    key_select_strategy?: KeySelectStrategy;
    disable_circuit_breaker?: boolean;
    base_urls: BaseUrl[];
    keys: Array<Pick<ChannelKey, 'enabled' | 'channel_key' | 'remark'>>;
    model: string;
    custom_model?: string;
    discovered_models?: string[];
    selected_models?: string[];
    anthropic_context_1m?: boolean;
    thinking_to_content?: boolean;
    proxy?: boolean;
    auto_sync?: boolean;
    custom_header?: CustomHeader[];
    cloak?: ChannelCloak;
    channel_proxy?: string | null;
    openai_chat_path?: string;
    openai_models_path?: string;
    param_override?: string | null;
    system_prompt_override?: string | null;
    prompt_override_mode?: PromptOverrideMode | null;
    match_regex?: string | null;
    model_mapping?: Record<string, string>;
};

/**
 * 更新渠道请求：id + 可选字段 + keys diff
 */
export type UpdateChannelRequest = {
    id: number;
    name?: string;
    type?: ChannelType;
    enabled?: boolean;
    priority?: number;
    max_concurrent?: number;
    rpm_limit?: number;
    key_select_strategy?: KeySelectStrategy;
    disable_circuit_breaker?: boolean;
    base_urls?: BaseUrl[];
    model?: string;
    custom_model?: string;
    discovered_models?: string[];
    selected_models?: string[];
    anthropic_context_1m?: boolean;
    thinking_to_content?: boolean;
    proxy?: boolean;
    auto_sync?: boolean;
    custom_header?: CustomHeader[];
    cloak?: ChannelCloak;
    channel_proxy?: string | null;
    openai_chat_path?: string;
    openai_models_path?: string;
    param_override?: string | null;
    system_prompt_override?: string | null;
    prompt_override_mode?: PromptOverrideMode | null;
    match_regex?: string | null;
    model_mapping?: Record<string, string>;
    // keys diff
    keys_to_add?: Array<Pick<ChannelKey, 'enabled' | 'channel_key' | 'remark'>>;
    keys_to_update?: Array<{ id: number; enabled?: boolean; channel_key?: string; remark?: string }>;
    keys_to_delete?: number[];
};

export type FetchModelRequest = {
    type: ChannelType;
    base_urls: BaseUrl[];
    keys: Array<Pick<ChannelKey, 'enabled' | 'channel_key'>>;
    proxy?: boolean;
    channel_proxy?: string | null;
    match_regex?: string | null;
    custom_header?: CustomHeader[];
    openai_chat_path?: string;
    openai_models_path?: string;
};

export type ChannelTestConfig = {
    id?: number;
    name?: string;
    type: ChannelType;
    enabled?: boolean;
    priority?: number;
    base_urls: BaseUrl[];
    keys: Array<Partial<ChannelKey> & Pick<ChannelKey, 'enabled' | 'channel_key'>>;
    model?: string;
    custom_model?: string;
    discovered_models?: string[];
    selected_models?: string[];
    anthropic_context_1m?: boolean;
    proxy?: boolean;
    auto_sync?: boolean;
    custom_header?: CustomHeader[];
    cloak?: ChannelCloak;
    channel_proxy?: string | null;
    openai_chat_path?: string;
    openai_models_path?: string;
    param_override?: string | null;
    system_prompt_override?: string | null;
    prompt_override_mode?: PromptOverrideMode | null;
    match_regex?: string | null;
    model_mapping?: Record<string, string>;
};

export type ChannelTestRequest = {
    channel: ChannelTestConfig;
    model?: string;
    endpoint?: string;
    prompt?: string;
    stream?: boolean;
    timeout_seconds?: number;
};

export type ChannelCSVImportRowResult = {
    row: number;
    name: string;
    action: 'created' | 'updated' | 'skipped' | 'failed' | string;
    channel_id?: number;
    error?: string;
};

export type ChannelCSVImportResult = {
    total: number;
    created: number;
    updated: number;
    skipped: number;
    failed: number;
    rows: ChannelCSVImportRowResult[];
};

/**
 * 获取渠道列表 Hook
 * 
 * @example
 * const { data: channels, isLoading, error } = useChannelList();
 * 
 * if (isLoading) return <Loading />;
 * if (error) return <Error message={error.message} />;
 * 
 * channels?.forEach(channel => console.log(channel.raw.name));
 */
export function useChannelList(options?: { enabled?: boolean }) {
    return useQuery({
        queryKey: ['channels', 'list'],
        queryFn: async () => {
            return apiClient.get<ChannelServer[]>('/api/v1/channel/list');
        },
        select: (data) => data.map((item) => ({
            raw: ({
                ...item,
                base_urls: item.base_urls ?? [],
                custom_header: item.custom_header ?? [],
                cloak: item.cloak ?? { mode: 'auto', profile_id: 0 },
                keys: item.keys ?? [],
                discovered_models: item.discovered_models ?? [],
                selected_models: item.selected_models ?? [],
                anthropic_context_1m: item.anthropic_context_1m ?? false,
                thinking_to_content: item.thinking_to_content ?? false,
                openai_chat_path: item.openai_chat_path ?? '',
                openai_models_path: item.openai_models_path ?? '',
                priority: item.priority ?? 0,
                max_concurrent: item.max_concurrent ?? 0,
                rpm_limit: item.rpm_limit ?? 0,
                key_select_strategy: item.key_select_strategy ?? 0,
                disable_circuit_breaker: item.disable_circuit_breaker ?? false,
                circuit_tripped: item.circuit_tripped ?? false,
                circuit_remaining_seconds: item.circuit_remaining_seconds ?? 0,
                circuit_open_keys: item.circuit_open_keys ?? 0,
            }) satisfies Channel,
            formatted: {
                input_token: formatCount(item.stats.input_token),
                output_token: formatCount(item.stats.output_token),
                total_token: formatCount(item.stats.input_token + item.stats.output_token),
                input_cost: formatMoney(item.stats.input_cost),
                output_cost: formatMoney(item.stats.output_cost),
                total_cost: formatMoney(item.stats.input_cost + item.stats.output_cost),
                request_success: formatCount(item.stats.request_success),
                request_failed: formatCount(item.stats.request_failed),
                request_count: formatCount(item.stats.request_success + item.stats.request_failed),
                wait_time: formatTime(item.stats.wait_time),
            }
        })) as Array<{ raw: Channel; formatted: StatsMetricsFormatted }>,
        enabled: options?.enabled ?? true,
        refetchInterval: 30000,
        refetchOnMount: 'always',
    });
}

/**
 * 创建渠道 Hook
 * 
 * @example
 * const createChannel = useCreateChannel();
 * 
 * createChannel.mutate({
 *   name: 'OpenAI',
 *   type: ChannelType.OpenAIChat,
 *   base_urls: [{ url: 'https://api.openai.com', delay: 0 }],
 *   keys: [{ enabled: true, channel_key: 'sk-xxx' }],
 *   model: 'gpt-4',
 * });
 */
export function useCreateChannel() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: async (data: CreateChannelRequest) => {
            return apiClient.post<ChannelServer>('/api/v1/channel/create', data);
        },
        onSuccess: (data) => {
            logger.log('渠道创建成功:', data);
            queryClient.invalidateQueries({ queryKey: ['channels', 'list'] });
            queryClient.invalidateQueries({ queryKey: ['models', 'list'] });
            queryClient.invalidateQueries({ queryKey: ['models', 'channel'] });
        },
        onError: (error) => {
            logger.error('渠道创建失败:', error);
        },
    });
}

/**
 * 复制渠道 Hook
 *
 * 客户端组装克隆 payload, 调用现有 POST /api/v1/channel/create, 后端零改动。
 * name 自动加 _copy / _copy_2 / _copy_3 后缀避免撞库; enabled 默认 false, 让用户审核后再启用。
 *
 * @example
 * const copyChannel = useCopyChannel();
 *
 * copyChannel.mutate(sourceChannel, {
 *   onSuccess: ({ newName }) => toast.success(`已复制为 ${newName}`),
 *   onError: () => toast.error('复制失败'),
 * });
 */
export function useCopyChannel() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: async (source: Channel): Promise<{ server: ChannelServer; newName: string }> => {
            const cached = queryClient.getQueryData<any>(['channels', 'list']);
            const existingNames = new Set<string>();
            if (Array.isArray(cached)) {
                cached.forEach((item) => {
                    const name = item?.raw?.name ?? item?.name;
                    if (name) {
                        existingNames.add(name);
                    }
                });
            }

            // name 撞库: _copy -> _copy_2 -> _copy_3 ...
            let newName = `${source.name}_copy`;
            let counter = 2;
            while (existingNames.has(newName)) {
                newName = `${source.name}_copy_${counter++}`;
            }

            const clone: CreateChannelRequest = {
                name: newName,
                type: source.type,
                enabled: false, // 用户明确要求默认禁用, 避免误启用克隆出来的渠道
                priority: source.priority,
                max_concurrent: source.max_concurrent,
                rpm_limit: source.rpm_limit,
                key_select_strategy: source.key_select_strategy,
                disable_circuit_breaker: source.disable_circuit_breaker,
                base_urls: (source.base_urls ?? []).map((u) => ({ url: u.url, delay: u.delay })),
                keys: (source.keys ?? [])
                    .filter((k) => k.channel_key) // 跳过空 key, 不带 id/status_code/last_use_time_stamp/total_cost 等运行时字段
                    .map((k) => ({
                        enabled: k.enabled,
                        channel_key: k.channel_key,
                        remark: k.remark ?? '',
                    })),
                model: source.model,
                custom_model: source.custom_model,
                discovered_models: [...(source.discovered_models ?? [])],
                selected_models: [...(source.selected_models ?? [])],
                anthropic_context_1m: source.anthropic_context_1m,
                thinking_to_content: source.thinking_to_content,
                proxy: source.proxy,
                auto_sync: source.auto_sync,
                custom_header: (source.custom_header ?? []).map((h) => ({ ...h })),
                cloak: source.cloak ? { ...source.cloak } : undefined,
                channel_proxy: source.channel_proxy,
                openai_chat_path: source.openai_chat_path,
                openai_models_path: source.openai_models_path,
                param_override: source.param_override,
                system_prompt_override: source.system_prompt_override,
                prompt_override_mode: source.prompt_override_mode,
                match_regex: source.match_regex,
                model_mapping: source.model_mapping ? { ...source.model_mapping } : undefined,
            };

            const server = await apiClient.post<ChannelServer>('/api/v1/channel/create', clone);
            return { server, newName };
        },
        onSuccess: ({ server }) => {
            logger.log('渠道复制成功:', server);
            queryClient.invalidateQueries({ queryKey: ['channels', 'list'] });
            queryClient.invalidateQueries({ queryKey: ['models', 'list'] });
            queryClient.invalidateQueries({ queryKey: ['models', 'channel'] });
            queryClient.invalidateQueries({ queryKey: ['access-plans', 'list'] });
        },
        onError: (error) => {
            logger.error('渠道复制失败:', error);
        },
    });
}

/**
 * 更新渠道 Hook
 * 
 * @example
 * const updateChannel = useUpdateChannel();
 * 
 * updateChannel.mutate({
 *   id: 1,
 *   name: 'OpenAI Updated',
 *   type: ChannelType.OpenAIChat,
 *   enabled: true,
 *   base_urls: [{ url: 'https://api.openai.com', delay: 0 }],
 *   keys_to_add: [{ enabled: true, channel_key: 'sk-xxx' }],
 *   model: 'gpt-4-turbo',
 *   proxy: false,
 * });
 */
export function useUpdateChannel() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: async (data: UpdateChannelRequest) => {
            return apiClient.post<ChannelServer>('/api/v1/channel/update', data);
        },
        onSuccess: (data) => {
            logger.log('渠道更新成功:', data);
            queryClient.invalidateQueries({ queryKey: ['channels', 'list'] });
            queryClient.invalidateQueries({ queryKey: ['models', 'channel'] });
            queryClient.invalidateQueries({ queryKey: ['models', 'list'] });
            // A disable/enable (or model change) reconciles access-plan routes on the
            // backend; refresh the plan view so the canvas drops the evicted targets.
            queryClient.invalidateQueries({ queryKey: ['access-plans', 'list'] });
        },
        onError: (error) => {
            logger.error('渠道更新失败:', error);
        },
    });
}

/**
 * 删除渠道 Hook
 * 
 * @example
 * const deleteChannel = useDeleteChannel();
 * 
 * deleteChannel.mutate(1); // 删除 ID 为 1 的渠道
 */
export function useDeleteChannel() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: async (id: number) => {
            return apiClient.delete<null>(`/api/v1/channel/delete/${id}`);
        },
        onSuccess: () => {
            logger.log('渠道删除成功');
            queryClient.invalidateQueries({ queryKey: ['channels', 'list'] });
            queryClient.invalidateQueries({ queryKey: ['models', 'channel'] });
            queryClient.invalidateQueries({ queryKey: ['models', 'list'] });
            // The delete handler reconciles access-plan routes on the backend; refresh the
            // plan view so the canvas drops the deleted channel's targets automatically.
            queryClient.invalidateQueries({ queryKey: ['access-plans', 'list'] });
        },
        onError: (error) => {
            logger.error('渠道删除失败:', error);
        },
    });
}

/**
 * 启用/禁用渠道 Hook
 * 
 * @example
 * const enableChannel = useEnableChannel();
 * 
 * enableChannel.mutate({ id: 1, enabled: true }); // 启用 ID 为 1 的渠道
 * enableChannel.mutate({ id: 1, enabled: false }); // 禁用 ID 为 1 的渠道
 */
export function useEnableChannel() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: async (data: { id: number; enabled: boolean }) => {
            return apiClient.post<null>('/api/v1/channel/enable', data);
        },
        onSuccess: () => {
            logger.log('渠道状态更新成功');
            queryClient.invalidateQueries({ queryKey: ['channels', 'list'] });
            queryClient.invalidateQueries({ queryKey: ['models', 'channel'] });
            queryClient.invalidateQueries({ queryKey: ['models', 'list'] });
            // The server reconciles AutoSyncChannels plan targets on every enable/disable.
            // Invalidate so the access-plan page reflects the updated route targets
            // automatically without requiring a manual rebuild click.
            queryClient.invalidateQueries({ queryKey: ['access-plans', 'list'] });
        },
        onError: (error) => {
            logger.error('渠道状态更新失败:', error);
        },
    });
}

function getAuthHeader() {
    const token = useAuthStore.getState().token;
    if (!token) throw new Error('Not authenticated');
    return `Bearer ${token}`;
}

function getAPIMessage(value: unknown): string | undefined {
    if (!value || typeof value !== 'object') return undefined;
    const message = (value as { message?: unknown }).message;
    return typeof message === 'string' ? message : undefined;
}

function getAPIData<T>(value: unknown): T | undefined {
    if (!value || typeof value !== 'object') return undefined;
    return (value as { data?: T }).data;
}

export function useImportChannelCSV() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: async (data: { file: File; replaceKey?: boolean; dryRun?: boolean }) => {
            const form = new FormData();
            form.append('file', data.file);
            if (data.replaceKey) form.append('replace_key', 'true');
            if (data.dryRun) form.append('dry_run', 'true');

            const res = await fetch(`${API_BASE_URL}/api/v1/channel/import-csv`, {
                method: 'POST',
                headers: { Authorization: getAuthHeader() },
                body: form,
            });
            const contentType = res.headers.get('content-type') || '';
            const payload = contentType.includes('application/json') ? await res.json() : await res.text();
            if (!res.ok) {
                throw new Error(getAPIMessage(payload) ?? (typeof payload === 'string' ? payload : res.statusText));
            }
            return getAPIData<ChannelCSVImportResult>(payload) ?? (payload as ChannelCSVImportResult);
        },
        onSuccess: () => {
            queryClient.invalidateQueries({ queryKey: ['channels', 'list'] });
            queryClient.invalidateQueries({ queryKey: ['models', 'list'] });
            queryClient.invalidateQueries({ queryKey: ['models', 'channel'] });
        },
        onError: (error) => {
            logger.error('渠道 CSV 导入失败:', error);
        },
    });
}

export function useResetChannelCircuit() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: async (data: { id: number }) => {
            return apiClient.post<null>('/api/v1/channel/reset-circuit', data);
        },
        onSuccess: () => {
            queryClient.invalidateQueries({ queryKey: ['channels', 'list'] });
        },
    });
}

export function useTestChannelConfig() {
    return useMutation({
        mutationFn: async (data: ChannelTestRequest) => {
            return apiClient.post<ModelTestResponse>('/api/v1/channel/test', data);
        },
        onError: (error) => {
            logger.error('渠道测试失败:', error);
        },
    });
}

export type ProxyTestResult = {
    ok: boolean;
    delay_ms: number;
    message: string;
};

export function useTestChannelProxy() {
    return useMutation({
        mutationFn: async (data: { channel_proxy: string; base_url: string }) => {
            return apiClient.post<ProxyTestResult>('/api/v1/channel/test-proxy', data);
        },
        onError: (error) => {
            logger.error('代理延迟测试失败:', error);
        },
    });
}

/**
 * 获取渠道模型列表 Hook
 * 
 * @example
 * const fetchModel = useFetchModel();
 * 
 * fetchModel.mutate({
 *   type: ChannelType.OpenAIChat,
 *   base_urls: [{ url: 'https://api.openai.com', delay: 0 }],
 *   keys: [{ enabled: true, channel_key: 'sk-xxx' }],
 *   proxy: false,
 * });
 * 
 * // 在 onSuccess 中获取模型列表
 * fetchModel.data // ['gpt-4', 'gpt-3.5-turbo', ...]
 */
export function useFetchModel() {
    return useMutation({
        mutationFn: async (data: FetchModelRequest) => {
            return apiClient.post<string[]>('/api/v1/channel/fetch-model', data);
        },
        onSuccess: (data) => {
            logger.log('模型列表获取成功:', data);
        },
        onError: (error) => {
            logger.error('模型列表获取失败:', error);
        },
    });
}

/**
 * 获取渠道最后同步时间 Hook
 * 
 * @example
 * const lastSyncTime = useLastSyncTime();
 * 
 * if (lastSyncTime) {
 *   console.log('最后同步时间:', new Date(lastSyncTime).toLocaleString());
 * }
 */
export function useLastSyncTime() {
    return useQuery({
        queryKey: ['channels', 'last-sync-time'],
        queryFn: async () => {
            return apiClient.get<string>('/api/v1/channel/last-sync-time');
        },
        refetchInterval: 30000,
    });
}
/**
 * 同步渠道 Hook
 * 
 * @example
 * const syncChannel = useSyncChannel();
 * 
 * syncChannel.mutate();
 */
export function useSyncChannel() {
    const queryClient = useQueryClient();
    return useMutation({
        mutationFn: async () => {
            return apiClient.post<null>('/api/v1/channel/sync');
        },
        onSuccess: () => {
            logger.log('渠道同步成功');
            queryClient.invalidateQueries({ queryKey: ['channels', 'last-sync-time'] });
            queryClient.invalidateQueries({ queryKey: ['channels', 'list'] });
            queryClient.invalidateQueries({ queryKey: ['models', 'channel'] });
            queryClient.invalidateQueries({ queryKey: ['models', 'list'] });
        },
        onError: (error) => {
            logger.error('渠道同步失败:', error);
        },
    });
}
