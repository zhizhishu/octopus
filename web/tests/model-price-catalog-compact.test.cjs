// Run from web/: node --test tests/model-price-catalog-compact.test.cjs
// Covers the single-row compact price catalog (option A): collapsed by default
// with a proper ARIA toggle, an always-visible update action and expanded auto
// settings opening the right dialogs, the held banner while collapsed,
// and unknown status/data never faked as enabled or as zero counts.
const assert = require('node:assert/strict');
const { test } = require('node:test');
const { readFileSync } = require('node:fs');
const path = require('node:path');
const { load } = require('./load-typescript.cjs');

function find(node, pred, out = []) {
    if (Array.isArray(node)) { node.forEach((c) => find(c, pred, out)); return out; }
    if (!node || typeof node !== 'object') return out;
    if (pred(node)) out.push(node);
    find(node.props?.children, pred, out);
    return out;
}
const text = (node) => {
    if (node == null || typeof node === 'boolean') return '';
    if (typeof node === 'string' || typeof node === 'number') return String(node);
    if (Array.isArray(node)) return node.map(text).join('');
    return text(node.props?.children);
};

const PriceSyncDialogMock = (props) => ({ type: 'PriceSyncDialog', props });
const AutoSyncDialogMock = (props) => ({ type: 'AutoSyncDialog', props });

const row = (source) => ({ input: 1, output: 1, source, exists: true });
const models = [row('library'), row('manual'), { input: 0, output: 0 }];
const status = { auto_enabled: true, interval_hours: 24, last_fetch_at: '2026-01-01T00:00:00Z', next_run_at: null, runs: [], held_pending: [] };

function harness({ models: m, status: s } = {}) {
    const slots = [];
    let cursor = 0;
    const react = {
        useState(initial) {
            const i = cursor++;
            if (i >= slots.length) slots[i] = { value: typeof initial === 'function' ? initial() : initial };
            const slot = slots[i];
            return [slot.value, (v) => { slot.value = typeof v === 'function' ? v(slot.value) : v; }];
        },
        useMemo: (fn) => fn(),
        useId: () => 'price-details',
    };
    const { ModelPriceCatalog } = load('src/components/modules/model/PriceCatalog.tsx', {
        react,
        'next-intl': { useTranslations: () => (key) => key },
        '@/api/endpoints/model': {
            useModelList: () => ({ data: m }),
            usePriceSyncStatus: () => ({ data: s }),
            useLastUpdateTime: () => ({ data: null }),
        },
        '@/components/ui/button': { Button: 'Button' },
        './price-sync-ui': { Chip: 'Chip', Hint: 'Hint', useDayTime: () => () => 'TIME' },
        './PriceSyncDialog': { PriceSyncDialog: PriceSyncDialogMock },
        './AutoSyncDialog': { AutoSyncDialog: AutoSyncDialogMock, RunSummary: 'RunSummary' },
        '@/lib/utils': { cn: (...a) => a.filter(Boolean).join(' ') },
        'lucide-react': new Proxy({}, { get: (_, k) => k }),
    });
    let tree;
    const h = {
        render() { cursor = 0; tree = ModelPriceCatalog({}); return tree; },
        tree: () => tree,
        details: () => find(tree, (n) => n.props?.id === 'price-details')[0],
        toggle: () => find(tree, (n) => n.type === 'Button' && n.props['aria-controls'] === 'price-details')[0],
        updateButton: () => find(tree, (n) => n.type === 'Button' && text(n) === 'updateButton')[0],
        autoButton: () => find(h.details(), (n) => n.type === 'Button' && text(n) === 'autoButton')[0],
        bannerAction: () => find(tree, (n) => n.type === 'button' && text(n) === 'bannerAction')[0],
        dialog: (mock) => find(tree, (n) => n.type === mock)[0],
        chips: () => find(tree, (n) => n.type === 'Chip'),
    };
    return h;
}

