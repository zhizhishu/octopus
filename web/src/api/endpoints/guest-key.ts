import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiClient } from '../client';
import type { APIKey, APIKeyUsage } from './apikey';
import type { UserAccessGroup, UserAccessLast, UserUsageRange } from './user';

/** 访客 Key：就是一把 APIKey，多了 guest / note / revoked_at。只有管理员能看到这些接口。 */
export type GuestKey = APIKey & { guest: true; note?: string; revoked_at?: number };

export interface GuestKeyRequest {
    id?: number;
    name: string;
    note: string;
    models: string[];
    expire_at: number;
    max_cost: number;
    enabled?: boolean;
}

export interface GuestKeyAccess {
    api_key_id: number;
    total_requests: number;
    keep_days: number;
    last?: UserAccessLast;
    ips: UserAccessGroup[];
    clients: UserAccessGroup[];
}

const LIST = ['guestkeys', 'list'];

export function useGuestKeyList(options?: { enabled?: boolean }) {
    return useQuery({
        queryKey: LIST,
        queryFn: () => apiClient.get<GuestKey[] | null>('/api/v1/guestkey/list'),
        select: (d) => d ?? [],
        refetchInterval: 30000,
        enabled: options?.enabled ?? true,
    });
}

export function useGuestKeyModels(options?: { enabled?: boolean }) {
    return useQuery({
        queryKey: ['guestkeys', 'models'],
        queryFn: () => apiClient.get<string[] | null>('/api/v1/guestkey/models'),
        select: (d) => d ?? [],
        enabled: options?.enabled ?? true,
    });
}

function useGuestMutation<TArgs, TOut>(fn: (a: TArgs) => Promise<TOut>) {
    const qc = useQueryClient();
    return useMutation({
        mutationFn: fn,
        onSuccess: () => {
            qc.invalidateQueries({ queryKey: ['guestkeys'] });
            qc.invalidateQueries({ queryKey: ['stats', 'apikey'] });
        },
    });
}

export const useCreateGuestKey = () => useGuestMutation((d: GuestKeyRequest) => apiClient.post<GuestKey>('/api/v1/guestkey/create', d));
export const useUpdateGuestKey = () => useGuestMutation((d: GuestKeyRequest) => apiClient.post<GuestKey>('/api/v1/guestkey/update', d));
export const useRevokeGuestKey = () => useGuestMutation((id: number) => apiClient.post<GuestKey>(`/api/v1/guestkey/revoke/${id}`, {}));
export const useDeleteGuestKey = () => useGuestMutation((id: number) => apiClient.delete<null>(`/api/v1/guestkey/delete/${id}`));

export function useGuestKeyUsage(id: number | undefined, range: UserUsageRange) {
    return useQuery({
        queryKey: ['guestkeys', 'usage', id, range],
        queryFn: () => apiClient.get<APIKeyUsage>(`/api/v1/guestkey/usage?id=${id}&range=${range}`),
        enabled: !!id,
    });
}

export function useGuestKeyAccess(id: number | undefined) {
    return useQuery({
        queryKey: ['guestkeys', 'access', id],
        queryFn: () => apiClient.get<GuestKeyAccess>(`/api/v1/guestkey/access?id=${id}`),
        enabled: !!id,
        refetchInterval: 30000,
    });
}
