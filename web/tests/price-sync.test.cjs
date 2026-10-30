// Run from web/: node --test tests/price-sync.test.cjs
const assert = require('node:assert/strict');
const { test } = require('node:test');
const { load } = require('./load-typescript.cjs');

const ps = load('src/components/modules/model/price-sync.ts');

const p = (input, output, cache_read = 0, cache_write = 0) => ({ input, output, cache_read, cache_write });
const item = (name, kind, extra = {}) => ({
    name, kind, source: kind === 'manual' ? 'manual' : 'library', exists: true,
    current: p(1, 1), next: p(1.2, 1.2), big: false, has_per_call: false, ...extra,
});

test('new and small changes are ticked; manual and big changes are not', () => {
    const items = [
        item('a', 'new'), item('b', 'changed'), item('c', 'changed', { big: true }),
        item('d', 'manual'), item('e', 'manual', { big: true }), item('f', 'same'),
    ];
    const sel = ps.defaultSelected(items);
    assert.deepEqual([...sel].sort(), ['a', 'b']);
    assert.deepEqual(ps.unselectedSummary(items, sel), { manual: 2, big: 1 });
    assert.deepEqual(ps.tabCounts(items), { pending: 5, new: 1, changed: 2, manual: 2, same: 1 });
    assert.deepEqual(ps.itemsForTab(items, 'pending', '').map((i) => i.name), ['a', 'b', 'c', 'd', 'e']);
    assert.deepEqual(ps.itemsForTab(items, 'changed', 'C').map((i) => i.name), ['c']);
});

test('pending tab lists ticked rows first, then big changes, then manual rows', () => {
    const items = [item('m', 'manual'), item('big', 'changed', { big: true }), item('ch', 'changed'), item('n', 'new'), item('n2', 'new')];
    assert.deepEqual(ps.itemsForTab(items, 'pending', '').map((i) => i.name), ['n', 'n2', 'ch', 'big', 'm']);
});

test('apply body carries what the preview showed, and never same rows', () => {
    const items = [item('a', 'new', { exists: false, source: '' }), item('f', 'same')];
    const body = ps.toApplyChanges(items, new Set(['a', 'f']));
    assert.deepEqual(body, [{ name: 'a', source: '', exists: false, current: p(1, 1), next: p(1.2, 1.2) }]);
});

test('percent change and biggest ratio', () => {
    assert.equal(ps.percentChange(3, 4), 33);
    assert.equal(ps.percentChange(0.14, 0.07), -50);
    assert.equal(ps.percentChange(0, 5), null);
    assert.equal(ps.biggestRatio(p(10, 50, 1, 12.5), p(25, 125, 2.5, 31.25)), 1.5);
    assert.equal(ps.biggestRatio(p(0, 0), p(5, 5)), 0);
});

test('display source matches billing', () => {
    assert.equal(ps.displaySource({ ...p(1, 2), source: 'manual' }), 'manual');
    assert.equal(ps.displaySource({ ...p(0, 0), source: 'library' }), 'none');
    assert.equal(ps.displaySource({ ...p(0, 0), source: 'library', per_call: 0.4 }), 'library');
    const placeholder = { ...p(0, 0), source: 'library', library: p(5, 25) };
    assert.equal(ps.displaySource(placeholder), 'library');
    assert.deepEqual(ps.libraryPlaceholder(placeholder), p(5, 25));
    assert.equal(ps.libraryPlaceholder({ ...p(0, 0), per_call: 1, library: p(5, 25) }), null);
    assert.deepEqual(ps.sourceCounts([placeholder, { ...p(1, 1), source: 'manual' }, p(0, 0)]), { library: 1, manual: 1, none: 1 });
});

test('relative day labels and zero time', () => {
    const now = new Date(2026, 9, 30, 10, 0);
    assert.equal(ps.relativeDay(new Date(2026, 9, 30, 9, 12), now), 'today');
    assert.equal(ps.relativeDay(new Date(2026, 9, 29, 23, 0), now), 'yesterday');
    assert.equal(ps.relativeDay(new Date(2026, 9, 31, 9, 12), now), 'tomorrow');
    assert.equal(ps.relativeDay(new Date(2026, 9, 28, 9, 12), now), null);
    assert.equal(ps.formatClock(new Date(2026, 9, 30, 9, 5)), '09:05');
    assert.equal(ps.parseServerTime('0001-01-01T00:00:00Z'), null);
});
