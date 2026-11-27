'use strict';

// fix-ui1127b-r2 B1：PNG 栅格化搬进隔离框。
// Run from web: node --test tests/creative-png-frame.test.cjs

const test = require('node:test');
const assert = require('node:assert');
const { readFileSync } = require('node:fs');
const path = require('node:path');
const { load } = require('./load-typescript.cjs');

const V6 = 'src/components/modules/model-audit/v6.ts';
const v6 = () => load(V6, {});

test('栅格化 srcdoc 带内联 CSP：外部资源在框内发不出请求', () => {
    const { creativeRasterSrcDoc, CREATIVE_FRAME_CSP } = v6();
    const doc = creativeRasterSrcDoc('<svg xmlns="http://www.w3.org/2000/svg"><image href="https://evil.example/beacon"/></svg>', 'tok1234567890abcdef', 512, 512);
    // CSP 就是显示框那一条：img-src 只放行 data:/blob:，外部 image/字体一律被拦。
    assert.ok(doc.includes(CREATIVE_FRAME_CSP), 'srcdoc must embed the frame CSP');
    assert.ok(/img-src data: blob:/.test(doc), 'img-src must be data/blob only');
    // SVG 本体以 data URI 进 img.src，不拼进 HTML/JS 源码。
    assert.ok(doc.includes('data:image/svg+xml'), 'svg must ride a data URI');
    assert.ok(!doc.includes('<image href="https://evil.example/beacon"/>'), 'raw svg must not be inlined into the html');
});

test('主页面栅格化模块零 new Image / 零 fetch：SVG 只在隔离框里加载', () => {
    // Hark 钦定的断言形态：主页面代码里不存在指向图像/网络的原语，
    // 外部资源绝无可能在主页面上下文发出请求。
    const src = readFileSync(path.join(__dirname, '..', 'src/components/modules/model-audit/creative-png.ts'), 'utf8');
    assert.ok(!/new\s+Image\s*\(/.test(src), 'main page must not construct Image');
    assert.ok(!/\bfetch\s*\(/.test(src), 'main page must not fetch');
    // 必须走隔离框 + postMessage 通路。
    assert.ok(src.includes('sandbox'), 'must use a sandboxed iframe');
    assert.ok(src.includes('creativePNGMessageParse'), 'must validate the postMessage payload');
});

test('postMessage 校验：类型/令牌/载荷格式不对的一律不认', () => {
    const { creativePNGMessageParse, creativePNGToken, CREATIVE_PNG_MESSAGE_TYPE } = v6();
    const token = creativePNGToken();
    assert.strictEqual(token.length, 32, '128 位十六进制');
    const png = new ArrayBuffer(8);
    // 合法消息。
    assert.strictEqual(creativePNGMessageParse({ type: CREATIVE_PNG_MESSAGE_TYPE, token, png }, token), png);
    // 失败信号。
    assert.strictEqual(creativePNGMessageParse({ type: CREATIVE_PNG_MESSAGE_TYPE, token, png: null }, token), null);
    // 令牌不对——框里不可信脚本伪造消息类型也猜不到令牌。
    assert.strictEqual(creativePNGMessageParse({ type: CREATIVE_PNG_MESSAGE_TYPE, token: 'wrongwrongwrongwrong', png }, token), undefined);
    // 类型不对/不是对象/载荷形状不对。
    assert.strictEqual(creativePNGMessageParse({ type: 'other', token, png }, token), undefined);
    assert.strictEqual(creativePNGMessageParse(null, token), undefined);
    assert.strictEqual(creativePNGMessageParse('string', token), undefined);
    assert.strictEqual(creativePNGMessageParse({ type: CREATIVE_PNG_MESSAGE_TYPE, token, png: 'not-a-buffer' }, token), undefined);
    assert.strictEqual(creativePNGMessageParse({ type: CREATIVE_PNG_MESSAGE_TYPE, token, png: new ArrayBuffer(0) }, token), undefined);
});
