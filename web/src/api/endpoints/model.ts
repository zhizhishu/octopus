import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiClient } from '../client';
import { logger } from '@/lib/logger';
import type { ChannelType } from './channel';

/**
 * LLM 价格信息
 */
export interface LLMPrice {
    input: number;
    output: number;
    cache_read: number;
    cache_write: number;
}

/**
 * LLM 模型信息
 */
export interface LLMInfo extends LLMPrice {
    name: string;
}

/**
 * LLM 渠道关联信息
 */
export interface LLMChannel {
    name: string;
    enabled: boolean;
    channel_id: number;
    channel_name: string;
    channel_type?: ChannelType;
}

export interface ModelTestRequest {
    model?: string;
    models?: string[];
    channel_id?: number;
    access_plan_slug?: string;
    endpoint?: string;
    prompt?: string;
    stream?: boolean;
    concurrency?: number;
    timeout_seconds?: number;
    user_id?: number;
    api_key_id?: number;
    audit_log?: boolean;
}

export interface ModelTestAttempt {
    channel_id: number;
    channel_key_id?: number;
    channel_name: string;
    model_name: string;
    upstream_path?: string;
    attempt_num: number;
    status: 'success' | 'failed' | 'skipped' | 'circuit_break';
    duration: number;
    sticky?: boolean;
    proxy_used?: boolean;
    proxy_source?: string;
    proxy_scheme?: string;
    proxy_target?: string;
    proxy_status?: number;
    msg?: string;
}

export interface ModelTestResult {
    model: string;
    request_model: string;
    upstream_model?: string;
    access_plan_slug?: string;
    access_plan_name?: string;
    access_plan_id?: number;
    request_endpoint?: string;
    request_path?: string;
    upstream_path?: string;
    route_used: boolean;
    route_fallback_used?: boolean;
    group_name?: string;
    channel_id?: number;
    channel_name?: string;
    channel_key_id?: number;
    status_code?: number;
    success: boolean;
    duration_ms: number;
    error?: string;
    error_code?: string;
    response_preview?: string;
    proxy_used?: boolean;
    proxy_source?: string;
    proxy_scheme?: string;
    proxy_target?: string;
    proxy_status?: number;
    input_tokens?: number;
    output_tokens?: number;
    attempts?: ModelTestAttempt[];
}

export interface ModelTestResponse {
    summary: {
        total: number;
        success: number;
        failed: number;
        duration_ms: number;
    };
    results: ModelTestResult[];
}

/** 某端点将呈现的上游身份（shape + UA），只读，与真实测试一字节一致 */
export interface EndpointIdentity {
    shape: 'codex' | 'claude' | 'generic';
    user_agent: string;
    detail?: string;
}

/** 渠道测试的逐端点身份，来自渠道的 fingerprint profile（测试永远用渠道真实 profile） */
export interface ChannelTestIdentity {
    profile_id: number;
    profile_name?: string;
    codex: EndpointIdentity;   // Responses 端点
    claude: EndpointIdentity;  // Claude 端点
    generic: EndpointIdentity; // Chat / Gemini 端点
}

/**
 * 获取渠道测试将呈现的身份（每端点 shape + UA）。只读、只用渠道真实 profile。
 */
export function useChannelTestIdentity(channelId: number | undefined, enabled: boolean) {
    return useQuery({
        queryKey: ['model', 'test-identity', channelId],
        queryFn: async () => {
            return apiClient.get<ChannelTestIdentity>(`/api/v1/model/test-identity?channel_id=${channelId}`);
        },
        enabled: enabled && !!channelId && channelId > 0,
        staleTime: 60_000,
    });
}

/**
 * 获取 LLM 模型列表 Hook
 * 
 * @example
 * const { data: models, isLoading, error } = useModelList();
 * 
 * if (isLoading) return <Loading />;
 * if (error) return <Error message={error.message} />;
 * 
 * models?.forEach(model => console.log(model.name, model.input));
 */
export function useModelList() {
    return useQuery({
        queryKey: ['models', 'list'],
        queryFn: async () => {
            return apiClient.get<LLMInfo[]>('/api/v1/model/list');
        },
        refetchInterval: 30000,
        refetchOnMount: 'always',
    });
}

