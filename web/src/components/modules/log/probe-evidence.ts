/**
 * 把探针的原始证据翻成"人能自己复算"的样子。
 *
 * 设计约束（改之前先读）：
 *
 * 1. **只翻译，不判断**。这里不产出任何"正常/异常/通过"结论——判断已经在
 *    `internal/op/relay_probe.go` 做完了，前端再判一次就会出现两套阈值。
 *    这个文件的唯一职责是把数字摆成人能看懂的顺序。
 *
 * 2. **认不出的键必须原样露出**。后端的 evidence 是 `map[string]any`，加一个
 *    新检查就可能多一个新键。这里如果把认不出的键丢掉，用户会看到一个"缺了几
 *    个数"的证据面板却毫无提示——那比不显示更糟，因为它看起来是完整的。
 *
 * 3. **数字不加工**。不做四舍五入之外的格式化，不加单位后缀，不把 1500 写成
 *    "1.5k"。用户来这里是核对的，任何"顺手美化"都是给核对添乱。
 */

/** 一行证据：左边是名字，右边是值，第三行可选是"怎么算出来的"。 */
export interface ProbeEvidenceRow {
    /** 这一行说的是什么。认不出的键就用原始 key。 */
    label: string;
    /** 值。数字保持数字，交给渲染层决定要不要等宽。 */
    value: string;
    /** 可选：这一行的算术来源，例如 "1500 + 400 = 1900"。 */
    formula?: string;
}

/** 已知键的中文名。加新探针时在这里补一行，别在渲染层写 if。 */
const LABELS: Record<string, string> = {
    // token 加减法
    input: '输入 token',
    output: '输出 token',
    sum: '输入 + 输出',
    total: '日志记的总数',
    diff: '差额',
    // 缓存超输入
    cache_hit: '缓存命中',
    cache_write: '缓存写入',
    cache_total: '缓存合计',
    input_tokens: '输入 token',
    // 空正文
    // (output 已在上方覆盖)
    // 字符/token 比值
    chars: '正文字符数',
    output_tokens: '输出 token',
    ratio: '字符 / token',
    min: '合理下限',
    max: '合理上限',
    // 分词器口径
    upstream_input: '上游自报输入',
    upstream_input_folded: '上游自报输入（含缓存）',
    upstream_input_raw: '上游自报输入（原始栏）',
    local_recount: '本地复算输入',
    relative_diff: '相对偏差',
    tolerance: '容差',
    min_tokens: '最少样本量',
    // 置信度附带的机器可读依据：数了哪些部分、哪些因素压了置信度。
    counted_parts: '复算覆盖的部分',
    confidence_factors: '置信度折减因素',
};

/**
 * 各探针码的"这一行怎么来的"。
 *
 * 键是 `${code}:${evidenceKey}`。只给真正能算的那几行加式子——给每个数字都
 * 编一个式子反而让人分不清哪个才是重点。
 */
const FORMULAS: Record<string, (ev: Record<string, unknown>) => string | undefined> = {
    'token_arithmetic:sum': (ev) => {
        const a = num(ev.input);
        const b = num(ev.output);
        return a === undefined || b === undefined ? undefined : `${a} + ${b} = ${a + b}`;
    },
    'token_arithmetic:diff': (ev) => {
        const t = num(ev.total);
        const s = num(ev.sum);
        return t === undefined || s === undefined ? undefined : `${t} - ${s} = ${t - s}`;
    },
    'cache_exceeds_prompt:cache_total': (ev) => {
        const h = num(ev.cache_hit);
        const w = num(ev.cache_write);
        return h === undefined || w === undefined ? undefined : `${h} + ${w} = ${h + w}`;
    },
    'char_token_ratio:ratio': (ev) => {
        const c = num(ev.chars);
        const t = num(ev.output_tokens);
        if (c === undefined || t === undefined || t === 0) return undefined;
        return `${c} / ${t} = ${(c / t).toFixed(2)}`;
    },
    'tokenizer_residual:relative_diff': (ev) => {
        const u = num(ev.upstream_input);
        const l = num(ev.local_recount);
        if (u === undefined || l === undefined || u === 0) return undefined;
        return `|${u} - ${l}| / ${u} = ${(Math.abs(u - l) / u).toFixed(3)}`;
    },
};

