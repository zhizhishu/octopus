// Run from web/: node --test tests/user-limits-semantics.test.cjs
// These execute the production TSX (src/components/modules/user/components/
// user-limits.tsx) through tests/load-typescript.cjs and assert the quota copy
// and handler behaviour that both roles must see:
//   * a cost limit is an admission threshold, not a hard cap on bills
//   * $10 limit + two $6 settlements = $12 shown unclamped
//   * accepted count includes later failures/cancellations
//   * fixed anchored 5-hour period, natural week/month in the policy timezone
//   * refunds and late settlements stay in the original window
//   * reservations / pending receipts are estimates, not a final bill
//   * 0 means unlimited, and the model list intersects API key permissions
//   * disabled and no-policy paths say what actually happens
//   * mobile touch targets and existing save validation/payload shape
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const path = require('node:path');
const { test } = require('node:test');
const { load } = require('./load-typescript.cjs');

const LOCALES = { en: 'en', 'zh-Hans': 'zh_hans', 'zh-Hant': 'zh_hant' };
const messages = Object.fromEntries(
    Object.entries(LOCALES).map(([tag, file]) => [tag, require(`../public/locale/${file}.json`)]),
);

function find(node, pred, out = []) {
    if (Array.isArray(node)) { node.forEach((child) => find(child, pred, out)); return out; }
    if (!node || typeof node !== 'object') return out;
    if (pred(node)) out.push(node);
    find(node.props?.children, pred, out);
    return out;
}
function text(node) {
    if (node == null || typeof node === 'boolean') return '';
    if (typeof node === 'string' || typeof node === 'number') return String(node);
    if (Array.isArray(node)) return node.map(text).join('');
    return text(node.props?.children);
}

// Real messages, real interpolation. A missing key fails loudly instead of
// silently rendering the key name, which is what the old stub translator did.
function translator(tag) {
    const dict = messages[tag].userLimits;
    return (key, values) => {
        const raw = dict[key];
        assert.equal(typeof raw, 'string', `${tag}: missing userLimits.${key}`);
        if (!values) return raw;
        return raw.replace(/\{(\w+)\}/g, (match, name) => (name in values ? String(values[name]) : match));
    };
}

// Deliberately not toFixed: the assertion must show the raw number that reached
// the DOM, so an accidental clamp to the limit cannot hide behind formatting.
const rawUSD = (value) => `USD(${value})`;

const policy = {
    user_id: 9, enabled: true, timezone: 'Asia/Shanghai', anchor_at: 1700000000,
    five_hour_cost: 10, weekly_cost: 0, monthly_cost: 0,
    five_hour_requests: 0, weekly_requests: 0, monthly_requests: 0,
    allowed_models: '', revision: 3, created_at: 1700000000, updated_at: 1700000000,
};

function status(over = {}) {
    return {
        policy: { ...policy },
        windows: [
            { period: 'five_hour', start_at: 1700000000, end_at: 1700018000, cost_used: 12, cost_reserved: 0, accepted: 2, failed_accepted: 1, cost_limit: 10, request_limit: 0 },
            { period: 'week', start_at: 1699977600, end_at: 1700582400, cost_used: 12, cost_reserved: 0, accepted: 2, failed_accepted: 1, cost_limit: 0, request_limit: 0 },
            { period: 'month', start_at: 1698768000, end_at: 1701446400, cost_used: 12, cost_reserved: 0, accepted: 2, failed_accepted: 1, cost_limit: 0, request_limit: 0 },
        ],
        pending_receipts: 2,
        realtime_supported: false,
        count_semantics: 'accepted_units_including_later_failures',
        cost_semantics: 'settled_plus_reserved_actual_inflight_cost_may_exceed_limit',
        ...over,
    };
}

