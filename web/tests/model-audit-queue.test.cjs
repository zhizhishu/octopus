'use strict';

const test = require('node:test');
const assert = require('node:assert');
const React = require('react');
const { load } = require('./load-typescript.cjs');

/**
 * 检测队列页（Queue.tsx）的契约测试。
 *
 * 这一页在 2026-10-20 之前一条测试都没有，但它接手的正是原先散在检测页上的
 * 三件事：后台进度、取消、回看。能力搬家了、测试没跟过来，就是真空档。
 *
 * 做法沿用本仓既有风格：不引 DOM、不引测试框架，用自建 hooks 替身直接驱动
 * 真实组件函数。区别只在于这里要把子组件（JobRow / JobResults / ResultRow）
 * 也真的展开，否则只能看到外壳文案，断言就成了自欺欺人。
 */

// ---------------------------------------------------------------------------
// 展开器：把 React 元素树连同函数组件一起求值成纯元素树
// ---------------------------------------------------------------------------

/**
 * 每个组件实例都有自己的 hook 槽位。
 *
 * 键用「组件函数 + 本轮第几次出现」：同一个 JobRow 在不同任务行上是不同实例，
 * 但重渲染时必须拿回同一份状态，否则点开又自动合上。
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

    /** 递归展开：函数组件被真实调用并继续展开它的返回值。 */
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
        /**
         * 开始一次渲染：重置出现序号，然后展开整棵树。
         *
         * 根组件也必须走 expand —— 直接把根函数调一遍会让 hook 替身在
         * current 为 null 时被调用（没有槽位可写），一进 useState 就崩。
         */
        render(rootFn) {
            occurrence = new Map();
            return expand({ type: rootFn, props: {} });
        },
        /** 跑本轮登记过的所有 effect。 */
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

// ---------------------------------------------------------------------------
// 元素树查询工具
// ---------------------------------------------------------------------------

function text(node) {
    if (node === null || node === undefined || node === false || node === true) return '';
    if (typeof node === 'string' || typeof node === 'number') return String(node);
    if (Array.isArray(node)) return node.map(text).join(' ');
    if (typeof node === 'object' && node.props) return text(node.props.children);
    return '';
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

// ---------------------------------------------------------------------------
// 数据与装载
// ---------------------------------------------------------------------------

function makeJob(over = {}) {
    return {
        id: 1,
        channel_id: 7,
        channel_name: '测试渠道',
        status: 'running',
        total: 4,
        finished: 2,
        probes: '',
        timeout_seconds: 0,
        trigger: 'manual',
        created_at: new Date('2026-10-20T10:00:00Z').toISOString(),
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
        verdict: 'none',
        score: 0,
        finding_n: 0,
        report: '',
        error_msg: '',
        duration_ms: 1200,
        created_at: new Date('2026-10-20T10:00:00Z').toISOString(),
        ...over,
    };
}

/**
 * 翻译替身：把参数一并写进字符串。
 *
 * 不能用 (key) => key —— 那样 "第 2 / 共 4" 这类带参文案会把数字吞掉，
 * 断言「进度显示 2/4」就永远看不到真假。
 */
function translate(key, vars) {
    return vars ? `${key}(${JSON.stringify(vars)})` : key;
}

function loadQueue({ jobs = [], detail = null } = {}) {
    const state = { jobs, detail, cancelled: [], enqueued: [] };
    const rt = createRuntime();

    const mod = load('src/components/modules/model-audit/Queue.tsx', {
        react: rt.react,
        'lucide-react': new Proxy({}, { get: (_, key) => key }),
        'next-intl': { useTranslations: () => translate },
        '@/components/ui/button': new Proxy({}, { get: (_, key) => key }),
        '@/lib/utils': { cn: (...p) => p.filter(Boolean).join(' ') },
        '@/lib/model-audit-verdict': { historyBucket: () => 'all', verdictHeadline: (v) => v, formatReportTime: (x) => x || '' },
        '@/components/common/Toast': { toast: { success() {}, error() {}, warning() {}, info() {} } },
        '@/api/endpoints/channel': {
            ChannelType: { Chat: 0, Anthropic: 2 },
            useChannelList: () => ({ data: [] }),
        },
        '@/api/endpoints/model-audit': {
            useAuditJobs: () => ({ data: state.jobs, isLoading: false }),
            useAuditJobDetail: () => ({ data: state.detail, isLoading: false }),
            useCancelAuditJob: () => ({
                isPending: false,
                variables: undefined,
                mutate: (id) => state.cancelled.push(id),
            }),
            useEnqueueAudit: () => ({
                isPending: false,
                mutate: (vars) => state.enqueued.push(vars),
                mutateAsync: (vars) => {
                    state.enqueued.push(vars);
                    return Promise.resolve({ job_id: 1 });
                },
            }),
        },
    });

    return {
        mod,
        state,
        // 必须传组件的稳定引用：若每次包一层箭头函数，组件身份就变了，
        // hook 槽位会跟着重建，重渲染时状态全部丢失（点开又自动合上）。
        render: () => rt.render(mod.ModelAuditQueue),
    };
}

// ---------------------------------------------------------------------------
// 测试
// ---------------------------------------------------------------------------

test('queue page renders progress from the server, not local state', () => {
    const q = loadQueue({ jobs: [makeJob({ total: 4, finished: 2 })] });
    const body = text(q.render());

    // 进度必须来自任务行（finished/total）：刷新页面、换个设备看到的都得一样。
    assert.ok(body.includes('测试渠道'), 'renders the channel name snapshot');
    assert.ok(body.includes('queue.statusRunning'), 'renders the running status text');
    assert.ok(/queue\.statusRunning\(\{[^}]*"done":2[^}]*"total":4[^}]*\}\)/.test(body.replace(/&quot;/g, '"')), `progress carries done=2 total=4, got: ${body.slice(0, 400)}`);
});

