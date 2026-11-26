import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiClient } from '../client';
import { logger } from '@/lib/logger';
import { useAuthStore, type UserUsage, type UserUsageRange, type UserBatchResult } from './user';
import {
    StatsAPIKeyUsage,
    StatsAPIKeyUsageFormatted,
    formatStatsAPIKeyUsage,
} from './stats';

export const API_KEY_ENDPOINT_FAMILIES = ['openai-compatible', 'video', 'gemini', 'anthropic'] as const;
export type APIKeyEndpointFamily = typeof API_KEY_ENDPOINT_FAMILIES[number];
export type APIKeyEndpointFamilyValue = APIKeyEndpointFamily[] | string | null;

/**
 * API Key 数据
 */
export interface APIKey {
    id: number;
    user_id?: number;
    user_name?: string;
    name: string;
    api_key: string;
    enabled: boolean;
    expire_at?: number; // Unix 时间戳（秒），不传表示永不过期
    max_cost?: number; // 不传表示无限制
    supported_models?: string; // 不传表示支持所有模型
    access_plan_ids?: number[];
    endpoint_families?: APIKeyEndpointFamilyValue;
    endpoint_scopes?: APIKeyEndpointFamilyValue;
    allowed_endpoint_families?: APIKeyEndpointFamilyValue;
    endpoint_family_scopes?: APIKeyEndpointFamilyValue;
    default_access_plan_id?: number;
    /** 最近一次转发的时间（Unix 秒，0 = 没用过）和来源 IP。 */
    last_used_at?: number;
    last_used_ip?: string;
    access_plans?: Array<{
        id: number;
        slug: string;
        display_name: string;
        enabled: boolean;
        is_default: boolean;
    }>;
}

/**
 * API Key Stats 响应（包含 stats 和 info）
 */
export interface APIKeyStatsResponse {
    stats: StatsAPIKeyUsage;
    info: APIKey;
}

export interface APIKeyStatsResponseFormatted {
    stats: StatsAPIKeyUsageFormatted;
    info: APIKey;
}

/**
 * API Key 登录 Hook（仅校验 key 是否有效）
 */
export function useAPIKeyLogin() {
    const { setAPIKeyAuth, logout } = useAuthStore();

    return useMutation({
        mutationFn: async (apiKey: string) => {
            // 先设置以便 apiClient 发送请求时带上 token
            setAPIKeyAuth(apiKey);
            await apiClient.get<null>('/api/v1/apikey/login');
            return apiKey;
        },
        onError: (error) => {
            logout();
            logger.error('API Key 登录失败:', error);
        },
    });
}

/**
 * 获取当前 API Key 的详细统计数据 Hook（仅 API Key 登录用户使用）
 */
export function useAPIKeyDashboardStats() {
    const { isAPIKeyAuth, isAuthenticated } = useAuthStore();

    return useQuery({
        queryKey: ['apikey', 'dashboard', 'stats'],
        queryFn: () => apiClient.get<APIKeyStatsResponse>('/api/v1/apikey/stats'),
        select: (data): APIKeyStatsResponseFormatted => ({
            stats: formatStatsAPIKeyUsage(data.stats),
            info: data.info,
        }),
        enabled: isAPIKeyAuth && isAuthenticated,
        refetchInterval: 30000,
    });
}

/**
 * 创建 API Key 请求
 */
export type CreateAPIKeyRequest = Omit<APIKey, 'id' | 'api_key'> & { enabled?: boolean };

/**
 * 更新 API Key 请求
 */
export type UpdateAPIKeyRequest = Pick<APIKey, 'id'> & CreateAPIKeyRequest;

/**
 * 获取 API Key 列表 Hook
 * 
 * @example
 * const { data: apiKeys, isLoading, error } = useAPIKeyList();
 * 
 * if (isLoading) return <Loading />;
 * if (error) return <Error message={error.message} />;
 * 
 * apiKeys?.forEach(key => console.log(key.name));
 */
export function useAPIKeyList() {
    return useQuery({
        queryKey: ['apikeys', 'list'],
        queryFn: async () => {
            return apiClient.get<APIKey[]>('/api/v1/apikey/list');
        },
        refetchInterval: 30000,
    });
}

/**
 * 创建 API Key Hook
 * 
 * @example
 * const createAPIKey = useCreateAPIKey();
 * 
 * createAPIKey.mutate({
 *   name: 'My API Key',
 * });
 */
