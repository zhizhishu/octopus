import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiClient, API_BASE_URL } from '../client';
import { logger } from '@/lib/logger';
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

