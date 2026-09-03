import { useEffect, useRef, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiClient, API_BASE_URL } from '../client';
import { logger } from '@/lib/logger';
import { toast } from '@/components/common/Toast';
import { useAuthStore } from './user';

/**
 * Setting 数据
 */
export interface Setting {
    key: string;
    value: string;
}

export const SettingKey = {
    ProxyURL: 'proxy_url',
    TrustedProxies: 'trusted_proxies',
    StatsSaveInterval: 'stats_save_interval',
    ModelInfoUpdateInterval: 'model_info_update_interval',
    SyncLLMInterval: 'sync_llm_interval',
    RelayLogKeepEnabled: 'relay_log_keep_enabled',
    RelayLogKeepPeriod: 'relay_log_keep_period',
    RelayLogMaxStorageGB: 'relay_log_max_storage_gb',
    AnthropicAutoCacheControl: 'anthropic_auto_cache_control',
    OpenAIAutoPromptCacheKey: 'openai_auto_prompt_cache_key',
    RelayStreamKeepaliveIntervalSeconds: 'relay_stream_keepalive_interval_seconds',
    RelayStreamDataIntervalTimeoutSeconds: 'relay_stream_data_interval_timeout_seconds',
    FirstByteKeepaliveDelaySeconds: 'first_byte_keepalive_delay_seconds',
    RelayInterventionEnabled: 'relay_intervention_enabled',
    RelayInterventionTimeoutSeconds: 'relay_intervention_timeout_seconds',
    RelayNoBreakerRetryBudgetSeconds: 'relay_no_breaker_retry_budget_seconds',
    ResponsesSessionTTLSeconds: 'responses_session_ttl_seconds',
    SessionKeepTimeDefault: 'session_keep_time_default',
    FirstTokenTimeOutDefault: 'first_token_time_out_default',
    RouteModeOverride: 'route_mode_override',
    ClaudeHeaderUserAgent: 'claude_header_defaults_user_agent',
    ClaudeHeaderPackageVersion: 'claude_header_defaults_package_version',
    ClaudeHeaderRuntimeVersion: 'claude_header_defaults_runtime_version',
    ClaudeHeaderOS: 'claude_header_defaults_os',
    ClaudeHeaderArch: 'claude_header_defaults_arch',
    ClaudeHeaderTimeout: 'claude_header_defaults_timeout',
    ClaudeHeaderStabilizeDeviceProfile: 'claude_header_defaults_stabilize_device_profile',
    ClaudeCLIAutoCompact: 'claude_cli_auto_compact',
    ClaudeCLIReasoningEffort: 'claude_cli_reasoning_effort',
    ClaudeBetaStripFlags: 'claude_beta_strip_flags',
    CodexHeaderUserAgent: 'codex_header_defaults_user_agent',
    CodexHeaderBetaFeatures: 'codex_header_defaults_beta_features',
    CodexFastMode: 'codex_fast_mode',
    UserRegistrationEnabled: 'user_registration_enabled',
    CORSAllowOrigins: 'cors_allow_origins',
    CircuitBreakerThreshold: 'circuit_breaker_threshold',
    CircuitBreakerCooldown: 'circuit_breaker_cooldown',
    CircuitBreakerMaxCooldown: 'circuit_breaker_max_cooldown',
    UpstreamErrorStatusPassthrough: 'upstream_error_status_passthrough',
    UpstreamErrorBodyMode: 'upstream_error_body_mode',
    UpstreamErrorCustomMessage: 'upstream_error_custom_message',
    UpstreamErrorPublicCode: 'upstream_error_public_code',
    CheckInEnabled: 'checkin_enabled',
    CheckInRewardMode: 'checkin_reward_mode',
    CheckInRewardAmount: 'checkin_reward_amount',
    CheckInRewardMin: 'checkin_reward_min',
    CheckInRewardMax: 'checkin_reward_max',
    PromptOverrideSystem: 'prompt_override_system',
    PromptOverrideMode: 'prompt_override_mode',
    EmailVerificationEnabled: 'email_verification_enabled',
    EmailProvider: 'email_provider',
    EmailSMTPHost: 'email_smtp_host',
    EmailSMTPPort: 'email_smtp_port',
    EmailSMTPUser: 'email_smtp_user',
    EmailSMTPPassword: 'email_smtp_password',
    EmailSMTPFrom: 'email_smtp_from',
    EmailSMTPFromName: 'email_smtp_from_name',
    EmailSMTPSSL: 'email_smtp_ssl',
    EmailHTTPBaseURL: 'email_http_base_url',
    EmailHTTPFrom: 'email_http_from',
    EmailHTTPAdminAuth: 'email_http_admin_auth',
    EmailHTTPSiteAuth: 'email_http_site_auth',
    AdminAccessToken: 'admin_access_token',
} as const;