export function useCreateAPIKey() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: async (data: CreateAPIKeyRequest) => {
            return apiClient.post<APIKey>('/api/v1/apikey/create', data);
        },
        onSuccess: (data) => {
            logger.log('API Key 创建成功:', data);
            queryClient.invalidateQueries({ queryKey: ['apikeys', 'list'] });
        },
        onError: (error) => {
            logger.error('API Key 创建失败:', error);
        },
    });
}

/**
 * 更新 API Key Hook
 * 
 * @example
 * const updateAPIKey = useUpdateAPIKey();
 * 
 * updateAPIKey.mutate({
 *   id: 1,
 *   name: 'Updated API Key',
 *   enabled: false,
 * });
 */
export function useUpdateAPIKey() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: async (data: UpdateAPIKeyRequest) => {
            return apiClient.post<APIKey>('/api/v1/apikey/update', data);
        },
        onSuccess: (data) => {
            logger.log('API Key 更新成功:', data);
            queryClient.invalidateQueries({ queryKey: ['apikeys', 'list'] });
        },
        onError: (error) => {
            logger.error('API Key 更新失败:', error);
        },
    });
}

/**
 * 删除 API Key Hook
 * 
 * @example
 * const deleteAPIKey = useDeleteAPIKey();
 * 
 * deleteAPIKey.mutate(1); // 删除 ID 为 1 的 API Key
 */
export function useDeleteAPIKey() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: async (id: number) => {
            return apiClient.delete<null>(`/api/v1/apikey/delete/${id}`);
        },
        onSuccess: () => {
            logger.log('API Key 删除成功');
            queryClient.invalidateQueries({ queryKey: ['apikeys', 'list'] });
        },
        onError: (error) => {
            logger.error('API Key 删除失败:', error);
        },
    });
}

/**
 * 获取当前 API Key 的统计数据 Hook
 * 
 * 此接口使用 API Key 认证，通过 API Key 获取对应的统计数据
 * 
 * @example
 * const { data: stats, isLoading } = useAPIKeyStats();
 */
export function useAPIKeyStats() {
    return useQuery({
        queryKey: ['apikey', 'stats'],
        queryFn: async () => {
            return apiClient.get<StatsAPIKeyUsage>('/api/v1/apikey/stats');
        },
        select: formatStatsAPIKeyUsage,
        refetchInterval: 30000,
        refetchOnMount: 'always',
    });
}

export type APIKeyBatchAction = 'enable' | 'disable' | 'delete';

/** 批量启用 / 停用 / 删除。逐个执行，谁没成功、为什么都会报出来（形状同 /user/batch）。 */
export function useBatchAPIKeys() {
    const queryClient = useQueryClient();
    return useMutation({
        mutationFn: (data: { ids: number[]; action: APIKeyBatchAction }) =>
            apiClient.post<UserBatchResult>('/api/v1/apikey/batch', data),
        onSuccess: () => {
            queryClient.invalidateQueries({ queryKey: ['apikeys'] });
            queryClient.invalidateQueries({ queryKey: ['stats', 'apikey'] });
        },
    });
}

/** 一把 Key 在某段时间的用量（形状同用户用量，多一个 api_key_id）。 */
export type APIKeyUsage = UserUsage & { api_key_id?: number };

export function useAPIKeyUsage(id: number | undefined, range: UserUsageRange) {
    return useQuery({
        queryKey: ['apikeys', 'usage', id, range],
        queryFn: () => apiClient.get<APIKeyUsage>(`/api/v1/apikey/usage?id=${id}&range=${range}`),
        enabled: !!id,
    });
}

/**
 * 某个用户的 Key 能限定的模型：他的方案里当前有可用渠道的模型。
 * 普通用户只能看自己的（userID 会被后端忽略）；管理员给别人建 Key 时带上那个人的 id。
 */
export function useAPIKeyModels(userID: number | undefined, options?: { enabled?: boolean }) {
    return useQuery({
        queryKey: ['apikeys', 'models', userID ?? 0],
        queryFn: () => apiClient.get<string[] | null>(`/api/v1/apikey/models${userID ? `?user_id=${userID}` : ''}`),
        select: (data) => data ?? [],
        enabled: options?.enabled ?? true,
    });
}
