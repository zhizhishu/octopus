import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiClient } from '../client';
import { logger } from '@/lib/logger';

export const AUDIT_PROBE_IDS = [
    'liveness',
    'identity',
    'glitch',
    'token_delta',
    'echo_rewrite',
    'context_canary',
    'signature',
    'fingerprint',
    'tokenizer_id',
    'tokenizer_baseline',
] as const;

export type AuditProbeId = (typeof AUDIT_PROBE_IDS)[number];

/**
 * 只能手动勾选的探针：不进「标准」方案，也不在切渠道时自动全选。
 *
 * - tokenizer_id：参考实现 lm-detector 的分词器探测默认关闭（CLI `--tokenizer` 才开），约 12 次请求；
 * - tokenizer_baseline：一次扫完 72 道题 + 2 次基线 = 74 次请求，只该在你确认可信的渠道上单独跑。
 */
export const AUDIT_OPT_IN_PROBE_IDS: readonly AuditProbeId[] = ['tokenizer_id', 'tokenizer_baseline'];

/** 「标准」方案与默认勾选用的探针（全部探针去掉手动项）。 */
export const AUDIT_STANDARD_PROBE_IDS: AuditProbeId[] = AUDIT_PROBE_IDS.filter(
    (id) => !AUDIT_OPT_IN_PROBE_IDS.includes(id),
);

export type AuditSeverity = 'low' | 'medium' | 'high';
export type AuditVerdict = 'none' | 'low' | 'medium' | 'high' | 'unknown';

export type AuditFinding = {
    probe: AuditProbeId;
    severity: AuditSeverity;
    score: number;
    title: string;
    evidence?: Record<string, unknown>;
    recommendation?: string;
    plain_evidence?: string;
};

export type AuditProbeResult = {
    probe_id: AuditProbeId;
    ok: boolean;
    data?: Record<string, unknown>;
    error?: string;
};

export type AuditProbeError = {
    probe: AuditProbeId;
    error: string;
};

/**
 * 结论的"人话版"。后端给稳定口径，前端只负责排版 —— 否则同一条结论在结果页、
 * 日志详情、通知里会各写一套并慢慢漂移。
 *
 * 注意 caveat：它说明"这个结论不能证明什么"。家族差异很大（Anthropic 有可验证
 * 签名，其它家族没有），所以这句话必须原样展示，不能省略或折叠。
 */
export type AuditPlainVerdict = {
    level: AuditVerdict;
    headline: string;
    meaning: string;
    caveat: string;
    next_step: string;
    confidence?: string;
    summary?: string;
};

export type AuditViewReport = {
    requested_model: string;
    requested_families?: string[];
    score: number;
    verdict: AuditVerdict;
    findings: AuditFinding[];
    results: AuditProbeResult[];
    errors: AuditProbeError[];
    disclaimer: string;
    plain?: AuditPlainVerdict;
    /** 从模型名推断的主家族（openai/anthropic/qwen...），空表示看不出来。 */
    family?: string;
};

export type ModelAuditRequest = {
    channel_id: number;
    model: string;
    probes?: AuditProbeId[];
    timeout_seconds?: number;
    /** 可选：带值时把这次手动审计结论先到先得地写回那条日志行（trigger=manual）。 */
    log_id?: number;
};

export type ModelAuditResponse = {
    /** 来自队列结果时的结果行 id（保存分词器基线要用）；同步检测没有。 */
    result_id?: number;
    channel_id: number;
    channel_name: string;
    upstream_model: string;
    endpoint: string;
    duration_ms: number;
    report: AuditViewReport;
    /** 报告自身完成时间（打开历史报告时带上，禁止用墙钟冒充）。 */
    finished_at?: string;
    model?: string;
    preset?: string;
};

export type LogAnomalyFinding = {
    code: string;
    severity: 'low' | 'medium';
    channel_id: number;
    channel_name: string;
    model: string;
    title: string;
    evidence?: Record<string, unknown>;
};

export type LogAnomalyPassiveBucket = {
    channel_id: number;
    model: string;
    endpoint: string;
    is_stream: boolean;
    language: string;
    feature_version: string;
    count: number;
    no_profile_count: number;
};

/** 硬事实校验的聚合桶：账目/缓存/分词器口径对不上的地方。 */
export type LogAnomalyProbeBucket = {
    code: string;
    severity: 'low' | 'medium';
    channel_id: number;
    channel_name: string;
    model: string;
    /** 命中条数。 */
    count: number;
    /** 同桶内参与检查的日志条数（分母）。只给命中数会让人误以为全部日志都出问题。 */
    total: number;
    title: string;
};

