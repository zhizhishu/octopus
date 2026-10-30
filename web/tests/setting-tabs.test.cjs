// Run from web/: node --test tests/setting-tabs.test.cjs
// Settings page top tabs (setting/index.tsx + setting/tabs.ts):
//   - hash parsing / writing / clearing
//   - clicking a tab switches the rendered cards and writes the hash
//   - an incoming hashchange switches the tab; leaving the page clears the hash
//   - every tab renders exactly the number of cards its badge claims
//   - System.tsx / Log.tsx section split renders the right blocks
/* eslint-disable @typescript-eslint/no-require-imports -- CommonJS test harness, same as the other tests/*.test.cjs */
const assert = require('node:assert/strict');
const { test } = require('node:test');
const { readFileSync } = require('node:fs');
const path = require('node:path');
const { load } = require('./load-typescript.cjs');

const widgets = (names) => Object.fromEntries(names.split(' ').map((name) => [name, name]));
const lucide = new Proxy({}, { get: (_, key) => key });

function walk(node, visit) {
    if (Array.isArray(node)) return node.forEach((child) => walk(child, visit));
    if (!node || typeof node !== 'object') return;
    visit(node);
    walk(node.props?.children, visit);
}
function collect(tree, predicate) {
    const found = [];
    walk(tree, (n) => { if (predicate(n)) found.push(n); });
    return found;
}

const tabs = load('src/components/modules/setting/tabs.ts');

function fakeBrowser(initialHash = '') {
    const listeners = {};
    const replaced = [];
    const location = { pathname: '/', search: '?x=1', hash: initialHash };
    const history = {
        replaceState(_data, _unused, url) {
            replaced.push(url);
            const i = url.indexOf('#');
            location.hash = i === -1 ? '' : url.slice(i);
        },
    };
    return {
        location, history, replaced, listeners,
        addEventListener(type, fn) { listeners[type] = fn; },
        removeEventListener(type, fn) { if (listeners[type] === fn) delete listeners[type]; },
    };
}

test('tabs: hash parsing accepts the six ids and falls back to general', () => {
    assert.deepEqual([...tabs.SETTING_TAB_IDS], ['general', 'relay', 'identity', 'users', 'system', 'logs']);
    assert.equal(tabs.parseSettingTabHash('#relay'), 'relay');
    assert.equal(tabs.parseSettingTabHash('relay'), 'relay');
    assert.equal(tabs.parseSettingTabHash('#/logs'), 'logs');
    assert.equal(tabs.parseSettingTabHash('#LOGS'), 'logs');
    for (const junk of ['', '#', '#nope', undefined, null, '#general-x']) {
        assert.equal(tabs.parseSettingTabHash(junk), 'general', `${String(junk)} falls back`);
    }
    assert.equal(tabs.settingTabHash('identity'), '#identity');
});

test('tabs: writing the hash uses replaceState (no history spam) and keeps path + query', () => {
    const b = fakeBrowser('');
    tabs.writeSettingTabHash(b.location, b.history, 'users');
    assert.deepEqual(b.replaced, ['/?x=1#users']);
    tabs.writeSettingTabHash(b.location, b.history, 'users');
    assert.equal(b.replaced.length, 1, 'same tab again is a no-op');
});

test('tabs: clearing only removes a settings-tab hash', () => {
    const b = fakeBrowser('#system');
    tabs.clearSettingTabHash(b.location, b.history);
    assert.deepEqual(b.replaced, ['/?x=1']);
    const other = fakeBrowser('#something-else');
    tabs.clearSettingTabHash(other.location, other.history);
    assert.equal(other.replaced.length, 0, 'foreign hashes are left alone');
});

// ---- Setting page (index.tsx) -------------------------------------------------

const CARD_TYPES = new Set([
    'SettingInfo', 'SettingAccount', 'SettingAppearance', 'SettingAccessToken', 'SettingCircuitBreaker',
    'SettingLog', 'SettingSystem', 'SettingFingerprintProfile', 'SettingLLMSync', 'SettingBackup',
]);

