// Run from web/: node --test tests/model-editor-dialog.test.cjs
// The model edit dialog moved from a card-rect portal to a Radix Dialog
// (@/components/ui/dialog). These cover the business paths that must survive
// the refactor: payload keeps per_call, a failed save stays open with the draft,
// restore closes on success, cancel writes nothing, and a pending mutation
// blocks every dismiss channel instead of losing the draft.
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

const model = {
    name: 'gpt-x',
    source: 'manual',
    input: 1,
    output: 2,
    cache_read: 0.5,
    cache_write: 0.25,
    per_call: 0,
    library: { input: 3, output: 4, cache_read: 0.1, cache_write: 0.2 },
};

function harness({ pendingSave = false, pendingRestore = false } = {}) {
    const notes = [];
    const writes = [];
    const restores = [];
    const deletes = [];
    const h = { notes, writes, restores };
    const slots = [];
    let cursor = 0;
    const react = {
        useState(initial) {
            const i = cursor++;
            if (i >= slots.length) slots[i] = { value: typeof initial === 'function' ? initial() : initial };
            const slot = slots[i];
            return [slot.value, (v) => { slot.value = typeof v === 'function' ? v(slot.value) : v; }];
        },
        useMemo(fn) { cursor++; return fn(); },
        useCallback(fn) { cursor++; return fn; },
        useId() { cursor++; return ':r0:'; },
        memo(fn) { return fn; },
    };
    const { ModelItem } = load('src/components/modules/model/Item.tsx', {
        react,
        'react/jsx-runtime': {
            jsx: (type, props) => typeof type === 'function' ? type(props) : ({ type, props }),
            jsxs: (type, props) => typeof type === 'function' ? type(props) : ({ type, props }),
        },
        '@/components/ui/input': { Input: 'Input' },
        'motion/react': new Proxy({}, { get: (_, k) => (props) => ({ type: `motion.${String(k)}`, props }) }),
        'next-intl': { useTranslations: () => (key) => key },
        '@/api/endpoints/model': {
            useUpdateModel: () => ({
                mutate: (payload, opts) => { writes.push(payload); h.saveOpts = opts; },
                isPending: pendingSave,
            }),
            useDeleteModel: () => ({ mutate: (name) => deletes.push(name), isPending: false }),
            useRestoreModelPrice: () => ({
                mutate: (name, opts) => { restores.push(name); h.restoreOpts = opts; },
                isPending: pendingRestore,
            }),
        },
        '@/lib/model-icons': { getModelIcon: () => ({ Avatar: 'ModelAvatar', color: '#abc123' }) },
        '@/components/common/Toast': {
            toast: {
                success: (m) => notes.push(['success', m]),
                error: (m) => notes.push(['error', m]),
            },
        },
        '@/components/animate-ui/components/animate/tooltip': {
            Tooltip: 'Tooltip', TooltipContent: 'TooltipContent', TooltipTrigger: 'TooltipTrigger',
        },
        '@/components/common/SafeText': (props) => ({ type: 'SafeText', props }),
        '@/components/ui/dialog': {
            Dialog: 'Dialog', DialogTrigger: 'DialogTrigger', DialogContent: 'DialogContent',
            DialogTitle: 'DialogTitle', DialogDescription: 'DialogDescription',
        },
        './price-sync': { displaySource: () => model.source, isZeroPrice: () => false, libraryPlaceholder: () => null },
        './price-sync-ui': { Hint: 'Hint' },
        '@/lib/utils': { cn: (...a) => a.filter(Boolean).join(' ') },
        'lucide-react': new Proxy({}, { get: (_, k) => String(k) }),
    });
    let tree;
    h.render = () => { cursor = 0; tree = ModelItem({ model, layout: 'grid' }); return tree; };
    h.dialog = () => find(tree, (n) => n.type === 'Dialog')[0];
    h.editTriggerButton = () => find(tree, (n) => n.type === 'DialogTrigger')[0]?.props.children;
    h.openEdit = () => {
        h.render();
        h.editTriggerButton().props.onClick();
        h.dialog().props.onOpenChange(true);
        h.render();
    };
    h.content = () => find(tree, (n) => n.type === 'DialogContent')[0];
    h.input = (i) => find(tree, (n) => n.type === 'Input')[i];
    h.button = (label) => find(tree, (n) => n.type === 'button' && text(n).includes(label))[0];
    h.title = () => find(tree, (n) => n.type === 'DialogTitle')[0];
    h.description = () => find(tree, (n) => n.type === 'DialogDescription')[0];
    return h;
}