/**
 * 后端在已存储密码时返回该哨兵值（未存储则返回空字符串）。
 * 保存时若回传该哨兵值，后端会保持原密码不变。
 */
export const SECRET_MASK = '__OCTOPUS_SECRET_KEPT__';

/**
 * 获取 Setting 列表 Hook
 * 
 * @example
 * const { data: settings, isLoading, error } = useSettingList();
 * 
 * if (isLoading) return <Loading />;
 * if (error) return <Error message={error.message} />;
 * 
 * settings?.forEach(setting => console.log(setting.key, setting.value));
 */
export function useSettingList(options?: { enabled?: boolean }) {
    return useQuery({
        queryKey: ['settings', 'list'],
        queryFn: async () => {
            return apiClient.get<Setting[]>('/api/v1/setting/list');
        },
        enabled: options?.enabled ?? true,
        refetchInterval: 30000,
        refetchOnMount: 'always',
    });
}

/**
 * 设置 Setting Hook
 *
 * @example
 * const setSetting = useSetSetting();
 *
 * setSetting.mutate({
 *   key: 'theme',
 *   value: 'dark',
 * });
 */
export function useSetSetting() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: async (data: Setting) => {
            return apiClient.post<Setting>('/api/v1/setting/set', data);
        },
        onSuccess: (data) => {
            logger.log('Setting 设置成功:', data);
            queryClient.invalidateQueries({ queryKey: ['settings', 'list'] });
        },
        onError: (error) => {
            logger.error('Setting 设置失败:', error);
        },
    });
}

/**
 * 全局默认分流模式（route_mode_override）。
 * 用户定 2027-02-21：只留「轮询 / 优先填充」两档，去掉「跟随各规则」；
 * 历史空值/未知值在 UI 归一到默认档，并一次性固化写回，保证后端缺省与 UI 显示一致。
 */
export type RouteModeOverrideValue = 'spread' | 'fill_first';
export const ROUTE_MODE_OVERRIDE_VALUES: readonly RouteModeOverrideValue[] = ['spread', 'fill_first'];
export const ROUTE_MODE_OVERRIDE_DEFAULT: RouteModeOverrideValue = 'fill_first';

export function routeModeOverrideLabel(value: RouteModeOverrideValue): string {
    return value === 'spread' ? '轮询' : '优先填充';
}

export function normalizeRouteModeOverride(raw: string | undefined | null): RouteModeOverrideValue {
    return ROUTE_MODE_OVERRIDE_VALUES.includes((raw ?? '') as RouteModeOverrideValue)
        ? (raw as RouteModeOverrideValue)
        : ROUTE_MODE_OVERRIDE_DEFAULT;
}

let routeModeOverrideFixupDone = false;

/**
 * 读写 route_mode_override 的共享 Hook：设置页与方案页顶栏下拉共用一套读写与归一逻辑。
 * 首次在 fresh 数据上发现历史空值/未知值时，自动固化为明确档位（失败可重试，带 toast 提示）。
 */
export function useRouteModeOverrideSetting() {
    const { data: settings, isFetching, isSuccess, isError } = useSettingList();
    const setSetting = useSetSetting();
    const [value, setValue] = useState<RouteModeOverrideValue>(ROUTE_MODE_OVERRIDE_DEFAULT);
    // 最近一次确认已持久化的原始值（空串 = 后端仍是历史空值/未知值）。
    // 同值短路只对已持久化值生效，固化写回失败后用户重选同档仍能重试。
    const persistedRef = useRef<string>('');
    const errorToastedRef = useRef(false);

    useEffect(() => {
        // 只信 fresh 数据（refetchOnMount:'always' 下仍可能先拿到旧缓存），防旧缓存抢先写回覆盖新值。
        if (!settings || isFetching) return;
        const raw = settings.find((s) => s.key === SettingKey.RouteModeOverride)?.value ?? '';
        persistedRef.current = raw;
        const next = normalizeRouteModeOverride(raw);
        setValue(next);
        if (raw !== next && !routeModeOverrideFixupDone) {
            routeModeOverrideFixupDone = true;
            setSetting.mutate(
                { key: SettingKey.RouteModeOverride, value: next },
                {
                    onSuccess: () => {
                        persistedRef.current = next;
                        toast.info(`全局默认分流模式已固化为：${routeModeOverrideLabel(next)}`);
                    },
                    onError: () => {
                        // 失败解除一次性 guard，下次 fresh 读取仍会重试固化。
                        routeModeOverrideFixupDone = false;
                        toast.error('全局默认分流模式固化失败');
                    },
                },
            );
        }
    }, [settings, isFetching]);

    useEffect(() => {
        if (isError && !errorToastedRef.current) {
            errorToastedRef.current = true;
            toast.error('默认分流模式读取失败');
        }
    }, [isError]);

    const update = (next: RouteModeOverrideValue) => {
        if (next === persistedRef.current) return;
        setValue(next);
        setSetting.mutate(
            { key: SettingKey.RouteModeOverride, value: next },
            {
                onSuccess: () => {
                    persistedRef.current = next;
                    toast.success('默认分流模式已保存');
                },
                onError: () => {
                    setValue(normalizeRouteModeOverride(persistedRef.current));
                    toast.error('默认分流模式保存失败');
                },
            },
        );
    };

    // isReady=false（列表未读到）时禁用下拉，避免把显示默认值冒充已保存状态。
    return { value, update, isPending: setSetting.isPending, isReady: isSuccess };
}

