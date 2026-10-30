// 「压缩数据库」一行：只在 SQLite 显示、显示大小、先确认再跑、409 提示"稍后再试"。
const test = require('node:test');
const assert = require('node:assert/strict');
const { load } = require('./load-typescript.cjs');

const widgets = (names) => Object.fromEntries(names.split(' ').map((name) => [name, name]));
const lucide = new Proxy({}, { get: (_, key) => key });

function walk(node, visit) {
    if (Array.isArray(node)) return node.forEach((child) => walk(child, visit));
    if (!node || typeof node !== 'object') return;
    visit(node);
    walk(node.props?.children, visit);
}
function textOf(tree) {
    const parts = [];
    walk(tree, (n) => {
        const c = n.props?.children;
        for (const x of Array.isArray(c) ? c : [c]) if (typeof x === 'string') parts.push(x);
    });
    return parts.join('|');
}

function render(info, { confirm = true } = {}) {
    const notes = [];
    const calls = [];
    const mutation = { isPending: false, mutate: (vars, cb) => calls.push(cb) };
    const mod = load('src/components/modules/setting/DBCompact.tsx', {
        'next-intl': { useTranslations: () => (key, vars) => (vars ? `${key}:${JSON.stringify(vars)}` : key) },
        'lucide-react': lucide,
        '@/components/ui/button': widgets('Button'),
        '@/components/animate-ui/components/animate/tooltip': widgets('Tooltip TooltipContent TooltipProvider TooltipTrigger'),
        '@/api/endpoints/setting': {
            useDBCompactInfo: () => ({ data: info }),
            useDBCompactNow: () => mutation,
        },
        '@/components/common/Toast': { toast: {
            success: (m) => notes.push(['success', m]),
            error: (m) => notes.push(['error', m]),
        } },
    });
    const prevWindow = global.window;
    global.window = { confirm: () => confirm };
    const tree = mod.DBCompactRow();
    let button = null;
    walk(tree, (n) => { if (n.type === 'Button') button = n; });
    return { tree, button, notes, calls, mod, restore: () => { global.window = prevWindow; } };
}

test('hidden when the database is not SQLite', () => {
    const r = render({ dialect: 'mysql', supported: false, file_bytes: 0, free_bytes: 0, running: false, needs_full: false });
    r.restore();
    assert.equal(r.tree, null);
});

test('shows size, reclaimable amount and the pause warning', () => {
    const r = render({ dialect: 'sqlite', supported: true, file_bytes: 18 * 1024 ** 3, free_bytes: 14 * 1024 ** 3, running: false, needs_full: false });
    r.restore();
    const text = textOf(r.tree);
    assert.match(text, /log\.compact\.size 18\.0 GB/);
    assert.match(text, /log\.compact\.reclaimable/);
    assert.match(text, /log\.compact\.hint/);
    assert.ok(r.button && !r.button.props.disabled);
});

test('first (full) compaction shows the stronger warning', () => {
    const r = render({ dialect: 'sqlite', supported: true, file_bytes: 1, free_bytes: 0, running: false, needs_full: true });
    r.restore();
    assert.match(textOf(r.tree), /log\.compact\.fullWarning/);
});

test('button is disabled while another compaction/reclaim runs', () => {
    const r = render({ dialect: 'sqlite', supported: true, file_bytes: 1, free_bytes: 0, running: true, needs_full: false });
    r.restore();
    assert.equal(r.button.props.disabled, true);
    assert.equal(r.button.props.children, 'log.compact.running');
});

test('cancelled confirm does not run; 409 shows busy, success shows before/after', () => {
    const info = { dialect: 'sqlite', supported: true, file_bytes: 1, free_bytes: 0, running: false, needs_full: false };
    const cancelled = render(info, { confirm: false });
    cancelled.button.props.onClick();
    cancelled.restore();
    assert.equal(cancelled.calls.length, 0, 'declining the confirm must not compact');

    const r = render(info);
    r.button.props.onClick();
    r.restore();
    assert.equal(r.calls.length, 1);
    r.calls[0].onError({ code: 409, message: 'busy' });
    r.calls[0].onSuccess({ before_bytes: 2048, after_bytes: 1024, freed_bytes: 1024, duration_ms: 5, mode: 'full' });
    assert.deepEqual(r.notes[0], ['error', 'log.compact.busy']);
    assert.equal(r.notes[1][0], 'success');
    assert.match(r.notes[1][1], /2\.00 KB/);
    assert.match(r.notes[1][1], /1\.00 KB/);
});
