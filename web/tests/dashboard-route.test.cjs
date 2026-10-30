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

test('log click-through uses the stats-timezone bounds while the dates are unchanged', () => {
    const { entryTimeRange } = load('src/components/modules/log/log-preset.ts', {});
    // 统计时区 Asia/Shanghai 的 2026-10-30 整天：[16:00Z 前一天, 16:00Z 当天)，日志接口两头都含。
    const intent = { startDate: '2026-10-30', endDate: '2026-10-30', startTime: 1793289600, endTime: 1793375999, userId: 3 };
    assert.deepEqual(entryTimeRange(intent, '2026-10-30', '2026-10-30'), { startTime: 1793289600, endTime: 1793375999 });
    assert.equal(entryTimeRange(intent, '2026-10-29', '2026-10-30'), null); // 改了日期 → 交回按天换算
    assert.equal(entryTimeRange(intent, '2026-10-30', '2026-10-31'), null);
    assert.equal(entryTimeRange({ ...intent, startTime: undefined }, '2026-10-30', '2026-10-30'), null);
    assert.equal(entryTimeRange({ ...intent, endTime: 1 }, '2026-10-30', '2026-10-30'), null);
    assert.equal(entryTimeRange(null, '2026-10-30', '2026-10-30'), null);
});

test('log page and dashboard are wired to pass and use those bounds', () => {
    const read = (p) => readFileSync(path.join(__dirname, '..', p), 'utf8');
    const log = read('src/components/modules/log/index.tsx');
    assert.match(log, /entryTimeRange\(entryPreset, startDate, endDate\) \?\? resolveLogTimeRange\(startDate, endDate\)/);
    const dash = read('src/components/modules/dashboard/index.tsx');
    assert.match(dash, /startTime: userRank\.data\.start, endTime: userRank\.data\.end - 1/);
});

test('one log preset store serves both the users page and the dashboard; user focus opens the drawer', () => {
    const { useLogPresetStore } = load('src/components/modules/log/log-preset.ts', {});
    const store = useLogPresetStore.getState();
    store.setUserID(7); // 用户页「看他的日志」
    assert.deepEqual(useLogPresetStore.getState().preset, { userId: 7 });
    store.setPreset({ userId: 3, startDate: '2026-10-24', endDate: '2026-10-30', startTime: 1, endTime: 2 }); // 仪表盘
    assert.equal(useLogPresetStore.getState().preset.startDate, '2026-10-24');
    store.clear();
    assert.equal(useLogPresetStore.getState().preset, null);

    const read = (p) => readFileSync(path.join(__dirname, '..', p), 'utf8');
    assert.equal(require('node:fs').existsSync(path.join(__dirname, '..', 'src/stores/nav-intent.ts')), false);
    const dash = read('src/components/modules/dashboard/index.tsx');
    assert.match(dash, /useLogPresetStore\(\(s\) => s\.setPreset\)/);
    assert.match(dash, /useUserPresetStore\(\(s\) => s\.setFocusUserID\)/);
    const users = read('src/components/modules/user/index.tsx');
    assert.match(users, /const focus = useUserPresetStore\.getState\(\)\.focusUserID;\n\s*return focus \? \{ open: true, userID: focus, tab: 'profile' \}/);
    assert.match(users, /useLogPresetStore\.getState\(\)\.setUserID\(u\.id\)/);
});
