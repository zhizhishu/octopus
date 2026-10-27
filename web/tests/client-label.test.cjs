'use strict';

const test = require('node:test');
const assert = require('node:assert');
const { load } = require('./load-typescript.cjs');

const { humanizeClient, stripUrls } = load('src/components/modules/log/humanize.ts');

/**
 * 客户端识别的两条约束：
 *  1. 认得出的客户端给人话名字（DSH 不能被漏成原始 UA）。
 *  2. 认不出的照实展示，但**不许把网址带进界面**。
 *
 * 第 2 条来自用户的实际反馈：日志里客户端一栏出现了带 GitHub 链接的一长串，
 * 根因就是这里——认不出时原文照搬，而很多 CLI 的 UA 尾巴上挂着仓库地址。
 */

test('DSH 的 UA 保留完整信息，但网址被剥掉', () => {
    // 认得出的短形态照旧。
    assert.equal(humanizeClient('dsh/0.1.5-rc.2'), 'DSH');

    // 认不出的长形态**不许改名或缩短**——完整名字和版本正是排障要的信息。
    const long = humanizeClient('deepseek-harness/0.1.5-rc.2 (win32; x64)');
    assert.equal(long, 'deepseek-harness/0.1.5-rc.2 (win32; x64)');

    // 带仓库地址的形态：名字版本都在，链接没了。
    const withUrl = humanizeClient(
        'deepseek-harness/0.1.5-rc.2 (https://github.com/deepseek-ai/deepseek-harness)',
    );
    assert.ok(withUrl.includes('deepseek-harness/0.1.5-rc.2'), `应保留名字与版本，实际: ${withUrl}`);
    assert.ok(!withUrl.includes('github.com'), `不该出现仓库地址，实际: ${withUrl}`);
});

// 真实 UA 大量写成 `Client/1.0 (+https://...)`——RFC 9110 注释语法里 `+` 表示附加信息。
// 只剥网址不剥 `+`，界面上就会留下一个孤零零的 `(+`（生产实际出现过）。
test('括号内带 + 的附加信息形态，剥完不留残渣', () => {
    const cases = [
        [
            'deepseek-harness/0.1.5-rc.2 (+https://github.com/deepseek-ai/deepseek-harness)',
            'deepseek-harness/0.1.5-rc.2',
        ],
        [
            'SomeTool/2.0 (+https://example.com/tool)',
            'SomeTool/2.0',
        ],
        [
            'SomeTool/2.0 (+ github.com/acme/tool)',
            'SomeTool/2.0',
        ],
    ];
    for (const [ua, want] of cases) {
        const got = humanizeClient(ua);
        assert.equal(got, want, `UA=${ua} 应精确剥成 ${want}，实际: ${got}`);
        // 残留的 `(`、`+`、多余空格都算失败——这正是这次修的 bug。
        assert.ok(!/[+(]\s*$/.test(got), `不该留下残渣，实际: ${got}`);
    }
});

test('其它已知客户端名不受影响', () => {
    assert.equal(humanizeClient('claude-cli/1.2.3'), 'Claude CLI');
    assert.equal(humanizeClient('codex_cli_rs/0.144.0'), 'Codex CLI');
    assert.equal(humanizeClient('Mozilla/5.0 (Windows NT 10.0)'), '浏览器');
    assert.equal(humanizeClient('curl/8.4.0'), 'curl');
});

test('认不出的 UA 不再把网址显示出来', () => {
    const ua = 'SomeInternalTool/1.0.0 (https://github.com/acme/some-internal-tool)';
    const label = humanizeClient(ua);
    assert.ok(label, '应当仍然显示一些内容');
    assert.ok(!label.includes('github.com'), `不应包含网址，实际: ${label}`);
    assert.ok(!label.includes('https://'), `不应包含协议头，实际: ${label}`);
    // 仍然保留客户端本身是谁，不能因为剥网址把人话也剥没了。
    assert.ok(label.includes('SomeInternalTool'), `应保留客户端名，实际: ${label}`);
});

test('裸 github.com 写法同样被剥掉', () => {
    const out = stripUrls('MyTool/2.0 github.com/acme/mytool');
    assert.ok(!out.includes('github.com'), `实际: ${out}`);
    assert.ok(out.includes('MyTool'), `实际: ${out}`);
});

test('只填了一个链接时不显示空壳或链接', () => {
    assert.equal(stripUrls('https://github.com/acme/x'), undefined);
    assert.equal(stripUrls('github.com/acme/x'), undefined);
});

test('普通 UA 不因剥网址逻辑被改动', () => {
    assert.equal(stripUrls('python-requests/2.31.0'), 'python-requests/2.31.0');
    assert.equal(stripUrls('okhttp/4.12.0'), 'okhttp/4.12.0');
    assert.equal(stripUrls(undefined), undefined);
    assert.equal(stripUrls(''), undefined);
    assert.equal(stripUrls('   '), undefined);
});

test('剥网址后不留空括号与多余空格', () => {
    const out = stripUrls('Tool/1.0   ( https://example.com/x )   extra');
    assert.ok(!out.includes('()'), `不应留空括号，实际: ${out}`);
    assert.ok(!out.includes('  '), `不应留双空格，实际: ${out}`);
    assert.equal(out, 'Tool/1.0 extra');
});
