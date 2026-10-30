import type { AuditProbeResult } from '@/api/endpoints/model-audit';

/**
 * 把「分词器识别」「录制分词器基线」两项探针的 data 读成人话。
 *
 * 字段名以后端为准（internal/modelverify/behavior/tokenizer_probe.go）。
 * 方法照抄 lm-detector：P t S 与 P S 的输入 token 差分，对 290 个开源分词器整理出的
 * 62 类做贝叶斯后验。语义边界与参考实现一致，展示层不放宽：
 * - 一个分词器常被多个模型共用，测出来的是「上游用的哪套分词器」，不是「哪个模型」；
 * - 测成 o200k / cl100k 时，也可能是中转站用 tiktoken 在本地估算用量。
 * - 结果只作参考（lm-detector principles/tokenizer.en.mdx:83）：后端 0 分、不改变结论，
 *   页面上也不算「需要复核」的一项。
 */

export type TokenizerKind = 'exact' | 'related' | 'unknown';
export type TokenizerClaim = 'consistent' | 'inconsistent' | 'uncertain' | '';

export type TokenizerCandidate = {
    id: string;
    series: string;
    lab: string;
    exact: number;
    related: number;
};

export type TokenizerSummary = {
    kind: TokenizerKind;
    series: string;
    lab: string;
    local: boolean;
    confidence?: number;
    claim: TokenizerClaim;
    claimProbability?: number;
    claimVendor: string;
    requests: number;
    /** 跑完且两次基线一致才算数；否则只作参考。 */
    usable: boolean;
    drift: boolean;
    relayEstimate: boolean;
    bankClasses: number;
    bankGeneratedAt: string;
    candidates: TokenizerCandidate[];
    warnings: string[];
    errorText?: string;
};

export type TokenizerRecording = {
    recordable: boolean;
    reason: string;
    targetModel: string;
    probeCount: number;
    bankGeneratedAt: string;
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
    return typeof value === 'number' && Number.isFinite(value) ? value : undefined;
}

function findResult(report: { results?: AuditProbeResult[] } | undefined, id: string): AuditProbeResult | undefined {
    return report?.results?.find((item) => item.probe_id === id);
}

export function readTokenizer(report: { results?: AuditProbeResult[] } | undefined): TokenizerSummary | null {
    const result = findResult(report, 'tokenizer_id');
    if (!result) return null;
    const data = asRecord(result.data);
    const rawKind = asString(data.kind);
    const kind: TokenizerKind = rawKind === 'exact' || rawKind === 'related' ? rawKind : 'unknown';
    const rawClaim = asString(data.claim_status);
    const claim: TokenizerClaim =
        rawClaim === 'consistent' || rawClaim === 'inconsistent' || rawClaim === 'uncertain' ? rawClaim : '';
    const candidates = Array.isArray(data.candidates)
        ? data.candidates.map((item) => {
              const c = asRecord(item);
              return {
                  id: asString(c.id),
                  series: asString(c.series) || asString(c.id),
                  lab: asString(c.lab),
                  exact: asNumber(c.exact) ?? 0,
                  related: asNumber(c.related) ?? 0,
              };
          }).filter((c) => c.id !== '')
        : [];
    const warnings = Array.isArray(data.warnings) ? data.warnings.map(asString).filter(Boolean) : [];
    return {
        kind,
        series: asString(data.top_series) || asString(data.top_class),
        lab: asString(data.top_lab),
        local: data.top_local === true,
        confidence: asNumber(data.confidence),
        claim,
        claimProbability: asNumber(data.claim_probability),
        claimVendor: asString(data.claim_vendor),
        requests: asNumber(data.requests) ?? 0,
        usable: data.usable === true,
        drift: data.baseline_drift === true,
        relayEstimate: data.relay_estimate_possible === true,
        bankClasses: asNumber(data.bank_classes) ?? 0,
        bankGeneratedAt: asString(data.bank_generated_at),
        candidates,
        warnings,
        errorText: result.ok ? undefined : asString(result.error) || '分词器探测失败',
    };
}

export function readTokenizerRecording(report: { results?: AuditProbeResult[] } | undefined): TokenizerRecording | null {
    const result = findResult(report, 'tokenizer_baseline');
    if (!result) return null;
    const data = asRecord(result.data);
    const counts = asRecord(data.counts);
    return {
        recordable: result.ok && data.recordable === true,
        reason: asString(data.reason) || (result.ok ? '' : asString(result.error)),
        targetModel: asString(data.target_model),
        probeCount: Object.keys(counts).length,
        bankGeneratedAt: asString(data.bank_generated_at),
    };
}

/** 一句话结论；措辞与后端 tokenizer_score.go 对齐。 */
export function describeTokenizer(summary: TokenizerSummary): string {
    if (summary.errorText) return summary.errorText;
    const name = summary.lab ? `${summary.series}（${summary.lab}）` : summary.series;
    let head: string;
    if (summary.kind === 'exact') head = `计数与 ${name} 完全吻合`;
    else if (summary.kind === 'related') head = `接近 ${name}，但不是库里的原版（可能是改过词表的变体）`;
    else head = '不像库里任何一类分词器';
    if (!summary.usable) {
        return `${head}。${summary.drift ? '两次基线计数不同，上游附加的隐藏输入在变，' : '探测中途失败，'}结果仅供参考。`;
    }
    switch (summary.claim) {
        case 'consistent':
            return `${head}，与声明型号相容。`;
        case 'inconsistent':
            return `${head}，与声明型号不符${summary.relayEstimate ? '（也可能是中转站本地估算用量）' : ''}。仅供参考，不计入风险分。`;
        case 'uncertain':
            return `${head}，与声明型号是否相容不确定。`;
        default:
            return `${head}。声明型号不在映射里，无法核对。`;
    }
}

export function formatShare(value: number | undefined): string {
    if (value === undefined) return '';
    if (value >= 0.995) return '>99%';
    if (value < 0.005) return '<1%';
    return `${Math.round(value * 100)}%`;
}

/**
 * 检测行的状态文字。分词器只作参考：行一律是 info，不进「待复核」计数，
 * 否则 0 分的参考提示会在页面上被数成「发现 N 项需要复核」。
 */
export function tokenizerRowStatus(summary: TokenizerSummary): string {
    if (summary.errorText) return '执行未完成';
    if (!summary.usable) return '仅供参考';
    switch (summary.claim) {
        case 'consistent':
            return '与声明相容';
        case 'inconsistent':
            return '与声明不符 · 仅供参考';
        case 'uncertain':
            return '相容性不确定';
        default:
            return '无法核对';
    }
}
