import { useMutation, useQuery } from '@tanstack/react-query';
import { apiClient } from '../client';
import { logger } from '@/lib/logger';

export const AUDIT_PROBE_IDS = [
    'liveness',
    'identity',
    'glitch',
    'token_delta',
    'echo_rewrite',
    'context_canary',
    'signature',
] as const;

export type AuditProbeId = (typeof AUDIT_PROBE_IDS)[number];

export type AuditSeverity = 'low' | 'medium' | 'high';
export type AuditVerdict = 'none' | 'low' | 'medium' | 'high' | 'unknown';

export type AuditFinding = {
    probe: AuditProbeId;
    severity: AuditSeverity;
    score: number;
    title: string;
    evidence?: Record<string, unknown>;
    recommendation?: string;
};

export type AuditProbeResult = {
    probe_id: AuditProbeId;
    ok: boolean;
    data?: Record<string, unknown>;
    error?: string;
};

export type AuditProbeError = {
    probe: AuditProbeId;
    error: string;
};

export type AuditViewReport = {
    requested_model: string;
    requested_families?: string[];
    score: number;
    verdict: AuditVerdict;
    findings: AuditFinding[];
    results: AuditProbeResult[];
    errors: AuditProbeError[];
    disclaimer: string;
};

export type ModelAuditRequest = {
    channel_id: number;
    model: string;
    probes?: AuditProbeId[];
    timeout_seconds?: number;
};

export type ModelAuditResponse = {
    channel_id: number;
    channel_name: string;
    upstream_model: string;
    endpoint: string;
    duration_ms: number;
    report: AuditViewReport;
};

export type LogAnomalyFinding = {
    code: string;
    severity: 'low' | 'medium';
    channel_id: number;
    channel_name: string;
    model: string;
    title: string;
    evidence?: Record<string, unknown>;
};

export type LogAnomalyReport = {
    window_hours: number;
    sample_count: number;
    bucket_count: number;
    findings: LogAnomalyFinding[];
    disclaimer: string;
};

export function useLogAnomalies(enabled = true) {
    return useQuery({
        queryKey: ['model-audit', 'log-anomalies'],
        queryFn: async () => apiClient.get<LogAnomalyReport>('/api/v1/model-audit/log-anomalies'),
        enabled,
        staleTime: 30_000,
    });
}

export function useRunModelAudit() {
    return useMutation({
        mutationFn: async (data: ModelAuditRequest) => {
            return apiClient.post<ModelAuditResponse>('/api/v1/model-audit/run', data);
        },
        onError: (error) => {
            logger.error('模型审计失败:', error);
        },
    });
}