test('queue page shows an empty state instead of a blank area', () => {
    const q = loadQueue({ jobs: [] });
    const body = text(q.render());
    assert.ok(body.includes('queue.empty'), 'empty state text is rendered');
    assert.equal(body.includes('queue.statusRunning'), false, 'no stray job row when there is nothing to show');
});

test('running job exposes a cancel action that targets the backend job id', () => {
    const q = loadQueue({ jobs: [makeJob({ id: 42, status: 'running' })] });
    const tree = q.render();

    // 取消必须打到后端任务，而不是 abort 一个浏览器请求 —— 否则"取消"只断开
    // 当前这一个连接，后台照样继续跑、继续花钱。
    const cancelBtn = findElement(tree, (n) => n.props?.onClick && text(n).includes('queue.cancel'));
    assert.ok(cancelBtn, 'cancel button is rendered for an active job');

    cancelBtn.props.onClick();
    assert.deepEqual(q.state.cancelled, [42], 'cancel targets the job id from the server');
});

test('finished job does not offer cancel', () => {
    const q = loadQueue({ jobs: [makeJob({ status: 'done' })] });
    const tree = q.render();
    const cancelBtn = findElement(tree, (n) => n.props?.onClick && text(n).includes('queue.cancel'));
    assert.equal(cancelBtn, undefined, 'a done job has nothing to cancel');
});

test('expanding a job shows per-model results, including failures with their reason', () => {
    const q = loadQueue({
        jobs: [makeJob({ status: 'done', total: 2, finished: 2 })],
        detail: {
            job: makeJob({ status: 'done', total: 2, finished: 2 }),
            results: [
                makeResult({ model: 'ok-model', status: 'done', verdict: 'none' }),
                makeResult({ id: 12, model: 'bad-model', status: 'failed', error_msg: '上游超时' }),
            ],
        },
    });

    // 折叠时看不到逐条结果，展开后才看得见 —— 这正是"回看"的入口。
    const collapsed = text(q.render());
    assert.equal(collapsed.includes('ok-model'), false, 'results stay hidden until the row is expanded');

    let tree = q.render();
    const toggle = findElement(tree, (n) => n.props?.onClick && text(n).includes('测试渠道'));
    assert.ok(toggle, 'job row is expandable');
    toggle.props.onClick();

    const body = text(q.render());
    assert.ok(body.includes('ok-model'), 'successful model is listed');
    assert.ok(body.includes('bad-model'), 'failed model is listed');
    assert.ok(body.includes('上游超时'), 'failure reason is shown, not a generic message');
});

test('a pending result never reads as finished', () => {
    // 失败重试期间结果行仍是 pending（退避最长 10 分钟）。这时候让用户以为
    // 跑完了，是最容易招骂的一种不诚实，所以单独立一条守着。
    const q = loadQueue({
        jobs: [makeJob({ status: 'running', total: 1, finished: 0 })],
        detail: {
            job: makeJob({ status: 'running', total: 1, finished: 0 }),
            results: [
                makeResult({
                    status: 'pending',
                    attempt: 2,
                    next_retry_at: new Date('2026-10-20T10:00:30Z').toISOString(),
                    error_msg: '上游超时',
                }),
            ],
        },
    });

    let tree = q.render();
    const toggle = findElement(tree, (n) => n.props?.onClick && text(n).includes('测试渠道'));
    toggle.props.onClick();

    const body = text(q.render());
    // 重试中的行用「重试中」而不是笼统的「等待中」—— 两者都还没完成，
    // 但前者说明"已经试过、正在退避"，后者说明"还在排队"。混为一谈就丢信息。
    assert.ok(body.includes('queue.resultRetrying'), `expected a retry label, got: ${body.slice(0, 400)}`);
    assert.equal(body.includes('queue.resultDone'), false, 'a retrying row must never read as done');
    assert.equal(body.includes('queue.resultFailed'), false, 'a retrying row is not labelled failed');
    assert.ok(body.includes('"attempt"'), 'the retry label carries the attempt count');
});
