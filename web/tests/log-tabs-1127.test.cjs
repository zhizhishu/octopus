// fix-ui1127a 日志页三标签的前端测试（含 r1 小修）：media-task 端点真实模块行为
// + 实时标签/合并任务面板/整页面板的接线断言。
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
    const options = h.options({ kind: 'video', status: 'in_progress,processing', model: 'veo', startTime: 100, endTime: 200 });
    await options.queryFn();
    const url = h.requests[0].url;
    assert.match(url, /status=in_progress/);
    assert.match(url, /model=veo/);
    assert.match(url, /start_time=100/);
    assert.match(url, /end_time=200/);
    assert.match(url, /type=video/);
});

test('media-task endpoint: user_id forwards for admin narrowing and keys the cache', async () => {
    const h = harness();
    const options = h.options({ userId: 8 });
    await options.queryFn();
    assert.match(h.requests[0].url, /user_id=8/);
    const keyed = h.options({ userId: 8 });
    const unkeyed = h.options({ userId: 0 });
    assert.notDeepEqual(keyed.queryKey, unkeyed.queryKey, 'user filter is part of the cache key');
});

test('media-task polling: non-terminal rows keep a 5s refetch, terminal rows stop it', () => {
    const h = harness();
    const options = h.options();
    assert.equal(typeof options.refetchInterval, 'function');
    const live = options.refetchInterval({ state: { data: { items: [{ status: 'processing' }] } } });
    const done = options.refetchInterval({ state: { data: { items: [{ status: 'success' }] } } });
    const imgDone = options.refetchInterval({ state: { data: { items: [{ status: 'completed' }] } } });
    const unknown = options.refetchInterval({ state: { data: { items: [{ status: 'unknown' }] } } });
    assert.equal(live, 5000, 'processing is actively running');
    assert.equal(done, false, 'success is terminal');
    assert.equal(imgDone, false, 'completed is terminal');
    assert.equal(unknown, false, 'unknown (midjourney/history) must not poll forever');
    const queued = options.refetchInterval({ state: { data: { items: [{ status: 'queued' }] } } });
    assert.equal(queued, 5000, 'queued still polls');
});

test('media-task terminal vocabulary covers both image and video statuses', () => {
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
    for (const polling of ['processing', 'in_progress', 'queued', 'submitted', 'not_start']) {
        assert.equal(mod.isMediaTaskPollingStatus(polling), true, `${polling} should poll`);
    }
    for (const idle of ['success', 'failure', 'completed', 'failed', 'unknown', '']) {
        assert.equal(mod.isMediaTaskPollingStatus(idle), false, `${idle || '(empty)'} should not poll`);
    }
});

