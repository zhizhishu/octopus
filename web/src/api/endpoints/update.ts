import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiClient } from '../client';
import { logger } from '@/lib/logger';

/**
 * 后端 /api/v1/update 返回的最新发布信息
 */
export interface LatestInfo {
    tag_name: string;
    published_at: string;
    body: string;
    message: string;
    source_repo?: string;
    update_repo?: string;
    update_url?: string;
    update_method?: string;
    update_hint?: string;
    future_build?: boolean;
}

export interface BuildInfo {
    version: string;
    commit: string;
    build_time: string;
    author: string;
    repo: string;
    image: string;
    image_tag: string;
    package_url: string;
    future_build: boolean;
    display_version: string;
}

export interface FutureLatestInfo {
    commit: string;
    commit_short: string;
    run_id: number;
    run_number: number;
    status: string;
    conclusion: string;
    created_at: string;
    updated_at: string;
    html_url: string;
    image: string;
    package_url: string;
    update_available: boolean;
    current_commit: string;
    message?: string;
}

/**
 * 获取最新发布信息 Hook
 * 
 * @example
 * const { data: latestInfo, isLoading, error } = useLatestInfo();
 * 
 * if (isLoading) return <Loading />;
 * if (error) return <Error message={error.message} />;
 * 
 * console.log('Latest tag:', latestInfo?.tag_name);
 */
export function useLatestInfo() {
    return useQuery({
        queryKey: ['update', 'latest'],
        queryFn: async () => {
            return apiClient.get<LatestInfo>('/api/v1/update');
        },
        refetchInterval: 3600000, // 1 小时
        refetchOnMount: 'always',
    });
}

/**
 * 获取后端当前版本 Hook
 *
 * 后端: GET /api/v1/update/now-version -> string
 */
export function useNowVersion() {
    return useQuery({
        queryKey: ['update', 'now-version'],
        queryFn: async () => {
            return apiClient.get<string>('/api/v1/update/now-version');
        },
        refetchInterval: 3600000, // 1 小时
        refetchOnMount: 'always',
    });
}

export function useBuildInfo() {
    return useQuery({
        queryKey: ['update', 'build-info'],
        queryFn: async () => {
            return apiClient.get<BuildInfo>('/api/v1/update/build-info');
        },
        refetchInterval: 3600000,
        refetchOnMount: 'always',
    });
}

export function useFutureLatestInfo(enabled = true) {
    return useQuery({
        queryKey: ['update', 'future-latest'],
        queryFn: async () => {
            return apiClient.get<FutureLatestInfo>('/api/v1/update/future-latest');
        },
        enabled,
        refetchInterval: 3600000,
        refetchOnMount: 'always',
        retry: 1,
    });
}

/**
 * 执行更新 Hook
 * 
 * @example
 * const updateCore = useUpdateCore();
 * 
 * updateCore.mutate(undefined, {
 *   onSuccess: () => {
 *     console.log('Update started successfully');
 *   },
 * });
 */
export function useUpdateCore() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: async () => {
            return apiClient.post<string>('/api/v1/update');
        },
        onSuccess: (data) => {
            logger.log('更新成功:', data);
            queryClient.invalidateQueries({ queryKey: ['update', 'latest'] });
            queryClient.invalidateQueries({ queryKey: ['update', 'now-version'] });
            queryClient.invalidateQueries({ queryKey: ['update', 'build-info'] });
            queryClient.invalidateQueries({ queryKey: ['update', 'future-latest'] });
        },
        onError: (error) => {
            logger.error('更新失败:', error);
        },
    });
}