export type LogAnomalyReport = {
    window_hours: number;
    sample_count: number;
    bucket_count: number;
    findings: LogAnomalyFinding[];
    /** 硬事实校验汇总，与行为类 findings 分开列（两者证据强度不同）。 */
    probe_findings?: LogAnomalyProbeBucket[];
    /** 被动观察摘要的独立汇总（可选）：只读计数，不参与 findings 判定。 */
    passive_observations?: LogAnomalyPassiveBucket[];
    disclaimer: string;
    /** 样本太少：前端必须说"样本不够"，不许说"没异常"。 */
    too_few_samples?: boolean;
};

export type ScheduledAuditResult = {
    channel_id: number;
    channel_name: string;
    model: string;
    trigger: string;
    /** 触发本次检测的那条日志行 ID（0/缺省 = 不是由单条日志触发）。 */
    log_id?: number;
    skipped?: boolean;
    skip_reason?: string;
    duration_ms?: number;
    verdict?: string;
    score?: number;
    finding_count?: number;
    error_count?: number;
};

export type ScheduledAuditSnapshot = {
    last_run_unix: number;
    target_count: number;
    ran_count: number;
    results: ScheduledAuditResult[];
    disclaimer: string;
};

export function useLogAnomalies(enabled = true) {
    return useQuery({
        queryKey: ['model-audit', 'log-anomalies'],
        queryFn: async () => apiClient.get<LogAnomalyReport>('/api/v1/model-audit/log-anomalies'),
        enabled,
        staleTime: 30_000,
    });
}

export function useScheduledAudit(enabled = true) {
    return useQuery({
        queryKey: ['model-audit', 'scheduled'],
        queryFn: async () => apiClient.get<ScheduledAuditSnapshot>('/api/v1/model-audit/scheduled'),
        enabled,
        staleTime: 30_000,
        // Let a failed read hide stale evidence immediately; the next poll retries.
        retry: false,
        // Read the saved snapshot only; this endpoint never runs upstream probes.
        refetchInterval: 5_000,
        refetchIntervalInBackground: false,
    });
}

/** 运行控制参数：本地交参，不进入网络 body。 */
export type RunModelAuditOptions = {
    signal?: AbortSignal;
};

export type RunModelAuditInput = ModelAuditRequest & RunModelAuditOptions;

export function useRunModelAudit() {
    const queryClient = useQueryClient();
    return useMutation({
        mutationFn: async ({ signal, ...payload }: RunModelAuditInput) => {
            // signal 只作 fetch 的取消信号，绝不打进请求 body。
            return apiClient.post<ModelAuditResponse>('/api/v1/model-audit/run', payload, undefined, { signal });
        },
        onSuccess: () => queryClient.invalidateQueries({ queryKey: ['model-audit'] }),
        onError: (error) => {
            logger.error('模型审计失败:', error);
        },
    });
}

/* ------------------------------------------------------------------ *
 * 批量检测队列：入队、列表、详情、取消
 *
 * 检测会真的打上游并计费，所以它不是"点一下立刻出结果"的同步操作。入队后由
 * 后台串行执行，任务与结果独立落库（不再依附日志行的保留期），随时可以回看。
 * ------------------------------------------------------------------ */

export type AuditJobStatus = 'queued' | 'running' | 'done' | 'canceled';
export type AuditResultStatus = 'pending' | 'running' | 'done' | 'failed' | 'skipped';

export type AuditJob = {
    id: number;
    channel_id: number;
    channel_name: string;
    status: AuditJobStatus;
    total: number;
    finished: number;
    probes: string;
    timeout_seconds: number;
    trigger: string;
    created_at: string;
    started_at?: string;
    ended_at?: string;
    preset?: string;
    single_model?: string;
    single_verdict?: string;
    single_status?: string;
    plain_headline?: string;
    plain_summary?: string;
    verdict_counts?: Record<string, number>;
    report_at?: string;
    retrying?: number;
};

export type AuditResult = {
    id: number;
    job_id: number;
    log_id: number;
    channel_id: number;
    channel_name: string;
    model: string;
    upstream_model: string;
    status: AuditResultStatus;
    verdict: string;
    score: number;
    finding_n: number;
    report: string;
    error_msg: string;
    duration_ms: number;
    created_at: string;
    finished_at?: string;
    /**
     * 已尝试次数。失败后会自动重试（翻倍退避，最长 10 分钟一次），
     * 重试期间状态仍是 pending —— 所以界面上必须能看出"它在等重试"，
     * 否则进度条不动，用户会以为卡死了。
     */
    attempt?: number;
    /** 下次重试时间点。有值 = 正在退避等待，不是排队等前面的人。 */
    next_retry_at?: string;
};

export type AuditJobDetail = {
    job: AuditJob;
    results: AuditResult[];
};

