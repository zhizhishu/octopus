import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiClient } from '../client';
import { logger } from '@/lib/logger';

export const AUDIT_PROBE_IDS = [
    'liveness',
    'identity',
    // 复述系统提示词：1 次请求，看它收到的系统提示词里有没有别家平台的字样。
    'system_prompt',
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

/**
 * 结论下面那几行短句（后端算好的「人话要点」）。
 *
 * percent 只有后端给了才展示，前端绝不自己算百分比；tone 未知按 info 处理。
 */
export type AuditHighlight = {
    kind: string;
    text: string;
    tone: 'ok' | 'warn' | 'info';
    /** 后端算好的百分比（0-100）。没有就不显示数字。 */
    percent?: number;
};

/** 一条渠道来源线索：只提示「疑似」，不能证明是官方正品。 */
export type AuditProvenanceHint = {
    platform: string;
    strength: 'strong' | 'medium' | 'weak';
    /** 机器码，翻译映射见 lib/passive-verdict 的 PASSIVE_REASON_LABEL_KEY。 */
    code: string;
    evidence?: string;
};

/** 渠道来源（被动观察汇总）：observed 是看过多少份回复，为 0 时前端不出这张卡。 */
export type AuditProvenance = {
    hints?: AuditProvenanceHint[];
    header_families?: string[];
    observed?: number;
};

export type AuditViewReport = {
    /**
     * 报告种类。历史报告没有这个键（等值于 'audit'），hlwy 基准对照是
     * 'hlwy_compare'：它不参与风险计分，结论放在 baseline_comparison 里。
     *
     * 刻意**可选**：旧报文的读取路径必须原样可用，不能因为多了一个键就要求
     * 所有存量报告重新生成。
     */
    kind?: string;
    /** hlwy 对照结果（见 HlwyBaselineComparison）。非本模式为 undefined。 */
    baseline_comparison?: Record<string, unknown>;
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
    /** 结论下方的要点短句（可选；旧报告没有）。 */
    highlights?: AuditHighlight[];
    /** 渠道来源线索（可选；旧报告或样本不足时没有）。 */
    provenance?: AuditProvenance;
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
    /**
     * 来源结果行的状态（从「检测记录」打开时带上）。
     *
     * 「能不能存成基准」必须看这个：同样的报告，正常跑完和中断后保留的结果，
     * 可信度完全不同。前端不猜，只用结果行上写着的事实。
     */
    result_status?: string;
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

/** 任务种类。缺省/空串 = 原有的多探针检测。 */
export type AuditJobKind = 'audit' | 'hlwy_compare';

/**
 * 队列行上的 hlwy 样本账目摘要。
 *
 * 与任务的 finished/total 是两件事：后者是「几个模型跑完了」，这里是「这一轮
 * 100/200 次采样走到哪」。混用会直接说错话（1/1 个模型跑完 ≠ 100 个样本到齐）。
 */
export type HlwyJobSummary = {
    execution: string;
    sufficiency: string;
    comparability: string;
    counts?: HlwyCounts;
    stop_reason?: string;
};

export type HlwyCounts = {
    planned_calls: number;
    started_slots: number;
    valid: number;
    invalid: number;
    failed: number;
    unresolved: number;
};

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
    /** 任务种类：'audit'（旧的多探针检测）或 'hlwy_compare'。 */
    kind?: AuditJobKind | string;
    /** hlwy 对照任务的采样摘要；其它种类为空。 */
    hlwy_summary?: HlwyJobSummary;
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
            if (data.job.status === 'done') return false;
            if (data.job.status === 'canceled') {
                // 取消只阻止**新的**采样：已经发出去的那一次还在上游跑着，它的结果
                // 仍会落库。此刻停掉轮询，用户看到的就是一行永远停在「检测中」
                // 的结果——取消不能变成「结果不给你了」。
                const stillRunning = (data.results ?? []).some((item) => item.status === 'running');
                return stillRunning ? 3_000 : false;
            }
            return 3_000;
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

/* ------------------------------------------------------------------ *
 * hlwy「1–355 单数字」基准对照
 *
 * 与上面的多探针检测是同一张任务表上的另一种 kind，但语义完全不同：
 * 它发同一个题目 N 次（100/200）、只统计数字分布，不参与风险计分。
 * 请求侧只允许三个字段（samples / baseline_id / protocol_id），候选基准由
 * 服务端在入队那一刻固化——客户端能注入候选，就能把相似度刷到自己想要的位置。
 * ------------------------------------------------------------------ */

/** 首版只允许 100 或 200：样本量直接决定费用，也决定「够不够当参考」。 */
export type HlwySampleCount = 100 | 200;

/** 采样条件（三态：unknown 未观测 / absent 观测到最终请求体省略 / value 显式值）。 */
export type HlwyConditions = {
    protocol?: string;
    protocol_id?: string;
    parser_version?: string;
    prompt_sha256?: string;
    transport_profile?: string;
    temperature_state?: string;
    temperature?: number;
    top_p_state?: string;
    top_p?: number;
    stream?: boolean;
    max_token_field?: string;
    max_tokens?: number;
    thinking?: string;
    reasoning_effort?: string;
    system_injected?: boolean;
    tools_injected?: boolean;
    completeness?: string;
    unknown_fields?: string[];
};

/**
 * 基准目录里的一条（catalog Entry）。
 *
 * 刻意不带 raw_numbers/counts：目录只回答「有哪些基准、它们从哪来、采了多少」。
 * 完整分布跟着任务选项冻结、跟着报告快照回放，不在列表里重复一遍重量级数据。
 */
export type HlwyBaselineEntry = {
    id: string;
    label: string;
    /** 'builtin_hlwy' | 'imported_hlwy' | 'local_capture'（后端权威，前端只映射文案）。 */
    source_kind: string;
    declared_model?: string;
    api_type?: string;
    /** 采集日期。旧文件缺失/无效时为 null —— 前端只能说「日期未知」，不能补今天。 */
    collected_at?: string | null;
    imported_at?: string;
    valid_count: number;
    payload_sha256: string;
    builtin?: boolean;
    source_result_id?: number;
    conditions?: HlwyConditions;
    /**
     * 服务端重读时没通过自洽校验的本地件（哈希/计数/统计对不上、内容读不出）。
     *
     * 这类件**只是自己不能选**：它不阻断内置件和别的健康件，也不隐藏已有历史。
     * 前端据此禁用该选项并把 `reason_code` 翻成人话；不在这里猜原因。
     */
    unavailable?: boolean;
    reason_code?: string;
};

export type HlwyBaselinesResponse = { baselines: HlwyBaselineEntry[] };

export type HlwyBaselineResponse = { baseline: HlwyBaselineEntry; existed: boolean };

/**
 * 服务器给出的计划（零请求、零费用）。
 *
 * 失败时**没有**计划：界面不许自己编一个「约 N 次」占位数字——那是拿猜测冒充
 * 价格承诺。没有计划就不给开始。
 */
export type HlwyPlanResponse = {
    kind: string;
    enabled: boolean;
    samples: HlwySampleCount;
    max_calls: number;
    timeout_seconds: number;
    sample_timeout_seconds: number;
    protocol_id: string;
    baseline: HlwyBaselineEntry;
    conditions_note: string;
};

export function useHlwyBaselines(enabled = true) {
    return useQuery({
        queryKey: ['model-audit', 'hlwy-baselines'],
        queryFn: async () => apiClient.get<HlwyBaselinesResponse>('/api/v1/model-audit/hlwy-baselines'),
        enabled,
        staleTime: 60_000,
        retry: false,
    });
}

/**
 * 读「这一单会花多少钱/多少时间」的计划。
 *
 * baseline_id 是必填的查询条件：同一轮里换了主对照，超时与调用上限可能不同，
 * 而计划正是给用户看「这一单」的账。
 */
export function useHlwyPlan(input: {
    channelId: number;
    model: string;
    samples: HlwySampleCount;
    baselineId: string;
    enabled?: boolean;
}) {
    const ready =
        input.channelId > 0 && input.model.trim() !== '' && input.baselineId.trim() !== '';
    return useQuery({
        queryKey: ['model-audit', 'hlwy-plan', input.channelId, input.model, input.samples, input.baselineId],
        queryFn: async () =>
            apiClient.get<HlwyPlanResponse>('/api/v1/model-audit/plan', {
                kind: 'hlwy_compare',
                channel_id: input.channelId,
                model: input.model,
                samples: input.samples,
                baseline_id: input.baselineId,
            }),
        enabled: (input.enabled ?? true) && ready,
        // 计划只是一次本地计算，但没必要每敲一个字都重读；换了基准/样本量键就变了。
        staleTime: 15_000,
        retry: false,
    });
}

/** 提交一轮基准对照。请求体里**没有** probes / log_id：多探针与日志回填都不适用于本模式。 */
export type EnqueueHlwyInput = {
    channel_id: number;
    /** 一轮只允许一个模型：采样账目与「哪个模型」必须一一对应。 */
    models: [string];
    kind: 'hlwy_compare';
    hlwy: { samples: HlwySampleCount; baseline_id: string; protocol_id: string };
};

export function useEnqueueHlwyCompare() {
    const queryClient = useQueryClient();
    return useMutation({
        mutationFn: async (payload: EnqueueHlwyInput) =>
            apiClient.post<{ job_id: number }>('/api/v1/model-audit/enqueue', payload),
        onSuccess: () => {
            void queryClient.invalidateQueries({ queryKey: ['model-audit', 'jobs'] });
        },
        onError: (error) => {
            logger.error('基准对照入队失败:', error);
        },
    });
}

/** 导入一份旧 hlwy JSON（最多 1 MiB，服务端还会计数校验）。 */
export function useImportHlwyBaseline() {
    const queryClient = useQueryClient();
    return useMutation({
        mutationFn: async (payload: Record<string, unknown>) =>
            apiClient.post<HlwyBaselineResponse>('/api/v1/model-audit/hlwy-baselines/import', payload),
        onSuccess: () => {
            queryClient.invalidateQueries({ queryKey: ['model-audit', 'hlwy-baselines'] });
        },
    });
}

/** 把一条已完成的对照结果存成「我的基准」（服务端还会再核一遍资格）。 */
export function useSaveHlwyBaselineFromResult() {
    const queryClient = useQueryClient();
    return useMutation({
        mutationFn: async (payload: { result_id: number; label?: string }) =>
            apiClient.post<HlwyBaselineResponse>('/api/v1/model-audit/hlwy-baselines/from-result', payload),
        onSuccess: () => {
            queryClient.invalidateQueries({ queryKey: ['model-audit', 'hlwy-baselines'] });
        },
    });
}

/**
 * 换一份基准重算：完全离线，不发请求、不写库、不改原报告。
 *
 * 因此这里刻意不 invalidate 任何列表——它没有改变任何服务端事实，
 * 只是把同一批已存数字重新算了一遍。
 */
export type HlwyRecompareInput = { result_id: number; baseline_id: string };

export function useRecompareHlwy() {
    return useMutation({
        mutationFn: async (payload: HlwyRecompareInput) =>
            apiClient.post<{ report: Record<string, unknown> }>('/api/v1/model-audit/hlwy-compare', payload),
        onError: (error) => {
            logger.error('换基准重算失败:', error);
        },
    });
}
