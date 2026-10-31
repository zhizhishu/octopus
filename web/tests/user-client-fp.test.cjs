const test = require('node:test');
const assert = require('node:assert/strict');

const { load } = require('./load-typescript.cjs');

const { isSecretHeader, headerNames, notableEntries, fpTitle, hasTLSFingerprint, filterByKey, shortHash } = load('src/components/modules/user/lib/client-fp.ts');

test('秘密头：和后端同一套规则（只记名字的那些给锁）', () => {
    for (const n of ['Authorization', 'x-api-key', 'Cookie', 'X-Goog-Api-Key', 'X-Custom-Token', 'mj-api-secret', 'Sec-WebSocket-Protocol']) {
        assert.equal(isSecretHeader(n), true, n);
    }
    for (const n of ['User-Agent', 'anthropic-beta', 'x-stainless-os', 'originator', 'session_id', 'Accept']) {
        assert.equal(isSecretHeader(n), false, n);
    }
});

test('头名按逗号拆，保持原顺序；身份头 JSON 身份类在前、通用头在后，坏 JSON 返回空', () => {
    assert.deepEqual(notableEntries({ notable: '{"accept":"*/*","x-app":"cli","sec-fetch-mode":"cors","anthropic-version":"1"}' }).map(([k]) => k), ['anthropic-version', 'x-app', 'accept', 'sec-fetch-mode']);
    assert.deepEqual(headerNames({ header_names: 'host,User-Agent, x-api-key,' }), ['host', 'User-Agent', 'x-api-key']);
    assert.deepEqual(headerNames({ header_names: '' }), []);
    assert.deepEqual(notableEntries({ notable: '{"x-stainless-os":"MacOS","anthropic-version":"2023-06-01"}' }), [['anthropic-version', '2023-06-01'], ['x-stainless-os', 'MacOS']]);
    assert.deepEqual(notableEntries({ notable: '{bad' }), []);
    assert.deepEqual(notableEntries({ notable: '' }), []);
});

test('卡片标题：名字 + 版本；认不出时用 UA 第一段', () => {
    assert.equal(fpTitle({ client_name: 'Claude Code', client_version: '2.0.14', user_agent: 'claude-cli/2.0.14' }), 'Claude Code 2.0.14');
    assert.equal(fpTitle({ client_name: 'Cursor', client_version: '', user_agent: 'x' }), 'Cursor');
    assert.equal(fpTitle({ client_name: '', client_version: '', user_agent: 'weird-tool/1 (linux)' }), 'weird-tool/1');
    assert.equal(fpTitle({ client_name: '', client_version: '', user_agent: '' }), undefined);
});

test('TLS 指纹有无、按 Key 筛、短指纹号', () => {
    assert.equal(hasTLSFingerprint([{ ja3: '', ja4: '' }]), false);
    assert.equal(hasTLSFingerprint([{ ja3: '', ja4: '' }, { ja3: 'abc', ja4: '' }]), true);
    const fps = [{ api_key_id: 1 }, { api_key_id: 2 }, { api_key_id: 1 }];
    assert.equal(filterByKey(fps, 0).length, 3);
    assert.equal(filterByKey(fps, 1).length, 2);
    assert.equal(shortHash('0123456789abcdef'), '01234567');
    assert.equal(shortHash(undefined), '');
});
