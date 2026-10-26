import type { AuditProbeResult } from '@/api/endpoints/model-audit';

/**
 * 把「随机数指纹」探针的原始 data 读成人能看懂的结论。
 *
 * 为什么需要这一层：后端其实一直在算结论——`behavior/fingerprint_probe.go` 会把
 * 最像的模型、家族、前五名候选、概率、参考库构建时间全塞进 `res.Data`。但检测页
 * 只把它当成一行「随机数指纹 · 未见异常/证据不足」，真正的内容（到底最像哪个模型）
 * 被埋在证据抽屉的原始 JSON 里。于是用户跑完一次检测，看到的全是"状态"，看不到
 * "内容"——这正是"检测不出东西"的观感来源。
 *
 * 字段名一律以后端 struct tag / Data 键为准（`internal/modelverify/behavior/fingerprint_probe.go`）：
 *   fingerprint_decision / fingerprint_prediction / fingerprint_prediction_name /
 *   fingerprint_family / fingerprint_family_name / fingerprint_reason /
 *   fingerprint_reference_built_at / fingerprint_used_outputs /
 *   fingerprint_probability_scope / fingerprint_probability / fingerprint_error /
 *   fingerprint_candidates[{model,display_name,family,family_name,score,probability}]
 *
 * 语义边界（与后端措辞一致，不许在展示层放宽）：结论是**参考库内的相对排名**，
 * 库外模型照样会得到一个"最像"的答案，所以它不是"确认就是某模型"。
 */

/** 后端 Decision 取值，语义与 lm-detector 上游一致。 */
export type FingerprintDecision = 'scorable' | 'partial' | 'unscorable' | 'unknown';

export type FingerprintCandidate = {
    model: string;
    displayName: string;
    familyName: string;
    score: number;
    probability?: number;
};

export type FingerprintSummary = {
    decision: FingerprintDecision;
    /** 最像的模型（人话名优先，没有就用 id）。 */
    predictionName: string;
    familyName: string;
    /** 后端给的原因：证据不足时说明为什么，可评分时为空。 */
    reason: string;
    /** 参考库内置信度（0–1）。只有三条都有效时后端才给。 */
    probability?: number;
    candidates: FingerprintCandidate[];
    /** 实际参与判定的回答条数。 */
    usedOutputs: number;
    /** 参考库构建时间（ISO 串），用来判断结论新不新。 */
    referenceBuiltAt: string;
    /** 概率口径说明，后端固定给 reference-closed-set。 */
    probabilityScope: string;
    /** 证据不足：不能当成"通过"。 */
    insufficient: boolean;
    /** 采集或分析直接失败时的原因（此时没有排名）。 */
    errorText?: string;
};

function asRecord(value: unknown): Record<string, unknown> {
    if (value && typeof value === 'object' && !Array.isArray(value)) {
        return value as Record<string, unknown>;
    }
    return {};
}

function asString(value: unknown): string {
    return typeof value === 'string' ? value.trim() : '';
}

function asNumber(value: unknown): number | undefined {
    if (typeof value === 'number' && Number.isFinite(value)) return value;
    // 后端 JSON 里数字偶尔会以字符串形态出现（例如经过某些序列化路径），
    // 这里容错一下，但绝不用 Number() 硬转任意值——那会把 null 变成 0。
    if (typeof value === 'string' && value.trim() !== '') {
        const parsed = Number(value);
        if (Number.isFinite(parsed)) return parsed;
    }
    return undefined;
}

function normalizeDecision(value: unknown): FingerprintDecision {
    const raw = asString(value);
    if (raw === 'scorable' || raw === 'partial' || raw === 'unscorable') return raw;
    return 'unknown';
}

function readCandidates(value: unknown): FingerprintCandidate[] {
    if (!Array.isArray(value)) return [];
    const out: FingerprintCandidate[] = [];
    for (const item of value) {
        const entry = asRecord(item);
        const model = asString(entry.model);
        const score = asNumber(entry.score);
        // 没有模型名或没有分数的项无法展示，直接跳过而不是塞个 0 分占位。
        if (!model || score === undefined) continue;
        const candidate: FingerprintCandidate = {
            model,
            displayName: asString(entry.display_name) || model,
            familyName: asString(entry.family_name),
            score,
        };
        const probability = asNumber(entry.probability);
        if (probability !== undefined) candidate.probability = probability;
        out.push(candidate);
    }
    return out;
}

/**
 * 从一份检测报告里读出指纹结论。没跑这项探针时返回 null（调用方据此不渲染整块）。
 */
export function readFingerprint(
    report: { results?: AuditProbeResult[] } | null | undefined,
): FingerprintSummary | null {
    const results = report?.results;
    if (!Array.isArray(results)) return null;
    const probe = results.find((item) => item.probe_id === 'fingerprint');
    if (!probe) return null;

    const data = asRecord(probe.data);
    const candidates = readCandidates(data.fingerprint_candidates);
    const decision = normalizeDecision(data.fingerprint_decision);
    const errorText = asString(data.fingerprint_error) || asString(probe.error);
    const predictionName =
        asString(data.fingerprint_prediction_name) || asString(data.fingerprint_prediction);

    // 探针跑过但什么结论都没有（既没 decision、没候选、也没错误）：
    // 说明这一行是旧数据或后端没写结论，不摆空壳。
    if (decision === 'unknown' && candidates.length === 0 && !errorText && !predictionName) {
        return null;
    }

    const summary: FingerprintSummary = {
        decision,
        predictionName,
        familyName: asString(data.fingerprint_family_name) || asString(data.fingerprint_family),
        reason: asString(data.fingerprint_reason) || asString(data.reason),
        candidates,
        usedOutputs: asNumber(data.fingerprint_used_outputs) ?? 0,
        referenceBuiltAt: asString(data.fingerprint_reference_built_at),
        probabilityScope: asString(data.fingerprint_probability_scope),
        insufficient: data.insufficient_evidence === true || decision === 'unscorable',
    };

    const probability = asNumber(data.fingerprint_probability);
    if (probability !== undefined) summary.probability = probability;
    if (errorText) summary.errorText = errorText;

    return summary;
}

/** 概率 → 百分比文案。后端只在三条都有效时给概率，没有就不显示。 */
export function formatProbability(probability: number | undefined): string {
    if (probability === undefined || !Number.isFinite(probability)) return '';
    const pct = probability * 100;
    if (pct > 0 && pct < 0.1) return '<0.1%';
    return `${pct.toFixed(pct >= 10 ? 0 : 1)}%`;
}

/** 决策 → 一句话说明。措辞必须保留"参考库内相对排名"这个限定。 */
export function describeDecision(summary: FingerprintSummary): string {
    if (summary.errorText) return `这次没测成：${summary.errorText}`;
    switch (summary.decision) {
        case 'scorable':
            return '三条回答都有效，按参考库给出排名与置信度';
        case 'partial':
            return '有效回答不足三条：只给排名，不给置信度';
        case 'unscorable':
            return summary.reason ? `证据不足：${summary.reason}` : '证据不足：有效数字太少，连排名都不该给';
        default:
            return summary.reason || '没有拿到可判定的结论';
    }
}
