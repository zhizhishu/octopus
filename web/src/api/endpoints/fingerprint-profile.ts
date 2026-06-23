import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiClient } from '../client';
import { logger } from '@/lib/logger';

/**
 * 指纹 Profile：一组可选的上游设备身份。渠道 cloak.profile_id 选中其中一条，
 * 让两个渠道（例如走不同出口 IP 的两把上游 key）呈现不同设备，而不是共用
 * 全局唯一指纹。
 *
 * 每个 UA / Header 字段都是可选的：留空则回落到对应的全局默认设置。
 * seed 留空 => 后端首次保存时自动生成并持久化一个随机种子。
 * claude_stabilize 是三态布尔：null = 跟随全局设置，否则为该 profile 的显式选择。
 */
export interface FingerprintProfile {
    id: number;
    name: string;
    seed: string;
    claude_user_agent: string;
    claude_package_version: string;
    claude_runtime_version: string;
    claude_os: string;
    claude_arch: string;
    claude_timeout: string;
    claude_stabilize: boolean | null;
    codex_user_agent: string;
    codex_originator: string;
    codex_beta_features: string;
    generic_ua: string;
}

/**
 * 创建指纹 Profile 请求：name 必填，其余留空则回落全局默认。
 */
export type CreateFingerprintProfileRequest = {
    name: string;
    seed?: string;
    claude_user_agent?: string;
    claude_package_version?: string;
    claude_runtime_version?: string;
    claude_os?: string;
    claude_arch?: string;
    claude_timeout?: string;
    claude_stabilize?: boolean | null;
    codex_user_agent?: string;
    codex_originator?: string;
    codex_beta_features?: string;
    generic_ua?: string;
};

/**
 * 更新指纹 Profile 请求：id 必填 + 各字段可选指针，只更新提供的字段。
 */
export type UpdateFingerprintProfileRequest = {
    id: number;
    name?: string;
    seed?: string;
    claude_user_agent?: string;
    claude_package_version?: string;
    claude_runtime_version?: string;
    claude_os?: string;
    claude_arch?: string;
    claude_timeout?: string;
    claude_stabilize?: boolean | null;
    codex_user_agent?: string;
    codex_originator?: string;
    codex_beta_features?: string;
    generic_ua?: string;
};

/**
 * 获取指纹 Profile 列表 Hook
 *
 * @example
 * const { data: profiles, isLoading } = useFingerprintProfileList();
 */
export function useFingerprintProfileList(options?: { enabled?: boolean }) {
    return useQuery({
        queryKey: ['fingerprint-profiles', 'list'],
        queryFn: async () => {
            const data = await apiClient.get<FingerprintProfile[] | null>('/api/v1/fingerprint-profile/list');
            return data ?? [];
        },
        enabled: options?.enabled ?? true,
        refetchOnMount: 'always',
    });
}

/**
 * 创建指纹 Profile Hook
 *
 * @example
 * const createProfile = useCreateFingerprintProfile();
 * createProfile.mutate({ name: 'macOS Claude' });
 */
export function useCreateFingerprintProfile() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: async (data: CreateFingerprintProfileRequest) => {
            return apiClient.post<FingerprintProfile>('/api/v1/fingerprint-profile/create', data);
        },
        onSuccess: (data) => {
            logger.log('指纹 Profile 创建成功:', data);
            queryClient.invalidateQueries({ queryKey: ['fingerprint-profiles', 'list'] });
        },
        onError: (error) => {
            logger.error('指纹 Profile 创建失败:', error);
        },
    });
}

/**
 * 更新指纹 Profile Hook
 *
 * @example
 * const updateProfile = useUpdateFingerprintProfile();
 * updateProfile.mutate({ id: 1, claude_os: 'macOS' });
 */
export function useUpdateFingerprintProfile() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: async (data: UpdateFingerprintProfileRequest) => {
            return apiClient.post<FingerprintProfile>('/api/v1/fingerprint-profile/update', data);
        },
        onSuccess: (data) => {
            logger.log('指纹 Profile 更新成功:', data);
            queryClient.invalidateQueries({ queryKey: ['fingerprint-profiles', 'list'] });
        },
        onError: (error) => {
            logger.error('指纹 Profile 更新失败:', error);
        },
    });
}

/**
 * 删除指纹 Profile Hook
 *
 * @example
 * const deleteProfile = useDeleteFingerprintProfile();
 * deleteProfile.mutate(1); // 删除 ID 为 1 的 profile
 */
export function useDeleteFingerprintProfile() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: async (id: number) => {
            return apiClient.delete<null>(`/api/v1/fingerprint-profile/delete/${id}`);
        },
        onSuccess: () => {
            logger.log('指纹 Profile 删除成功');
            queryClient.invalidateQueries({ queryKey: ['fingerprint-profiles', 'list'] });
        },
        onError: (error) => {
            logger.error('指纹 Profile 删除失败:', error);
        },
    });
}
