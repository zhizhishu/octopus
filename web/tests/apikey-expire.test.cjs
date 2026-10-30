const test = require('node:test');
const assert = require('node:assert/strict');
const { execFileSync } = require('node:child_process');
const path = require('node:path');
const { readFileSync } = require('node:fs');

// 时区要在进程启动时定下来才稳，所以每个时区起一个子进程跑断言。
const SELF = __filename;

function runIn(tz) {
    const out = execFileSync(process.execPath, [SELF, '--child'], {
        env: { ...process.env, TZ: tz },
        encoding: 'utf8',
    });
    return JSON.parse(out);
}

if (process.argv.includes('--child')) {
    const { load } = require('./load-typescript.cjs');
    const m = load('src/components/modules/setting/apikey-expire.ts');
    const now = new Date(2026, 9, 30, 11, 48); // 本地 2026-10-30 11:48
    const at = m.toExpireAt(new Date(2026, 10, 1), '00:00'); // 选「11/1 00:00」
    const back = new Date(at * 1000);
    const def = m.defaultExpireAt(now);
    const defDate = new Date(def * 1000);
    process.stdout.write(JSON.stringify({
        at,
        back: [back.getFullYear(), back.getMonth() + 1, back.getDate(), back.getHours(), back.getMinutes()],
        timeOf: m.expireTimeOf(at),
        roundTrip: m.toExpireAt(back, m.expireTimeOf(at)) === at,
        def: [defDate.getFullYear(), defDate.getMonth() + 1, defDate.getDate(), defDate.getHours(), defDate.getMinutes()],
        defFuture: !m.isPastExpire(def, now.getTime()),
        pastToday: m.isPastExpire(m.toExpireAt(now, '00:00'), now.getTime()),
        never: m.isPastExpire(undefined, now.getTime()),
        bad: m.toExpireAt(new Date(2026, 10, 1), '9:5') === at,
    }));
    return;
}

for (const [tz, expectedUTC] of [
    ['America/Los_Angeles', Date.UTC(2026, 10, 1, 7, 0) / 1000], // PDT -7
    ['Asia/Shanghai', Date.UTC(2026, 9, 31, 16, 0) / 1000],      // +8
    ['UTC', Date.UTC(2026, 10, 1, 0, 0) / 1000],
]) {
    test(`过期时间按本地时区存取：${tz}`, () => {
        const r = runIn(tz);
        assert.equal(r.at, expectedUTC, '选的本地 11/1 00:00 应存成对应的 UTC 秒');
        assert.deepEqual(r.back, [2026, 11, 1, 0, 0], '读回来日期和时分都应是本地的');
        assert.equal(r.timeOf, '00:00');
        assert.equal(r.roundTrip, true);
        assert.equal(r.bad, true, '时分格式不对时按 00:00');
    });

    test(`取消永不过期的默认值是 30 天后本地 23:59，不是过去时间：${tz}`, () => {
        const r = runIn(tz);
        assert.deepEqual(r.def, [2026, 11, 29, 23, 59]);
        assert.equal(r.defFuture, true);
        assert.equal(r.pastToday, true, '今天 00:00 已经过去，应判为过去时间');
        assert.equal(r.never, false, '永不过期不算过去');
    });
}

test('APIKey 表单不再按 UTC 拼/读时分，且用同一组本地时间工具', () => {
    const src = readFileSync(path.resolve(__dirname, '../src/components/modules/setting/APIKey.tsx'), 'utf8');
    assert.doesNotMatch(src, /Date\.UTC\(|getUTCHours|getUTCMinutes/);
    assert.match(src, /from '\.\/apikey-expire'/);
    assert.match(src, /defaultExpireAt\(\)/, '取消永不过期用默认的未来时间');
    assert.match(src, /isPastExpire\(/, '保存前拦过去时间');
});
