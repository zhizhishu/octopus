import { useEffect } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { create } from 'zustand';
import { persist } from 'zustand/middleware';
import { apiClient, setAuthStoreGetter } from '../client';
import { logger } from '@/lib/logger';
import type { AccessPlan } from './access-plan';

/**
 * 用户登录请求
 */
export interface UserLoginRequest {
    username: string;
    password: string;
    expire: number; // token 过期时间（秒）
}

export interface UserRegisterRequest {
    username: string;
    password: string;
    invite_code?: string;
    email?: string;
    verification_code?: string;
    expire: number;
}

export type UserRole = 'admin' | 'user';
export type UserStatus = 'active' | 'disabled';

export interface User {
    id: number;
    username: string;
    role: UserRole;
    status: UserStatus;
    balance: number;
    // 兼容后端字段：前端按“每日额度 / 今日已用 / 到期 / 下次重置”展示。
    monthly_limit: number;
    monthly_used: number;
    monthly_expire_at: number;
    monthly_reset_at: number;
    daily_limit?: number;
    daily_used?: number;
    daily_remaining?: number;
    monthly_active?: boolean;
    monthly_status?: string;
    monthly_expire_at_iso?: string;
    monthly_reset_at_iso?: string;
    next_reset_at_iso?: string;
    days_left?: number;
    register_ip?: string;
    last_relay_ip?: string;
    last_relay_at: number;
    note?: string;
    email?: string;
    email_verified?: boolean;
    access_plan_ids?: number[];
    default_access_plan_id?: number;
    access_plans?: AccessPlan[];
    created_at?: number;
    updated_at?: number;
}

export interface UserRegistrationOptions {
    open_registration: boolean;
    invite_required: boolean;
    email_verification_enabled: boolean;
}

export type CheckInRewardMode = 'fixed' | 'random';

export interface UserCheckInStatus {
    enabled: boolean;
    checked_today: boolean;
    today: string;
    reward_mode: CheckInRewardMode;
    reward_amount: number;
    reward_min: number;
    reward_max: number;
    last_amount: number;
    balance: number;
    checked_at: number;
    next_check_in_at: number;
}

export interface UserCheckInResponse {
    user: User;
    status: UserCheckInStatus;
    reward: number;
}

/**
 * 用户登录响应
 */
export interface UserLoginResponse {
    token: string;
    expire_at: string; // ISO 8601 格式
    user: User;
}

/**
 * 修改密码请求
 */
export interface ChangePasswordRequest {
    old_password: string;
    new_password: string;
}

/**
 * 修改用户名请求
 */
export interface ChangeUsernameRequest {
    new_username: string;
}

/**
 * 认证状态 Store
 */
interface AuthState {
    isAuthenticated: boolean;
    isLoading: boolean;
    isAPIKeyAuth: boolean;
    token: string | null;
    expireAt: string | null;
    user: User | null;

    // Actions
    setAuth: (token: string, expireAt: string, user: User) => void;
    setAPIKeyAuth: (apiKey: string) => void;
    checkAuth: () => Promise<void>;
    logout: () => void;
}

/**
 * 认证状态管理 Store（使用 zustand + persist）
 */