/**
 * 数据库导入/导出
 */
export interface DBImportResult {
    rows_affected: Record<string, number>;
}

export interface DBExportOptions {
    include_logs?: boolean;
    include_stats?: boolean;
}

type ApiResponse<T> = {
    code?: number;
    message?: string;
    data?: T;
};

function isRecord(value: unknown): value is Record<string, unknown> {
    return typeof value === 'object' && value !== null;
}

function getMessageField(value: unknown): string | undefined {
    if (!isRecord(value)) return undefined;
    const msg = value.message;
    return typeof msg === 'string' ? msg : undefined;
}

function getDataField<T>(value: unknown): T | undefined {
    if (!isRecord(value)) return undefined;
    return (value as ApiResponse<T>).data;
}

function getAuthHeader(): string {
    const token = useAuthStore.getState().token;
    if (!token) throw new Error('Not authenticated');
    return `Bearer ${token}`;
}

function parseFilename(contentDisposition: string | null): string | null {
    if (!contentDisposition) return null;
    // e.g. attachment; filename="octopus-export-20250101120000.json"
    const match = contentDisposition.match(/filename="([^"]+)"/i);
    return match?.[1] ?? null;
}

function exportFallbackFilename() {
    const d = new Date();
    const pad = (n: number) => String(n).padStart(2, '0');
    const ts = `${d.getFullYear()}${pad(d.getMonth() + 1)}${pad(d.getDate())}${pad(d.getHours())}${pad(d.getMinutes())}${pad(d.getSeconds())}`;
    return `octopus-export-${ts}.json`;
}

async function downloadBlob(blob: Blob, filename: string) {
    const url = URL.createObjectURL(blob);
    try {
        const a = document.createElement('a');
        a.href = url;
        a.download = filename;
        document.body.appendChild(a);
        a.click();
        a.remove();
    } finally {
        URL.revokeObjectURL(url);
    }
}

/**
 * 导出数据库（下载 JSON 文件）
 */
export function useExportDB() {
    return useMutation({
        mutationFn: async (options: DBExportOptions = {}) => {
            const params = new URLSearchParams();
            params.set('include_logs', String(!!options.include_logs));
            params.set('include_stats', String(!!options.include_stats));

            const res = await fetch(`${API_BASE_URL}/api/v1/setting/export?${params.toString()}`, {
                method: 'GET',
                headers: {
                    Authorization: getAuthHeader(),
                },
            });

            if (!res.ok) {
                const text = await res.text();
                throw new Error(text || res.statusText);
            }

            const blob = await res.blob();
            const filename = parseFilename(res.headers.get('content-disposition')) || exportFallbackFilename();
            await downloadBlob(blob, filename);
            return { filename };
        },
        onError: (error) => {
            logger.error('导出数据库失败:', error);
        },
    });
}

/**
 * 导入数据库（上传 JSON 文件，增量导入）
 */
export function useImportDB() {
    return useMutation({
        mutationFn: async (file: File) => {
            const form = new FormData();
            form.append('file', file);

            const res = await fetch(`${API_BASE_URL}/api/v1/setting/import`, {
                method: 'POST',
                headers: {
                    Authorization: getAuthHeader(),
                },
                body: form,
            });

            const contentType = res.headers.get('content-type') || '';
            const isJson = contentType.includes('application/json');
            const data = isJson ? await res.json() : await res.text();

            if (!res.ok) {
                const message = getMessageField(data) ?? (typeof data === 'string' ? data : res.statusText);
                throw new Error(message);
            }

            // 支持后端标准 ApiResponse：{code,message,data:{...}}
            const nested = getDataField<DBImportResult>(data);
            return nested ?? (data as DBImportResult);
        },
        onError: (error) => {
            logger.error('导入数据库失败:', error);
        },
    });
}
