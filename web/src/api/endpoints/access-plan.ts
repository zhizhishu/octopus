import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiClient } from '../client';
import { logger } from '@/lib/logger';

export type BillingModelSource = 'request_model' | 'upstream_model' | 'override_model';
export type RouteFallbackMode = 'none' | 'failover' | 'return_group';
export type PromptOverrideMode = 'append_system' | 'replace_system';

export interface AccessPlanBillingRule {
    id?: number;
    access_plan_id?: number;
    model_name: string;
    multiplier: number;
    enabled?: boolean;
}

export interface AccessPlanRouteTarget {
    id?: number;
    access_plan_id?: number;
    request_model: string;
    channel_id: number;
    upstream_model: string;
    priority: number;
    weight: number;
    enabled: boolean;
    billing_model_source?: BillingModelSource;
    billing_model_override?: string;
    fallback_mode?: RouteFallbackMode;
    system_prompt_override?: string;
    prompt_override_mode?: PromptOverrideMode;
}

export interface AccessPlan {
    id: number;
    slug: string;
    display_name: string;
    enabled: boolean;
    is_default: boolean;
    default_multiplier: number;
    system_prompt_override?: string;
    prompt_override_mode?: PromptOverrideMode;
    sort?: number;
    billing_rules: AccessPlanBillingRule[];
    route_targets: AccessPlanRouteTarget[];
    created_at?: number;
    updated_at?: number;
}

type AccessPlanServer = Omit<AccessPlan, 'billing_rules' | 'route_targets'> & {
    billing_rules?: AccessPlanBillingRule[] | null;
    route_targets?: AccessPlanRouteTarget[] | null;
};

export type CreateAccessPlanRequest = {
    slug: string;
    display_name: string;
    enabled?: boolean;
    is_default?: boolean;
    default_multiplier?: number;
    sort?: number;
};

export type UpdateAccessPlanRequest = Partial<Omit<CreateAccessPlanRequest, 'slug'>> & {
    id: number;
    slug?: string;
    system_prompt_override?: string;
    prompt_override_mode?: PromptOverrideMode;
};

export type UpdateAccessPlanBillingRulesRequest = {
    access_plan_id: number;
    default_multiplier: number;
    rules: AccessPlanBillingRule[];
};

export type UpdateAccessPlanRouteTargetsRequest = {
    access_plan_id: number;
    targets: AccessPlanRouteTarget[];
};

const ACCESS_PLAN_LIST_KEY = ['access-plans', 'list'] as const;

function normalizeAccessPlan(plan: AccessPlanServer): AccessPlan {
    return {
        ...plan,
        default_multiplier: plan.default_multiplier ?? 1,
        billing_rules: plan.billing_rules ?? [],
        route_targets: plan.route_targets ?? [],
    };
}

function invalidateAccessPlans(queryClient: ReturnType<typeof useQueryClient>) {
    queryClient.invalidateQueries({ queryKey: ACCESS_PLAN_LIST_KEY });
}

export function useAccessPlanList(options?: { enabled?: boolean }) {
    return useQuery({
        queryKey: ACCESS_PLAN_LIST_KEY,
        queryFn: () => apiClient.get<AccessPlanServer[]>('/api/v1/access-plan/list'),
        select: (data) => data.map(normalizeAccessPlan),
        enabled: options?.enabled ?? true,
        refetchInterval: 30000,
        refetchOnMount: 'always',
    });
}

export function useCreateAccessPlan() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: (data: CreateAccessPlanRequest) => (
            apiClient.post<AccessPlanServer>('/api/v1/access-plan/create', data)
        ),
        onSuccess: (data) => {
            logger.log('Access plan created:', data);
            invalidateAccessPlans(queryClient);
        },
        onError: (error) => {
            logger.error('Access plan create failed:', error);
        },
    });
}

export function useUpdateAccessPlan() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: (data: UpdateAccessPlanRequest) => (
            apiClient.post<AccessPlanServer>('/api/v1/access-plan/update', data)
        ),
        onSuccess: (data) => {
            logger.log('Access plan updated:', data);
            invalidateAccessPlans(queryClient);
        },
        onError: (error) => {
            logger.error('Access plan update failed:', error);
        },
    });
}

export function useDeleteAccessPlan() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: (id: number) => apiClient.delete<null>(`/api/v1/access-plan/delete/${id}`),
        onSuccess: () => {
            logger.log('Access plan deleted');
            invalidateAccessPlans(queryClient);
        },
        onError: (error) => {
            logger.error('Access plan delete failed:', error);
        },
    });
}

export function useUpdateAccessPlanBillingRules() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: (data: UpdateAccessPlanBillingRulesRequest) => (
            apiClient.post<AccessPlanServer>('/api/v1/access-plan/billing-rule/update', data)
        ),
        onSuccess: (data) => {
            logger.log('Access plan billing updated:', data);
            invalidateAccessPlans(queryClient);
        },
        onError: (error) => {
            logger.error('Access plan billing update failed:', error);
        },
    });
}

export function useUpdateAccessPlanRouteTargets() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: (data: UpdateAccessPlanRouteTargetsRequest) => (
            apiClient.post<AccessPlanServer>('/api/v1/access-plan/route-target/update', data)
        ),
        onSuccess: (data) => {
            logger.log('Access plan routes updated:', data);
            invalidateAccessPlans(queryClient);
        },
        onError: (error) => {
            logger.error('Access plan routes update failed:', error);
        },
    });
}