export const useAuthStore = create<AuthState>()(
    persist(
        (set, get) => ({
            isAuthenticated: false,
            isLoading: true,
            isAPIKeyAuth: false,
            token: null,
            expireAt: null,
            user: null,

            setAuth: (token: string, expireAt: string, user: User) => {
                set({
                    isAuthenticated: true,
                    isAPIKeyAuth: false,
                    token,
                    expireAt,
                    user,
                    isLoading: false
                });
            },

            setAPIKeyAuth: (apiKey: string) => {
                set({
                    isAuthenticated: true,
                    isAPIKeyAuth: true,
                    token: apiKey,
                    expireAt: null,
                    user: null,
                    isLoading: false
                });
            },

            checkAuth: async () => {
                const { token, expireAt, isAPIKeyAuth } = get();

                if (!token) {
                    set({ isAuthenticated: false, isLoading: false });
                    return;
                }

                // API Key 不检查本地过期时间
                if (!isAPIKeyAuth) {
                    if (!expireAt || Date.now() >= new Date(expireAt).getTime()) {
                        get().logout();
                        return;
                    }
                }

                try {
                    // API Key 模式只需校验 key 是否有效即可
                    const endpoint = isAPIKeyAuth ? '/api/v1/apikey/login' : '/api/v1/user/status';
                    const user = await apiClient.get<User | null>(endpoint);
                    set({ isAuthenticated: true, isLoading: false, user: isAPIKeyAuth ? null : user });
                } catch (error) {
                    logger.error('认证验证失败:', error);
                    get().logout();
                }
            },

            logout: () => {
                set({
                    isAuthenticated: false,
                    isAPIKeyAuth: false,
                    token: null,
                    expireAt: null,
                    user: null,
                    isLoading: false
                });
            }
        }),
        {
            name: 'auth-storage',
            partialize: (state) => ({
                token: state.token,
                expireAt: state.expireAt,
                isAPIKeyAuth: state.isAPIKeyAuth,
                user: state.user,
            })
        }
    )
);

// 注册 auth store getter 到 apiClient
if (typeof window !== 'undefined') {
    setAuthStoreGetter(() => {
        const state = useAuthStore.getState();
        return {
            token: state.token,
            logout: state.logout
        };
    });
}

/**
 * 用户登录 Hook
 * 
 * @example
 * const login = useLogin();
 * login.mutate({ username: 'admin', password: '123456', expire: 86400 });
 * 
 * if (login.isPending) return <Loading />;
 * if (login.isError) return <Error message={login.error.message} />;
 */
export function useLogin() {
    const { setAuth } = useAuthStore();

    return useMutation({
        mutationFn: async (data: UserLoginRequest) => {
            return apiClient.post<UserLoginResponse>('/api/v1/user/login', data);
        },
        onSuccess: (data) => {
            // 保存到 zustand store
            setAuth(data.token, data.expire_at, data.user);
        },
        onError: (error) => {
            logger.error('登录失败:', error);
        },
    });
}

/**
 * 修改密码 Hook
 * 
 * @example
 * const changePassword = useChangePassword();
 * changePassword.mutate({ oldPassword: '123', newPassword: '456' });
 */
export function useChangePassword() {
    return useMutation({
        mutationFn: async (data: { oldPassword: string; newPassword: string }) => {
            const payload: ChangePasswordRequest = {
                old_password: data.oldPassword,
                new_password: data.newPassword,
            };
            return apiClient.post<string>('/api/v1/user/change-password', payload);
        },
        onSuccess: (message) => {
            logger.log('密码修改成功:', message);
        },
        onError: (error) => {
            logger.error('密码修改失败:', error);
        },
    });
}

/**
 * 修改用户名 Hook
 * 
 * @example
 * const changeUsername = useChangeUsername();
 * changeUsername.mutate({ newUsername: 'newname' });
 */
export function useChangeUsername() {
    return useMutation({
        mutationFn: async (data: { newUsername: string }) => {
            const payload: ChangeUsernameRequest = {
                new_username: data.newUsername,
            };
            return apiClient.post<string>('/api/v1/user/change-username', payload);
        },
        onSuccess: (message) => {
            logger.log('用户名修改成功:', message);
        },
        onError: (error) => {
            logger.error('用户名修改失败:', error);
        },
    });
}

/**
 * 认证状态和方法 Hook
 * 
 * @example
 * const auth = useAuth();
 * 
 * if (auth.isAuthenticated) {
 *   // 已登录
 * }
 * 
 * auth.logout(); // 登出
 */
