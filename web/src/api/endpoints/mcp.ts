import { useQuery } from '@tanstack/react-query';
import { apiClient } from '../client';

export interface MCPUsage {
    calls: number;
    images: number;
    cost: number;
    failed: number;
}

export interface MCPStats {
    totals: MCPUsage;
    top_keys: (MCPUsage & { api_key_id: number; name: string })[];
}

export function useMCPStats(userId?: number) {
    return useQuery({
        queryKey: ['mcp', 'stats', userId, 7],
        queryFn: () => apiClient.get<MCPStats>('/api/v1/mcp/stats?days=7'),
        enabled: !!userId,
        refetchInterval: 15000,
    });
}
