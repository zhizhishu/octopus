'use strict';

const test = require('node:test');
const assert = require('node:assert');
const { load } = require('./load-typescript.cjs');

const { sanitizeChannelTestError } = load('src/lib/channel-test.ts', {
    '@/api/endpoints/channel': { ChannelType: {} },
    './model-aliases': { isStreamRequiredModel: () => false },
});

/**
 * 回归：sub2api 的 opus-5-5 报错曾被显示成 "use 上游=adaptive"——点分字段路径被当成主机名。
 * 字段路径必须原样保留，真实主机仍然隐藏。
 */
test('field paths in provider errors stay readable', () => {
    const cases = [
        'upstream status 400: claude-opus-5-5 requires adaptive thinking; omit thinking or use thinking.type=adaptive and output_config.effort',
        '"thinking.type.disabled" is not supported for this model. Use "thinking.type.adaptive" and "output_config.effort" to control thinking behavior.',
        'messages.0.content: Input should be a valid list',
        'messages.12.content.0.tool_use.id: String should match pattern',
        'at internal/relay/relay.go:2256 and web/src/lib/channel-test.ts:61',
        'retry after status:429 at line:42',
        'claude-opus-4.7 not available; claude-sonnet-5.5 neither',
    ];
    for (const input of cases) {
        assert.strictEqual(sanitizeChannelTestError(input), input, input);
    }
});

test('real hosts, urls, ips and ports are hidden', () => {
    const cases = [
        ['dial tcp api.example.com: timeout', 'dial tcp 上游: timeout'],
        ['GET https://x.y.z/path?q=1 failed', 'GET 上游 failed'],
        ['connect to host:443 refused', 'connect to 上游 refused'],
        ['connect to localhost:8080 refused', 'connect to 上游 refused'],
        ['relay-api.mycorp.io:8443 reset', '上游 reset'],
        ['proxy.internal.lan said no', '上游 said no'],
        ['from 10.0.0.12:3128', 'from 上游'],
        ['"api.anthropic.com" rejected', '"上游" rejected'],
        ['openrouter.ai rate limited', '上游 rate limited'],
    ];
    for (const [input, want] of cases) {
        assert.strictEqual(sanitizeChannelTestError(input), want, input);
    }
});

test('credentials are still hidden', () => {
    assert.strictEqual(sanitizeChannelTestError('bad key sk-abc_123'), 'bad key [已隐藏]');
    assert.match(sanitizeChannelTestError('Authorization: Bearer abc.def'), /\[已隐藏\]/);
});
