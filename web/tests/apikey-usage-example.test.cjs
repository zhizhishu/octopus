const test = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const path = require('node:path');

// 模板字符串里行尾单个 `\` 是续行符，会把换行吃掉，curl 示例挤成一行；要写 `\\` 才输出 `\` + 换行。
test('API Key 使用示例里每行 curl 续行都写成 \\\\（视频示例曾漏写）', () => {
    const src = readFileSync(path.resolve(__dirname, '../src/components/modules/setting/APIKey.tsx'), 'utf8');
    const start = src.indexOf('function buildUsageExample(');
    assert.ok(start >= 0);
    const body = src.slice(start, src.indexOf('\n}\n', start));
    const continued = body.split('\n').filter((line) => /\\$/.test(line));
    assert.ok(continued.length >= 12, `应找到各端点的续行，实际 ${continued.length}`);
    for (const line of continued) {
        assert.match(line, /[^\\]\\\\$/, `行尾应是 \\\\：${line.trim()}`);
    }
    // 4 种端点都要有：curl 行续行
    for (const p of ['/v1beta/models/', '/v1/messages', '/v1/videos', '/v1/chat/completions']) {
        assert.match(body, new RegExp(`curl "\\$\\{baseUrl\\}${p.replace(/\//g, '\\/')}[^\\n]*\\\\\\\\\\n`));
    }
});