test('edit button is a DialogTrigger inside the same Dialog Root for focus return', () => {
    const h = harness();
    h.render();
    const trigger = find(h.dialog().props.children, (n) => n.type === 'DialogTrigger')[0];
    assert.equal(trigger.props.asChild, true);
    assert.equal(trigger.props.children.type, 'button');
    h.openEdit();
    const content = h.dialog().props.children;
    assert.equal(find(content, (n) => n.type === 'DialogContent').length, 1, 'the trigger and content share one Radix context');
});

test('dialog renders accessible title (model name) and description, no built-in close button', () => {
    const h = harness();
    h.openEdit();
    assert.equal(text(h.title()), model.name);
    assert.equal(text(h.description()), 'manualHint');
    assert.equal(h.content().props.showCloseButton, false);
    assert.equal(typeof h.content().props.onEscapeKeyDown, 'function');
    assert.equal(typeof h.content().props.onInteractOutside, 'function');
});

test('save writes the payload with per_call', () => {
    const h = harness();
    h.openEdit();
    h.input(0).props.onChange({ target: { value: '9' } });
    h.render();
    h.input(4).props.onChange({ target: { value: '7' } });
    h.render();
    h.button('save').props.onClick();
    assert.deepEqual(h.writes, [{
        name: model.name,
        input: 9,
        output: 2,
        cache_read: 0.5,
        cache_write: 0.25,
        per_call: 7,
    }]);
});

test('a failed save toasts, stays open and keeps the draft', () => {
    const h = harness();
    h.openEdit();
    h.input(0).props.onChange({ target: { value: '9' } });
    h.render();
    h.button('save').props.onClick();
    h.saveOpts.onError({ message: '500' });
    h.render();
    assert.ok(h.notes.some(([k, m]) => k === 'error' && m === 'toast.updateFailed'));
    assert.ok(!h.notes.some(([k]) => k === 'success'));
    assert.ok(h.content(), 'dialog stays open after a failed save');
    assert.equal(h.input(0).props.value, '9', 'draft kept');
});

test('restore hits the endpoint and closes the dialog on success', () => {
    const h = harness();
    h.openEdit();
    h.button('restore').props.onClick();
    assert.deepEqual(h.restores, [model.name]);
    h.restoreOpts.onSuccess();
    h.render();
    assert.ok(h.notes.some(([k, m]) => k === 'success' && m === 'overlay.toastRestored'));
    assert.ok(!h.content(), 'dialog closed after a successful restore');
});

test('cancel closes without writing anything', () => {
    const h = harness();
    h.openEdit();
    h.input(0).props.onChange({ target: { value: '42' } });
    h.render();
    h.button('cancel').props.onClick();
    h.render();
    assert.deepEqual(h.writes, []);
    assert.deepEqual(h.restores, []);
    assert.ok(!h.content(), 'dialog closed by cancel');
});

test('while pending, every dismiss channel is blocked and the draft survives', () => {
    const h = harness({ pendingSave: true });
    h.openEdit();
    h.input(0).props.onChange({ target: { value: '9' } });
    h.render();

    h.dialog().props.onOpenChange(false);
    h.render();
    assert.ok(h.content(), 'onOpenChange(false) ignored while pending');

    const ev = { preventDefault() { ev.blocked = true; } };
    h.content().props.onEscapeKeyDown(ev);
    assert.equal(ev.blocked, true, 'ESC prevented while pending');

    const ev2 = { preventDefault() { ev2.blocked = true; } };
    h.content().props.onInteractOutside(ev2);
    assert.equal(ev2.blocked, true, 'outside pointer prevented while pending');

    assert.equal(h.button('cancel').props.disabled, true);
    assert.equal(h.button('save').props.disabled, true);
    h.render();
    assert.equal(h.input(0).props.value, '9', 'draft kept while pending');
});
