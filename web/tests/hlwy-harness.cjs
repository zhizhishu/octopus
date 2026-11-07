'use strict';

/**
 * hlwy 基准对照几个测试共用的最小 harness。
 *
 * 本仓的测试一贯不用 DOM、不用 jsdom、不用测试框架：直接调用真实组件函数，
 * 自己给 hook 造替身，然后对着「已经求值完的元素树」断言。这个文件只是把那套
 * 做法从 model-audit-queue.test.cjs 里抽出来复用，不引入任何新依赖。
 */

const React = require('react');

/**
 * 每个组件实例一份 hook 槽位。
 *
 * 键是「组件函数 + 本轮第几次出现」：同一个 ResultRow 在不同行上是不同实例，
 * 但同一个页面在两次渲染之间必须拿回同一份 state，否则点一下就又退回初始值。
 */
function createRuntime() {
    const stores = new Map(); // fn -> store[]
    let occurrence = new Map();
    let current = null;

    function storeFor(fn) {
        const k = occurrence.get(fn) ?? 0;
        occurrence.set(fn, k + 1);
        let arr = stores.get(fn);
        if (!arr) {
            arr = [];
            stores.set(fn, arr);
        }
        if (!arr[k]) arr[k] = { memo: [], refs: [], states: [], effects: [], m: 0, r: 0, s: 0, e: 0 };
        return arr[k];
    }

    const react = {
        ...React,
        useState(initial) {
            const st = current;
            const i = st.s++;
            if (!(i in st.states)) st.states[i] = typeof initial === 'function' ? initial() : initial;
            const set = (v) => {
                st.states[i] = typeof v === 'function' ? v(st.states[i]) : v;
            };
            return [st.states[i], set];
        },
        useRef(initial) {
            const st = current;
            const i = st.r++;
            if (!st.refs[i]) st.refs[i] = { current: initial };
            return st.refs[i];
        },
        useMemo(fn, deps) {
            const st = current;
            const i = st.m++;
            const d = deps ?? [];
            const prev = st.memo[i];
            if (!prev || !deps || d.some((v, j) => !Object.is(v, prev.deps[j]))) st.memo[i] = { deps: d, value: fn() };
            return st.memo[i].value;
        },
        useCallback(fn) {
            return fn;
        },
        useEffect(fn) {
            const st = current;
            st.effects[st.e++] = fn;
        },
    };

    /** 递归展开：函数组件被真实调用，返回值继续展开。 */
    function expand(node) {
        if (node === null || node === undefined || typeof node !== 'object') return node;
        if (Array.isArray(node)) return node.map(expand);
        if (!node.props) return node;

        const { type, props } = node;
        const children = expand(props.children);
        const nextProps = { ...props, children };

        if (typeof type === 'function') {
            const st = storeFor(type);
            st.m = 0;
            st.r = 0;
            st.s = 0;
            st.e = 0;
            const prev = current;
            current = st;
            let out;
            try {
                out = type(nextProps);
            } finally {
                current = prev;
            }
            return expand(out);
        }

        return { ...node, props: nextProps };
    }

    return {
        react,
        render(rootFn, props = {}) {
            occurrence = new Map();
            return expand({ type: rootFn, props });
        },
        runEffects() {
            for (const arr of stores.values()) {
                for (const st of arr) {
                    if (!st) continue;
                    for (const fn of st.effects) if (typeof fn === 'function') fn();
                }
            }
        },
    };
}

/** 和 DOM 的 textContent 一样：子节点直接相连，不插分隔符。 */
function text(node) {
    if (node === null || node === undefined || node === false || node === true) return '';
    if (typeof node === 'string' || typeof node === 'number') return String(node);
    if (Array.isArray(node)) return node.map(text).join('');
    if (typeof node === 'object' && node.props) return text(node.props.children);
    return '';
}

/** 再把连续空白压成一个空格：JSX 换行缩进不该影响断言。 */
function squash(node) {
    return text(node).replace(/\s+/g, ' ').trim();
}

function findElement(node, predicate) {
    if (!node || typeof node !== 'object') return undefined;
    if (Array.isArray(node)) {
        for (const child of node) {
            const hit = findElement(child, predicate);
            if (hit) return hit;
        }
        return undefined;
    }
    if (node.props) {
        if (predicate(node)) return node;
        return findElement(node.props.children, predicate);
    }
    return undefined;
}

function findAll(node, predicate, out = []) {
    if (!node || typeof node !== 'object') return out;
    if (Array.isArray(node)) {
        for (const child of node) findAll(child, predicate, out);
        return out;
    }
    if (node.props) {
        if (predicate(node)) out.push(node);
        findAll(node.props.children, predicate, out);
    }
    return out;
}

/** 翻译替身：保留 key，参数一并带出来（断言里要看得见数字）。 */
function translate(key, vars) {
    return vars ? `${key}(${JSON.stringify(vars)})` : key;
}

const widgets = (names) => Object.fromEntries(names.split(' ').map((name) => [name, name]));

/* ------------------------------------------------------------------ *
 * 数据夹具：形状对齐 shared-contract-v1（全部 snake_case）
 * ------------------------------------------------------------------ */

/** 355 项、和为 1 的分布。前 10 段 40%、10–19 段 20%、最后 5 个数 40%。 */
function dist355() {
    const dist = new Array(355).fill(0);
    dist[0] = 0.4;
    dist[9] = 0.2;
    dist[354] = 0.4;
    return dist;
}

function countsFixture(over = {}) {
    return {
        planned_calls: 100,
        started_slots: 100,
        valid: 100,
        invalid: 0,
        failed: 0,
        unresolved: 0,
        ...over,
    };
}

