const assert = require('node:assert/strict');
const { test } = require('node:test');
const { readFileSync } = require('node:fs');
const path = require('node:path');
const { load } = require('./load-typescript.cjs');

// 仪表盘：导航里紧跟「主页」，只给管理员；三种语言的文案键一一对应。
const icons = new Proxy({}, { get: (_t, name) => ({ displayName: String(name) }) });
const config = load('src/route/config.tsx', {
    react: { lazy: () => ({}) },
    'lucide-react': icons,
    './lazy-with-preload': { lazyWithPreload: (factory) => ({ preload: factory }) },
});

test('dashboard sits right under home and is admin-only', () => {
    const ids = config.ROUTES.map((r) => r.id);
    assert.equal(ids[0], 'home');
    assert.equal(ids[1], 'dashboard');
    assert.deepEqual([...config.ADMIN_ROUTE_IDS].slice(0, 2), ['home', 'dashboard']);
    assert.equal(config.USER_ROUTE_IDS.includes('dashboard'), false);
    assert.equal(config.routesForRole('admin').some((r) => r.id === 'dashboard'), true);
    assert.equal(config.routesForRole('user').some((r) => r.id === 'dashboard'), false);
    assert.equal(config.routesForRole(undefined).some((r) => r.id === 'dashboard'), false);
});

function flatKeys(obj, prefix = '') {
    return Object.entries(obj).flatMap(([k, v]) => (v && typeof v === 'object' ? flatKeys(v, `${prefix}${k}.`) : [`${prefix}${k}`]));
}

test('nav label is 仪表盘 and dashboard strings exist in every locale', () => {
    const read = (name) => JSON.parse(readFileSync(path.join(__dirname, '..', 'public', 'locale', `${name}.json`), 'utf8'));
    const zh = read('zh_hans');
    const en = read('en');
    const hant = read('zh_hant');
    assert.equal(zh.navbar.dashboard, '仪表盘');
    assert.equal(en.navbar.dashboard, 'Dashboard');
    assert.equal(hant.navbar.dashboard, '儀表盤');
    const keys = flatKeys(zh.dashboard).sort();
    assert.ok(keys.length > 50);
    assert.deepEqual(flatKeys(en.dashboard).sort(), keys);
    assert.deepEqual(flatKeys(hant.dashboard).sort(), keys);
    // 日志清理的提示必须写明「算不进来」。
    assert.match(zh.dashboard.cleaned, /算不进来/);
    assert.match(zh.dashboard.share.tip, /日志被清理的日子算不进来/);
    assert.match(zh.dashboard.card.activeTip, /日志被清理的日子算不进来/);
});

test('every dashboard key used in the page exists in zh_hans', () => {
    const zh = JSON.parse(readFileSync(path.join(__dirname, '..', 'public', 'locale', 'zh_hans.json'), 'utf8')).dashboard;
    const have = new Set(flatKeys(zh));
    const dir = path.join(__dirname, '..', 'src', 'components', 'modules', 'dashboard');
    const files = ['index.tsx', 'sections.tsx', 'range-bar.tsx'];
    // 组件里 t 的作用域：useTranslations('dashboard' | 'dashboard.xxx')。
    for (const file of files) {
        const src = readFileSync(path.join(dir, file), 'utf8');
        // 每次调用取它前面最近一次同名 useTranslations 的作用域。
        const decls = [...src.matchAll(/const (\w+) = useTranslations\('dashboard(?:\.([\w.]+))?'\)/g)]
            .map((m) => ({ fn: m[1], scope: m[2] ? `${m[2]}.` : '', at: m.index }));
        // 顶层函数为界；把 t 当参数传进来的小函数不在这里查。
        const starts = [...src.matchAll(/^(?:export )?function /gm)].map((m) => m.index);
        let checked = 0;
        for (const m of src.matchAll(/\b(\w+)\('([\w.]+)'/g)) {
            const fnStart = starts.filter((at) => at < m.index).pop() ?? 0;
            const decl = decls.filter((d) => d.fn === m[1] && d.at < m.index && d.at > fnStart).pop();
            if (!decl) continue;
            checked++;
            assert.ok(have.has(decl.scope + m[2]), `${file}: ${m[1]}('${m[2]}') -> dashboard.${decl.scope}${m[2]} missing`);
        }
        assert.ok(checked > 5, `${file}: no t() calls found`);
    }
});
