import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiClient } from '../client';

export type RedTeamCase = {
    id: string;
    name: string;
    prompt: string;
    expected: string;
    max_tokens: number;
};
export type RedTeamConfig = {
    enabled: boolean;
    cases: RedTeamCase[];
};
export type RedTeamRun = {
    id: number;
    created_at: string;
    channel_id: number;
    channel_name: string;
    request_model: string;
    case_id: string;
    case_name: string;
    prompt_text: string;
    expected_text: string;
    response_text: string;
    response_clipped: boolean;
    status: 'running' | 'done' | 'failed';
    verdict: 'matched' | 'not_matched' | 'unscored';
    error_message: string;
    duration_ms: number;
    input_tokens: number;
    output_tokens: number;
};

const BASE = '/api/v1/redteam-lab';
export const blankRedTeamConfig = (): RedTeamConfig => ({ enabled: false, cases: [] });

export function useRedTeamConfig(enabled: boolean) {
    return useQuery({
        queryKey: ['redteam-lab', 'config'],
        queryFn: () => apiClient.get<RedTeamConfig>(`${BASE}/config`),
        enabled,
    });
}
export function useRedTeamHistory(enabled: boolean) {
    return useQuery({
        queryKey: ['redteam-lab', 'runs'],
        queryFn: () => apiClient.get<RedTeamRun[]>(`${BASE}/runs`),
        enabled,
    });
}
export function useSaveRedTeamConfig() {
    const cache = useQueryClient();
    return useMutation({
        mutationFn: (data: RedTeamConfig) => apiClient.put<RedTeamConfig>(`${BASE}/config`, data),
        onSuccess: (saved) => cache.setQueryData(['redteam-lab', 'config'], saved),
    });
}
export function useRunRedTeamCase() {
    const cache = useQueryClient();
    return useMutation({
        mutationFn: (args: { channel_id: number; model: string; case_id: string; confirm_cost: boolean }) =>
            apiClient.post<RedTeamRun>(`${BASE}/run`, args),
        onSettled: () => { void cache.invalidateQueries({ queryKey: ['redteam-lab', 'runs'] }); },
    });
}
