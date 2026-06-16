import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiClient } from '../client';

export type NewAPIMigrationJobStatus = 'queued' | 'running' | 'succeeded' | 'failed' | 'canceled';

export interface NewAPIMigrationSummary {
    dry_run: boolean;
    quota_per_unit: number;
    source_users: number;
    active_users: number;
    inactive_users_skipped: number;
    users_created: number;
    users_merged: number;
    users_renamed: number;
    users_skipped_conflict: number;
    users_skipped_invalid: number;
    api_keys_considered: number;
    api_keys_created: number;
    api_keys_skipped: number;
    logs_considered: number;
    logs_created: number;
    logs_skipped: number;
    stats_updated: boolean;
    imported_balance: number;
    imported_used_quota: number;
    imported_log_cost: number;
    warnings?: string[];
    applied_at?: string;
    source_reference: string;
    imported_api_key_prefix?: string;
    password_mode: string;
    conflict_strategy: string;
    preserve_admin_role: boolean;
    included_logs: boolean;
    included_api_keys: boolean;
}

export interface NewAPIMigrationJobRequestView {
    source_type: string;
    source_dsn: string;
    source_log_type: string;
    source_log_dsn?: string;
    apply: boolean;
    include_logs: boolean;
    include_api_keys: boolean;
    quota_per_unit: number;
    batch_size: number;
    conflict_strategy: string;
    password_mode: string;
    api_key_prefix: string;
    preserve_admin_role: boolean;
}

export interface NewAPIMigrationJob {
    id: string;
    status: NewAPIMigrationJobStatus;
    stage: string;
    percent: number;
    message: string;
    apply: boolean;
    can_apply: boolean;
    fingerprint: string;
    request: NewAPIMigrationJobRequestView;
    summary?: NewAPIMigrationSummary;
    error?: string;
    created_at: string;
    started_at?: string;
    finished_at?: string;
    updated_at: string;
}

export interface StartNewAPIMigrationJobRequest {
    source_type: string;
    source_dsn: string;
    source_log_type?: string;
    source_log_dsn?: string;
    apply?: boolean;
    confirm_apply?: boolean;
    dry_run_job_id?: string;
    include_logs: boolean;
    include_api_keys: boolean;
    quota_per_unit?: number;
    batch_size: number;
    conflict_strategy: 'skip' | 'merge' | 'rename';
    password_mode: 'preserve' | 'random' | 'disabled';
    api_key_prefix: string;
    preserve_admin_role: boolean;
}

export function useNewAPIMigrationJobs() {
    return useQuery({
        queryKey: ['migration', 'newapi', 'jobs'],
        queryFn: async () => apiClient.get<NewAPIMigrationJob[]>('/api/v1/migration/newapi/jobs'),
        refetchInterval: 5000,
    });
}

export function useNewAPIMigrationJob(id?: string, polling = false) {
    return useQuery({
        queryKey: ['migration', 'newapi', 'job', id],
        queryFn: async () => apiClient.get<NewAPIMigrationJob>(`/api/v1/migration/newapi/jobs/${id}`),
        enabled: Boolean(id),
        refetchInterval: polling ? 1500 : false,
    });
}

export function useStartNewAPIMigrationJob() {
    const queryClient = useQueryClient();
    return useMutation({
        mutationFn: async (data: StartNewAPIMigrationJobRequest) => (
            apiClient.post<NewAPIMigrationJob>('/api/v1/migration/newapi/jobs', data)
        ),
        onSuccess: (job) => {
            queryClient.invalidateQueries({ queryKey: ['migration', 'newapi', 'jobs'] });
            queryClient.setQueryData(['migration', 'newapi', 'job', job.id], job);
        },
    });
}

export function useCancelNewAPIMigrationJob() {
    const queryClient = useQueryClient();
    return useMutation({
        mutationFn: async (id: string) => apiClient.post<NewAPIMigrationJob>(`/api/v1/migration/newapi/jobs/${id}/cancel`, {}),
        onSuccess: (job) => {
            queryClient.invalidateQueries({ queryKey: ['migration', 'newapi', 'jobs'] });
            queryClient.setQueryData(['migration', 'newapi', 'job', job.id], job);
        },
    });
}