function harness({ tag = 'en', data = status(), user = { id: 9, role: 'user' }, isAPIKeyAuth = false, save = {} } = {}) {
    const notes = [];
    const writes = [];
    const slots = [];
    let cursor = 0;
    const jsx = (type, props) => (typeof type === 'function' ? type(props) : { type, props });
    const mod = load('src/components/modules/user/components/user-limits.tsx', {
        react: {
            useState(initial) {
                const i = cursor++;
                if (i >= slots.length) slots[i] = { value: typeof initial === 'function' ? initial() : initial };
                const slot = slots[i];
                return [slot.value, (next) => { slot.value = typeof next === 'function' ? next(slot.value) : next; }];
            },
        },
        'react/jsx-runtime': { jsx, jsxs: jsx },
        'next-intl': { useTranslations: () => translator(tag), useLocale: () => tag },
        'lucide-react': new Proxy({}, { get: (_, key) => `Icon:${String(key)}` }),
        '@/api/endpoints/user-limits': {
            useUserLimits: () => ({ data, isLoading: false, isError: false }),
            useCurrentUserLimits: () => ({ data, isLoading: false, isError: false }),
            useSaveUserLimits: () => ({
                isPending: !!save.pending,
                mutateAsync: async (payload) => {
                    writes.push(payload);
                    if (save.reject) throw save.reject;
                    return save.result ?? { policy: payload, windows: data.windows };
                },
            }),
        },
        '@/api/endpoints/user': { useAuthStore: (selector) => selector({ user, isAPIKeyAuth }) },
        '@/components/ui/switch': { Switch: (props) => ({ type: 'Switch', props }) },
        '@/components/common/Toast': {
            toast: {
                success: (message, options) => notes.push(['success', message, options]),
                error: (message, options) => notes.push(['error', message, options]),
            },
        },
        '../lib/balance-preview': { formatUSD: rawUSD },
        './ui-bits': { btnPrimary: 'btn-primary', label: 'label' },
    });
    const h = { notes, writes, mod };
    // Admin editor path (exported UserLimitsPanel).
    h.admin = () => { cursor = 0; return mod.UserLimitsPanel({ userID: policy.user_id }); };
    // Ordinary user card (exported CurrentUserLimits).
    h.card = () => { cursor = 0; return mod.CurrentUserLimits(); };
    h.button = (label) => find(h.admin(), (n) => n.type === 'button' && text(n).includes(label))[0];
    // DOM order of text inputs: [timezone, five_hour_cost, five_hour_requests, ...]
    h.input = (index) => find(h.admin(), (n) => n.type === 'input')[index];
    return h;
}

test('ordinary user card states admission semantics, the $10 -> $12 example and unclamped usage', () => {
    const h = harness();
    const tree = h.card();
    const body = text(tree);
    assert.match(body, /not a hard cap/i);
    assert.match(body, /limit of \$10/);
    assert.match(body, /\$12/);
    assert.match(body, /USD\(12\) \/ USD\(10\)/, 'settled 12 must render against limit 10, never clamped');
    assert.match(body, /1 of them failed or were cancelled/, 'accepted count includes later failures');
    assert.match(body, /2 requests or batches are not settled yet/);
    assert.match(body, /not a sliding last-5-hours window/);
    assert.match(body, /Week starts Monday 00:00 and month starts on day 1, both in Asia\/Shanghai/);
    assert.match(body, /Refunds and late settlements adjust the period/);
    assert.match(body, /estimates, not a final bill/);
    assert.match(body, /A limit of 0 means unlimited/);
    assert.match(body, /Applies to every model your API key allows/);
    assert.match(body, /Realtime sessions are unavailable while this user policy is on/);
    // One pending paragraph only: the count must not be explained twice.
    assert.equal((body.match(/not settled yet/g) || []).length, 1);
});

test('admin editor shows the same disclaimer plus the admission heading', () => {
    const h = harness({ user: { id: 1, role: 'admin' } });
    const body = text(h.admin());
    assert.match(body, /A cost limit is an admission threshold, not a hard cap/);
    assert.match(body, /limit of \$10/);
    assert.match(body, /USD\(12\) \/ USD\(10\)/);
    assert.match(body, /Period limits are on/);
});

test('admin cost-limit heading appears exactly once and mobile targets are 44px', () => {
    const h = harness();
    const tree = h.admin();
    const headings = find(tree, (n) => text(n) === 'A cost limit is an admission threshold, not a hard cap');
    assert.equal(headings.length, 1, 'the disclaimer heading is not duplicated in the editor');
    const summary = find(tree, (n) => n.type === 'summary')[0];
    assert.match(summary.props.className, /max-sm:min-h-11/);
    assert.match(summary.props.className, /max-sm:min-w-11/);
    assert.match(h.input(1).props.className, /max-sm:min-h-11/);
    const textarea = find(tree, (n) => n.type === 'textarea')[0];
    assert.match(textarea.props.className, /max-sm:min-h-11/);
    const saveButton = find(tree, (n) => n.type === 'button' && text(n).includes('Save period limits'))[0];
    assert.match(saveButton.props.className, /max-sm:min-h-11/);
    assert.match(saveButton.props.className, /max-sm:min-w-11/);
});

test('disabled policy tells the truth: saved, not enforced, usage kept', () => {
    const h = harness({ data: status({ policy: { ...policy, enabled: false }, realtime_supported: true }) });
    const body = text(h.card());
    assert.match(body, /Period limits are off/);
    assert.match(body, /saved but turned off/);
    assert.match(body, /not checked against it and use the existing balance or daily card path/);
    assert.doesNotMatch(body, /Realtime sessions are unavailable/, 'realtime is supported again once disabled');
});

test('no policy path explains the legacy route and hides the user card', () => {
    const h = harness({ data: status({ policy: null, windows: [], pending_receipts: 0, realtime_supported: true }) });
    assert.match(text(h.admin()), /No period limits are set for this user\. Requests are not checked against these limits and use the existing balance or daily card path\./);
    assert.equal(h.card(), null, 'ordinary user card is not shown without a policy');
});

