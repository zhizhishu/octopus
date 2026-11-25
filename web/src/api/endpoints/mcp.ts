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
    /** 仅管理员返回内容；普通用户恒为空数组。 */
    top_users?: (MCPUsage & { user_id: number; name: string })[];
    /** 仅管理员返回内容；普通用户恒为空数组。 */
    top_channels?: (MCPUsage & { channel_id: number; name: string })[];
}

export const MCP_RANGES = [1, 7, 30] as const;
export type MCPRange = typeof MCP_RANGES[number];

export function useMCPStats(userId?: number, days: MCPRange = 7) {
    return useQuery({
        queryKey: ['mcp', 'stats', userId, days],
        queryFn: () => apiClient.get<MCPStats>(`/api/v1/mcp/stats?days=${days}`),
        enabled: !!userId,
        refetchInterval: 15000,
    });
}