function makePage(initialHash) {
    const browser = fakeBrowser(initialHash);
    const slots = [];
    const effects = [];
    let cursor = 0;
    const react = {
        useState(initial) {
            const i = cursor++;
            if (i >= slots.length) slots[i] = { value: typeof initial === 'function' ? initial() : initial };
            const slot = slots[i];
            return [slot.value, (v) => { slot.value = typeof v === 'function' ? v(slot.value) : v; }];
        },
        useEffect(fn, deps) {
            const i = cursor++;
            if (!slots[i]) { slots[i] = { deps }; effects.push(fn); }
        },
    };
    const previousWindow = global.window;
    global.window = browser;
    const mod = load('src/components/modules/setting/index.tsx', {
        react,
        'next-intl': { useTranslations: () => (key) => key },
        'lucide-react': lucide,
        '@/components/common/PageWrapper': widgets('PageWrapper'),
        '@/components/common/PillTabButton': widgets('PillTabButton'),
        './Appearance': widgets('SettingAppearance'),
        './System': widgets('SettingSystem'),
        './FingerprintProfile': widgets('SettingFingerprintProfile'),
        './Account': widgets('SettingAccount'),
        './AccessToken': widgets('SettingAccessToken'),
        './Info': widgets('SettingInfo'),
        './LLMSync': widgets('SettingLLMSync'),
        './Log': widgets('SettingLog'),
        './Backup': widgets('SettingBackup'),
        './CircuitBreaker': widgets('SettingCircuitBreaker'),
    });
    let tree;
    const cleanups = [];
    const page = {
        browser,
        render() {
            cursor = 0;
            tree = mod.Setting();
            for (const fn of effects.splice(0)) { const c = fn(); if (c) cleanups.push(c); }
            return tree;
        },
        unmount() { cleanups.splice(0).forEach((c) => c()); global.window = previousWindow; },
        get tree() { return tree; },
        tabButtons() { return collect(tree, (n) => n.type === 'PillTabButton'); },
        activeTab() { return page.tabButtons().find((n) => n.props.active)?.key; },
        cards() { return collect(tree, (n) => CARD_TYPES.has(n.type)); },
        description() { return collect(tree, (n) => n.type === 'p' && typeof n.props.children === 'string' && n.props.children.endsWith('.description'))[0]?.props.children; },
    };
    return page;
}

test('page: opens on the tab named in the URL hash', () => {
    const page = makePage('#identity');
    page.render();
    assert.equal(page.activeTab(), 'identity');
    assert.equal(page.description(), 'tabs.identity.description');
    const types = page.cards().map((n) => n.props.section ? `${n.type}:${n.props.section}` : n.type);
    assert.deepEqual(types, ['SettingSystem:claude', 'SettingSystem:codex', 'SettingSystem:claudeBetaStrip', 'SettingFingerprintProfile']);
    page.unmount();
});

test('page: without a hash it opens on 常规 and renders six tab buttons with badges', () => {
    const page = makePage('');
    page.render();
    assert.equal(page.activeTab(), 'general');
    const buttons = page.tabButtons();
    assert.deepEqual(buttons.map((b) => b.key), ['general', 'relay', 'identity', 'users', 'system', 'logs']);
    for (const b of buttons) {
        assert.equal(b.props.label, `tabs.${b.key}.label`);
        assert.equal(b.props.count, tabs.SETTING_TAB_CARD_COUNTS[b.key]);
        assert.equal(b.props.role, 'tab');
    }
    page.unmount();
});

test('page: clicking a tab switches the cards and writes the hash', () => {
    const page = makePage('');
    page.render();
    page.tabButtons().find((b) => b.key === 'relay').props.onClick();
    page.render();
    assert.equal(page.activeTab(), 'relay');
    assert.equal(page.browser.location.hash, '#relay');
    const types = page.cards().map((n) => n.props.section ? `${n.type}:${n.props.section}` : n.type);
    assert.deepEqual(types, ['SettingCircuitBreaker', 'SettingLog:rescue', 'SettingSystem:stream', 'SettingSystem:upstreamError']);
    assert.ok(collect(page.tree, (n) => n.type?.name === 'SettingRouteModeMoved').length === 1, 'route-mode hint card lives in 转发与容错');

    page.tabButtons().find((b) => b.key === 'logs').props.onClick();
    page.render();
    assert.equal(page.browser.location.hash, '#logs');
    assert.deepEqual(page.cards().map((n) => n.props.section ? `${n.type}:${n.props.section}` : n.type), ['SettingLog:log', 'SettingBackup']);
    page.unmount();
});

