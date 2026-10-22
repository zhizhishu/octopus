// Run from web/: node --test tests/video-task-user-scope.test.cjs
//
// 守的是一条跨用户的越权可见性。后端按会话身份过滤任务列表（前端连别人的 id 都
// 传不了），但缓存是浏览器本地的、全站共用一套 QueryClient（src/provider/query.tsx，
// staleTime 30s），登出也不清缓存。于是只要 queryKey 不区分「这是谁的列表」：
//   A 看过任务 → 登出 → B 在同一浏览器登录 → 30 秒内直接命中 A 的缓存（含结果
//   视频地址），过期后也会先拿 A 的旧数据占位（keepPreviousData 是全局的）。
// 这条测试用同一个 QueryClient 做 A→B 切换，钉住键里的归属维度和占位数据的边界。
const assert = require('node:assert/strict');
const { test } = require('node:test');
const { setImmediate } = require('node:timers/promises');
const { load } = require('./load-typescript.cjs');

// QueryObserver 要走浏览器分支的定时器，而不是 SSR 分支。
global.window = {};
const realReactQuery = require('@tanstack/react-query');
const { QueryClient, QueryObserver } = realReactQuery;

const task = (owner, page) => ({
    task_id: `${owner}-page${page}`,
    model: 'video-model',
    status: 'success',
    progress: '100%',
    fail_reason: '',
    result_url: `https://cdn.invalid/u${owner}/page${page}.mp4`,
    submit_time: 100,
    start_time: 100,
    finish_time: 200,
    created_at: 100,
});

function harness() {
    // 身份来自 auth store：测试里随时切换/清空，模拟登出与换人。
    const auth = { user: { id: 1, username: 'alice' } };
    const requests = [];
    const endpoint = load('src/api/endpoints/video-task.ts', {
        // useQuery 只吐选项：测试自己用真的 QueryObserver 观察缓存/占位行为。
        '@tanstack/react-query': { ...realReactQuery, useQuery: (options) => options },
        '../client': {
            apiClient: {
                async get(url) {
                    const owner = auth.user ? auth.user.id : 'anonymous';
                    const page = Number(new URL(url, 'http://x.invalid').searchParams.get('page'));
                    requests.push({ url, owner, page });
                    return { items: [task(owner, page)], total: 40, page, page_size: 20 };
                },
            },
        },
        './user': { useAuthStore: (selector) => selector({ user: auth.user }) },
    });
    return {
        auth,
        requests,
        client: new QueryClient({ defaultOptions: { queries: { gcTime: Infinity, staleTime: 30 * 1000 } } }),
        options: (options = {}) => endpoint.useVideoTaskList({ page: 1, pageSize: 20, ...options }),
        keyOf: (options) => options.queryKey,
    };
}

test('switching accounts never reuses the previous user task cache or its result urls', async (t) => {
    const h = harness();
    const observer = new QueryObserver(h.client, h.options());
    const observed = [];
    const unsubscribe = observer.subscribe((result) => observed.push(result));
    t.after(() => { unsubscribe(); h.client.clear(); });

    await setImmediate();
    assert.equal(observer.getCurrentResult().data.items[0].task_id, '1-page1', 'A sees A rows');
    assert.equal(h.requests.length, 1);

    // A 登出、B 在同一浏览器登录：同一个 QueryClient，缓存还在（30s staleTime 内）。
    const beforeSwitch = observed.length;
    h.auth.user = { id: 2, username: 'bob' };
    observer.setOptions(h.options());

    const during = observer.getCurrentResult();
    assert.equal(during.data, undefined, 'B must not be shown A rows while B list loads');
    assert.equal(during.isPlaceholderData, false, 'A rows are not a legal placeholder for B');

    await setImmediate();
    const after = observer.getCurrentResult();
    assert.equal(after.data.items[0].task_id, '2-page1', 'B sees B rows');
    assert.equal(h.requests.length, 2, 'B list is fetched, not read out of A cache');
    assert.equal(h.requests[1].owner, 2);

    // 换人之后任何一个被通知的结果里都不能出现 A 的结果地址。
    for (const result of observed.slice(beforeSwitch)) {
        assert.equal(JSON.stringify(result.data ?? null).includes('/u1/'), false, 'A result urls never reach B results');
    }

    // 回切到 A：命中 A 自己的缓存条目（不是 B 的），也不该多打一次请求。
    h.auth.user = { id: 1, username: 'alice' };
    observer.setOptions(h.options());
    const backToA = observer.getCurrentResult();
    assert.equal(backToA.data.items[0].task_id, '1-page1', 'A reads its own cache entry, never B rows');
    assert.equal(JSON.stringify(backToA.data).includes('/u2/'), false, 'B rows never leak back into A');
    assert.equal(h.requests.length, 2, 'A cache entry is still fresh: returning to A costs no request');
});

test('no identity means no request: an unauthenticated list never hits the endpoint', async (t) => {
    const h = harness();
    h.auth.user = null;
    const observer = new QueryObserver(h.client, h.options());
    const unsubscribe = observer.subscribe(() => {});
    t.after(() => { unsubscribe(); h.client.clear(); });

    await setImmediate();
    assert.equal(h.requests.length, 0, 'no identity -> no request');
    const idle = observer.getCurrentResult();
    assert.equal(idle.data, undefined);
    assert.equal(idle.fetchStatus, 'idle');
    assert.equal(idle.isFetching, false);

    // 身份到了才开始拉；而且拉的是这个身份自己的列表。
    h.auth.user = { id: 3, username: 'carol' };
    observer.setOptions(h.options());
    await setImmediate();
    assert.equal(h.requests.length, 1);
    assert.equal(h.requests[0].owner, 3);
    assert.equal(observer.getCurrentResult().data.items[0].task_id, '3-page1');
});

// 既有体验不能退化：同一个人翻页/换筛选时仍然保留上一页，不闪一下空列表。
test('same-user page change keeps the previous page visible (no flicker)', async (t) => {
    const h = harness();
    const observer = new QueryObserver(h.client, h.options());
    const unsubscribe = observer.subscribe(() => {});
    t.after(() => { unsubscribe(); h.client.clear(); });

    await setImmediate();
    assert.equal(observer.getCurrentResult().data.items[0].task_id, '1-page1');

    observer.setOptions(h.options({ page: 2 }));
    const during = observer.getCurrentResult();
    assert.equal(during.isPlaceholderData, true, 'same user keeps previous data as placeholder');
    assert.equal(during.data.items[0].task_id, '1-page1', 'previous page still painted while page 2 loads');

    await setImmediate();
    assert.equal(observer.getCurrentResult().data.items[0].task_id, '1-page2');
    assert.equal(observer.getCurrentResult().isPlaceholderData, false);
});

// 归属维度必须在 queryKey 里：否则同一 QueryClient 下所有人共用一个缓存条目。
test('the cache key carries the identity dimension', () => {
    const h = harness();
    h.auth.user = { id: 7 };
    const alice = h.keyOf(h.options());
    h.auth.user = { id: 8 };
    const bob = h.keyOf(h.options());
    assert.deepEqual(alice.slice(0, 2), ['video-tasks', 'list'], 'key keeps its existing namespace');
    assert.notDeepEqual(alice, bob, 'different users must not share a cache entry');
    assert.notEqual(alice[2], bob[2], 'the identity sits in its own slot');
});
