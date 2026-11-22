import { useQuery } from '@tanstack/react-query';
import { apiClient } from '../client';
import { useAuthStore } from './user';
import type { APIKey } from './apikey';
import type { LogUsage } from '@/components/modules/dashboard/model';

export interface UsageReportRow {
    id: string;
    name: string;
    usage: LogUsage;
    models: UsageReportRow[];
}

export interface UsageReport {
    start: string;
    end: string;
    group: string;
    summary: LogUsage;
    daily: { date: string; cost: number; models: Record<string, number> }[];
    rows: UsageReportRow[];
}

export interface UsageReportQuery {
    range: 'today' | '7d' | '30d' | 'custom';
    group: 'user' | 'apikey' | 'model' | 'day';
    start?: string;
    end?: string;
    user_id?: number;
    api_key_id?: number;
    model?: string;
}

export function usageReportParams(query: UsageReportQuery): Record<string, string | number> {
    return Object.fromEntries(Object.entries(query).filter(([, value]) => value !== undefined && value !== ''));
}

export function useUsageReport(query: UsageReportQuery) {
    const user = useAuthStore(s => s.user);
    return useQuery({
        queryKey: ['usage-report', user?.id, user?.role, query],
        queryFn: ({ signal }) => apiClient.get<UsageReport>('/api/v1/stats/usage-report', usageReportParams(query), { signal }),
        enabled: !!user && (query.range !== 'custom' || !!(query.start && query.end)),
        refetchInterval: 30000,
    });
}

export function useUsageReportKeys() {
    const user = useAuthStore(s => s.user);
    return useQuery({
        queryKey: ['usage-report-keys', user?.id, user?.role],
        queryFn: async () => (await apiClient.get<APIKey[]>('/api/v1/apikey/list'))
            .map(({ id, name, user_id }) => ({ id, name, user_id })),
        enabled: !!user,
    });
}

export async function downloadUsageReport(query: UsageReportQuery) {
    const params = new URLSearchParams(Object.entries(usageReportParams(query)).map(([k, v]) => [k, String(v)]));
    const blob = await apiClient.getBlob(`/api/v1/stats/usage-report.csv?${params}`);
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = 'usage-report.csv';
    a.click();
    URL.revokeObjectURL(url);
}