test('page: an external hashchange switches the tab; leaving the page clears the hash', () => {
    const page = makePage('#general');
    page.render();
    assert.equal(typeof page.browser.listeners.hashchange, 'function', 'listens for hashchange');
    page.browser.location.hash = '#users';
    page.browser.listeners.hashchange();
    page.render();
    assert.equal(page.activeTab(), 'users');
    page.unmount();
    assert.equal(page.browser.listeners.hashchange, undefined, 'listener removed on unmount');
    assert.equal(page.browser.location.hash, '', 'settings hash cleared when the page goes away');
});

test('page: every tab renders exactly as many cards as its badge says (route hint counted)', () => {
    const page = makePage('');
    for (const id of tabs.SETTING_TAB_IDS) {
        page.render();
        page.tabButtons().find((b) => b.key === id).props.onClick();
        page.render();
        const hints = collect(page.tree, (n) => n.type?.name === 'SettingRouteModeMoved').length;
        assert.equal(page.cards().length + hints, tabs.SETTING_TAB_CARD_COUNTS[id], `${id} badge matches cards`);
    }
    page.unmount();
});

// ---- Section split (System.tsx / Log.tsx) -------------------------------------

function renderOnce(componentPath, exportName, props, extraMocks, settings = []) {
    const react = {
        useState: (initial) => [typeof initial === 'function' ? initial() : initial, () => {}],
        useRef: (initial) => ({ current: initial }),
        useMemo: (fn) => fn(),
        useEffect: () => {},
    };
    const settingModule = {
        useSettingList: () => ({ data: settings }),
        useSetSetting: () => ({ mutate() {}, mutateAsync: async () => {}, isPending: false }),
        SettingKey: load('src/api/endpoints/setting.ts', {
            '@tanstack/react-query': {}, '../client': { apiClient: {}, API_BASE_URL: '.' },
            '@/lib/logger': { logger: {} }, '@/components/common/Toast': { toast: {} }, './user': { useAuthStore: {} },
        }).SettingKey,
        SECRET_MASK: '__OCTOPUS_SECRET_KEPT__',
    };
    const Component = load(componentPath, {
        react,
        'next-intl': { useTranslations: () => (key) => key },
        '@/api/endpoints/setting': settingModule,
        '@/components/common/Toast': { toast: { success() {}, error() {} } },
        'lucide-react': lucide,
        ...extraMocks,
    })[exportName];
    return Component(props);
}

const systemMocks = {
    '@/components/ui/input': widgets('Input'),
    '@/components/ui/switch': widgets('Switch'),
    '@/components/ui/button': widgets('Button'),
    '@/components/ui/dialog': widgets('Dialog DialogContent DialogDescription DialogFooter DialogHeader DialogTitle'),
    '@/components/ui/popover': widgets('Popover PopoverContent PopoverTrigger'),
    '@/components/animate-ui/components/animate/tooltip': widgets('Tooltip TooltipContent TooltipProvider TooltipTrigger'),
};
const logMocks = {
    '@/components/ui/input': widgets('Input'),
    '@/components/ui/switch': widgets('Switch'),
    '@/components/ui/button': widgets('Button'),
    '@/components/ui/progress': widgets('Progress'),
    '@/api/endpoints/log': {
        useClearLogs: () => ({ mutate() {}, isPending: false }),
        useLogStorage: () => ({ data: undefined }),
        useLogQueueHealth: () => ({ data: undefined }),
        useLogCleanupNow: () => ({ mutate() {}, isPending: false }),
    },
};

// Translation keys used by a rendered subtree, as a cheap "what is on this card" fingerprint.
function keysIn(tree) {
    const keys = new Set();
    walk(tree, (n) => {
        for (const v of [n.props?.['aria-label'], n.props?.placeholder, n.props?.title]) if (typeof v === 'string') keys.add(v);
        const c = n.props?.children;
        for (const x of Array.isArray(c) ? c : [c]) if (typeof x === 'string') keys.add(x);
    });
    return keys;
}

