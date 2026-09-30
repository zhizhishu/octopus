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
    RedactEnabled: 'redact_enabled',
    RedactDefaultFlags: 'redact_default_flags',
    RedactNoticeEnabled: 'redact_notice_enabled',
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
 * 读取密钥类设置的明文（后端受保护的一次性接口 `GET /setting/secret`）。
 * 只支持 admin_access_token 这一个 key；用管理员令牌自身鉴权的会话会被后端 403。
 * 刻意不走 react-query：明文不进任何查询缓存，随调用方组件卸载即丢。
 */
export type SettingSecretSource = 'setting' | 'env' | 'none';

export interface SettingSecret {
    key: string;
    value: string;
    source: SettingSecretSource;
}

export function fetchSettingSecret(key: string): Promise<SettingSecret> {
    return apiClient.get<SettingSecret>('/api/v1/setting/secret', { key });
}

/**
 * 全局默认分流模式（route_mode_override）。
 *
 * 后端是**三态**：`''` = 跟随各分组自己的模式（出厂默认）/ `'spread'` = 轮询 /
 * `'fill_first'` = 优先填充；空串是后端显式校验通过的合法值。
 *
 * 曾经的实现是「读到空值/未知值就在 UI 归一到默认档，并一次性写回」，理由是让后端缺省与
 * 界面显示保持一致。**那是个坑**：这个值在选路时实时生效，静默写回等于把「跟随各分组」
 * 这一档从线上抹掉 —— 全新安装的实例，管理员打开一次画布页，所有没单独指定过模式的分组
 * 就从「各按各的」变成「全体优先填充」，而且没有任何人点过保存。所以这里改成：
 *
 *   - 空值如实显示「未设（跟随各规则）」，**不写回**，等用户自己点；
 *   - 读到不认识的取值就如实标「不支持」并原样留着，不假装成另一档、也不覆盖它；
 *   - 只有用户在界面上主动选择时才发写请求（含主动选回「未设」）。
 */
export type RouteModeOverrideValue = 'spread' | 'fill_first' | '';
/** 用户可主动选择的明确档位（不含「未设」）。 */
export const ROUTE_MODE_OVERRIDE_VALUES: readonly Exclude<RouteModeOverrideValue, ''>[] = ['spread', 'fill_first'];

export function routeModeOverrideLabel(value: RouteModeOverrideValue): string {
    if (value === 'spread') return '轮询';
    if (value === 'fill_first') return '优先填充';
    return '未设（跟随各规则）';
}

/**
 * 把后端原始值分类成「能对上的档位 / 空 / 不认识的三方值」，**不做静默归一**。
 * `unsupported` 非空 = 后端存着一个我们不认识的取值：界面要如实说不支持，
 * 既不能当成某一档显示，也不能顺手把它覆盖掉。
 */
export function classifyRouteModeOverride(raw: string | undefined | null): {
    value: RouteModeOverrideValue;
    unsupported: string | null;
} {
    const trimmed = (raw ?? '').trim();
    if (trimmed === '') return { value: '', unsupported: null };
    if ((ROUTE_MODE_OVERRIDE_VALUES as readonly string[]).includes(trimmed)) {
        return { value: trimmed as RouteModeOverrideValue, unsupported: null };
    }
    return { value: '', unsupported: trimmed };
}

/**
 * 读写 route_mode_override 的共享 Hook：设置页与方案页顶栏下拉共用一套读写与分类逻辑。
 * **进页面只读不写**；写只发生在用户主动选择时。
 */
export function useRouteModeOverrideSetting() {
    const { data: settings, isFetching, isSuccess, isError } = useSettingList();
    const setSetting = useSetSetting();
    // 显示值：'' 表示后端就是「未设」，界面照实显示，不再假装成某一档。
    const [value, setValue] = useState<RouteModeOverrideValue>('');
    // 后端存着我们不认识的取值时原样带出来，界面标「不支持」而不是当成另一档。
    const [unsupported, setUnsupported] = useState<string | null>(null);
    // 最近一次确认已持久化的**原始**值（空串 = 后端就是未设）。
    // 同值短路只对已持久化值生效；保存失败也要回滚到它，而不是回滚成某个「默认档」。
    const persistedRef = useRef<string>('');
    const errorToastedRef = useRef(false);

    useEffect(() => {
        // 只信 fresh 数据（refetchOnMount:'always' 下仍可能先拿到旧缓存）。
        if (!settings || isFetching) return;
        const raw = settings.find((s) => s.key === SettingKey.RouteModeOverride)?.value ?? '';
        persistedRef.current = raw;
        const classified = classifyRouteModeOverride(raw);
        setValue(classified.value);
        setUnsupported(classified.unsupported);
        // 这里**故意不发写请求**：进页面只读。空值与未知值都如实显示，等用户自己点。
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
        // 用户主动选了一个明确档位，那个「不支持」的陈旧提示就该收掉。
        setUnsupported(null);
        setSetting.mutate(
            { key: SettingKey.RouteModeOverride, value: next },
            {
                onSuccess: () => {
                    persistedRef.current = next;
                    toast.success('默认分流模式已保存');
                },
                onError: () => {
                    // 回到真实的持久化值（可能还是空 = 未设），不假装成某一档写成功了。
                    const restored = classifyRouteModeOverride(persistedRef.current);
                    setValue(restored.value);
                    setUnsupported(restored.unsupported);
                    toast.error('默认分流模式保存失败');
                },
            },
        );
    };

    // isReady=false（列表未读到）时禁用下拉，避免把显示值冒充已保存状态。
    return { value, unsupported, update, isPending: setSetting.isPending, isReady: isSuccess };
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
