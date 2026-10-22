// Run from web/: node --test tests/video-task-page-clamp.test.cjs
//
// 筛选「排队中」有 21 条 → 进第 2 页 → 一条完成，总数降到 20：
// totalPages 变回 1，导航整块消失，而 page 还停在 2，本页返回空列表。页面上只剩
// 「该状态下没有视频任务」，实际上一页还有 20 条；空列表还会让请求层的 5 秒轮询
// （video-task.ts 的 refetchInterval）停掉，于是一直空着、不再自愈。
// 这条测试钉住「拿到非占位响应后把页码收回最后一页」，同时钉住拿占位数据收页的反面。
const assert = require('node:assert/strict');
const { test } = require('node:test');
const { load } = require('./load-typescript.cjs');
const React = require('react');

const PAGE_SIZE = 20;

const queuedTask = (index) => ({
    task_id: `queued-${index}`,
    model: 'video-model',
    status: 'queued',
    progress: '',
    fail_reason: '',
    result_url: '',
    submit_time: 100,
    start_time: 0,
    finish_time: 0,
    created_at: 100,
});

const widgets = (names) => Object.fromEntries(names.split(' ').map((name) => [name, name]));

function findAll(node, predicate, found = []) {
    if (Array.isArray(node)) {
        for (const child of node) findAll(child, predicate, found);
        return found;
    }
    if (!node || typeof node !== 'object') return found;
    if (predicate(node)) found.push(node);
    findAll(node.props?.children, predicate, found);
    return found;
}
function findElement(node, predicate) {
    return findAll(node, predicate)[0];
}
function text(node) {
    if (Array.isArray(node)) return node.map(text).join('');
    if (!node || typeof node === 'boolean') return '';
    if (typeof node !== 'object') return String(node);
    return text(node.props?.children);
}
const countCards = (tree) => findAll(tree, (node) => typeof node.type === 'function').length;
const nextButton = (tree) => findElement(tree, (node) => node.type === 'Button' && text(node).includes('下一页'));
// 状态筛选下拉：onChange 顺手把页码重置成 1（既有行为）。
const statusSelect = (tree) => findElement(tree, (node) => node.type === 'select');

// 直接调用真实的 VideoTaskPanel（无 DOM）：只需能用的 useState/useEffect +
// 一个假服务端，就能观察「页码会不会自己收回来」。
function panelHarness() {
    const server = { rows: [], placeholder: false };
    const calls = [];
    const states = [];
    const effects = new Map();
    let slot = 0;

    const hooks = {
        ...React,
        useState(initial) {
            const index = slot++;
            if (!(index in states)) states[index] = typeof initial === 'function' ? initial() : initial;
            const set = (value) => { states[index] = typeof value === 'function' ? value(states[index]) : value; };
            return [states[index], set];
        },
        useEffect(fn, deps) {
            const index = slot++;
            const previous = effects.get(index);
            const unchanged = previous && deps && previous.deps.length === deps.length
                && deps.every((value, i) => Object.is(value, previous.deps[i]));
            if (!unchanged) effects.set(index, { deps: deps ?? [], fn, dirty: true });
        },
        useCallback(fn) { return fn; },
        useRef(initial) { const index = slot++; return states[index] ??= { current: initial }; },
    };

    const { VideoTaskPanel } = load('src/components/modules/log/VideoTaskPanel.tsx', {
        react: hooks,
        'lucide-react': new Proxy({}, { get: (_, key) => key }),
        '@/components/ui/badge': widgets('Badge'),
        '@/components/ui/button': widgets('Button'),
        '@/components/ui/dialog': widgets('Dialog DialogContent DialogHeader DialogTitle'),
        '@/components/ui/progress': widgets('Progress'),
        '@/components/common/CopyButton': widgets('CopyIconButton'),
        '@/lib/utils': { cn: (...parts) => parts.filter(Boolean).join(' ') },
        '@/api/endpoints/video-task': {
            VIDEO_TASK_STATUS_LABELS: {
                not_start: '未提交', submitted: '已提交', queued: '排队中', in_progress: '生成中',
                success: '成功', failure: '失败', unknown: '状态未知',
            },
            // 假服务端：按 (page, status) 切当前数据集的一页。
            useVideoTaskList: (options = {}) => {
                const { page = 1, pageSize = PAGE_SIZE, status = '' } = options;
                calls.push({ page, pageSize, status });
                const rows = server.rows.filter((row) => !status || row.status === status);
                const start = (page - 1) * pageSize;
                return {
                    data: { items: rows.slice(start, start + pageSize), total: rows.length, page, page_size: pageSize },
                    isPlaceholderData: server.placeholder,
                    isLoading: false,
                    isFetching: false,
                    isError: false,
                    refetch: () => {},
                };
            },
        },
    });

    return {
        server,
        calls,
        render() { slot = 0; return VideoTaskPanel(); },
        // 真实 React 在渲染之后跑 effect；这里显式跑，好观察「不修 / 修了」的差别。
        flushEffects() {
            let ran = 0;
            for (const entry of effects.values()) {
                if (!entry.dirty) continue;
                entry.dirty = false;
                entry.fn();
                ran++;
            }
            return ran;
        },
    };
}