export function useAuth() {
    const store = useAuthStore();
    const { checkAuth, isLoading } = store;

    // 只在首次挂载时检查认证状态
    useEffect(() => {
        if (isLoading) {
            checkAuth();
        }
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, []); // 有意只在挂载时执行一次

    return {
        isAuthenticated: store.isAuthenticated,
        isAPIKeyAuth: store.isAPIKeyAuth,
        isLoading: store.isLoading,
        user: store.user,
        isAdmin: store.user?.role === 'admin',
        logout: store.logout,
    };
}

export function useRegistrationOptions() {
    return useQuery({
        queryKey: ['user', 'registration-options'],
        queryFn: () => apiClient.get<UserRegistrationOptions>('/api/v1/user/registration-options'),
        staleTime: 30000,
    });
}

export function useCheckInStatus(options?: { enabled?: boolean }) {
    return useQuery({
        queryKey: ['user', 'check-in', 'status'],
        queryFn: () => apiClient.get<UserCheckInStatus>('/api/v1/user/check-in/status'),
        enabled: options?.enabled ?? true,
        refetchInterval: 30000,
    });
}

export function useCheckIn() {
    const queryClient = useQueryClient();
    return useMutation({
        mutationFn: () => apiClient.post<UserCheckInResponse>('/api/v1/user/check-in', {}),
        onSuccess: (data) => {
            useAuthStore.setState({ user: data.user });
            queryClient.invalidateQueries({ queryKey: ['user', 'check-in', 'status'] });
        },
        onError: (error) => {
            logger.error('签到失败:', error);
        },
    });
}

export function useRegister() {
    const { setAuth } = useAuthStore();

    return useMutation({
        mutationFn: async (data: UserRegisterRequest) => {
            return apiClient.post<UserLoginResponse>('/api/v1/user/register', data);
        },
        onSuccess: (data) => {
            setAuth(data.token, data.expire_at, data.user);
        },
        onError: (error) => {
            logger.error('注册失败:', error);
        },
    });
}

export function useSendVerificationCode() {
    return useMutation({
        mutationFn: async (email: string) => {
            return apiClient.post<string>('/api/v1/user/send-verification-code', { email });
        },
        onError: (error) => {
            logger.error('发送验证码失败:', error);
        },
    });
}

export type CreateUserRequest = {
    username: string;
    password: string;
    role: UserRole;
    status: UserStatus;
    balance?: number;
    monthly_limit?: number;
    monthly_expire_at?: number;
    note?: string;
    access_plan_ids?: number[];
    default_access_plan_id?: number;
};

export type UpdateUserRequest = Omit<User, 'created_at' | 'updated_at'>;

export function useUserList(options?: { enabled?: boolean }) {
    return useQuery({
        queryKey: ['users', 'list'],
        queryFn: () => apiClient.get<User[]>('/api/v1/user/list'),
        refetchInterval: 30000,
        enabled: options?.enabled ?? true,
    });
}

export function useCreateUser() {
    const queryClient = useQueryClient();
    return useMutation({
        mutationFn: (data: CreateUserRequest) => apiClient.post<User>('/api/v1/user/create', data),
        onSuccess: () => {
            queryClient.invalidateQueries({ queryKey: ['users'] });
        },
    });
}

export function useUpdateUser() {
    const queryClient = useQueryClient();
    return useMutation({
        mutationFn: (data: UpdateUserRequest) => apiClient.post<User>('/api/v1/user/update', data),
        onSuccess: () => {
            queryClient.invalidateQueries({ queryKey: ['users', 'list'] });
        },
    });
}

export function useResetUserPassword() {
    return useMutation({
        mutationFn: (data: { id: number; password: string }) => apiClient.post<null>('/api/v1/user/reset-password', data),
    });
}

export function useDeleteUser() {
    const queryClient = useQueryClient();
    return useMutation({
        mutationFn: (id: number) => apiClient.delete<null>(`/api/v1/user/delete/${id}`),
        onSuccess: () => {
            queryClient.invalidateQueries({ queryKey: ['users'] });
        },
    });
}


/** 部分更新：只发改了的字段，没发的后端保持原值。 */
export type PatchUserRequest = { id: number } & Partial<{
    username: string;
    role: UserRole;
    status: UserStatus;
    note: string;
    monthly_limit: number;
    monthly_used: number;
    monthly_expire_at: number;
    access_plan_ids: number[];
    default_access_plan_id: number;
}>;

export function usePatchUser() {
    const queryClient = useQueryClient();
    return useMutation({
        mutationFn: (data: PatchUserRequest) => apiClient.post<User>('/api/v1/user/update', data),
        onSuccess: () => {
            queryClient.invalidateQueries({ queryKey: ['users'] });
        },
    });
}

export type UserBalanceMode = 'add' | 'subtract' | 'set';

export interface UserBalanceLog {
    id: number;
    user_id: number;
    actor_id: number;
    actor_name: string;
    mode: UserBalanceMode;
    delta: number;
    balance_before: number;
    balance_after: number;
    reason: string;
    created_at: number;
}

export function useAdjustUserBalance() {
    const queryClient = useQueryClient();
    return useMutation({
        mutationFn: (data: { id: number; mode: UserBalanceMode; amount: number; reason?: string }) =>
            apiClient.post<{ user: User; log: UserBalanceLog }>('/api/v1/user/balance', data),
        onSuccess: () => {
            queryClient.invalidateQueries({ queryKey: ['users'] });
        },
    });
}

export function useUserBalanceLog(id: number | undefined, limit = 10) {
    return useQuery({
        queryKey: ['users', 'balance-log', id, limit],
        queryFn: () => apiClient.get<UserBalanceLog[]>(`/api/v1/user/balance-log?id=${id}&limit=${limit}`),
        enabled: !!id,
    });
}

export type UserUsageRange = 'today' | '7d' | '30d' | 'all';

export interface UserUsage {
    user_id: number;
    range: UserUsageRange;
    start: number;
    end: number;
    request_success: number;
    request_failed: number;
    input_tokens: number;
    output_tokens: number;
    total_cost: number;
    daily: { date: string; requests: number; cost: number }[];
    top_models: { model: string; requests: number; tokens: number; cost: number }[];
}

export function useUserUsage(id: number | undefined, range: UserUsageRange) {
    return useQuery({
        queryKey: ['users', 'usage', id, range],
        queryFn: () => apiClient.get<UserUsage>(`/api/v1/user/usage?id=${id}&range=${range}`),
        enabled: !!id,
    });
}

export type UserBatchAction = 'enable' | 'disable' | 'add_balance' | 'set_plans' | 'delete';

export interface UserBatchResult {
    done: number[];
    failed: { id: number; error: string }[];
}

export function useBatchUsers() {
    const queryClient = useQueryClient();
    return useMutation({
        mutationFn: (data: {
            ids: number[];
            action: UserBatchAction;
            amount?: number;
            reason?: string;
            access_plan_ids?: number[];
            default_access_plan_id?: number;
        }) => apiClient.post<UserBatchResult>('/api/v1/user/batch', data),
        onSuccess: () => {
            queryClient.invalidateQueries({ queryKey: ['users'] });
        },
    });
}

// ---- IP / 客户端（用户页，仅管理员）----

export type UserAccessAlertKind = 'many_ips' | 'new_ip' | 'new_client';

/** 一条「异常切换」提示。value 是新冒出来的 IP / 客户端 UA，others 是同一时间窗里也在用的其它 IP / UA。 */
export interface UserAccessAlert {
    kind: UserAccessAlertKind;
    window_sec: number;
    count: number;
    value?: string;
    others?: string[];
    at: number;
}

export interface UserAccessLast {
    ip: string;
    user_agent: string;
    api_key_id: number;
    api_key_name?: string;
    at: number;
}

export interface UserAccessSummary {
    user_id: number;
    last: UserAccessLast;
    alerts: UserAccessAlert[];
}

export interface UserAccessKeyUse {
    api_key_id: number;
    api_key_name?: string;
    requests: number;
    last_seen: number;
}

export interface UserAccessGroup {
    value: string;
    first_seen: number;
    last_seen: number;
    requests: number;
    keys: UserAccessKeyUse[];
    values?: string[];
}

/** 风控等级（设置 user_access_alert_level）。只影响用户页的提示，不拦截请求。 */
export type UserAccessAlertLevel = 'off' | 'loose' | 'standard' | 'strict' | 'custom';
export const USER_ACCESS_ALERT_LEVELS: readonly UserAccessAlertLevel[] = ['off', 'loose', 'standard', 'strict', 'custom'];

/** 一套判定阈值（后端 model.UserAccessAlertConfig）。many_ips = 0 不看 IP 数；ipv4_prefix 32 / 24。 */
export interface UserAccessAlertConfig {
    level: UserAccessAlertLevel;
    window_sec: number;
    many_ips: number;
    new_ip: boolean;
    new_client: boolean;
    ipv4_prefix: number;
}

export interface UserAccessAlertSettings {
    current: UserAccessAlertConfig;
    presets: Partial<Record<UserAccessAlertLevel, UserAccessAlertConfig>>;
}

export interface UserAccessDetail {
    user_id: number;
    config?: UserAccessAlertConfig;
    window_sec: number;
    many_ips_limit: number;
    keep_days: number;
    last?: UserAccessLast;
    alerts: UserAccessAlert[];
    ips: UserAccessGroup[];
    clients: UserAccessGroup[];
    total_requests: number;
}

export function useUserAccessList(options?: { enabled?: boolean }) {
    return useQuery({
        queryKey: ['users', 'access'],
        queryFn: () => apiClient.get<UserAccessSummary[]>('/api/v1/user/access'),
        enabled: options?.enabled ?? true,
        refetchInterval: 60_000,
    });
}

export function useUserAccessDetail(id: number | undefined) {
    return useQuery({
        queryKey: ['users', 'access', 'detail', id],
        queryFn: () => apiClient.get<UserAccessDetail>(`/api/v1/user/access/detail?id=${id}`),
        enabled: !!id,
        refetchInterval: 60_000,
    });
}

/** 一个客户端指纹（后端 model.UserClientFingerprint）。只来自入站请求；秘密头只有名字没有值。 */
export interface UserClientFingerprint {
    id: number;
    user_id: number;
    api_key_id: number;
    fp_hash: string;
    client_name: string;
    client_version: string;
    user_agent: string;
    /** 头名，逗号拼接；header_order=true 时是原始顺序和大小写，否则按字母排。 */
    header_names: string;
    header_order: boolean;
    /** 留了值的非秘密头（JSON 对象字符串）。 */
    notable: string;
    http_version: string;
    ja3: string;
    ja4: string;
    ja4h: string;
    via_proxy: boolean;
    country: string;
    last_ip: string;
    last_path: string;
    first_seen: number;
    last_seen: number;
    requests: number;
}

export interface UserClientKeyInfo {
    api_key_id: number;
    api_key_name?: string;
    /** 管理员名下的访客 Key。 */
    guest?: boolean;
    fingerprints: number;
    requests: number;
    last_seen: number;
}

export interface UserClientDetail {
    user_id: number;
    api_key_id?: number;
    keep_days: number;
    per_key_max: number;
    tls_terminated: boolean;
    keys: UserClientKeyInfo[];
    fingerprints: UserClientFingerprint[];
}

/** 客户端指纹：按用户看（userId），或只按 Key 看（apiKeyId，比如管理员名下发给别人用的 Key）。 */
export function useUserClientTelemetry(params: { userId?: number; apiKeyId?: number }) {
    const { userId, apiKeyId } = params;
    const qs = new URLSearchParams();
    if (userId) qs.set('id', String(userId));
    else if (apiKeyId) qs.set('key', String(apiKeyId));
    return useQuery({
        queryKey: ['users', 'telemetry', userId ?? 0, userId ? 0 : apiKeyId ?? 0],
        queryFn: () => apiClient.get<UserClientDetail>(`/api/v1/user/telemetry?${qs.toString()}`),
        enabled: !!(userId || apiKeyId),
        refetchInterval: 60_000,
    });
}

/** 风控等级的当前阈值和各预设阈值（设置页说明文字按这里的数拼，和后端判定同一份）。 */
export function useUserAccessConfig(options?: { enabled?: boolean }) {
    return useQuery({
        queryKey: ['users', 'access', 'config'],
        queryFn: () => apiClient.get<UserAccessAlertSettings>('/api/v1/user/access/config'),
        enabled: options?.enabled ?? true,
    });
}
