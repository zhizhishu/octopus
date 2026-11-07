import type { HlwyBaselineEntry, HlwyConditions, HlwyCounts, HlwyJobSummary } from '@/api/endpoints/model-audit';

/**
 * hlwy「1–355 单数字」基准对照的纯读模型。
 *
 * 这个文件没有 React、没有 fetch、没有 i18n：所有判断都是「从一份已存的报告
 * 里读得出什么」。这样测试可以直接跑真实实现，而 UI 只负责把这些结论画出来。
 *
 * 三条硬规矩贯穿全文件：
 *  1. 读不出来就说读不出来。缺字段、类型不对、数组长度不对，一律不补默认值、
 *     不补今天的日期、不补 0 —— 界面上「0 个有效样本」和「不知道有几个有效
 *     样本」是完全不同的两句话。
 *  2. 分数只在报告自己说可评分时展示。不兼容（incompatible）或样本不足一律
 *     不出百分比：一个四位小数的相似度看上去比它实际的含义精确得多。
 *  3. 导出走显式白名单。报告里可能混进来 endpoint、header、备注之类的字段，
 *     「整份 spread 出去」等于把渠道信息抄进一个即将被到处转发的文件。
 */

/** 展示用的分箱数：355 个数字压成 36 段（前 35 段各 10 个，最后 5 个一段）。 */
export const HLWY_BIN_COUNT = 36;
/** 1–355 的取值上界（协议约定，不是经验值）。 */
export const HLWY_MAX_DIGIT = 355;
/** 采样槽位总数上限之一：存成基准要求至少 100 个有效样本（服务端会再核一遍）。 */
export const HLWY_MIN_CAPTURE_VALID = 100;
/** 导入文件大小上限（与服务端 MaxBytes 一致，先在本地挡一次）。 */
export const HLWY_IMPORT_MAX_BYTES = 1 << 20;
/** 报告里 raw_numbers / distribution 的固定长度。 */
const DIST_LENGTH = 355;

export type HlwyBaselineSourceKind = 'builtin_hlwy' | 'imported_hlwy' | 'local_capture';

export type HlwyMatchView = {
    baselineId: string;
    rank: number;
    /** 综合相似度 0..1（原始值，不四舍五入）。 */
    overall: number | null;
    /** 展示用百分比文本；不可评分时为 null。 */
    percent: string | null;
    cosine: number | null;
    jsd: number | null;
    modeScore: number | null;
    /** 与上一名的差距（百分点）；第一名没有。 */
    gapPoints: number | null;
    gapText: string | null;
    label: string;
    sourceKind: string;
    sourceText: string;
    collectedAt: string | null;
    validCount: number | null;
    /** 基准快照自身的 sha256；从快照表查，查不到就是空串（不编）。 */
    payloadSha256: string;
};

export type HlwySnapshotView = {
    id: string;
    label: string;
    sourceKind: string;
    sourceText: string;
    declaredModel: string;
    apiType: string;
    collectedAt: string | null;
    validCount: number | null;
    payloadSha256: string;
    distribution: number[] | null;
    conditions: Record<string, unknown> | null;
};

export type HlwyBin = {
    /** 覆盖的数字区间（含两端）。 */
    from: number;
    to: number;
    /** 本次采样落在该段的占比 0..1。 */
    run: number;
    /** 主对照基准落在该段的占比；没有可用基准分布时为 null。 */
    baseline: number | null;
};

export type HlwyStatView = {
    mean: number | null;
    stdDev: number | null;
    median: number | null;
    min: number | null;
    max: number | null;
    mode: number | null;
    modeCount: number | null;
    unique: number | null;
};

export type HlwyQualityView = {
    strictInteger: number | null;
    permissive: number | null;
    truncated: number | null;
};

export type HlwyComparisonView = {
    execution: string;
    sufficiency: string;
    comparability: string;
    reasonCodes: string[];
    /** 采样账目。报告没写或写坏了就是 null —— 界面此时不许报 0。 */
    counts: HlwyCounts | null;
    stopReason: string;
    selfCompare: boolean;
    algorithmVersion: string;
    schemaVersion: number | null;
    chosenBaselineId: string;
    chosen: HlwyMatchView | null;
    matches: HlwyMatchView[];
    top: HlwyMatchView[];
    snapshots: HlwySnapshotView[];
    conditions: Record<string, unknown> | null;
    rawNumbers: number[] | null;
    stats: HlwyStatView | null;
    quality: HlwyQualityView | null;
    distribution: number[] | null;
    bins: HlwyBin[];
    /** 报告自带的可评分判定（缺失时为 null，不做本地推断）。 */
    scorable: boolean | null;
    /** 是否允许展示相似度百分比。 */
    hasScore: boolean;
    /** 是否只有部分样本（失败/取消/中断后仍保留了能描述的结果）。 */
    partial: boolean;
    /** 由已解析的状态、名称与计数生成的展示文案，不透传后端摘要。 */
    headline: string;
    summary: string;
    /** 解析中遇到的问题；非空表示「只能展示能确认的部分」。 */
    problem: string;
};

/* ------------------------------------------------------------------ *
 * 基础类型守卫
 * ------------------------------------------------------------------ */