/** 展示顺序：把"结论性"的数放前面，把阈值放最后，别让阈值跟观测值混着看。 */
const ORDER: Record<string, string[]> = {
    token_arithmetic: ['input', 'output', 'sum', 'total', 'diff'],
    cache_exceeds_prompt: ['cache_hit', 'cache_write', 'cache_total', 'input_tokens'],
    empty_output_with_tokens: ['output'],
    char_token_ratio: ['chars', 'output_tokens', 'ratio', 'min', 'max', 'counted_parts'],
    tokenizer_residual: ['upstream_input_folded', 'local_recount', 'relative_diff', 'tolerance', 'min_tokens', 'counted_parts', 'confidence_factors', 'upstream_input_raw'],
};

function num(v: unknown): number | undefined {
    if (typeof v === 'number' && Number.isFinite(v)) return v;
    if (typeof v === 'string' && v.trim() !== '') {
        const n = Number(v);
        if (Number.isFinite(n)) return n;
    }
    return undefined;
}

/**
 * 把一个探针的 evidence 摊成有序、带中文名、带算式的行。
 *
 * 顺序规则：先按该探针码的 ORDER 排，再按原对象里剩下的键的插入顺序 append。
 * 不排序剩下的键——`Object.keys` 对字符串键保留插入顺序，后端写入顺序本身
 * 就是有意义的（先观测值后阈值），重排等于毁掉一层信息。
 */
export function describeProbeEvidence(
    code: string,
    evidence: Record<string, unknown> | undefined
): ProbeEvidenceRow[] {
    if (!evidence) return [];
    const keys = Object.keys(evidence);
    if (keys.length === 0) return [];

    const preferred = ORDER[code] ?? [];
    const ordered = [
        ...preferred.filter((k) => k in evidence),
        // 认不出的键（包括后端新加、前端还没跟上翻译的）原样保留在最后。
        ...keys.filter((k) => !preferred.includes(k)),
    ];

    return ordered.map((key) => {
        const raw = evidence[key];
        const formula = FORMULAS[`${code}:${key}`]?.(evidence);
        return {
            label: LABELS[key] ?? key,
            value: formatValue(raw),
            formula,
        };
    });
}

/** 值转字符串。数字不加千分位——核对时不希望 1200 和 1,200 混着看。 */
/** counted_parts / confidence_factors 里的机器码翻译成人话。认不出的码原样露出。 */
const CODE_LABELS: Record<string, string> = {
    // counted_parts
    content: '消息正文',
    system: '系统提示',
    tool_arguments: '工具调用参数',
    tools_schema: '工具声明',
    reasoning: '推理内容',
    // confidence_factors
    upstream_templated_tools: '上游模板包装不可算',
    tool_arguments_counted: '工具参数已计入',
    replayed_reasoning: '含重放推理',
    tokenizer_family_mismatch: '分词器不同族',
    multi_choice: '多候选回复',
    near_threshold: '偏差贴阈值',
    folded_input_fallback: '分母回退自折',
    log_filter_stripped_media: '日志已剥媒体',
};

function formatValue(v: unknown): string {
    if (v === null || v === undefined) return '—';
    if (typeof v === 'number') return String(v);
    if (typeof v === 'boolean') return v ? '是' : '否';
    if (typeof v === 'string') return v;
    if (Array.isArray(v) && v.every((x) => typeof x === 'string')) {
        return (v as string[]).map((x) => CODE_LABELS[x] ?? x).join('、');
    }
    try {
        return JSON.stringify(v);
    } catch {
        return String(v);
    }
}