/**
 * 获取 LLM 模型与渠道关联列表 Hook
 * 
 * @example
 * const { data: channelModels, isLoading, error } = useModelChannelList();
 * 
 * if (isLoading) return <Loading />;
 * if (error) return <Error message={error.message} />;
 * 
 * channelModels?.forEach(item => console.log(item.name, item.channel_name));
 */
export function useModelChannelList(options: { enabled?: boolean } = {}) {
    return useQuery({
        queryKey: ['models', 'channel'],
        queryFn: async () => {
            return apiClient.get<LLMChannel[]>('/api/v1/model/channel');
        },
        enabled: options.enabled ?? true,
        refetchInterval: 30000,
        refetchOnMount: 'always',
    });
}

export function useModelTest() {
    return useMutation({
        mutationFn: async (data: ModelTestRequest) => {
            return apiClient.post<ModelTestResponse>('/api/v1/model/test', data);
        },
        onError: (error) => {
            logger.error('模型测试失败:', error);
        },
    });
}

/**
 * 更新 LLM 模型 Hook
 * 
 * @example
 * const updateModel = useUpdateModel();
 * 
 * updateModel.mutate({
 *   name: 'gpt-4',
 *   input: 0.03,
 *   output: 0.06,
 *   cache_read: 0.015,
 *   cache_write: 0.03,
 * });
 */
export function useUpdateModel() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: async (data: LLMInfo) => {
            return apiClient.post<LLMInfo>('/api/v1/model/update', data);
        },
        onSuccess: (data) => {
            logger.log('模型更新成功:', data);
            queryClient.invalidateQueries({ queryKey: ['models', 'list'] });
        },
        onError: (error) => {
            logger.error('模型更新失败:', error);
        },
    });
}

/**
 * 创建 LLM 模型 Hook
 * 
 * @example
 * const createModel = useCreateModel();
 * 
 * createModel.mutate({
 *   name: 'gpt-4',
 *   input: 0.03,
 *   output: 0.06,
 *   cache_read: 0.015,
 *   cache_write: 0.03,
 * });
 */
export function useCreateModel() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: async (data: LLMInfo) => {
            return apiClient.post<LLMInfo>('/api/v1/model/create', data);
        },
        onSuccess: (data) => {
            logger.log('模型创建成功:', data);
            queryClient.invalidateQueries({ queryKey: ['models', 'list'] });
        },
        onError: (error) => {
            logger.error('模型创建失败:', error);
        },
    });
}

/**
 * 删除 LLM 模型 Hook
 * 
 * @example
 * const deleteModel = useDeleteModel();
 * 
 * deleteModel.mutate('gpt-4'); // 删除名称为 'gpt-4' 的模型
 */
export function useDeleteModel() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: async (name: string) => {
            return apiClient.post<null>('/api/v1/model/delete', { name });
        },
        onSuccess: () => {
            logger.log('模型删除成功');
            queryClient.invalidateQueries({ queryKey: ['models', 'list'] });
        },
        onError: (error) => {
            logger.error('模型删除失败:', error);
        },
    });
}

/**
 * 更新 LLM 模型价格 Hook
 * 
 * @example
 * const updatePrice = useUpdateModelPrice();
 * 
 * updatePrice.mutate(); // 触发价格更新
 */
export function useUpdateModelPrice() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: async () => {
            return apiClient.post<null>('/api/v1/model/update-price', {});
        },
        onSuccess: () => {
            logger.log('模型价格更新成功');
            queryClient.invalidateQueries({ queryKey: ['models', 'list'] });
            queryClient.invalidateQueries({ queryKey: ['models', 'last-update-time'] });
        },
        onError: (error) => {
            logger.error('模型价格更新失败:', error);
        },
    });
}

/**
 * 获取 LLM 模型价格最后更新时间 Hook
 * 
 * @example
 * const { data: lastUpdateTime } = useLastUpdateTime();
 * 
 * if (lastUpdateTime) {
 *   console.log('最后更新:', new Date(lastUpdateTime).toLocaleString());
 * }
 */
export function useLastUpdateTime() {
    return useQuery({
        queryKey: ['models', 'last-update-time'],
        queryFn: async () => {
            return apiClient.get<string>('/api/v1/model/last-update-time');
        },
        refetchInterval: 30000,
    });
}
