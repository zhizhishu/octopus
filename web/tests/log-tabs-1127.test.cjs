// fix-ui1127a 日志页三标签的前端测试：media-task 端点的真实模块行为（缓存作用域、
// 筛选参数、轮询）+ 实时标签/合并任务面板的接线断言。
// Run from web/: node --test tests/log-tabs-1127.test.cjs
const test = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const path = require('node:path');
const { setImmediate } = require('node:timers/promises');
const { QueryClient, QueryObserver, ...realReactQuery } = require('@tanstack/react-query');
const { load } = require('./load-typescript.cjs');

function harness() {
    const auth = { user: { id: 1, username: 'alice' } };
    const requests = [];
    const endpoint = load('src/api/endpoints/media-task.ts', {
        '@tanstack/react-query': { ...realReactQuery, useQuery: (options) => options },
        '../client': {
            apiClient: {
                async get(url) {
                    const owner = auth.user ? auth.user.id : 'anonymous';
                    requests.push({ url, owner });
                    return { items: [{ kind: 'video', task_id: `${owner}-t`, status: 'success' }], total: 1, page: 1, page_size: 20 };
                },
            },
        },
        './user': { useAuthStore: (selector) => selector({ user: auth.user }) },
    });
    return {
        auth,
        requests,
        client: new QueryClient({ defaultOptions: { queries: { gcTime: Infinity, staleTime: 30 * 1000 } } }),
        options: (options = {}) => endpoint.useMediaTaskList({ page: 1, pageSize: 20, ...options }),
    };
}

test('media-task list: cache is scoped per user, switching accounts never reuses rows', async (t) => {
    const h = harness();
    const observer = new QueryObserver(h.client, h.options());
    const unsubscribe = observer.subscribe(() => {});
    t.after(() => { unsubscribe(); h.client.clear(); });

    await setImmediate();
    assert.equal(observer.getCurrentResult().data.items[0].task_id, '1-t', 'A sees A rows');
    assert.equal(h.requests.length, 1);

    h.auth.user = { id: 2, username: 'bob' };
    observer.setOptions(h.options());
    const during = observer.getCurrentResult();
    assert.equal(during.data, undefined, 'B must not be shown A rows while loading');
    assert.equal(during.isPlaceholderData, false);

    await setImmediate();
    assert.equal(observer.getCurrentResult().data.items[0].task_id, '2-t', 'B sees B rows');
    assert.equal(h.requests.length, 2, 'B list is fetched, not read out of A cache');
});

test('media-task list: no identity means no request', () => {
    const h = harness();
    h.auth.user = null;
    const options = h.options();
    assert.equal(options.enabled, false, 'unauthenticated list never hits the endpoint');
});

test('media-task list: kind/status/model/time filters land in the query string and the cache key', () => {
    const h = harness();
    const plain = h.options();
    const filtered = h.options({ kind: 'image', status: 'completed', model: 'mj', startTime: 10, endTime: 20 });
    assert.notDeepEqual(plain.queryKey, filtered.queryKey, 'filters change the cache key');
    assert.equal(filtered.queryKey[5], 'image');
    assert.equal(filtered.queryKey[6], 'completed');
    assert.equal(filtered.queryKey[7], 'mj');
    assert.equal(filtered.queryKey[8], 10);
    assert.equal(filtered.queryKey[9], 20);
});

test('media-task list: queryFn forwards every filter to the backend', async () => {
    const h = harness();
    const options = h.options({ kind: 'video', status: 'in_progress', model: 'veo', startTime: 100, endTime: 200 });
    await options.queryFn();
    const url = h.requests[0].url;
    assert.match(url, /type=video/);
    assert.match(url, /status=in_progress/);
    assert.match(url, /model=veo/);
    assert.match(url, /start_time=100/);
    assert.match(url, /end_time=200/);
});

test('media-task polling: non-terminal rows keep a 5s refetch, terminal rows stop it', () => {
    const h = harness();
    const options = h.options();
    assert.equal(typeof options.refetchInterval, 'function');
    const live = options.refetchInterval({ state: { data: { items: [{ status: 'processing' }] } } });
    const done = options.refetchInterval({ state: { data: { items: [{ status: 'success' }] } } });
    const imgDone = options.refetchInterval({ state: { data: { items: [{ status: 'completed' }] } } });
    const unknown = options.refetchInterval({ state: { data: { items: [{ status: 'unknown' }] } } });
    assert.equal(live, 5000, 'processing is not terminal');
    assert.equal(done, false, 'success is terminal');
    assert.equal(imgDone, false, 'completed is terminal');
    assert.equal(unknown, 5000, 'unknown (midjourney) is not terminal');
});

test('media-task terminal vocabulary covers both image and video statuses', () => {
    const h = harness();
    const endpoint = h.options ? null : null;
    const mod = load('src/api/endpoints/media-task.ts', {
        '@tanstack/react-query': { useQuery: () => ({}) },
        '../client': { apiClient: { get: async () => ({}) } },
        './user': { useAuthStore: (selector) => selector({ user: { id: 1 } }) },
    });
    for (const terminal of ['success', 'failure', 'completed', 'failed']) {
        assert.equal(mod.isMediaTaskTerminal(terminal), true, `${terminal} is terminal`);
    }
    for (const live of ['processing', 'in_progress', 'queued', 'submitted', 'not_start', 'unknown', '']) {
        assert.equal(mod.isMediaTaskTerminal(live), false, `${live || '(empty)'} is not terminal`);
    }
});

