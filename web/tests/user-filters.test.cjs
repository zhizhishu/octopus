const test = require('node:test');
const assert = require('node:assert/strict');

const { load } = require('./load-typescript.cjs');

const { cardState, filterUsers, statusCounts, matchesSearch, paginate, pageButtons, EMPTY_FILTERS } = load('src/components/modules/user/lib/user-filters.ts');

const NOW = 1_800_000_000;
const DAY = 86400;

function user(over) {
    return { id: 1, username: 'u', email: '', note: '', role: 'user', status: 'active', monthly_limit: 0, monthly_used: 0, monthly_expire_at: 0, access_plan_ids: [], ...over };
}

const users = [
    user({ id: 1, username: 'alice', email: 'alice@example.com', role: 'admin', access_plan_ids: [1] }),
    user({ id: 2, username: 'bob', note: 'VIP 客户', monthly_limit: 5, monthly_used: 1, monthly_expire_at: NOW + 20 * DAY, access_plan_ids: [1, 2] }),
    user({ id: 3, username: 'carol', status: 'disabled', monthly_limit: 5, monthly_used: 5, monthly_expire_at: NOW + 10 * DAY, access_plan_ids: [2] }),
    user({ id: 4, username: 'dave', monthly_limit: 5, monthly_expire_at: NOW + 2 * DAY, access_plans: [{ id: 3 }] }),
    user({ id: 5, username: 'erin', status: 'disabled', monthly_limit: 5, monthly_expire_at: NOW - DAY }),
    user({ id: 12, username: 'frank' }),
];

const f = (over) => ({ ...EMPTY_FILTERS, ...over });
const ids = (list) => list.map((u) => u.id);

test('cardState covers every state; admins never have a card', () => {
    assert.equal(cardState(users[0], NOW), 'none');
    assert.equal(cardState(user({ role: 'admin', monthly_limit: 5, monthly_expire_at: NOW + DAY }), NOW), 'none');
    assert.equal(cardState(users[1], NOW), 'active');
    assert.equal(cardState(users[2], NOW), 'exhausted');
    assert.equal(cardState(users[3], NOW), 'soon');
    assert.equal(cardState(users[4], NOW), 'expired');
    assert.equal(cardState(users[5], NOW), 'none');
    // 到期时刻等于现在算已过期（后端 userMonthlyActive 用的是 expire_at > now）
    assert.equal(cardState(user({ monthly_limit: 1, monthly_expire_at: NOW }), NOW), 'expired');
});

test('search: username / email / note, case-insensitive; #id by number', () => {
    assert.deepEqual(ids(filterUsers(users, f({ search: 'ALICE@' }), NOW)), [1]);
    assert.deepEqual(ids(filterUsers(users, f({ search: 'vip' }), NOW)), [2]);
    assert.deepEqual(ids(filterUsers(users, f({ search: '#12' }), NOW)), [12]);
    assert.deepEqual(ids(filterUsers(users, f({ search: '#1' }), NOW)), [1]);
    assert.equal(matchesSearch(users[0], '   '), true);
});

test('status, role, plan (any-of), card filters combine with AND', () => {
    assert.deepEqual(ids(filterUsers(users, f({ status: 'disabled' }), NOW)), [3, 5]);
    assert.deepEqual(ids(filterUsers(users, f({ roles: ['admin'] }), NOW)), [1]);
    assert.deepEqual(ids(filterUsers(users, f({ planIDs: [2] }), NOW)), [2, 3]);
    assert.deepEqual(ids(filterUsers(users, f({ planIDs: [1, 3] }), NOW)), [1, 2, 4], 'access_plans fallback is used when ids are missing');
    assert.deepEqual(ids(filterUsers(users, f({ cards: ['soon', 'exhausted'] }), NOW)), [3, 4]);
    assert.deepEqual(ids(filterUsers(users, f({ status: 'active', planIDs: [2] }), NOW)), [2]);
});

test('status counts ignore the status chip but follow every other filter', () => {
    assert.deepEqual(statusCounts(users, f({}), NOW), { all: 6, active: 4, disabled: 2 });
    assert.deepEqual(statusCounts(users, f({ status: 'disabled' }), NOW), { all: 6, active: 4, disabled: 2 });
    assert.deepEqual(statusCounts(users, f({ planIDs: [2] }), NOW), { all: 2, active: 1, disabled: 1 });
    assert.deepEqual(statusCounts(users, f({ search: 'nobody' }), NOW), { all: 0, active: 0, disabled: 0 });
});

test('paginate clamps page into range', () => {
    const list = Array.from({ length: 45 }, (_, i) => i + 1);
    assert.deepEqual(paginate(list, 1, 20).items, list.slice(0, 20));
    assert.deepEqual(paginate(list, 3, 20), { items: [41, 42, 43, 44, 45], page: 3, pages: 3, total: 45 });
    assert.equal(paginate(list, 99, 20).page, 3);
    assert.equal(paginate(list, 0, 20).page, 1);
    assert.deepEqual(paginate([], 2, 20), { items: [], page: 1, pages: 1, total: 0 });
});

test('pageButtons keeps first/last/neighbours with ellipsis (0)', () => {
    assert.deepEqual(pageButtons(1, 5), [1, 2, 3, 4, 5]);
    assert.deepEqual(pageButtons(1, 10), [1, 2, 0, 10]);
    assert.deepEqual(pageButtons(5, 10), [1, 0, 4, 5, 6, 0, 10]);
    assert.deepEqual(pageButtons(10, 10), [1, 0, 9, 10]);
    assert.deepEqual(pageButtons(3, 10), [1, 2, 3, 4, 0, 10]);
});