function isRecord(value: unknown): value is Record<string, unknown> {
    return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function numOrNull(value: unknown): number | null {
    return typeof value === 'number' && Number.isFinite(value) ? value : null;
}

function intOrNull(value: unknown): number | null {
    const n = numOrNull(value);
    return n !== null && Number.isInteger(n) ? n : null;
}

function strOrEmpty(value: unknown): string {
    return typeof value === 'string' ? value : '';
}

function boolOrNull(value: unknown): boolean | null {
    return typeof value === 'boolean' ? value : null;
}

function strArray(value: unknown): string[] {
    if (!Array.isArray(value)) return [];
    return value.filter((item): item is string => typeof item === 'string');
}

/** 355 项、有限、非负的分布数组；否则 null（不截断、不补齐）。 */
function readDistribution(value: unknown): number[] | null {
    if (!Array.isArray(value) || value.length !== DIST_LENGTH) return null;
    const out: number[] = [];
    for (const item of value) {
        const n = numOrNull(item);
        if (n === null || n < 0) return null;
        out.push(n);
    }
    return out;
}

/** 1..355 的整数样本序列；任一项越界就整条丢弃（不静默过滤）。 */
function readNumbers(value: unknown): number[] | null {
    if (!Array.isArray(value) || value.length === 0) return null;
    const out: number[] = [];
    for (const item of value) {
        const n = intOrNull(item);
        if (n === null || n < 1 || n > HLWY_MAX_DIGIT) return null;
        out.push(n);
    }
    return out;
}

function readCounts(value: unknown): HlwyCounts | null {
    if (!isRecord(value)) return null;
    const plannedCalls = intOrNull(value.planned_calls);
    const startedSlots = intOrNull(value.started_slots);
    const valid = intOrNull(value.valid);
    const invalid = intOrNull(value.invalid);
    const failed = intOrNull(value.failed);
    const unresolved = intOrNull(value.unresolved);
    if (
        plannedCalls === null ||
        startedSlots === null ||
        valid === null ||
        invalid === null ||
        failed === null ||
        unresolved === null
    ) {
        return null;
    }
    return {
        planned_calls: plannedCalls,
        started_slots: startedSlots,
        valid,
        invalid,
        failed,
        unresolved,
    };
}

function readStats(value: unknown): HlwyStatView | null {
    if (!isRecord(value)) return null;
    return {
        mean: numOrNull(value.mean),
        stdDev: numOrNull(value.std_dev),
        median: numOrNull(value.median),
        min: numOrNull(value.min),
        max: numOrNull(value.max),
        mode: numOrNull(value.mode),
        modeCount: numOrNull(value.mode_count),
        unique: numOrNull(value.unique),
    };
}

function readQuality(value: unknown): HlwyQualityView | null {
    if (!isRecord(value)) return null;
    return {
        strictInteger: intOrNull(value.strict_integer),
        permissive: intOrNull(value.permissive),
        truncated: intOrNull(value.truncated),
    };
}

/* ------------------------------------------------------------------ *
 * 文案映射（全部中性：只说条件与分布，不说真假）
 * ------------------------------------------------------------------ */

export function hlwySourceLabel(sourceKind: string): string {
    switch (sourceKind) {
        case 'builtin_hlwy':
            return 'hlwy 社区基准';
        case 'imported_hlwy':
            return '导入的旧文件';
        case 'local_capture':
            return '本地自采';
        default:
            return sourceKind ? `来源未识别（${sourceKind}）` : '来源未标注';
    }
}

export function hlwyExecutionLabel(execution: string): string {
    switch (execution) {
        case 'sampling':
            return '采样中';
        case 'complete':
            return '对照完成';
        case 'canceled':
            return '已停止，保留已有结果';
        case 'timed_out':
            return '超时结束，保留已有结果';
        case 'failed':
            return '失败';
        case 'interrupted':
            return '执行中断，保留已有结果';
        default:
            return '状态未知';
    }
}

/** 仅翻译本项目已有的停止原因；原始码仍由完整导出保留。 */
export function hlwyStopReasonLabel(code: string): string {
    if (!code) return '';
    const labels: Record<string, string> = {
        canceled: '已取消采样',
        interrupted: '采样意外中断',
        run_timeout: '采样时间已到，提前结束',
        consecutive_failures: '连续多次调用失败，已停止采样',
        reserve_failed: '无法登记下一次调用，已停止采样',
        settle_failed: '无法保存本次回答，已停止采样',
        prompt_mismatch: '采样题目发生变化，已停止采样',
        protocol_mismatch: '采样方法不一致，已停止采样',
        conditions_changed: '采样设置发生变化，已停止采样',
        request_rejected: '请求未被接受，已停止采样',
        compare_failed: '未能完成比较，已保留采样结果',
        temperature_changed: '随机性设置发生变化，已停止采样',
        top_p_changed: '选词范围设置发生变化，已停止采样',
        stream_changed: '回复方式发生变化，已停止采样',
        max_tokens_changed: '回复长度上限发生变化，已停止采样',
        thinking_changed: '思考设置发生变化，已停止采样',
        reasoning_effort_changed: '思考强度发生变化，已停止采样',
        system_changed: '系统提示发生变化，已停止采样',
        tools_changed: '工具设置发生变化，已停止采样',
        http_401: '渠道认证失败，已停止采样',
        http_403: '渠道拒绝访问，已停止采样',
        http_404: '请求的模型或接口不可用，已停止采样',
        http_429: '调用过于频繁或额度受限，已停止采样',
    };
    return labels[code] || (/^http_4\d{2}$/.test(code)
        ? '请求未被接受，已停止采样'
        : '采样未能继续，具体原因未记录');
}

export function hlwyComparabilityLabel(comparability: string): string {
    switch (comparability) {
        case 'matched':
            return '两边采样条件一致';
        case 'different':
            return '两边采样条件不同，分布只能作参考';
        case 'unknown':
            return '采样条件没有完整记录';
        case 'incompatible':
            return '协议或题目不兼容，不能放在一起比';
        default:
            return '可比性未标注';
    }
}

export function hlwyComparabilityTone(comparability: string): 'ok' | 'warn' | 'ref' {
    switch (comparability) {
        case 'matched':
            return 'ok';
        case 'different':
            return 'warn';
        default:
            return 'ref';
    }
}

/** 采样条件逐项文案；缺的写「未知」，绝不拿默认值代替。 */
export function hlwyConditionRows(conditions: Record<string, unknown> | null): { label: string; value: string }[] {
    if (!conditions) {
        return [{ label: '采样条件', value: '未知（报告里没有记录）' }];
    }
    const typed = conditions as HlwyConditions;
    const triState = (state: unknown, value: unknown): string => {
        if (state === 'absent') return '未下发（观测到省略）';
        if (state === 'unknown' || state === undefined) return '未知';
        const n = numOrNull(value);
        return n === null ? '未知' : String(n);
    };
    const boolText = (value: unknown): string => {
        const b = boolOrNull(value);
        return b === null ? '未知' : b ? '是' : '否';
    };
    const rows: { label: string; value: string }[] = [
        // 「接口协议」和「题目协议」分开写：前者是请求走的协议族（openai-chat /
        // anthropic-messages），后者是题目与解析的版本（hlwy-number355-v1）。
        // 合成一行「协议」就会让人把方法版本当成请求协议读。
        { label: '接口协议', value: strOrEmpty(typed.protocol) || '未知' },
        { label: '题目协议', value: strOrEmpty(typed.protocol_id) || '未知' },
        { label: '解析版本', value: strOrEmpty(typed.parser_version) || '未知' },
        {
            label: '题目指纹',
            value: strOrEmpty(typed.prompt_sha256) ? strOrEmpty(typed.prompt_sha256).slice(0, 12) : '未知',
        },
        { label: '传输形态', value: strOrEmpty(typed.transport_profile) || '未知' },
        { label: 'temperature', value: triState(typed.temperature_state, typed.temperature) },
        { label: 'top_p', value: triState(typed.top_p_state, typed.top_p) },
        {
            label: 'max_tokens',
            value:
                numOrNull(typed.max_tokens) === null
                    ? '未知'
                    : `${strOrEmpty(typed.max_token_field) || 'max_tokens'}=${typed.max_tokens}`,
        },
        { label: 'stream', value: boolText(typed.stream) },
        { label: 'thinking', value: strOrEmpty(typed.thinking) || '未知' },
        { label: 'reasoning_effort', value: strOrEmpty(typed.reasoning_effort) || '未知' },
        { label: '注入 system', value: boolText(typed.system_injected) },
        { label: '注入 tools', value: boolText(typed.tools_injected) },
        { label: '条件完整度', value: strOrEmpty(typed.completeness) || '未知' },
    ];
    const unknownFields = strArray(typed.unknown_fields);
    if (unknownFields.length > 0) {
        rows.push({ label: '未记录字段', value: unknownFields.join('、') });
    }
    return rows;
}

/** 条件对照里一行的一致性判定。 */
export type HlwyConditionCompareRow = {
    label: string;
    run: string;
    baseline: string;
    /** same=两边都记了且相同；differs=两边都记了但不同；unknown=至少一边没记录。 */
    state: 'same' | 'differs' | 'unknown';
};

const CONDITION_UNKNOWN_TEXT = '未知';
const CONDITION_ABSENT_TEXT = '未下发（观测到省略）';

/**
 * 本渠道与基准的采样条件逐项对照。
 *
 * 只拿「两边都记了」的值判相同/不同；有一边是「未知」就写 unknown，不能说
 * 一致，更不能拿默认值补齐再比。基准没记录条件时返回空数组，由界面直说。
 */
export function hlwyConditionCompareRows(
    run: Record<string, unknown> | null,
    baseline: Record<string, unknown> | null
): HlwyConditionCompareRow[] {
    if (!run || !baseline) return [];
    const baseRows = new Map(hlwyConditionRows(baseline).map((row) => [row.label, row.value]));
    return hlwyConditionRows(run).map((row) => {
        const base = baseRows.get(row.label) ?? CONDITION_UNKNOWN_TEXT;
        const known = (text: string) => text !== CONDITION_UNKNOWN_TEXT && !text.startsWith('未知');
        let state: HlwyConditionCompareRow['state'] = 'unknown';
        if (known(row.value) && known(base)) {
            state = row.value === base ? 'same' : 'differs';
        }
        // 「未下发」和「未知」不是同一件事，两者对比一律当未知，不当成相同。
        if (row.value === CONDITION_ABSENT_TEXT || base === CONDITION_ABSENT_TEXT) {
            if (row.value !== base) state = 'unknown';
        }
        return { label: row.label, run: row.value, baseline: base, state };
    });
}

/** 一条原因码的人话说明；severity 用来选颜色。 */
export type HlwyReasonMessage = {
    code: string;
    text: string;
    severity: 'incompatible' | 'differs' | 'unknown' | 'other';
};

/**
 * 原因码 → 人话。
 *
 * 这份表对应后端 `HlwyComparability` 发出的枚举（不可对照 / 有差异 / 不知道）。
 * 认不出的码不猜意思，也不把机器串当解释摆给用户看：直接说本地没有这条说明。
 */
const HLWY_REASON_TEXT: Record<string, { text: string; severity: HlwyReasonMessage['severity'] }> = {
    protocol_mismatch: { text: '两边题目协议不同，分布不能放在一起比', severity: 'incompatible' },
    parser_mismatch: { text: '两边解析版本不同，抽出来的数字口径不一样', severity: 'incompatible' },
    prompt_mismatch: { text: '两边问的题目不同（指纹对不上），不能放在一起比', severity: 'incompatible' },
    api_protocol_differs: { text: '两边请求走的接口协议不同（仍可对照，但要留痕）', severity: 'differs' },
    temperature_differs: { text: '两边 temperature 不同', severity: 'differs' },
    top_p_differs: { text: '两边 top_p 不同', severity: 'differs' },
    stream_differs: { text: '一边流式一边非流式', severity: 'differs' },
    max_tokens_differs: { text: '两边输出长度上限不同', severity: 'differs' },
    thinking_differs: { text: '两边 thinking 设置不同', severity: 'differs' },
    reasoning_effort_differs: { text: '两边 reasoning_effort 不同', severity: 'differs' },
    system_differs: { text: '一边注入了 system，一边没有', severity: 'differs' },
    tools_differs: { text: '一边注入了 tools，一边没有', severity: 'differs' },
    transport_differs: { text: '两边传输形态不同（裸客户端 / 中转链）', severity: 'differs' },
    api_protocol_unknown: { text: '接口协议至少一边没记录', severity: 'unknown' },
    protocol_unknown: { text: '题目协议至少一边没记录', severity: 'unknown' },
    parser_unknown: { text: '解析版本至少一边没记录', severity: 'unknown' },
    prompt_unknown: { text: '题目指纹至少一边没记录', severity: 'unknown' },
    temperature_unknown: { text: 'temperature 至少一边没记录', severity: 'unknown' },
    top_p_unknown: { text: 'top_p 至少一边没记录', severity: 'unknown' },
    stream_unknown: { text: 'stream 至少一边没记录', severity: 'unknown' },
    max_tokens_unknown: { text: '输出长度上限至少一边没记录', severity: 'unknown' },
    thinking_unknown: { text: 'thinking 至少一边没记录', severity: 'unknown' },
    reasoning_effort_unknown: { text: 'reasoning_effort 至少一边没记录', severity: 'unknown' },
    system_unknown: { text: '是否注入 system 至少一边没记录', severity: 'unknown' },
    tools_unknown: { text: '是否注入 tools 至少一边没记录', severity: 'unknown' },
    transport_unknown: { text: '传输形态至少一边没记录', severity: 'unknown' },
    conditions_incomplete: { text: '至少一边的采样条件没记全', severity: 'unknown' },
};

export function hlwyReasonMessages(codes: string[]): HlwyReasonMessage[] {
    const out: HlwyReasonMessage[] = [];
    for (const code of codes) {
        const known = HLWY_REASON_TEXT[code];
        if (known) {
            out.push({ code, text: known.text, severity: known.severity });
            continue;
        }
        out.push({ code, text: '本地还没有这条原因的说明，以服务端记录为准', severity: 'other' });
    }
    return out;
}

/* ------------------------------------------------------------------ *
 * 分箱
 * ------------------------------------------------------------------ */

/** 把 355 项分布压成 36 段占比；输入不合法时返回空数组。 */
export function hlwyBinShares(distribution: number[] | null): number[] {
    if (!distribution || distribution.length !== DIST_LENGTH) return [];
    const shares: number[] = [];
    for (let i = 0; i < HLWY_BIN_COUNT - 1; i += 1) {
        const from = i * 10;
        let sum = 0;
        for (let j = from; j < from + 10; j += 1) sum += distribution[j];
        shares.push(sum);
    }
    let tail = 0;
    for (let j = (HLWY_BIN_COUNT - 1) * 10; j < DIST_LENGTH; j += 1) tail += distribution[j];
    shares.push(tail);
    return shares;
}

/** 分箱区间文案，例如 `1–10` / `351–355`。 */
export function hlwyBinRange(index: number): { from: number; to: number; label: string } {
    const from = index * 10 + 1;
    const to = index === HLWY_BIN_COUNT - 1 ? HLWY_MAX_DIGIT : from + 9;
    return { from, to, label: `${from}–${to}` };
}

/** 分布图的高度（px）；360px 宽的手机上也要读得出来，所以不能太矮。 */
export const HLWY_CHART_HEIGHT = 80;

/**
 * 一根柱子的像素高。
 *
 * 两条硬规矩：
 *  - 份额为 0 时高度就是 0——画一个 1px 的伪正数柱子等于无中生有；
 *  - 份额大于 0 时至少 1px——不然 0.1% 的尾巴在屏幕上直接消失，看着就像没有。
 * 缺值（null）由调用方决定根本不渲染柱子，不交给本函数处理。
 */
export function hlwyBarHeightPx(share: number, maxShare: number, height: number = HLWY_CHART_HEIGHT): number {
    if (!Number.isFinite(share) || share <= 0) return 0;
    if (!Number.isFinite(maxShare) || maxShare <= 0) return 0;
    const raw = Math.round((share / maxShare) * height);
    return raw < 1 ? 1 : raw;
}

/** 横轴刻度：数字 1 / 100 / 200 / 355 在 355 格里的位置（百分比）。 */
export const HLWY_AXIS_TICKS: { digit: number; pct: number }[] = [1, 100, 200, HLWY_MAX_DIGIT].map((digit) => ({
    digit,
    pct: ((digit - 1) / (HLWY_MAX_DIGIT - 1)) * 100,
}));

/** 由有效数字直接数出分布（与后端 parse 侧同口径：count/total）。 */
function distributionFromNumbers(numbers: number[] | null): number[] | null {
    if (!numbers || numbers.length === 0) return null;
    const counts = new Array<number>(DIST_LENGTH).fill(0);
    for (const n of numbers) {
        if (n >= 1 && n <= HLWY_MAX_DIGIT) counts[n - 1] += 1;
    }
    return counts.map((c) => c / numbers.length);
}

/** 355 桶整数计数（numberdist.Baseline.counts）；任一项不是非负整数就整条丢弃。 */
function readDigitCounts(value: unknown): number[] | null {
    if (!Array.isArray(value) || value.length !== DIST_LENGTH) return null;
    const out: number[] = [];
    for (const item of value) {
        const n = intOrNull(item);
        if (n === null || n < 0) return null;
        out.push(n);
    }
    return out;
}

/** 由 355 桶计数换算占比；总数为 0（空基准）时返回 null，不拿 0 冒充分布。 */
function distributionFromCounts(counts: number[] | null): number[] | null {
    if (!counts || counts.length !== DIST_LENGTH) return null;
    let total = 0;
    for (const c of counts) total += c;
    if (total <= 0) return null;
    return counts.map((c) => c / total);
}

/* ------------------------------------------------------------------ *
 * 主解析
 * ------------------------------------------------------------------ */

/**
 * 读报告里的基准快照。
 *
 * 快照就是 numberdist.Baseline 的**完整**对象：来源在 `source.kind`，原始数字在
 * `raw_numbers`，355 桶计数在 `counts`。这几样是离线换基准重算的唯一输入，所以
 * 不能用「只回 id/label/valid_count」的缩水副本顶替——那种副本一换基准就得回头
 * 找原文件，等于把证据换成了索引。
 *
 * 因此这里只认完整形状：`source.kind` 缺失就按「来源未标注」显示，不退回扁平
 * `source_kind`；有效数由 `raw_numbers` 数出来（Baseline 不另存派生值），分布由
 * 数字/计数现算。缺哪样就少哪样，不补默认值。
 */
function readSnapshots(value: unknown): HlwySnapshotView[] {
    if (!Array.isArray(value)) return [];
    const out: HlwySnapshotView[] = [];
    for (const item of value) {
        if (!isRecord(item)) continue;
        const id = strOrEmpty(item.id);
        if (!id) continue;
        const source = isRecord(item.source) ? item.source : null;
        const sourceKind = strOrEmpty(source?.kind);
        const rawNumbers = readNumbers(item.raw_numbers);
        const counts = readDigitCounts(item.counts);
        let declaredCount: number | null = null;
        if (counts) {
            let total = 0;
            for (const c of counts) total += c;
            declaredCount = total;
        }
        out.push({
            id,
            label: strOrEmpty(item.label) || '未命名参考',
            sourceKind,
            sourceText: hlwySourceLabel(sourceKind),
            declaredModel: strOrEmpty(item.declared_model),
            apiType: strOrEmpty(item.api_type),
            collectedAt: strOrEmpty(item.collected_at) || null,
            // 有效数 = len(raw_numbers)；没有原始数字时退到计数总和（也是同一批样本）。
            validCount: rawNumbers ? rawNumbers.length : declaredCount,
            payloadSha256: strOrEmpty(item.payload_sha256),
            distribution: distributionFromNumbers(rawNumbers) ?? distributionFromCounts(counts),
            conditions: isRecord(item.conditions) ? item.conditions : null,
        });
    }
    return out;
}

function formatPercentValue(value: number | null): string | null {
    if (value === null) return null;
    return `${(value * 100).toFixed(2)}%`;
}

function readMatches(value: unknown, snapshots: HlwySnapshotView[]): HlwyMatchView[] {
    if (!Array.isArray(value)) return [];
    const byId = new Map(snapshots.map((snap) => [snap.id, snap]));
    const rows: HlwyMatchView[] = [];
    value.forEach((item, index) => {
        if (!isRecord(item)) return;
        const baselineId = strOrEmpty(item.baseline_id);
        if (!baselineId) return;
        const snap = byId.get(baselineId);
        const overall = numOrNull(item.overall);
        const gap = numOrNull(item.gap_to_previous);
        // 匹配行只有分数与名次；来源/日期/样本量一律从快照表里查。查不到就说未标注，
        // 不拿匹配行上顺手带的字段充数。
        const sourceKind = snap ? snap.sourceKind : '';
        rows.push({
            baselineId,
            rank: intOrNull(item.rank) ?? index + 1,
            overall,
            percent: formatPercentValue(overall),
            cosine: numOrNull(item.cosine),
            jsd: numOrNull(item.jsd),
            modeScore: numOrNull(item.mode_score),
            gapPoints: gap === null ? null : Math.round(gap * 10000) / 100,
            gapText: gap === null ? null : `${(gap * 100).toFixed(2)} 个百分点`,
            label: snap ? snap.label : '参考名称未知',
            sourceKind,
            sourceText: hlwySourceLabel(sourceKind),
            collectedAt: snap ? snap.collectedAt : null,
            validCount: snap ? snap.validCount : null,
            payloadSha256: snap ? snap.payloadSha256 : '',
        });
    });
    rows.sort((a, b) => a.rank - b.rank);
    return rows;
}

/**
 * 从一份报告对象里读出 hlwy 对照视图。
 *
 * 返回 null 表示「这根本不是 hlwy 对照报告」——调用方应完全走旧的多探针展示，
 * 不要因为这里读不出来就改变旧报告的行为。
 */
export function readHlwyComparison(report: unknown): HlwyComparisonView | null {
    if (!isRecord(report)) return null;
    const kind = strOrEmpty(report.kind);
    const raw = report.baseline_comparison;
    if (!isRecord(raw) && kind !== 'hlwy_compare') return null;

    const data = isRecord(raw) ? raw : null;

    const problems: string[] = [];
    if (!data) problems.push('报告里没有对照细节');
    const execution = strOrEmpty(data?.execution) || (data ? 'unknown' : '');
    const sufficiency = strOrEmpty(data?.sufficiency) || 'unknown';
    const comparability = strOrEmpty(data?.comparability) || 'unknown';
    const counts = readCounts(data?.counts);
    if (data && !counts) problems.push('采样计数缺失或不完整');
    const snapshots = readSnapshots(data?.baseline_snapshots);
    const matches = readMatches(data?.matches, snapshots);
    const rawNumbers = readNumbers(data?.raw_numbers);
    if (data && data.raw_numbers !== undefined && data.raw_numbers !== null && !rawNumbers) {
        problems.push('原始数字不合法');
    }
    const distribution = readDistribution(data?.distribution) ?? distributionFromNumbers(rawNumbers);
    const scorable = data ? boolOrNull(data.scorable) : null;
    if (data && scorable === null && sufficiency === 'unknown') problems.push('报告没有说明能不能评分');
    const chosenBaselineId =
        strOrEmpty(data?.chosen_baseline_id) || matches[0]?.baselineId || '';
    const chosen = matches.find((m) => m.baselineId === chosenBaselineId) ?? null;
    if (matches.length > 0 && !chosen) problems.push('主对照不在匹配列表里');

    const hasScore =
        data !== null &&
        counts !== null &&
        sufficiency === 'scorable' &&
        scorable !== false &&
        comparability !== 'incompatible' &&
        matches.length > 0;
    const partial = execution === 'canceled' || execution === 'timed_out' || execution === 'interrupted';

    const baselineDist = chosen ? snapshots.find((s) => s.id === chosen.baselineId)?.distribution ?? null : null;
    const runShares = hlwyBinShares(distribution);
    const baseShares = hlwyBinShares(baselineDist);
    const bins: HlwyBin[] = runShares.map((share, index) => {
        const range = hlwyBinRange(index);
        return { from: range.from, to: range.to, run: share, baseline: baseShares[index] ?? null };
    });

    return {
        execution,
        sufficiency,
        comparability,
        reasonCodes: strArray(data?.reason_codes),
        counts,
        stopReason: strOrEmpty(data?.stop_reason),
        selfCompare: boolOrNull(data?.self_compare) === true,
        algorithmVersion: strOrEmpty(data?.algorithm_version),
        schemaVersion: intOrNull(data?.schema_version),
        chosenBaselineId,
        chosen,
        matches,
        top: matches.slice(0, 3),
        snapshots,
        conditions: isRecord(data?.conditions) ? data.conditions : null,
        rawNumbers,
        stats: readStats(data?.stats),
        quality: readQuality(data?.quality),
        distribution,
        bins,
        scorable,
        hasScore,
        partial,
        headline: `数字习惯对照 · ${hlwyExecutionLabel(execution)}`,
        summary: [
            counts ? `收到${counts.valid}个有效回答` : '有效回答数量未知',
            chosen ? `对照参考：${chosen.label}` : '参考名称未知',
        ].join(' · '),
        problem: problems.join('；'),
    };
}

/**
 * 打开展示旧的「检测记录」结果。
 *
 * hlwy 报告没有 results 数组（它没有多探针结论），旧代码用 `results` 长度当
 * 门槛，于是这类结果一打开就是「这份报告不完整」——报告其实好着，是门槛错
 * 了。这里把门槛收回到「报告能不能解析」上：hlwy 补空数组，其它照旧。
 */
export type StoredReportRead =
    | { ok: true; view: Record<string, unknown>; hlwy: boolean }
    | { ok: false; error: string };

export function readStoredReport(text: string): StoredReportRead {
    if (!text || !text.trim()) return { ok: false, error: '这份报告是空的' };
    let parsed: unknown;
    try {
        parsed = JSON.parse(text);
    } catch {
        return { ok: false, error: '这份报告解析失败，可能已损坏' };
    }
    if (!isRecord(parsed)) return { ok: false, error: '这份报告格式不对' };
    const hlwy = strOrEmpty(parsed.kind) === 'hlwy_compare' || isRecord(parsed.baseline_comparison);
    if (hlwy) {
        if (!Array.isArray(parsed.results)) parsed.results = [];
        return { ok: true, view: parsed, hlwy: true };
    }
    if (!Array.isArray(parsed.results)) return { ok: false, error: '这份报告不完整，无法展示' };
    return { ok: true, view: parsed, hlwy: false };
}

/* ------------------------------------------------------------------ *
 * 提交前的资格判断（界面只说事实，不替服务端下结论）
 * ------------------------------------------------------------------ */

export type HlwySaveEligibility = { eligible: boolean; reason: string };

/** 「把这次结果存成基准」的按钮该不该亮。服务端仍会独立再校验一次。 */
export function hlwySaveEligibility(
    view: HlwyComparisonView | null,
    opts: { resultId?: number; resultStatus?: string }
): HlwySaveEligibility {
    if (!view) return { eligible: false, reason: '这份结果里没有可保存的对照快照' };
    if (!opts.resultId) return { eligible: false, reason: '要从「检测记录」里打开这条结果才能保存' };
    if (opts.resultStatus !== 'done') {
        return { eligible: false, reason: '只有正常跑完的结果才能存成基准' };
    }
    if (view.execution !== 'complete') {
        return { eligible: false, reason: '这次没有完整跑完，不存成基准' };
    }
    if (!view.counts) return { eligible: false, reason: '报告缺少采样计数，无法确认样本量' };
    if (view.counts.unresolved > 0) {
        return { eligible: false, reason: `还有 ${view.counts.unresolved} 个采样槽位没确认结果` };
    }
    if (view.counts.started_slots !== view.counts.planned_calls) {
        return { eligible: false, reason: '计划里的采样槽位没有全部走完' };
    }
    if (view.counts.valid < HLWY_MIN_CAPTURE_VALID) {
        return {
            eligible: false,
            reason: `有效样本 ${view.counts.valid} 个，少于 ${HLWY_MIN_CAPTURE_VALID} 个`,
        };
    }
    if (!view.rawNumbers) return { eligible: false, reason: '报告里没有原始数字，存不成基准' };
    if (view.rawNumbers.length !== view.counts.valid) {
        return {
            eligible: false,
            reason: `原始数字 ${view.rawNumbers.length} 个，和有效计数 ${view.counts.valid} 对不上`,
        };
    }
    return { eligible: true, reason: '' };
}

export type HlwyStartState = { disabled: boolean; reason: string };

/**
 * 唯一的「开始对照」按钮什么时候能按。
 *
 * 顺序是刻意的：先看有没有计划，再看开关，最后才看用户选项。计划读不到就
 * 一定不给按——没有计划就没有价格承诺，按钮亮着等于骗一次点击。
 */
export function hlwyStartState(input: {
    hasTarget: boolean;
    /** 上一轮用的渠道已经不在列表里（或已被删掉）：说清原因，不悄悄换一个渠道。 */
    channelMissing?: string;
    baselineId: string;
    /** 选中的基准已经被服务端标成校验不通过（目录刷新后残留的旧选中）。 */
    baselineUnavailable?: boolean;
    flagEnabled: boolean;
    plan: { enabled?: boolean; max_calls?: number; protocol_id?: string } | null;
    planError: string;
    planLoading: boolean;
    pending: boolean;
}): HlwyStartState {
    if (input.pending) return { disabled: true, reason: '正在提交…' };
    // 渠道没了要先说这一条：它比「先选渠道」具体，也更接近用户真正要处理的事。
    if (input.channelMissing) return { disabled: true, reason: input.channelMissing };
    if (!input.hasTarget) return { disabled: true, reason: '先选渠道和模型' };
    if (input.planLoading && !input.plan) return { disabled: true, reason: '正在读取这一单的计划…' };
    if (input.planError) return { disabled: true, reason: `计划没读出来：${input.planError}` };
    if (!input.plan) return { disabled: true, reason: '计划没读出来，先别提交' };
    if (input.plan.enabled === false) return { disabled: true, reason: '服务器当前没有启用基准对照' };
    if (typeof input.plan.max_calls === 'number' && input.plan.max_calls <= 0) {
        return { disabled: true, reason: '计划里没有可用的调用额度' };
    }
    if (!input.baselineId) return { disabled: true, reason: '先选一份基准' };
    // 目录刷新后旧 baselineId 可能已经不可用：照样不给提交，理由写清楚。
    if (input.baselineUnavailable === true) {
        return { disabled: true, reason: '选中的基准现在不能用（服务端校验没通过），换一份再提交' };
    }
    if (!input.flagEnabled) {
        return { disabled: true, reason: '基准对照已在设置里关闭；开启后才能提交新的对照' };
    }
    return { disabled: false, reason: '' };
}

/* ------------------------------------------------------------------ *
 * 队列 / 历史记录上的 kind 感知
 * ------------------------------------------------------------------ */

export type HlwyJobBucket = 'running' | 'complete' | 'insufficient' | 'stopped' | 'failed';

type JobLike = {
    status?: string;
    kind?: string;
    hlwy_summary?: HlwyJobSummary;
};

export function isHlwyJob(job: JobLike | null | undefined): boolean {
    return !!job && job.kind === 'hlwy_compare';
}

/**
 * 对照任务落在哪个桶里。
 *
 * 关键一条：**对照完成但结论 unknown 不是失败**。旧的多探针逻辑用
 * `single_verdict === 'unknown'` 判失败，直接套过来会让一次跑满 100 个样本、
 * 报告齐全的对照显示成「没测成」。
 */
export function hlwyJobBucket(job: JobLike | null | undefined): HlwyJobBucket {
    const execution = job?.hlwy_summary?.execution ?? '';
    const sufficiency = job?.hlwy_summary?.sufficiency ?? '';
    if (execution === 'failed') return 'failed';
    if (execution === 'canceled' || execution === 'timed_out' || execution === 'interrupted') {
        return 'stopped';
    }
    if (execution === 'complete' && sufficiency === 'insufficient') return 'insufficient';
    if (execution === 'complete') return 'complete';
    if (job?.status === 'done') return 'complete';
    return 'running';
}

export function hlwyJobHeadline(job: JobLike | null | undefined): string {
    switch (hlwyJobBucket(job)) {
        case 'failed':
            return '失败';
        case 'stopped':
            return '已停止，保留已有结果';
        case 'insufficient':
            return '样本不足';
        case 'complete':
            return '对照完成';
        default:
            return '对照中';
    }
}

/**
 * 队列行进度条该走多少。
 *
 * 分母是计划上限，分子是**落定的**槽位：已发起还没回的那一次（unresolved）
 * 不能算进去。用 started_slots 当分子，最后一个样本还在飞的时候进度条就
 * 顶在 100%，看起来像「跑完了但没结果」——它其实还在收尾。
 *
 * 摘要缺失或计划为 0 时返回 null：没有分母就不画百分数，不编一个 0%。
 */
export function hlwyProgressPercent(job: JobLike | null | undefined): number | null {
    const counts = job?.hlwy_summary?.counts;
    if (!counts || !(counts.planned_calls > 0)) return null;
    const settled = Math.max(0, counts.started_slots - Math.max(0, counts.unresolved));
    return Math.min(100, Math.round((settled / counts.planned_calls) * 100));
}

/**
 * 「还在收尾」的说明：已发起但有槽位没回。
 *
 * 取消/超时以后也会停在这里（已发出去的那一次还在跑），所以这条文案不写
 * 「正在检测」——它描述的是事实：有几个槽位还没确认结果。
 */
export function hlwyInFlightNote(job: JobLike | null | undefined): string {
    const counts = job?.hlwy_summary?.counts;
    if (!counts) return '';
    if (!(counts.started_slots > 0) || !(counts.unresolved > 0)) return '';
    return `正在收尾（已发起 ${counts.started_slots}，${counts.unresolved} 个样本还没回）`;
}

/** 队列行上的采样账目文案；摘要缺失时返回空串（不编数字）。 */
export function hlwyJobLedger(job: JobLike | null | undefined): string {
    const counts = job?.hlwy_summary?.counts;
    if (!counts) return '';
    // 计划数是上限，不是保证：写「最多 N 次调用」，不把上限说成一定拿到的有效样本。
    const parts = [`最多 ${counts.planned_calls} 次调用`, `已发起 ${counts.started_slots}`, `有效 ${counts.valid}`];
    if (counts.invalid > 0) parts.push(`无效 ${counts.invalid}`);
    if (counts.failed > 0) parts.push(`失败 ${counts.failed}`);
    if (counts.unresolved > 0) parts.push(`未确认 ${counts.unresolved}`);
    return parts.join(' · ');
}

/**
 * 一条被跳过的结果到底跑过没有。
 *
 * `skipped` 不等于「未开始」：任务被取消时，已经发出去的那一次会被标成
 * skipped，但它的报告里 started_slots > 0，钱也花了。这种要写「已取消，
 * 保留已采结果」，只有真的一槽没走才写「未开始」。报告读不出来时不敢下结论，
 * 按未开始说，但保留可以点开的部分报告入口。
 */
export function hlwySkippedText(report: string | null | undefined): string {
    const read = readStoredReport(report ?? '');
    const view = read.ok ? readHlwyComparison(read.view) : null;
    const started = view?.counts?.started_slots ?? 0;
    return started > 0 ? '已取消，保留已采结果' : '任务已停，未开始';
}

/* ------------------------------------------------------------------ *
 * 导出
 * ------------------------------------------------------------------ */

const CONDITION_KEYS = [
    'protocol',
    'protocol_id',
    'parser_version',
    'prompt_sha256',
    'transport_profile',
    'temperature_state',
    'temperature',
    'top_p_state',
    'top_p',
    'stream',
    'max_token_field',
    'max_tokens',
    'thinking',
    'reasoning_effort',
    'system_injected',
    'tools_injected',
    'completeness',
    'unknown_fields',
] as const;

const STAT_KEYS = ['mean', 'std_dev', 'median', 'min', 'max', 'unique', 'mode', 'mode_count'] as const;

function pickPrimitive(source: Record<string, unknown>, keys: readonly string[]): Record<string, unknown> {
    const out: Record<string, unknown> = {};
    for (const key of keys) {
        const value = source[key];
        if (value === undefined) continue;
        if (Array.isArray(value)) {
            // 只有纯字符串数组才是条件字段（currently 仅 unknown_fields）。
            out[key] = value.filter((item) => typeof item === 'string');
            continue;
        }
        if (typeof value === 'string' || typeof value === 'boolean' || (typeof value === 'number' && Number.isFinite(value))) {
            out[key] = value;
        }
        // 其余（对象/函数/undefined）一律不抄：白名单之外没有「顺手带上」。
    }
    return out;
}

export function hlwyExportConditions(value: unknown): Record<string, unknown> | undefined {
    if (!isRecord(value)) return undefined;
    const picked = pickPrimitive(value, CONDITION_KEYS);
    return Object.keys(picked).length > 0 ? picked : undefined;
}

function exportStats(value: unknown): Record<string, unknown> | undefined {
    if (!isRecord(value)) return undefined;
    const picked = pickPrimitive(value, STAT_KEYS);
    return Object.keys(picked).length > 0 ? picked : undefined;
}

/**
 * 快照白名单 = numberdist.Baseline 的字段。
 *
 * 来源只留 `source.kind`：repo/commit/path/note 是导入方的自由文本（note 里什么
 * 都可能写），导出的边界是「来源类别」这条事实，不是那份文本。
 */
function exportSnapshot(value: unknown): Record<string, unknown> | null {
    if (!isRecord(value)) return null;
    const out: Record<string, unknown> = {};
    const schemaVersion = intOrNull(value.schema_version);
    if (schemaVersion !== null) out.schema_version = schemaVersion;
    for (const key of ['id', 'label', 'payload_sha256', 'declared_model', 'api_type', 'collected_at', 'imported_at']) {
        const v = value[key];
        if (typeof v === 'string') out[key] = v;
    }
    const source = isRecord(value.source) ? value.source : null;
    const sourceKind = strOrEmpty(source?.kind);
    if (sourceKind) out.source = { kind: sourceKind };
    const conditions = hlwyExportConditions(value.conditions);
    if (conditions) out.conditions = conditions;
    const rawNumbers = readNumbers(value.raw_numbers);
    if (rawNumbers) out.raw_numbers = rawNumbers;
    const counts = readDigitCounts(value.counts);
    if (counts) out.counts = counts;
    const stats = exportStats(value.stats);
    if (stats) out.stats = stats;
    return Object.keys(out).length > 0 ? out : null;
}

function exportMatch(value: unknown): Record<string, unknown> | null {
    if (!isRecord(value)) return null;
    const baselineId = strOrEmpty(value.baseline_id);
    if (!baselineId) return null;
    const out: Record<string, unknown> = { baseline_id: baselineId };
    for (const key of ['cosine', 'jsd', 'mode_score', 'overall', 'gap_to_previous']) {
        const n = numOrNull(value[key]);
        if (n !== null) out[key] = n;
    }
    const rank = intOrNull(value.rank);
    if (rank !== null) out.rank = rank;
    return out;
}

/**
 * 导出「完整对照报告」：逐层显式白名单。
 *
 * 这里不能写成 `{...report}` 再删几个键——报告的每一层都可能混入渠道信息
 * （endpoint、header、备注、url），而删键永远漏一个。只能反过来做：只抄
 * 明确点名、且类型正确的字段。
 */
export function hlwyExportFull(report: unknown): Record<string, unknown> | null {
    if (!isRecord(report)) return null;
    const comparison = isRecord(report.baseline_comparison) ? report.baseline_comparison : null;
    if (!comparison) return null;

    const out: Record<string, unknown> = { kind: 'hlwy_compare' };
    for (const key of ['requested_model', 'upstream_model', 'verdict', 'preset', 'model']) {
        const v = report[key];
        if (typeof v === 'string') out[key] = v;
    }
    const score = numOrNull(report.score);
    if (score !== null) out.score = score;
    for (const key of ['started_at', 'finished_at']) {
        const v = report[key];
        if (typeof v === 'string') out[key] = v;
    }
    if (isRecord(report.plain)) {
        const plain: Record<string, unknown> = {};
        for (const key of ['headline', 'summary']) {
            const v = report.plain[key];
            if (typeof v === 'string') plain[key] = v;
        }
        if (Object.keys(plain).length > 0) out.plain = plain;
    }
    // 报告骨架里的空数组原样保留，非空的（正常情况下不存在）不抄。
    for (const key of ['results', 'findings', 'errors']) {
        const v = report[key];
        if (Array.isArray(v) && v.length === 0) out[key] = [];
    }

    const bc: Record<string, unknown> = {};
    for (const key of ['execution', 'sufficiency', 'comparability', 'reason', 'stop_reason', 'algorithm_version', 'chosen_baseline_id']) {
        const v = comparison[key];
        if (typeof v === 'string') bc[key] = v;
    }
    for (const key of ['valid_count', 'schema_version']) {
        const n = intOrNull(comparison[key]);
        if (n !== null) bc[key] = n;
    }
    for (const key of ['scorable', 'self_compare']) {
        const b = boolOrNull(comparison[key]);
        if (b !== null) bc[key] = b;
    }
    const reasonCodes = strArray(comparison.reason_codes);
    if (reasonCodes.length > 0) bc.reason_codes = reasonCodes;
    const counts = readCounts(comparison.counts);
    if (counts) bc.counts = counts;
    const rawNumbers = readNumbers(comparison.raw_numbers);
    if (rawNumbers) bc.raw_numbers = rawNumbers;
    const conditions = hlwyExportConditions(comparison.conditions);
    if (conditions) bc.conditions = conditions;
    const distribution = readDistribution(comparison.distribution);
    if (distribution) bc.distribution = distribution;
    const stats = exportStats(comparison.stats);
    if (stats) bc.stats = stats;
    if (isRecord(comparison.quality)) {
        const quality: Record<string, unknown> = {};
        for (const key of ['strict_integer', 'permissive', 'truncated']) {
            const n = intOrNull(comparison.quality[key]);
            if (n !== null) quality[key] = n;
        }
        if (Object.keys(quality).length > 0) bc.quality = quality;
    }
    if (Array.isArray(comparison.baseline_snapshots)) {
        bc.baseline_snapshots = comparison.baseline_snapshots
            .map(exportSnapshot)
            .filter((item): item is Record<string, unknown> => item !== null);
    }
    if (Array.isArray(comparison.matches)) {
        bc.matches = comparison.matches
            .map(exportMatch)
            .filter((item): item is Record<string, unknown> => item !== null);
    }
    out.baseline_comparison = bc;
    return out;
}

export type HlwyLegacyExport = {
    name: string;
    model: string;
    apiType: string;
    timestamp: string;
    iterations: number;
    distribution: number[];
    rawData: number[];
    stats: {
        mean: number | null;
        stdDev: number | null;
        median: number | null;
        min: number | null;
        max: number | null;
        unique: number | null;
        mode: number | null;
        modeCount: number | null;
    };
};

/**
 * 导出「旧 hlwy 格式」的 8 个字段，好让这次结果能被别的 hlwy 工具直接读。
 *
 * 两个容易错的地方：
 *  - `stats.mode` 是**数字本身**（1..355），不是出现次数；次数在 modeCount。
 *  - `stats.median` 用报告里的整数中位数（上游口径是上中位数），不在这里
 *    重新拿浮点插值算一遍——那会与原始文件对不上。
 * `iterations` 必须等于 rawData 长度（导入侧会校验），因此由本地长度决定。
 */
export function hlwyExportLegacy(
    view: HlwyComparisonView | null,
    meta: { model?: string; apiType?: string; timestamp?: string }
): HlwyLegacyExport | null {
    if (!view || !view.rawNumbers || view.rawNumbers.length === 0) return null;
    const numbers = view.rawNumbers;
    const distribution = distributionFromNumbers(numbers) ?? view.distribution;
    if (!distribution) return null;
    const stats = view.stats;
    return {
        name: meta.model || view.chosen?.label || 'hlwy 对照结果',
        model: meta.model || '',
        apiType: meta.apiType || '',
        timestamp: meta.timestamp || '',
        iterations: numbers.length,
        distribution,
        rawData: numbers,
        stats: {
            mean: stats?.mean ?? null,
            stdDev: stats?.stdDev ?? null,
            median: stats?.median ?? null,
            min: stats?.min ?? null,
            max: stats?.max ?? null,
            unique: stats?.unique ?? null,
            mode: stats?.mode ?? null,
            modeCount: stats?.modeCount ?? null,
        },
    };
}

/** 触发一次浏览器下载。只在浏览器里调用（测试不碰这里）。 */
export function downloadJson(filename: string, payload: unknown): void {
    const text = JSON.stringify(payload, null, 2);
    const blob = new Blob([text], { type: 'application/json' });
    const url = URL.createObjectURL(blob);
    const anchor = document.createElement('a');
    anchor.href = url;
    anchor.download = filename;
    document.body.appendChild(anchor);
    anchor.click();
    document.body.removeChild(anchor);
    URL.revokeObjectURL(url);
}

/** 选项只显示名称；来源、日期与样本数由选择器下面的资料行展示。 */
export function hlwyBaselineOptionLabel(entry: HlwyBaselineEntry): string {
    return entry.label || '未命名参考';
}

/**
 * 服务端标记「这份基准读不出来」时的人话原因。
 *
 * 已知原因码翻译成人话，不认识的原因码把原码用括号带上（不丢掉可追查的那一点）。
 * 无论哪种情况，都只说「这份不能选」，不说「基准目录坏了」——一份坏的本地件
 * 不该把健康的内置件一起弄成不可用。
 */
const HLWY_UNAVAILABLE_REASONS: Record<string, string> = {
    hash_mismatch: '内容和哈希对不上，可能被改过',
    payload_hash_mismatch: '内容和哈希对不上，可能被改过',
    counts_mismatch: '计数和原始数字对不上',
    distribution_mismatch: '分布和原始数字对不上',
    stats_mismatch: '统计值和原始数字对不上',
    parse_error: '内容读不出来',
    corrupt: '内容已损坏',
    unsupported_schema: '结构版本不认识',
    missing_numbers: '没有原始数字，无法离线重算',
};

export function hlwyEntryUnavailableText(entry: HlwyBaselineEntry): string {
    const code = (entry.reason_code ?? '').trim();
    const known = code ? HLWY_UNAVAILABLE_REASONS[code] : '';
    if (known) return known;
    return code ? `服务端校验没通过（${code}）` : '服务端校验没通过';
}