test('collapsed by default: details hidden, no duplicate description or sync hint', () => {
    const h = harness({ models, status });
    h.render();
    assert.equal(h.toggle().props['aria-expanded'], false);
    assert.equal(h.details().props.hidden, true);
    const all = text(h.tree());
    assert.ok(!all.includes('description'), 'long description stays out of the collapsed row');
    assert.ok(!all.includes('syncHint'), 'sync hint stays out of the collapsed row');
    assert.ok(all.includes('compactTitle'));
    assert.ok(all.includes('banner') === false, 'no held banner without held models');
});

test('toggle flips aria-expanded and shows the details panel', () => {
    const h = harness({ models, status });
    h.render();
    h.toggle().props.onClick();
    h.render();
    assert.equal(h.toggle().props['aria-expanded'], true);
    assert.equal(h.toggle().props['aria-label'], 'collapse');
    assert.equal(h.details().props.hidden, false);
    const detailText = text(h.details());
    assert.ok(detailText.includes('lastUpdate'), 'short status in details');
    assert.ok(detailText.includes('autoTile'), 'auto setting in details');
    h.toggle().props.onClick();
    h.render();
    assert.equal(h.toggle().props['aria-expanded'], false);
    assert.equal(h.details().props.hidden, true);
});

test('update button opens the pending preview', () => {
    const h = harness({ models, status });
    h.render();
    assert.equal(h.dialog(PriceSyncDialogMock).props.open, false);
    h.updateButton().props.onClick();
    h.render();
    const dlg = h.dialog(PriceSyncDialogMock);
    assert.equal(dlg.props.open, true);
    assert.equal(dlg.props.initialTab, 'pending');
});

test('held banner is visible while collapsed and opens the changed preview', () => {
    const h = harness({ models, status: { ...status, held_pending: ['m1'] } });
    h.render();
    assert.equal(h.toggle().props['aria-expanded'], false, 'still collapsed');
    const all = text(h.tree());
    assert.ok(all.includes('banner'), 'held warning visible even when collapsed');
    h.bannerAction().props.onClick();
    h.render();
    const dlg = h.dialog(PriceSyncDialogMock);
    assert.equal(dlg.props.open, true);
    assert.equal(dlg.props.initialTab, 'changed');
});

test('expanded auto settings button opens the auto dialog with the live status', () => {
    const h = harness({ models, status });
    h.render();
    assert.equal(h.details().props.hidden, true);
    assert.equal(h.dialog(AutoSyncDialogMock).props.open, false);
    h.toggle().props.onClick();
    h.render();
    assert.equal(h.details().props.hidden, false);
    h.autoButton().props.onClick();
    h.render();
    const dlg = h.dialog(AutoSyncDialogMock);
    assert.equal(dlg.props.open, true);
    assert.equal(dlg.props.status, status);
});

test('compact labels exist in all three locales and retain the old update label', () => {
    for (const locale of ['en', 'zh_hans', 'zh_hant']) {
        const messages = JSON.parse(readFileSync(path.join(__dirname, '../public/locale', `${locale}.json`), 'utf8'));
        for (const key of ['updateButton', 'compactTitle', 'expand', 'collapse', 'unknown', 'syncButton']) {
            assert.equal(typeof messages.model.priceSync[key], 'string', `${locale}: ${key}`);
            assert.ok(messages.model.priceSync[key].trim(), `${locale}: ${key} is not empty`);
        }
    }
});

test('undefined status and models never fake enabled state or zero counts', () => {
    const h = harness({});
    h.render();
    const all = text(h.tree());
    assert.ok(all.includes('unknown'), 'auto state reads as unknown');
    assert.ok(!all.includes('autoShort'), 'no fake interval');
    assert.ok(!all.includes('autoOn'), 'no fake enabled state');
    assert.ok(!all.includes('nextRun'), 'no fake next run');
    assert.equal(h.chips().length, 0, 'no fake zero chips while models are loading');
});
