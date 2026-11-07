'use strict';

// Run from web/: node --test tests/hlwy-endpoints.test.cjs

const test = require('node:test');
const assert = require('node:assert');
const { load } = require('./load-typescript.cjs');

/**
 * hlwy 的请求面。
 *
 * 这一层是「客户端能对服务端做什么」的唯一出口，所以断言都落在**实际发出的
 * 路径、方法和请求体**上：入队体里多一个 probes 或 log_id，多探针与日志回填
 * 的语义就会混进对照任务里；少一个 kind，后端就分不清这是哪种任务。
 */

function endpoint() {
    const calls = [];
    const invalidated = [];
    const apiClient = {
        get: async (path, params) => {
            calls.push({ method: 'GET', path, params });
            return { ok: true };
        },
        post: async (path, body) => {
            calls.push({ method: 'POST', path, body });
            return { ok: true };
        },
    };
    const queryClient = {
        invalidateQueries: (arg) => invalidated.push(arg),
    };
    const errors = [];
    const api = load('src/api/endpoints/model-audit.ts', {
        '@tanstack/react-query': {
            useQuery: (options) => options,
            useMutation: (options) => options,
            useQueryClient: () => queryClient,
        },
        '../client': { apiClient },
        '@/lib/logger': { logger: { error: (...args) => errors.push(args), warn: () => {}, info: () => {} } },
    });
    return { api, calls, invalidated, errors };
}

/* ------------------------------------------------------------------ *
 * 计划：零请求地读「这一单要花多少」
 * ------------------------------------------------------------------ */

test('plan query carries kind, channel, model, samples and the frozen baseline', async () => {
    const { api, calls } = endpoint();
    const options = api.useHlwyPlan({
        channelId: 7,
        model: 'gpt-test',
        samples: 200,
        baselineId: 'hlwy:base@aaa111',
    });
    assert.equal(options.enabled, true);
    assert.deepEqual(options.queryKey, [
        'model-audit',
        'hlwy-plan',
        7,
        'gpt-test',
        200,
        'hlwy:base@aaa111',
    ]);

    await options.queryFn();
    assert.equal(calls.length, 1);
    assert.equal(calls[0].method, 'GET');
    assert.equal(calls[0].path, '/api/v1/model-audit/plan');
    assert.deepEqual(calls[0].params, {
        kind: 'hlwy_compare',
        channel_id: 7,
        model: 'gpt-test',
        samples: 200,
        baseline_id: 'hlwy:base@aaa111',
    });
});

test('plan query stays disabled until there is a target and a baseline', () => {
    const { api } = endpoint();
    assert.equal(api.useHlwyPlan({ channelId: 0, model: 'gpt-test', samples: 100, baselineId: 'x' }).enabled, false);
    assert.equal(api.useHlwyPlan({ channelId: 7, model: '  ', samples: 100, baselineId: 'x' }).enabled, false);
    assert.equal(api.useHlwyPlan({ channelId: 7, model: 'gpt-test', samples: 100, baselineId: '' }).enabled, false);
    // 显式关掉（例如切回探针模式）时不发请求。
    assert.equal(
        api.useHlwyPlan({ channelId: 7, model: 'gpt-test', samples: 100, baselineId: 'x', enabled: false }).enabled,
        false
    );
});

test('baseline catalog is readable and can be switched off', async () => {
    const { api, calls } = endpoint();
    const on = api.useHlwyBaselines();
    assert.equal(on.enabled, true);
    await on.queryFn();
    assert.equal(calls[0].path, '/api/v1/model-audit/hlwy-baselines');
    assert.equal(api.useHlwyBaselines(false).enabled, false);
});

/* ------------------------------------------------------------------ *
 * 提交
 * ------------------------------------------------------------------ */

