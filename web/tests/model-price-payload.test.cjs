// Run from web/: node --test tests/model-price-payload.test.cjs
// The price edit dialog must round-trip per_call; dropping it from the update
// body used to reset per-call (video) billing to 0 on every price edit.
const assert = require('node:assert/strict');
const { test } = require('node:test');
const { load } = require('./load-typescript.cjs');

const { toPriceEditValues, buildModelUpdatePayload } = load('src/components/modules/model/price-edit.ts');

test('editing token prices keeps the existing per_call', () => {
    const model = { name: 'video-x', input: 1, output: 2, cache_read: 0.5, cache_write: 0.7, per_call: 0.25 };
    const values = { ...toPriceEditValues(model), input: '3' };
    assert.deepEqual(buildModelUpdatePayload(model.name, values), {
        name: 'video-x', input: 3, output: 2, cache_read: 0.5, cache_write: 0.7, per_call: 0.25,
    });
});

test('per_call is always sent, can be edited and cleared', () => {
    const model = { name: 'm', input: 0, output: 0, cache_read: 0, cache_write: 0 };
    const values = toPriceEditValues(model);
    assert.equal(values.per_call, '0');
    assert.equal(buildModelUpdatePayload('m', values).per_call, 0);
    assert.equal(buildModelUpdatePayload('m', { ...values, per_call: '1.5' }).per_call, 1.5);
    assert.equal(buildModelUpdatePayload('m', { ...values, per_call: '' }).per_call, 0);
});
