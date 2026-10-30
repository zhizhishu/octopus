const test = require('node:test');
const assert = require('node:assert/strict');
const { load } = require('./load-typescript.cjs');

// curl 示例每行续行都要是「空格 + \ + 换行」，否则贴进终端会挤成一行（视频示例以前漏过）。
const g = load('src/components/modules/key/lib/key-guide.ts');
const input = { baseUrl: 'https://octo.example/', apiKey: 'sk-octopus-TESTKEY1234', models: [], shell: 'unix' };

for (const family of ['openai-compatible', 'anthropic', 'gemini', 'video']) {
    test(`curl 示例续行完整：${family}`, () => {
        for (const plan of [undefined, 'svip']) {
            const lines = g.buildCurl(family, { ...input, plan }).split('\n');
            assert.ok(lines.length >= 4);
            for (const line of lines.slice(0, -1)) assert.match(line, / \\$/, `行尾应是 " \\"：${line}`);
            assert.doesNotMatch(lines.at(-1), /\\$/);
            assert.match(lines[0], /^curl "https:\/\/octo\.example\/v1/, '去掉末尾的 /');
            if (plan) assert.ok(lines.some((l) => l.includes('X-Octopus-Plan: svip')));
            else assert.ok(!lines.some((l) => l.includes('X-Octopus-Plan')));
        }
    });
}