test('live tab: full-page panel, filters, in-flight count, elapsed-desc ordering, virtualization', () => {
    const tab = readFileSync(path.join(__dirname, '..', 'src', 'components', 'modules', 'log', 'LiveTab.tsx'), 'utf8');
    // SSE 常开（不再跟「实时模式」开关走——实时标签本身就是实时）。
    assert.match(tab, /useRequestStateStream\(true\)/);
    // 只放在途（running）请求；结束的从列表移走。
    assert.match(tab, /state\.status !== 'running'\) return false/);
    // 筛选一排：用户/Key（仅管理员）、渠道、模型、端点。
    assert.match(tab, /isAdmin && \(/);
    assert.match(tab, /filterUserKey/);
    assert.match(tab, /filterChannel/);
    assert.match(tab, /filterModel/);
    assert.match(tab, /filterEndpoint/);
    // 在途总数。
    assert.match(tab, /inFlight', \{ count: sorted\.length \}/);
    // 默认按已耗时倒序 = 开始最早的在最前。
    assert.match(tab, /return ta - tb/);
    // 面板整页形态。
    assert.match(tab, /fullPage/);
});

test('live panel fullPage: no collapse, no height cap, rows virtualized', () => {
    const panel = readFileSync(path.join(__dirname, '..', 'src', 'components', 'modules', 'log', 'LiveActivityPanel.tsx'), 'utf8');
    assert.match(panel, /useVirtualizer/);
    assert.match(panel, /const collapsed = fullPage \? false : collapsedStateValue/);
    assert.match(panel, /rowVirtualizer\.measureElement/);
    // fullPage 分支不戴 max-h 帽子。
    assert.match(panel, /fullPage \? "h-full /);
});

test('log page: three tabs in order, requests tab strips live insertion', () => {
    const page = readFileSync(path.join(__dirname, '..', 'src', 'components', 'modules', 'log', 'index.tsx'), 'utf8');
    const liveIdx = page.indexOf("id: 'live'");
    const reqIdx = page.indexOf("id: 'requests'");
    const taskIdx = page.indexOf("id: 'tasks'");
    assert.ok(liveIdx !== -1 && reqIdx !== -1 && taskIdx !== -1, 'all three tabs exist');
    assert.ok(liveIdx < reqIdx && reqIdx < taskIdx, 'order: live, requests, tasks');
    // 合并任务面板取代原视频任务面板。
    assert.match(page, /<MediaTaskPanel \/>/);
    assert.doesNotMatch(page, /<VideoTaskPanel/);
    // 请求日志不再自动插入/实时按钮/实时插入中。
    assert.doesNotMatch(page, /<AutoRefreshControl/);
    assert.doesNotMatch(page, /<PendingLogsPill/);
});

test('media task panel: type/status/model/time filters and video-task-aligned fields', () => {
    const panel = readFileSync(path.join(__dirname, '..', 'src', 'components', 'modules', 'log', 'MediaTaskPanel.tsx'), 'utf8');
    assert.match(panel, /typeFilter/);
    assert.match(panel, /typeAll/);
    assert.match(panel, /typeImage/);
    assert.match(panel, /typeVideo/);
    assert.match(panel, /statusFilter/);
    assert.match(panel, /modelFilter/);
    assert.match(panel, /useMediaTaskList/);
    // 字段词表与视频任务一致：task_id/model/status/progress/fail_reason/result_url/时间。
    for (const field of ['task_id', 'model', 'status', 'progress', 'fail_reason', 'result_url']) {
        assert.match(panel, new RegExp(field), `${field} rendered`);
    }
});

test('all three locales carry every new log key the components render', () => {
    const locales = ['zh_hans.json', 'zh_hant.json', 'en.json'].map((name) =>
        JSON.parse(readFileSync(path.join(__dirname, '..', 'public', 'locale', name), 'utf8')));
    const tabsKeys = ['live', 'requests', 'tasks'];
    const liveTabKeys = ['filterUserKey', 'filterChannel', 'filterModel', 'filterEndpoint', 'inFlight'];
    const mediaKeys = [
        'typeFilter', 'typeAll', 'typeImage', 'typeVideo', 'statusFilter', 'statusAll', 'modelFilter', 'dateFilter',
        'total', 'refresh', 'updating', 'loading', 'empty', 'emptyFiltered', 'emptyHint', 'loadFailed', 'retry',
        'submit', 'progress', 'finish', 'submitTime', 'finishTime', 'preview', 'resultLoadFailed', 'resultLoadFailedHint',
        'download', 'duration', 'prevPage', 'nextPage', 'pageOf',
        'statusNotStart', 'statusSubmitted', 'statusQueued', 'statusInProgress', 'statusProcessing',
        'statusSuccess', 'statusCompleted', 'statusFailure', 'statusFailed', 'statusUnknown',
    ];
    for (const locale of locales) {
        for (const key of tabsKeys) assert.ok(locale.log.tabs[key], `tabs.${key}`);
        for (const key of liveTabKeys) assert.ok(locale.log.liveTab[key], `liveTab.${key}`);
        for (const key of mediaKeys) assert.ok(locale.log.mediaTasks[key], `mediaTasks.${key}`);
    }
});
