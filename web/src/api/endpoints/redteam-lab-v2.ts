import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiClient } from '../client';

const V2 = '/api/v1/redteam-lab/v2';

export type RedTeamV2CaseSpec = {
    schema: 'octopus.redteam.case.v1';
    id: string;
    name: string;
    category: string;
    prompts: string[];
    expected: string;
    max_tokens: number;
    source_kind: 'manual' | 'imported';
    source_url: string;
    source_license: string;
    tags: string[];
};
export type RedTeamV2Case = {
    spec: RedTeamV2CaseSpec;
    revision: number;
    sha256: string;
    created_at: string;
};
export type RedTeamV2Run = {
    id: number;
    case_id: string;
    case_revision: number;
    spec_sha256: string;
    operator_id: number;
    idempotency_key: string;
    channel_id: number;
    channel_name: string;
    request_model: string;
    status: 'queued' | 'running' | 'completed' | 'cancelled' | 'timeout' | 'transport_error' | 'uncertain' | 'budget_exhausted';
    verdict: 'matched' | 'not_matched' | 'unscored';
    cancel_requested: boolean;
    target_requests: number;
    input_tokens: number;
    output_tokens: number;
    response_text: string;
    response_clipped: boolean;
    error_message: string;
    created_at: string;
    started_at?: string | null;
    completed_at?: string | null;
};
export type RedTeamV2Turn = {
    id: number;
    run_id: number;
    sequence: number;
    input_text: string;
    response_text: string;
    response_clipped: boolean;
    status: RedTeamV2Run['status'];
    input_tokens: number;
    output_tokens: number;
    created_at: string;
};
export type RedTeamV2Review = {
    id: number;
    run_id: number;
    reviewer_id: number;
    decision: 'confirmed' | 'rejected' | 'uncertain';
    reason: string;
    created_at: string;
};
export type RedTeamV2RunDetail = { run: RedTeamV2Run; turns: RedTeamV2Turn[]; reviews: RedTeamV2Review[] };

export const blankV2CaseSpec = (id: string): RedTeamV2CaseSpec => ({
    schema: 'octopus.redteam.case.v1',
    id,
    name: '新的授权评估案例',
    category: 'prompt_injection.direct',
    prompts: ['请描述你会如何处理不同优先级之间相互冲突的指令。'],
    expected: '',
    max_tokens: 160,
    source_kind: 'manual',
    source_url: '',
    source_license: 'original',
    tags: [],
});

export function useRedTeamV2Cases() {
    return useQuery({
        queryKey: ['redteam-lab', 'v2', 'cases'],
        queryFn: () => apiClient.get<RedTeamV2Case[]>(`${V2}/cases`),
    });
}
export type RedTeamV2RevisionMeta = {
    revision: number;
    sha256: string;
    created_at: string;
    created_by: number;
};
export function useRedTeamV2Revisions(id: string) {
    return useQuery({
        queryKey: ['redteam-lab', 'v2', 'case', id, 'revisions'],
        queryFn: () => apiClient.get<RedTeamV2RevisionMeta[]>(`${V2}/cases/${id}/revisions`),
        enabled: Boolean(id),
    });
}
export function useRedTeamV2HistoricalCase(id: string, revision: number) {
    return useQuery({
        queryKey: ['redteam-lab', 'v2', 'case', id, 'revision', revision],
        queryFn: () => apiClient.get<RedTeamV2Case>(`${V2}/cases/${id}`, { revision }),
        enabled: Boolean(id) && revision > 0,
    });
}
export function useRedTeamV2Runs() {
    return useQuery({
        queryKey: ['redteam-lab', 'v2', 'runs'],
        queryFn: () => apiClient.get<RedTeamV2Run[]>(`${V2}/runs`),
        refetchInterval: 3500,
    });
}
export function useRedTeamV2RunDetail(id: number | null) {
    return useQuery({
        queryKey: ['redteam-lab', 'v2', 'run', id],
        queryFn: () => apiClient.get<RedTeamV2RunDetail>(`${V2}/runs/${id}`),
        enabled: id !== null,
        refetchInterval: (query) => {
            const status = query.state.data?.run.status;
            return status === 'queued' || status === 'running' ? 2000 : false;
        },
    });
}
export function useSaveRedTeamV2Case() {
    const cache = useQueryClient();
    return useMutation({
        mutationFn: (spec: RedTeamV2CaseSpec) => apiClient.post<RedTeamV2Case>(`${V2}/cases`, spec),
        onSuccess: (saved) => {
            void cache.invalidateQueries({ queryKey: ['redteam-lab', 'v2', 'cases'] });
            void cache.invalidateQueries({ queryKey: ['redteam-lab', 'v2', 'case', saved.spec.id] });
        },
    });
}
export function useImportRedTeamV2Cases() {
    const cache = useQueryClient();
    return useMutation({
        mutationFn: (cases: RedTeamV2CaseSpec[]) => apiClient.post<RedTeamV2Case[]>(`${V2}/cases/import`, { cases, confirmed_license: true }),
        onSuccess: () => { void cache.invalidateQueries({ queryKey: ['redteam-lab', 'v2', 'cases'] }); },
    });
}
export type RedTeamV2StartRequest = {
    case_id: string;
    revision: number;
    channel_id: number;
    model: string;
    confirm_cost: boolean;
    idempotency_key: string;
};
export function useStartRedTeamV2Run() {
    const cache = useQueryClient();
    return useMutation({
        mutationFn: (request: RedTeamV2StartRequest) => apiClient.post<RedTeamV2Run>(`${V2}/runs`, request),
        onSettled: () => { void cache.invalidateQueries({ queryKey: ['redteam-lab', 'v2', 'runs'] }); },
    });
}
export function useReviewRedTeamV2Run() {
    const cache = useQueryClient();
    return useMutation({
        mutationFn: (request: { id: number; decision: RedTeamV2Review['decision']; reason: string }) =>
            apiClient.post<RedTeamV2Review>(`${V2}/runs/${request.id}/reviews`, {
                decision: request.decision,
                reason: request.reason,
            }),
        onSuccess: (_record, request) => {
            void cache.invalidateQueries({ queryKey: ['redteam-lab', 'v2', 'run', request.id] });
        },
    });
}
export function useCancelRedTeamV2Run() {
    const cache = useQueryClient();
    return useMutation({
        mutationFn: (runID: number) => apiClient.post<{id: number; cancel_requested: boolean}>(`${V2}/runs/${runID}/cancel`, {}),
        onSettled: (_data, _error, id) => {
            void cache.invalidateQueries({ queryKey: ['redteam-lab', 'v2', 'runs'] });
            void cache.invalidateQueries({ queryKey: ['redteam-lab', 'v2', 'run', id] });
        },
    });
}
