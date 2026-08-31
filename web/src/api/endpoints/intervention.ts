import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiClient } from '../client';
import type { ChannelAttempt } from './log';

export type InterventionSnapshot = {
    id: string;
    log_id: number;
    request_model: string;
    endpoint: string;
    attempts: ChannelAttempt[];
    last_error: string;
    created_at: string;
    waiting_for: string;
    status?: string;
    rescue_round?: number;
    next_retry_at?: string | null;
};

export type InterventionRetryRequest = {
    channel_id: number;
    key_id?: number;
    model_name?: string;
};

export function useInterventionList(options?: { enabled?: boolean }) {
    return useQuery({
        queryKey: ['interventions', 'list'],
        queryFn: () => apiClient.get<InterventionSnapshot[]>('/api/v1/intervention/list'),
        enabled: options?.enabled ?? true,
        refetchInterval: 2000,
    });
}

export function useRetryIntervention() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: ({ id, request }: { id: string; request: InterventionRetryRequest }) =>
            apiClient.post<null>(`/api/v1/intervention/${id}/retry`, request),
        onSuccess: () => {
            queryClient.invalidateQueries({ queryKey: ['interventions', 'list'] });
            queryClient.invalidateQueries({ queryKey: ['logs'] });
        },
    });
}

export function useAbortIntervention() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: (id: string) => apiClient.post<null>(`/api/v1/intervention/${id}/abort`),
        onSuccess: () => {
            queryClient.invalidateQueries({ queryKey: ['interventions', 'list'] });
            queryClient.invalidateQueries({ queryKey: ['logs'] });
        },
    });
}
