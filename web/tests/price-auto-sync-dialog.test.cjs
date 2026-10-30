// Run from web/: node --test tests/price-auto-sync-dialog.test.cjs
// The price-interval input moved from a blur-save field on the price page into the
// 自动更新 dialog, which saves on its own button. These cover that save path: only
// changed settings are written, a failed save is visible and keeps the dialog open
// with the draft, and an invalid interval never reaches the wire.
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
const text = (node) => {
    if (node == null || typeof node === 'boolean') return '';
    if (typeof node === 'string' || typeof node === 'number') return String(node);
    if (Array.isArray(node)) return node.map(text).join('');
    return text(node.props?.children);
};
const widgets = (names) => Object.fromEntries(names.split(' ').map((n) => [n, n]));

function harness(status, { failWrites = false } = {}) {
    const writes = [];
    const notes = [];
    const closes = [];
    const slots = [];
    let cursor = 0;
    const effects = [];
    const react = {
        useState(initial) {
            const i = cursor++;
            if (i >= slots.length) slots[i] = { value: typeof initial === 'function' ? initial() : initial };
            const slot = slots[i];
            return [slot.value, (v) => { slot.value = typeof v === 'function' ? v(slot.value) : v; }];
        },
        useEffect(fn, deps) {
            const i = cursor++;
            if (!slots[i] || deps.some((d, j) => !Object.is(d, slots[i].deps[j]))) {
                slots[i] = { deps };
                effects.push(fn);
            }
        },
    };
    const { AutoSyncDialog } = load('src/components/modules/model/AutoSyncDialog.tsx', {
        react,
        '@tanstack/react-query': { useQueryClient: () => ({ invalidateQueries: async () => {} }) },
        'next-intl': { useTranslations: () => (key) => key },
        '@/api/endpoints/setting': {
            SettingKey: { PriceAutoSyncEnabled: 'price_auto_sync_enabled', PriceSyncHoldBigChange: 'price_sync_hold_big_change', ModelInfoUpdateInterval: 'model_info_update_interval' },
            useSetSetting: () => ({
                mutateAsync: async (v) => {
                    writes.push(v);
                    if (failWrites) throw new Error('500');
                    return v;
                },
            }),
        },
        '@/components/common/Toast': { toast: { success: (m) => notes.push(['success', m]), error: (m) => notes.push(['error', m]) } },
        '@/components/ui/dialog': widgets('Dialog DialogContent DialogDescription DialogTitle'),
        '@/components/ui/button': widgets('Button'),
        '@/components/ui/input': widgets('Input'),
        '@/components/ui/switch': widgets('Switch'),
        './price-sync-ui': { Chip: 'Chip', Hint: 'Hint', useDayTime: () => () => '' },
        '@/lib/utils': { cn: (...a) => a.filter(Boolean).join(' ') },
        'lucide-react': new Proxy({}, { get: (_, k) => k }),
    });
    let tree;
    const props = { open: true, onOpenChange: (v) => closes.push(v), status };
    const h = {
        writes, notes, closes,
        async render() {
            cursor = 0; effects.length = 0;
            tree = AutoSyncDialog(props);
            for (const fn of effects.splice(0)) fn();
            await Promise.resolve(); await Promise.resolve();
            cursor = 0; effects.length = 0;
            tree = AutoSyncDialog(props);
            return tree;
        },
        switches: () => find(tree, (n) => n.type === 'Switch'),
        input: () => find(tree, (n) => n.type === 'Input')[0],
        button: (label) => find(tree, (n) => n.type === 'Button' && text(n).includes(label))[0],
    };
    return h;
}

const status = { auto_enabled: true, hold_big_change: true, interval_hours: 24, last_fetch_at: '', runs: [], held_pending: [] };
const flush = () => new Promise((r) => setTimeout(r, 0));

test('loads the stored values; the manual lock is always on and disabled', async () => {
    const h = harness(status);
    await h.render();
    const [enabled, locked, hold] = h.switches();
    assert.equal(enabled.props.checked, true);
    assert.equal(locked.props.checked, true);
    assert.equal(locked.props.disabled, true);
    assert.equal(hold.props.checked, true);
    assert.equal(String(h.input().props.value), '24');
});

test('save writes only the settings that changed, then closes', async () => {
    const h = harness(status);
    await h.render();
    h.switches()[2].props.onCheckedChange(false);
    h.input().props.onChange({ target: { value: '12' } });
    await h.render();
    h.button('save').props.onClick();
    await flush();
    assert.deepEqual(h.writes, [
        { key: 'price_sync_hold_big_change', value: 'false' },
        { key: 'model_info_update_interval', value: '12' },
    ]);
    assert.deepEqual(h.closes, [false]);
    assert.deepEqual(h.notes, [['success', 'saved']]);
});

test('a failed save toasts once, keeps the dialog open and the draft', async () => {
    const h = harness(status, { failWrites: true });
    await h.render();
    h.switches()[0].props.onCheckedChange(false);
    await h.render();
    h.button('save').props.onClick();
    await flush();
    assert.equal(h.notes.filter(([k]) => k === 'error').length, 1);
    assert.ok(!h.notes.some(([k]) => k === 'success'));
    assert.deepEqual(h.closes, []);
    await h.render();
    assert.equal(h.switches()[0].props.checked, false, 'draft kept');
});

test('an invalid interval never reaches the wire', async () => {
    const h = harness(status);
    await h.render();
    h.input().props.onChange({ target: { value: '0' } });
    await h.render();
    h.button('save').props.onClick();
    await flush();
    assert.deepEqual(h.writes, []);
    assert.deepEqual(h.notes, [['error', 'intervalInvalid']]);
});
