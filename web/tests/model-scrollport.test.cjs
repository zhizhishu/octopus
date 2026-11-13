// Run from web/: node --test tests/model-scrollport.test.cjs
// The model page stopped clipping its grid with rounded-t-3xl: the outer wrapper
// keeps overflow-hidden but drops the top radius, the price bar becomes shrink-0,
// and the grid scroller takes scrollClassName="rounded-none" instead. The
// VirtualizedGrid default radius and scroll behavior must stay unchanged for
// callers that do not pass scrollClassName.
const assert = require('node:assert/strict');
const { test } = require('node:test');
const { load } = require('./load-typescript.cjs');

function find(node, pred, out = []) {
    if (Array.isArray(node)) { node.forEach((c) => find(c, pred, out)); return out; }
    if (!node || typeof node !== 'object') return out;
    if (pred(node)) out.push(node);
    find(node.props?.children, pred, out);
    return out;
}

const reactHarness = (effects) => {
    const slots = [];
    let cursor = 0;
    return {
        useState(initial) {
            const i = cursor++;
            if (i >= slots.length) slots[i] = { value: typeof initial === 'function' ? initial() : initial };
            const slot = slots[i];
            return [slot.value, (v) => { slot.value = typeof v === 'function' ? v(slot.value) : v; }];
        },
        useEffect(fn) { effects.push(fn); cursor++; },
        useMemo(fn) { cursor++; return fn(); },
        useCallback(fn) { cursor++; return fn; },
        useRef(initial) { cursor++; return { current: initial }; },
    };
};

for (const itemCount of [1, 81]) {
test(`VirtualizedGrid default scroller keeps rounded-t-3xl (${itemCount} items)`, () => {
    const effects = [];
    const { VirtualizedGrid } = load('src/components/common/VirtualizedGrid.tsx', {
        react: reactHarness(effects),
        '@tanstack/react-virtual': {
            useVirtualizer: () => ({ getVirtualItems: () => [], getTotalSize: () => 0, measureElement: () => {} }),
        },
        '@/lib/utils': { cn: (...a) => a.filter(Boolean).join(' ') },
    });
    const tree = VirtualizedGrid({
        items: Array.from({ length: itemCount }, (_, i) => ({ id: String(i) })),
        columns: { default: 1 },
        estimateItemHeight: 100,
        getItemKey: (item) => item.id,
        renderItem: () => null,
    });
    const scroller = find(tree, (n) => typeof n.props?.className === 'string' && n.props.className.includes('overflow-y-auto'))[0];
    assert.ok(scroller, 'scroller div exists');
    assert.ok(scroller.props.className.includes('rounded-t-3xl'), 'default keeps the top radius');
    assert.ok(!scroller.props.className.includes('rounded-none'), 'default never rounds off');
});

test(`VirtualizedGrid swaps the scroller radius for scrollClassName (${itemCount} items)`, () => {
    const effects = [];
    const { VirtualizedGrid } = load('src/components/common/VirtualizedGrid.tsx', {
        react: reactHarness(effects),
        '@tanstack/react-virtual': {
            useVirtualizer: () => ({ getVirtualItems: () => [], getTotalSize: () => 0, measureElement: () => {} }),
        },
        '@/lib/utils': { cn: (...a) => a.filter(Boolean).join(' ') },
    });
    const tree = VirtualizedGrid({
        items: Array.from({ length: itemCount }, (_, i) => ({ id: String(i) })),
        columns: { default: 1 },
        estimateItemHeight: 100,
        getItemKey: (item) => item.id,
        renderItem: () => null,
        scrollClassName: 'rounded-none',
    });
    const scroller = find(tree, (n) => typeof n.props?.className === 'string' && n.props.className.includes('overflow-y-auto'))[0];
    assert.ok(scroller.props.className.includes('rounded-none'));
    assert.ok(!scroller.props.className.includes('rounded-t-3xl'), 'model path has no leftover top radius');
});

}

test('model page: outer keeps overflow-hidden but drops rounded-t-3xl; price bar shrink-0; list flex-1 min-h-0; scroller rounded-none', () => {
    const effects = [];
    const model = { name: 'gpt-x', source: 'manual', input: 1, output: 2, cache_read: 0.5, cache_write: 0.25, per_call: 0 };
    const { Model } = load('src/components/modules/model/index.tsx', {
        react: reactHarness(effects),
        '@/api/endpoints/model': { useModelList: () => ({ data: [model] }) },
        './Item': { ModelItem: (props) => ({ type: 'ModelItem', props }) },
        './PriceCatalog': { ModelPriceCatalog: 'ModelPriceCatalog' },
        '@/components/modules/toolbar': {
            useSearchStore: (sel) => sel({ getSearchTerm: () => '' }),
            useToolbarViewOptionsStore: (sel) => sel({ getLayout: () => 'grid', getSortOrder: () => 'asc', modelFilter: 'all' }),
        },
        '@/components/common/VirtualizedGrid': { VirtualizedGrid: 'VirtualizedGrid' },
        'next-intl': { useTranslations: () => (key) => key },
    });
    const tree = Model();
    const grid = find(tree, (n) => n.type === 'VirtualizedGrid')[0];
    assert.equal(grid.props.scrollClassName, 'rounded-none', 'model passes the square scroller override');

    const outer = find(tree, (n) => typeof n.props?.className === 'string' && n.props.className.includes('overflow-hidden'))[0];
    assert.ok(outer, 'outer wrapper with overflow-hidden exists');
    assert.ok(!outer.props.className.includes('rounded-t-3xl'), 'outer no longer clips the top radius');

    const priceBarWrapper = find(tree, (n) =>
        n.props?.children?.type === 'ModelPriceCatalog'
    )[0];
    assert.ok(priceBarWrapper, 'price bar has a dedicated wrapper');
    assert.ok(priceBarWrapper.props.className.includes('shrink-0'), 'price bar is shrink-0');

    const listWrapper = find(tree, (n) =>
        n.props?.children?.type === 'VirtualizedGrid'
    )[0];
    assert.ok(listWrapper.props.className.includes('flex-1') && listWrapper.props.className.includes('min-h-0'), 'list container is flex-1 min-h-0');
});