test('enqueue payload is exactly what the contract allows', async () => {
    const { api, calls, invalidated } = endpoint();
    const options = api.useEnqueueHlwyCompare();
    const payload = {
        channel_id: 7,
        models: ['gpt-test'],
        kind: 'hlwy_compare',
        hlwy: { samples: 100, baseline_id: 'hlwy:base@aaa111', protocol_id: 'hlwy-number355-v1' },
    };
    await options.mutationFn(payload);

    assert.equal(calls[0].method, 'POST');
    assert.equal(calls[0].path, '/api/v1/model-audit/enqueue');
    assert.deepEqual(calls[0].body, payload);

    const body = calls[0].body;
    assert.equal('probes' in body, false, 'multi-probe settings do not belong to a comparison');
    assert.equal('log_id' in body, false, 'a comparison is not a log refill');
    assert.equal('timeout_seconds' in body, false, 'timeout comes from the plan, not the client');
    assert.deepEqual(Object.keys(body.hlwy).sort(), ['baseline_id', 'protocol_id', 'samples']);
    assert.equal(body.models.length, 1, 'one channel + one model per run');

    options.onSuccess();
    assert.deepEqual(invalidated, [{ queryKey: ['model-audit', 'jobs'] }]);
});

test('import, save-from-result and recompare hit their own paths', async () => {
    const { api, calls, invalidated } = endpoint();

    const imported = api.useImportHlwyBaseline();
    await imported.mutationFn({ name: 'x', rawData: [1], distribution: [], stats: {} });
    const saved = api.useSaveHlwyBaselineFromResult();
    await saved.mutationFn({ result_id: 11, label: '我的基准' });
    await api.useRecompareHlwy().mutationFn({ result_id: 11, baseline_id: 'hlwy:other@bbb222' });

    assert.deepEqual(
        calls.map((call) => [call.method, call.path]),
        [
            ['POST', '/api/v1/model-audit/hlwy-baselines/import'],
            ['POST', '/api/v1/model-audit/hlwy-baselines/from-result'],
            ['POST', '/api/v1/model-audit/hlwy-compare'],
        ]
    );
    assert.deepEqual(calls[1].body, { result_id: 11, label: '我的基准' });
    assert.deepEqual(calls[2].body, { result_id: 11, baseline_id: 'hlwy:other@bbb222' });

    // 导入与存基准改变了服务端事实，要刷基准目录。
    assert.deepEqual(invalidated, [], 'nothing is invalidated merely by firing the request');
    imported.onSuccess();
    saved.onSuccess();
    assert.deepEqual(invalidated, [
        { queryKey: ['model-audit', 'hlwy-baselines'] },
        { queryKey: ['model-audit', 'hlwy-baselines'] },
    ]);
});

test('recompare never invalidates anything', () => {
    const { api, invalidated } = endpoint();
    const options = api.useRecompareHlwy();
    // 换基准重算只在浏览器里把同一批数字重算一遍：没有服务端事实改变，
    // 一旦 invalidate 就会变成「重算触发重新拉取/重跑」。
    assert.equal(options.onSuccess, undefined);
    assert.deepEqual(invalidated, []);
});

/* ------------------------------------------------------------------ *
 * 轮询：跑完就停，但取消要等最后那一刀落地
 * ------------------------------------------------------------------ */

test('job polling stops on done and keeps going while a canceled run still has work', () => {
    const { api } = endpoint();
    const options = api.useAuditJobDetail(5);
    const interval = (data) => options.refetchInterval({ state: { data } });

    assert.equal(interval(undefined), 3_000, 'nothing loaded yet: keep polling');
    assert.equal(interval({ job: { status: 'running' }, results: [] }), 3_000);
    assert.equal(interval({ job: { status: 'done' }, results: [{ status: 'running' }] }), false, 'done is done');
    // 取消只挡住**新的**采样：已经发出去的那一次还在跑，停下轮询就会把它
    // 钉死在「检测中」。
    assert.equal(interval({ job: { status: 'canceled' }, results: [{ status: 'running' }] }), 3_000);
    assert.equal(interval({ job: { status: 'canceled' }, results: [{ status: 'done' }] }), false);
    assert.equal(interval({ job: { status: 'canceled' }, results: [] }), false);
});