export function useAuditJobs(limit = 50) {
    return useQuery({
        queryKey: ['model-audit', 'jobs', limit],
        queryFn: async () => apiClient.get<AuditJob[]>(`/api/v1/model-audit/jobs?limit=${limit}`),
        staleTime: 5_000,
        retry: false,
        // 有任务在跑时靠列表里的状态驱动轮询；这里固定间隔重取，代价是一条索引查询。
        refetchInterval: 5_000,
    });
}

export function useAuditJobDetail(jobId: number | null) {
    return useQuery({
        queryKey: ['model-audit', 'job', jobId],
        queryFn: async () => apiClient.get<AuditJobDetail>(`/api/v1/model-audit/jobs/${jobId}`),
        enabled: jobId !== null && jobId > 0,
        staleTime: 2_000,
        retry: false,
        refetchInterval: (query) => {
            // 任务跑完就停止轮询，避免空转。
            const data = query.state.data as AuditJobDetail | undefined;
            if (!data) return 3_000;
            return data.job.status === 'done' || data.job.status === 'canceled' ? false : 3_000;
        },
    });
}

export type EnqueueAuditInput = {
    channel_id: number;
    models: string[];
    probes?: AuditProbeId[];
    timeout_seconds?: number;
    log_id?: number;
};

export function useEnqueueAudit() {
    const queryClient = useQueryClient();
    return useMutation({
        mutationFn: async (payload: EnqueueAuditInput) =>
            apiClient.post<{ job_id: number }>('/api/v1/model-audit/enqueue', payload),
        onSuccess: () => {
            void queryClient.invalidateQueries({ queryKey: ['model-audit', 'jobs'] });
        },
        onError: (error) => {
            logger.error('检测入队失败:', error);
        },
    });
}

export function useCancelAuditJob() {
    const queryClient = useQueryClient();
    return useMutation({
        mutationFn: async (jobId: number) =>
            apiClient.post<{ job_id: number }>(`/api/v1/model-audit/jobs/${jobId}/cancel`, {}),
        onSuccess: (_data, jobId) => {
            void queryClient.invalidateQueries({ queryKey: ['model-audit', 'jobs'] });
            void queryClient.invalidateQueries({ queryKey: ['model-audit', 'job', jobId] });
        },
    });
}

export type TokenizerBaseline = {
    id: number;
    model: string;
    label: string;
    bank_generated_at: string;
    source_channel_id: number;
    source_channel_name: string;
    source_result_id: number;
    created_at: string;
};

/**
 * 把一条「录制分词器基线」结果存成可信基线。渠道是否可信由管理员自己判断，后端无法替你验证。
 * 同型号已有基线时后端返回 409，需 replace=true 才覆盖。
 */

export type AuditProbePlan = {
    id: AuditProbeId;
    requests: number;
    applicable: boolean;
    skip_reason?: string;
    reference_only?: boolean;
    advanced?: boolean;
};

export type AuditPlanResponse = {
    protocol: string;
    model: string;
    probes: AuditProbePlan[];
    quick: { ids: AuditProbeId[]; requests: number };
    full: { ids: AuditProbeId[]; requests: number };
};

export function useAuditPlan(channelId: number, model: string, enabled = true) {
    return useQuery({
        queryKey: ['model-audit', 'plan', channelId, model],
        queryFn: async () =>
            apiClient.get<AuditPlanResponse>(
                `/api/v1/model-audit/plan?channel_id=${channelId}&model=${encodeURIComponent(model)}`,
            ),
        enabled: enabled && channelId > 0,
        staleTime: 30_000,
    });
}

export type ChannelLatestAudit = AuditResult & {
    chip: 'ok' | 'warn' | 'fail' | 'none';
    chip_label: string;
    headline?: string;
    summary?: string;
    points?: string[];
};

export function useChannelLatestAudits(channelId: number, enabled = true) {
    return useQuery({
        queryKey: ['model-audit', 'channel-latest', channelId],
        queryFn: async () =>
            apiClient.get<ChannelLatestAudit[]>(`/api/v1/model-audit/channels/${channelId}/latest`),
        enabled: enabled && channelId > 0,
        staleTime: 15_000,
    });
}

export function useSaveTokenizerBaseline() {
    const queryClient = useQueryClient();
    return useMutation({
        mutationFn: async (payload: { result_id: number; label?: string; replace?: boolean }) =>
            apiClient.post<{ baseline: TokenizerBaseline }>('/api/v1/model-audit/tokenizer-baselines', payload),
        onSuccess: () => {
            queryClient.invalidateQueries({ queryKey: ['model-audit', 'tokenizer-baselines'] });
        },
    });
}
