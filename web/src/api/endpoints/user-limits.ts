import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiClient } from '../client';

export interface UserLimitPolicy {
    user_id: number;
    enabled: boolean;
    timezone: string;
    anchor_at: number;
    five_hour_cost: number;
    weekly_cost: number;
    monthly_cost: number;
    five_hour_requests: number;
    weekly_requests: number;
    monthly_requests: number;
    allowed_models: string;
    revision: number;
    created_at: number;
    updated_at: number;
}

export interface UserLimitWindow {
    period: 'five_hour' | 'week' | 'month';
    start_at: number;
    end_at: number;
    cost_used: number;
    cost_reserved: number;
    accepted: number;
    failed_accepted: number;
    cost_limit: number;
    request_limit: number;
}

export interface UserLimitStatus {
    policy: UserLimitPolicy | null;
    windows: UserLimitWindow[];
    pending_receipts: number;
    realtime_supported: boolean;
    count_semantics: string;
    cost_semantics: string;
}

export function useUserLimits(userID: number) {
    return useQuery({
        queryKey: ['user-limits', userID],
        queryFn: () => apiClient.get<UserLimitStatus>(`/api/v1/user-limits/${userID}`),
        enabled: userID > 0,
        refetchInterval: 30_000,
    });
}

export function useCurrentUserLimits(enabled: boolean) {
    return useQuery({
        queryKey: ['user-limits', 'current'],
        queryFn: () => apiClient.get<UserLimitStatus>('/api/v1/user-limits/current'),
        enabled,
        refetchInterval: 30_000,
    });
}

export function useSaveUserLimits(userID: number) {
    const client = useQueryClient();
    return useMutation({
        mutationFn: (policy: UserLimitPolicy) => apiClient.put<UserLimitStatus>(`/api/v1/user-limits/${userID}`, policy),
        onSuccess: (result) => {
            client.setQueryData(['user-limits', userID], result);
            client.invalidateQueries({ queryKey: ['user-limits', 'current'] });
        },
    });
}
