import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiClient } from '../client';
import type { User } from './user';

export type RedeemCodeType = 'balance' | 'monthly';

export interface RedeemCode {
    id: number;
    code: string;
    type: RedeemCodeType;
    enabled: boolean;
    used: boolean;
    balance_amount: number;
    // 兼容后端字段：monthly_limit 在 UI 中表示月卡每日额度。
    monthly_limit: number;
    monthly_days: number;
    daily_quota?: number;
    valid_days?: number;
    created_by_user_id: number;
    used_by_user_id: number;
    used_at: number;
    note?: string;
    created_at: number;
    updated_at: number;
}

export interface GenerateRedeemCodeRequest {
    type: RedeemCodeType;
    count: number;
    balance_amount?: number;
    // 兼容后端字段：生成月卡时作为每日额度提交。
    monthly_limit?: number;
    monthly_days?: number;
    daily_quota?: number;
    valid_days?: number;
    note?: string;
}

export function useRedeemCodeList() {
    return useQuery({
        queryKey: ['redeem', 'list'],
        queryFn: () => apiClient.get<RedeemCode[]>('/api/v1/redeem/list'),
        refetchInterval: 30000,
    });
}

export function useGenerateRedeemCode() {
    const queryClient = useQueryClient();
    return useMutation({
        mutationFn: (data: GenerateRedeemCodeRequest) => apiClient.post<RedeemCode[]>('/api/v1/redeem/generate', data),
        onSuccess: () => queryClient.invalidateQueries({ queryKey: ['redeem', 'list'] }),
    });
}

export function useUpdateRedeemCode() {
    const queryClient = useQueryClient();
    return useMutation({
        mutationFn: (data: { id: number; enabled: boolean; note?: string }) => apiClient.post<RedeemCode>('/api/v1/redeem/update', data),
        onSuccess: () => queryClient.invalidateQueries({ queryKey: ['redeem', 'list'] }),
    });
}

export function useDeleteRedeemCode() {
    const queryClient = useQueryClient();
    return useMutation({
        mutationFn: (id: number) => apiClient.delete<null>(`/api/v1/redeem/delete/${id}`),
        onSuccess: () => queryClient.invalidateQueries({ queryKey: ['redeem', 'list'] }),
    });
}

export function useRedeemCode() {
    return useMutation({
        mutationFn: (code: string) => apiClient.post<User>('/api/v1/redeem/redeem', { code }),
    });
}