test('system sections: each card carries only its own settings; header dialog rides with claude/codex', () => {
    const render = (section) => renderOnce('src/components/modules/setting/System.tsx', 'SettingSystem', { section }, systemMocks);
    const expectations = {
        network: { has: ['sections.network', 'proxyUrl.placeholder', 'trustedProxies.placeholder', 'corsAllowOrigins.label'], not: ['statsSaveInterval.placeholder', 'streamKeepalive.label'] },
        stats: { has: ['sections.stats', 'statsSaveInterval.placeholder'], not: ['proxyUrl.placeholder'] },
        claude: { has: ['sections.claude', 'anthropicAutoCacheControl.label', 'claudeCLIReasoningEffort.label'], not: ['openAIAutoPromptCacheKey.label'] },
        codex: { has: ['sections.codex', 'openAIAutoPromptCacheKey.label', 'codexFastMode.label'], not: ['anthropicAutoCacheControl.label'] },
        claudeBetaStrip: { has: ['claudeBetaStripFlags.label'], not: ['anthropicAutoCacheControl.label'] },
        stream: { has: ['sections.stream', 'streamKeepalive.label', 'firstByteKeepalive.label', 'streamDataTimeout.label', 'upstreamHeaderTimeout.label', 'responsesSessionTTL.label'], not: ['upstreamError.statusPassthrough'] },
        registration: { has: ['sections.registration', 'userRegistration.label', 'emailVerification.enabled', 'emailVerification.provider'], not: ['checkIn.enabled'] },
        checkIn: { has: ['checkIn.title', 'checkIn.enabled', 'checkIn.rewardMode', 'checkIn.amountPlaceholder', 'checkIn.minPlaceholder', 'checkIn.maxPlaceholder'], not: ['userRegistration.label'] },
        upstreamError: { has: ['upstreamError.title', 'upstreamError.statusPassthrough', 'upstreamError.publicCode', 'upstreamError.bodyMode', 'upstreamError.customPlaceholder'], not: ['corsAllowOrigins.label'] },
    };
    for (const [section, { has, not }] of Object.entries(expectations)) {
        const keys = keysIn(render(section));
        for (const k of has) assert.ok(keys.has(k), `${section}: expected ${k}`);
        for (const k of not) assert.ok(!keys.has(k), `${section}: must not contain ${k}`);
        const dialogs = collect(render(section), (n) => n.type === 'Dialog').length;
        assert.equal(dialogs, section === 'claude' || section === 'codex' ? 1 : 0, `${section}: header dialog placement`);
    }
    // No section → every block (the old "everything" render, kept for tests and standalone use).
    const all = keysIn(render(undefined));
    for (const { has } of Object.values(expectations)) for (const k of has) assert.ok(all.has(k), `all: expected ${k}`);
    assert.ok(!all.has('system'), 'the old single 系统 mega-card title is gone');
});

test('log sections: 自动救援 lives in its own card, the log card keeps the log settings', () => {
    const render = (section) => keysIn(renderOnce('src/components/modules/setting/Log.tsx', 'SettingLog', { section }, logMocks));
    const log = render('log');
    assert.ok(log.has('log.title') && log.has('log.upstreamRequestBody.label') && log.has('log.clear.label'));
    assert.ok(!log.has('log.intervention.label') && !log.has('log.noBreaker.label'), 'rescue is no longer inside 日志设置');
    const rescue = render('rescue');
    assert.ok(rescue.has('log.rescue.title') && rescue.has('log.intervention.label') && rescue.has('log.noBreaker.label'));
    assert.ok(!rescue.has('log.title') && !rescue.has('log.upstreamRequestBody.label'));
});

test('locale: tab labels/descriptions, section titles and the new switch exist in all three languages', () => {
    for (const lang of ['zh_hans', 'zh_hant', 'en']) {
        const s = JSON.parse(readFileSync(path.join(__dirname, '..', 'public', 'locale', `${lang}.json`), 'utf8')).setting;
        assert.ok(s.tabs?.ariaLabel, `${lang}: tabs.ariaLabel`);
        for (const id of tabs.SETTING_TAB_IDS) {
            assert.ok(s.tabs[id]?.label && s.tabs[id]?.description, `${lang}: tabs.${id}`);
        }
        for (const k of ['network', 'stats', 'claude', 'codex', 'stream', 'registration']) assert.ok(s.sections?.[k], `${lang}: sections.${k}`);
        assert.ok(s.log.upstreamRequestBody?.label && s.log.upstreamRequestBody?.hint, `${lang}: log.upstreamRequestBody`);
        assert.ok(s.log.rescue?.title, `${lang}: log.rescue.title`);
        assert.ok(s.checkIn.randomRange, `${lang}: checkIn.randomRange`);
    }
    const zh = JSON.parse(readFileSync(path.join(__dirname, '..', 'public', 'locale', 'zh_hans.json'), 'utf8')).setting;
    assert.equal(zh.log.upstreamRequestBody.label, '记录发给上游的实际请求');
    assert.equal(zh.log.upstreamRequestBody.hint, '排查用，请求原样保存（含密钥），开启后会增加日志存储');
});
