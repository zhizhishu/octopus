const assert = require('node:assert/strict');
const { test } = require('node:test');
const { load } = require('./load-typescript.cjs');

// Exercise the actual hook with controlled query/mutation state. Rendering and
// network polling are additionally checked in the browser against the image.
function harness(raw = 'fill_first') {
    const state = {
        query: { data: [{ key: 'route_mode_override', value: raw }], isSuccess: true, isFetching: false, isError: false },
        mutation: { isPending: false, variables: undefined },
        writes: [], effects: [], notifications: [], invalidations: 0,
    };
    const queryClient = {
        setQueryData: (_key, update) => { state.query.data = update(state.query.data); },
        invalidateQueries: () => { state.invalidations++; },
    };
    const module = load('src/api/endpoints/setting.ts', {
        react: { useEffect: (effect) => { state.effects.push(effect); } },
        '@tanstack/react-query': {
            useQuery: () => state.query,
            useQueryClient: () => queryClient,
            useMutation: (options) => ({
                ...state.mutation,
                mutate: (data, callbacks) => {
                    state.writes.push(data);
                    state.mutation = { isPending: true, variables: data };
                    state.settle = (success) => {
                        state.mutation.isPending = false;
                        if (success) {
                            options.onSuccess?.(data);
                            callbacks.onSuccess?.(data);
                        } else {
                            options.onError?.(new Error('rejected'));
                            callbacks.onError?.(new Error('rejected'));
                        }
                    };
                },
            }),
        },
        '../client': { apiClient: {}, API_BASE_URL: '.' },
        '@/lib/logger': { logger: { log() {}, error() {} } },
        '@/components/common/Toast': { toast: {
            success: (text) => state.notifications.push(['success', text]),
            error: (text) => state.notifications.push(['error', text]),
        } },
        './user': { useAuthStore: {} },
    });
    return {
        state, module,
        render() {
            state.effects = [];
            const result = module.useRouteModeOverrideSetting();
            state.effects.forEach((effect) => effect());
            return result;
        },
    };
}

test('only two choices; mounting or refetching never writes a default', () => {
    for (const raw of ['fill_first', 'spread', '', 'unknown']) {
        const h = harness(raw);
        assert.deepEqual(h.module.ROUTE_MODE_OVERRIDE_VALUES, ['spread', 'fill_first']);
        assert.equal(h.module.routeModeOverrideLabel('spread'), '轮询');
        assert.equal(h.module.routeModeOverrideLabel('fill_first'), '优先填充');
        h.render();
        h.state.query.isFetching = true;
        h.render();
        h.state.query.isFetching = false;
        h.render();
        assert.equal(h.state.writes.length, 0, `mount wrote ${JSON.stringify(raw)}`);
    }
});

test('unsupported or unread server state is disabled, never silently normalized', () => {
    for (const raw of ['', 'unknown', undefined]) {
        const h = harness(raw);
        if (raw === undefined) h.state.query.data = [];
        const view = h.render();
        assert.equal(view.isReady, false);
        view.update('spread');
        assert.equal(h.state.writes.length, 0);
    }
    const h = harness();
    h.state.query.isSuccess = false;
    assert.equal(h.render().isReady, false);
    h.state.query.isSuccess = true;
    h.state.query.isError = true;
    assert.equal(h.render().isReady, false);
});

test('explicit selection writes once and successful cache state matches server', () => {
    const h = harness();
    h.render().update('spread');
    assert.deepEqual(h.state.writes, [{ key: 'route_mode_override', value: 'spread' }]);
    const pending = h.render();
    assert.equal(pending.value, 'spread');
    assert.equal(pending.isPending, true);
    pending.update('fill_first');
    assert.equal(h.state.writes.length, 1, 'pending save must not race another save');
    h.state.settle(true);
    assert.equal(h.render().value, 'spread');
    assert.equal(h.state.query.data[0].value, 'spread');
    assert.equal(h.state.invalidations, 1);
    assert.equal(h.state.notifications.filter(([kind]) => kind === 'success').length, 1);
    h.render().update('spread');
    assert.equal(h.state.writes.length, 1, 'unchanged selection is a no-op');
});

test('failed save rolls back to confirmed data, never emits success', () => {
    const h = harness('spread');
    h.render().update('fill_first');
    assert.equal(h.render().value, 'fill_first');
    h.state.settle(false);
    assert.equal(h.render().value, 'spread');
    assert.equal(h.state.query.data[0].value, 'spread');
    assert.equal(h.state.notifications.some(([kind]) => kind === 'success'), false);
    assert.equal(h.state.notifications.some(([kind]) => kind === 'error'), true);
});

test('empty and unknown are not writable UI modes', () => {
    const h = harness();
    h.render().update('');
    h.render().update('unknown');
    assert.equal(h.state.writes.length, 0);
});