test('model scope intersects key permissions and wraps long identifiers', () => {
    const longList = 'anthropic/claude-sonnet-4-5-20250929-very-long-identifier,openai/gpt-5-mini-2025-08-07';
    const h = harness({ data: status({ policy: { ...policy, allowed_models: longList } }) });
    const tree = h.card();
    assert.match(text(tree), /The policy list and your API key's allowed models both apply/);
    const scopeItem = find(tree, (n) => n.type === 'li' && text(n).includes(longList))[0];
    assert.ok(scopeItem, 'long model list rendered');
    assert.match(scopeItem.props.className, /break-all/, 'long identifiers wrap instead of overflowing');
});

test('save rejects an invalid numeric draft without writing anything', async () => {
    const h = harness();
    h.input(1).props.onChange({ target: { value: '-1' } });
    await h.button('Save period limits').props.onClick();
    assert.deepEqual(h.writes, [], 'no mutation for a negative cost');
    assert.deepEqual(h.notes.map(([kind, message]) => [kind, message]), [['error', 'Use non-negative amounts and whole request counts.']]);
});

test('a legitimate edit keeps the original revision and the exact payload shape', async () => {
    const h = harness();
    h.input(1).props.onChange({ target: { value: '15' } });
    await h.button('Save period limits').props.onClick();
    assert.equal(h.writes.length, 1);
    assert.deepEqual(h.writes[0], { ...policy, five_hour_cost: 15 });
    assert.equal(h.writes[0].revision, 3, 'revision from the read is preserved, not bumped client-side');
    assert.deepEqual(Object.keys(h.writes[0]).sort(), Object.keys(policy).sort(), 'no API fields added or dropped');
    assert.deepEqual(h.notes.map(([kind]) => kind), ['success']);
});

test('a failed save surfaces the server message and keeps the draft', async () => {
    const h = harness({ save: { reject: new Error('quota policy changed; reload before saving') } });
    h.input(1).props.onChange({ target: { value: '15' } });
    await h.button('Save period limits').props.onClick();
    assert.deepEqual(h.notes.map(([kind, message]) => [kind, message]), [['error', 'Could not save period limits']]);
    assert.equal(h.notes[0][2].description, 'quota policy changed; reload before saving');
});

test('simplified Chinese renders the same admission semantics', () => {
    const h = harness({ tag: 'zh-Hans', data: status({ policy: { ...policy, allowed_models: 'model-a' } }) });
    const body = text(h.card());
    assert.match(body, /金额上限是受理门槛，不是账单硬上限/);
    assert.match(body, /上限 10 美元/);
    assert.match(body, /12 美元/);
    assert.match(body, /USD\(12\) \/ USD\(10\)/);
    assert.match(body, /不是滚动的“最近 5 小时”/);
    assert.match(body, /均按 Asia\/Shanghai 计算/);
    assert.match(body, /上限为 0 表示不限/);
    assert.match(body, /策略列表与 API Key 允许的模型需同时满足/);
    assert.doesNotMatch(body, /admissionNote|modelScopeAll|zeroMeansUnlimited/, 'no raw key leaked into the UI');
});

test('all three locales carry every key the component renders', () => {
    const source = readFileSync(path.join(__dirname, '..', 'src', 'components', 'modules', 'user', 'components', 'user-limits.tsx'), 'utf8');
    const used = new Set([...source.matchAll(/\bt\('([A-Za-z_][A-Za-z0-9_]*)'/g)].map((match) => match[1]));
    // t(window.period) resolves these at runtime.
    ['five_hour', 'week', 'month'].forEach((period) => used.add(period));
    assert.ok(used.size >= 20, `expected the component to render its copy through keys, saw ${used.size}`);
    for (const tag of Object.keys(messages)) {
        const dict = messages[tag].userLimits;
        for (const key of used) {
            assert.equal(typeof dict[key], 'string', `${tag}: userLimits.${key} missing`);
            assert.ok(dict[key].trim().length > 0, `${tag}: userLimits.${key} empty`);
        }
        for (const key of ['admissionTitle', 'admissionNote', 'summary', 'detailTitle', 'fiveHourNote', 'calendarNote', 'refundNote', 'estimateNote', 'zeroMeansUnlimited', 'modelScope', 'modelScopeAll', 'disabledNote', 'pending']) {
            assert.equal(typeof dict[key], 'string', `${tag}: ${key} must exist in every locale`);
        }
        assert.doesNotMatch(JSON.stringify(dict), /cost_reserved|pending_receipts|count_semantics/, `${tag}: no internal field names in user copy`);
    }
    const english = Object.keys(messages.en.userLimits).sort();
    for (const tag of Object.keys(messages)) {
        assert.deepEqual(Object.keys(messages[tag].userLimits).sort(), english, `${tag}: key set must match en`);
    }
});