test('live tab: dropdown filters from live set, count, elapsed-desc ordering, virtualization', () => {
    const tab = readFileSync(path.join(__dirname, '..', 'src', 'components', 'modules', 'log', 'LiveTab.tsx'), 'utf8');
    // SSE 常开——实时标签本身就是实时。
    assert.match(tab, /useRequestStateStream\(true\)/);
    // 只放在途（running）请求；结束的从列表移走。
    assert.match(tab, /state\.status === 'running'/);
    // fix-ui1127a-r1 第 2 条：下拉框不是文本框，选项取当前在途集合里出现过的值。
    assert.doesNotMatch(tab, /type="text"/);
    const selectCount = (tab.match(/<select/g) || []).length;
    assert.ok(selectCount >= 4, `expected >= 4 filter selects, got ${selectCount}`);
    assert.match(tab, /uniq\(running\.map/);
    assert.match(tab, /userKeyOptions/);
    // 用户/Key 下拉仅管理员。
    assert.match(tab, /isAdmin && \(/);
    // 在途总数。
    assert.match(tab, /inFlight', \{ count: sorted\.length \}\)/);
    // 默认按已耗时倒序 = 开始最早的在最前。
    assert.match(tab, /return ta - tb/);
    assert.match(tab, /fullPage/);
});

test('live panel fullPage: no collapse header at all, rows virtualized', () => {
    const panel = readFileSync(path.join(__dirname, '..', 'src', 'components', 'modules', 'log', 'LiveActivityPanel.tsx'), 'utf8');
    // fix-ui1127a-r1 第 1 条：整页形态根本没有可折叠表头（计数已在筛选排）。
    assert.match(panel, /\{!fullPage && \(/);
    assert.match(panel, /useVirtualizer/);
    assert.match(panel, /const collapsed = fullPage \? false : collapsedStateValue/);
});

test('log page: three tabs in order, requests tab strips live insertion', () => {
    const page = readFileSync(path.join(__dirname, '..', 'src', 'components', 'modules', 'log', 'index.tsx'), 'utf8');
    const liveIdx = page.indexOf("id: 'live'");
    const reqIdx = page.indexOf("id: 'requests'");
    const taskIdx = page.indexOf("id: 'tasks'");
    assert.ok(liveIdx !== -1 && reqIdx !== -1 && taskIdx !== -1, 'all three tabs exist');
    assert.ok(liveIdx < reqIdx && reqIdx < taskIdx, 'order: live, requests, tasks');
    assert.match(page, /<MediaTaskPanel \/>/);
    assert.doesNotMatch(page, /<VideoTaskPanel/);
    assert.doesNotMatch(page, /<AutoRefreshControl/);
    assert.doesNotMatch(page, /<PendingLogsPill/);
});

test('media task panel: unified status buckets, fail-reason gating, copy size, user filter', () => {
    const panel = readFileSync(path.join(__dirname, '..', 'src', 'components', 'modules', 'log', 'MediaTaskPanel.tsx'), 'utf8');
    assert.match(panel, /typeFilter/);
    assert.match(panel, /statusFilter/);
    assert.match(panel, /modelFilter/);
    assert.match(panel, /useMediaTaskList/);
    // fix-ui1127a-r1 第 3 条：状态显示统一五桶，后端原始状态只在前端映射。
    assert.match(panel, /not_start: 'queued'/);
    assert.match(panel, /completed: 'success'/);
    assert.match(panel, /failure: 'failed'/);
    assert.match(panel, /processing: 'in_progress'/);
    // 筛选按桶发逗号分隔的原始状态集（后端 IN 过滤）。
    assert.match(panel, /'not_start,submitted,queued'/);
    assert.match(panel, /'in_progress,processing'/);
    // 第 4 条：失败原因只在失败/未知态显示。
    assert.match(panel, /statusBucket\(task\.status\) === 'failed' \|\| statusBucket\(task\.status\) === 'unknown'/);
    // 第 5 条：复制图标小尺寸，与徽章对齐。
    assert.match(panel, /copyIconClassName="size-3"/);
    // 第 6 条：管理员多一个「用户」下拉。
    assert.match(panel, /userFilter/);
    assert.match(panel, /useUserList\(\{ enabled: isAdmin \}\)/);
    // 字段词表与视频任务一致。
    for (const field of ['task_id', 'model', 'status', 'progress', 'fail_reason', 'result_url']) {
        assert.match(panel, new RegExp(field), `${field} rendered`);
    }
});

test('all three locales carry every new log key the components render', () => {
    const locales = ['zh_hans.json', 'zh_hant.json', 'en.json'].map((name) =>
        JSON.parse(readFileSync(path.join(__dirname, '..', 'public', 'locale', name), 'utf8')));
    const tabsKeys = ['live', 'requests', 'tasks'];
    const liveTabKeys = ['filterUserKey', 'filterChannel', 'filterModel', 'filterEndpoint', 'inFlight', 'userPrefix'];
    const mediaKeys = [
        'typeFilter', 'typeAll', 'typeImage', 'typeVideo', 'statusFilter', 'statusAll', 'modelFilter', 'dateFilter',
        'total', 'refresh', 'updating', 'loading', 'empty', 'emptyFiltered', 'emptyHint', 'loadFailed', 'retry',
        'submit', 'progress', 'finish', 'submitTime', 'finishTime', 'preview', 'resultLoadFailed', 'resultLoadFailedHint',
        'download', 'duration', 'prevPage', 'nextPage', 'pageOf',
        'statusQueued', 'statusInProgress', 'statusSuccess', 'statusFailed', 'statusUnknown',
        'userFilter', 'userAll',
    ];
    for (const locale of locales) {
        for (const key of tabsKeys) assert.ok(locale.log.tabs[key], `tabs.${key}`);
        for (const key of liveTabKeys) assert.ok(locale.log.liveTab[key], `liveTab.${key}`);
        for (const key of mediaKeys) assert.ok(locale.log.mediaTasks[key], `mediaTasks.${key}`);
    }
});