test('a shrinking filtered total pulls the page back instead of hiding pagination on an empty page', () => {
    const h = panelHarness();
    h.server.rows = Array.from({ length: 21 }, (_, i) => queuedTask(i + 1));

    // 筛到「排队中」：共 21 条，两页。
    let tree = h.render();
    statusSelect(tree).props.onChange({ target: { value: 'queued' } });
    tree = h.render();
    assert.equal(h.calls.at(-1).page, 1);
    assert.equal(h.calls.at(-1).status, 'queued');
    assert.match(text(tree), /第 1 \/ 2 页/);
    assert.equal(countCards(tree), 20);

    // 进第 2 页：这里还剩 1 条。
    nextButton(tree).props.onClick();
    tree = h.render();
    assert.equal(h.calls.at(-1).page, 2);
    assert.match(text(tree), /第 2 \/ 2 页/);
    assert.equal(countCards(tree), 1);

    // 其中一条完成（不再是 queued）→ queued 只剩 20 条，第 2 页不再存在。
    h.server.rows = Array.from({ length: 20 }, (_, i) => queuedTask(i + 1));
    const broken = h.render();
    assert.equal(nextButton(broken), undefined, 'bug 现场：totalPages 变 1，导航整块消失');
    assert.equal(text(broken).includes('该状态下没有视频任务'), true, 'bug 现场：空页谎称没有任务');

    // 拿到非占位响应后收页。
    h.flushEffects();
    tree = h.render();
    assert.equal(h.calls.at(-1).page, 1, '页码收回最后一页');
    assert.equal(h.calls.at(-1).status, 'queued', '筛选条件不被收页动作改掉');
    assert.equal(countCards(tree), 20, '20 条任务重新可见');
    assert.equal(text(tree).includes('该状态下没有视频任务'), false, '不再谎称没有任务');

    // 收回之后不再来回跳（没有无限重渲染）。
    const settled = h.calls.length;
    h.render();
    h.flushEffects();
    const rerender = h.render();
    assert.equal(h.calls.length, settled + 2, '收页之后每个渲染只查一次，不再反复换页');
    assert.equal(h.calls.at(-1).page, 1, '页码稳定停在最后一页');
    assert.equal(countCards(rerender), 20);
});

test('placeholder data never resyncs the page: only a settled response may pull it back', () => {
    const h = panelHarness();
    h.server.rows = Array.from({ length: 21 }, (_, i) => queuedTask(i + 1));

    let tree = h.render();
    nextButton(tree).props.onClick();
    h.render();
    assert.equal(h.calls.at(-1).page, 2);

    // 占位数据（上一页/上次筛选的）带着更小的 total：不能据此收页，否则加载中会来回跳。
    h.server.rows = Array.from({ length: 20 }, (_, i) => queuedTask(i + 1));
    h.server.placeholder = true;
    h.render();
    h.flushEffects();
    assert.equal(h.calls.at(-1).page, 2, 'placeholder total must not move the page');

    // 同一份数字，换成真实响应才允许收页。
    h.server.placeholder = false;
    h.render();
    h.flushEffects();
    h.render();
    assert.equal(h.calls.at(-1).page, 1, 'settled response pulls the page back');
});
