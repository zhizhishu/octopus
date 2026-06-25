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
            queryClient.invalidateQueries({ queryKey: ['users', 'list'] });
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
            queryClient.invalidateQueries({ queryKey: ['users', 'list'] });
        },
    });
}