/** 100 个 1..355 的整数（长度要和 counts.valid 对上）。 */
function rawNumbers(n = 100) {
    return Array.from({ length: n }, (_, i) => (i % 355) + 1);
}

/** 355 桶整数计数（numberdist.Baseline.counts），由同一批原始数字数出来。 */
function digitCounts(numbers) {
    const counts = new Array(355).fill(0);
    for (const n of numbers) counts[n - 1] += 1;
    return counts;
}

function statsFixture(over = {}) {
    return {
        mean: 42.5,
        std_dev: 30.25,
        median: 43,
        min: 1,
        max: 100,
        unique: 100,
        mode: 43,
        mode_count: 2,
        ...over,
    };
}

/**
 * 报告里的基准快照 = numberdist.Baseline 的**完整**形状。
 *
 * 来源在 source.kind（嵌套），原始数字在 raw_numbers，355 桶计数在 counts；
 * 没有扁平的 source_kind/valid_count，也没有预先算好的 distribution——
 * 有效数和分布都得从原始数字/计数现算，否则快照就变成一份不能重算的复印件。
 */
function snapshotFixture(over = {}) {
    const numbers = rawNumbers(200);
    return {
        schema_version: 1,
        id: 'hlwy:base@aaa111',
        label: '社区基准 A',
        payload_sha256: 'a'.repeat(64),
        declared_model: 'some-model',
        api_type: 'openai',
        collected_at: '2026-01-02T03:04:05Z',
        imported_at: '2026-01-03T00:00:00Z',
        source: { kind: 'builtin_hlwy', repo: 'hlwy', commit: 'deadbeef' },
        conditions: { protocol: 'hlwy-number355-v1', temperature_state: 'value', temperature: 0 },
        raw_numbers: numbers,
        counts: digitCounts(numbers),
        stats: statsFixture(),
        ...over,
    };
}

/** 基准目录项（catalog Entry）：扁平 source_kind/valid_count，不带原始数组。 */
function baselineEntryFixture(over = {}) {
    return {
        id: 'hlwy:base@aaa111',
        label: '社区基准 A',
        source_kind: 'builtin_hlwy',
        declared_model: 'some-model',
        api_type: 'openai',
        collected_at: '2026-01-02T03:04:05Z',
        valid_count: 200,
        payload_sha256: 'a'.repeat(64),
        builtin: true,
        conditions: { protocol: 'hlwy-number355-v1', temperature_state: 'value', temperature: 0 },
        ...over,
    };
}

function matchFixture(over = {}) {
    return {
        baseline_id: 'hlwy:base@aaa111',
        rank: 1,
        overall: 0.865,
        cosine: 0.9,
        jsd: 0.12,
        mode_score: 0.5,
        ...over,
    };
}

/** 一份合规、可评分的对照块。 */
function comparisonFixture(over = {}) {
    const base = snapshotFixture();
    return {
        execution: 'complete',
        sufficiency: 'scorable',
        comparability: 'matched',
        scorable: true,
        self_compare: false,
        reason_codes: [],
        stop_reason: '',
        algorithm_version: 'hlwy-compare-v1',
        schema_version: 1,
        chosen_baseline_id: base.id,
        counts: countsFixture(),
        raw_numbers: rawNumbers(100),
        distribution: dist355(),
        stats: statsFixture(),
        quality: { strict_integer: 98, permissive: 100, truncated: 0 },
        conditions: {
            protocol: 'hlwy-number355-v1',
            parser_version: 'v1',
            prompt_sha256: 'b'.repeat(64),
            temperature_state: 'value',
            temperature: 0,
            top_p_state: 'absent',
            max_token_field: 'max_tokens',
            max_tokens: 8,
            stream: false,
            thinking: 'disabled',
            completeness: 'complete',
            unknown_fields: ['seed'],
        },
        baseline_snapshots: [base],
        matches: [matchFixture()],
        ...over,
    };
}

/** 一份完整的 hlwy 对照报告（与 shared-contract-v1 的根字段一致）。 */
function reportFixture(over = {}) {
    return {
        kind: 'hlwy_compare',
        requested_model: 'gpt-test',
        upstream_model: 'gpt-test',
        model: 'gpt-test',
        score: 0,
        verdict: 'unknown',
        plain: { headline: '基准对照完成', summary: '只比数字分布，不判定真假。' },
        results: [],
        findings: [],
        errors: [],
        baseline_comparison: comparisonFixture(),
        ...over,
    };
}

function makeJob(over = {}) {
    return {
        id: 1,
        channel_id: 7,
        channel_name: '测试渠道',
        status: 'done',
        total: 1,
        finished: 1,
        probes: '',
        timeout_seconds: 0,
        trigger: 'manual',
        created_at: '2026-01-02T03:04:05Z',
        ...over,
    };
}

function makeResult(over = {}) {
    return {
        id: 11,
        job_id: 1,
        log_id: 0,
        channel_id: 7,
        channel_name: '测试渠道',
        model: 'gpt-test',
        upstream_model: '',
        status: 'done',
        verdict: 'unknown',
        score: 0,
        finding_n: 0,
        report: '',
        error_msg: '',
        duration_ms: 1200,
        created_at: '2026-01-02T03:04:05Z',
        ...over,
    };
}

module.exports = {
    createRuntime,
    text,
    squash,
    findElement,
    findAll,
    translate,
    widgets,
    dist355,
    countsFixture,
    rawNumbers,
    digitCounts,
    statsFixture,
    snapshotFixture,
    baselineEntryFixture,
    matchFixture,
    comparisonFixture,
    reportFixture,
    makeJob,
    makeResult,
};
