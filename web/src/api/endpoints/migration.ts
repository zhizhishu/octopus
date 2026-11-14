import { useQuery } from '@tanstack/react-query';
import { apiClient } from '../client';

export type MigrationJobStatus = 'queued' | 'running' | 'succeeded' | 'failed' | 'canceled';

export interface MigrationSummary {
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

export interface MigrationJobRequestView {
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

export interface MigrationJob {
    id: string;
    status: MigrationJobStatus;
    stage: string;
    percent: number;
    message: string;
    apply: boolean;
    can_apply: boolean;
    fingerprint: string;
    request: MigrationJobRequestView;
    summary?: MigrationSummary;
    error?: string;
    created_at: string;
    started_at?: string;
    finished_at?: string;
    updated_at: string;
}

// The frontend is a read-only view of backend migration jobs. Creation,
// cancellation, credentials and batch orchestration stay in server operations.
// Existing endpoint paths and JSON shapes are intentionally unchanged.
export function useMigrationJobs() {
    return useQuery({
        queryKey: ['migration', 'jobs'],
        queryFn: async () => apiClient.get<MigrationJob[]>('/api/v1/migration/newapi/jobs'),
        refetchInterval: 5000,
        refetchIntervalInBackground: false,
        refetchOnWindowFocus: false,
    });
}

export function useMigrationJob(id?: string, polling = false) {
    return useQuery({
        queryKey: ['migration', 'job', id],
        queryFn: async () => apiClient.get<MigrationJob>(`/api/v1/migration/newapi/jobs/${encodeURIComponent(id ?? '')}`),
        enabled: Boolean(id),
        refetchInterval: polling ? 1500 : false,
        refetchIntervalInBackground: false,
        refetchOnWindowFocus: false,
    });
}
