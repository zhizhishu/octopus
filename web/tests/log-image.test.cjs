const test = require('node:test');
const assert = require('node:assert/strict');

const { load } = require('./load-typescript.cjs');

// 「真实出站请求体」图片占位符：解析与渲染契约。
// 后端格式见 internal/relay/log_image_capture.go，两边必须对得上——
// 一旦对不上，详情页就只剩一串看不懂的方括号，而且不会报任何错。

const HASH = 'a'.repeat(32) + '0123456789abcdef0123456789abcdef';

function loadParser() {
    return load('src/components/modules/log/log-image.ts');
}

function loadRender() {
    return load('src/components/modules/log/LogImagePlaceholder.tsx', {
        'next-intl': { useTranslations: () => (key) => `t:${key}` },
        'lucide-react': { Ban: 'Ban' },
        './LogImageChip': { LogImageChip: 'LogImageChip' },
    });
}

test('认出已保存图片的占位符', () => {
    const { parseLogMediaPlaceholder } = loadParser();
    const p = parseLogMediaPlaceholder(`[image: image/png, 245KB, id=${HASH}]`);
    assert.deepEqual(p, { kind: 'saved', mime: 'image/png', size: '245KB', hash: HASH, raw: `[image: image/png, 245KB, id=${HASH}]` });
});

test('认出带原因和不带原因的 omitted 占位符', () => {
    const { parseLogMediaPlaceholder } = loadParser();
    const withReason = parseLogMediaPlaceholder('[image omitted: image/png, 12MB, too large]');
    assert.equal(withReason.kind, 'omitted');
    assert.equal(withReason.media, 'image');
    assert.equal(withReason.reason, 'too large');
    for (const reason of ['too many images', 'request image limit reached', 'save queue full']) {
        assert.equal(parseLogMediaPlaceholder(`[image omitted: image/jpeg, 9MB, ${reason}]`).reason, reason);
    }
    const legacy = parseLogMediaPlaceholder('[audio omitted: audio/wav, 3KB]');
    assert.equal(legacy.media, 'audio');
    assert.equal(legacy.reason, null);
});

test('不是占位符的字符串一律不认（包括伪造的、残缺的、大写哈希）', () => {
    const { parseLogMediaPlaceholder } = loadParser();
    for (const value of [
        'hello',
        `[image: image/png, 245KB, id=${HASH.slice(0, 63)}]`,
        `[image: image/png, 245KB, id=${HASH.toUpperCase()}]`,
        `[image: image/png, 245KB, id=${HASH}`,
        `看这里 [image: image/png, 245KB, id=${HASH}]`,
        `[image: <script>, 1KB, id=${HASH}]`,
        '[image omitted: image/png, 12MB, because reasons]',
        42,
        null,
        undefined,
    ]) {
        assert.equal(parseLogMediaPlaceholder(value), null, `should reject ${String(value)}`);
    }
});

test('纯文本视图：把占位符切出来，其余文字原样', () => {
    const { splitSavedImagePlaceholders, containsSavedImagePlaceholder } = loadParser();
    const text = `{"data":"[image: image/png, 2KB, id=${HASH}]","t":"x"}\n...[truncated: 99 bytes total]`;
    const segs = splitSavedImagePlaceholders(text);
    assert.equal(segs.length, 3);
    assert.deepEqual(segs[0], { type: 'text', text: '{"data":"' });
    assert.equal(segs[1].type, 'image');
    assert.equal(segs[1].placeholder.hash, HASH);
    assert.equal(segs[2].text, '","t":"x"}\n...[truncated: 99 bytes total]');
    assert.equal(segs.map((s) => s.type === 'text' ? s.text : s.placeholder.raw).join(''), text, '拼回去必须与原文一字不差');
    assert.equal(containsSavedImagePlaceholder(text), true);
    assert.equal(containsSavedImagePlaceholder(text), true, '全局正则的 lastIndex 不能让第二次调用失灵');
    // 截在占位符中间的那张认不出来，当文字显示。
    const cut = `{"data":"[image: image/png, 2KB, id=${HASH.slice(0, 20)}`;
    assert.deepEqual(splitSavedImagePlaceholders(cut), [{ type: 'text', text: cut }]);
    assert.equal(containsSavedImagePlaceholder(cut), false);
    assert.deepEqual(splitSavedImagePlaceholders(''), []);
});

test('原因 → 文案 key，以及下载文件名', () => {
    const { omitReasonKey, logImageFileName, shortLogImageHash } = loadParser();
    assert.equal(omitReasonKey('too large'), 'omitTooLarge');
    assert.equal(omitReasonKey('too many images'), 'omitTooMany');
    assert.equal(omitReasonKey('request image limit reached'), 'omitOverBudget');
    assert.equal(omitReasonKey('save queue full'), 'omitQueueFull');
    assert.equal(omitReasonKey(null), 'omitNotSaved');
    assert.equal(logImageFileName({ hash: HASH, mime: 'image/jpeg' }), `${HASH}.jpg`);
    assert.equal(logImageFileName({ hash: HASH, mime: 'image/webp' }), `${HASH}.webp`);
    assert.equal(shortLogImageHash(HASH), `${HASH.slice(0, 12)}…`);
});

test('JSON 树：已保存图片渲染成缩略图卡片，omitted 渲染成标签，其它字符串走默认', () => {
    const { renderLogImageJsonString } = loadRender();
    const saved = renderLogImageJsonString({}, { type: 'value', value: `[image: image/png, 2KB, id=${HASH}]` });
    assert.equal(saved.type, 'LogImageChip');
    assert.equal(saved.props.placeholder.hash, HASH);

    const omitted = renderLogImageJsonString({}, { type: 'value', value: '[image omitted: image/png, 12MB, too large]' });
    assert.equal(typeof omitted.type, 'function', '应渲染 LogMediaOmittedBadge');
    assert.equal(omitted.props.placeholder.reason, 'too large');

    assert.equal(renderLogImageJsonString({}, { type: 'value', value: 'https://cdn.example.com/a.png' }), undefined,
        'http 链接不存、不自动加载，保持 JsonView 默认渲染');
    assert.equal(renderLogImageJsonString({}, { type: 'value', value: 'plain' }), undefined);
});

test('omitted 标签写明原因', () => {
    const { LogMediaOmittedBadge } = loadRender();
    const { parseLogMediaPlaceholder } = loadParser();
    const tree = LogMediaOmittedBadge({ placeholder: parseLogMediaPlaceholder('[image omitted: image/png, 12MB, too large]') });
    const flat = JSON.stringify(tree);
    assert.match(flat, /t:omitTooLarge/);
    assert.match(flat, /12MB/);
    const audio = LogMediaOmittedBadge({ placeholder: parseLogMediaPlaceholder('[audio omitted: audio/wav, 3KB]') });
    assert.match(JSON.stringify(audio), /t:omitMedia/);
});

test('纯文本视图组件：占位符变卡片，文字保留', () => {
    const { LogImagePlaceholderText } = loadRender();
    const text = `before "[image: image/gif, 1KB, id=${HASH}]" after`;
    const tree = LogImagePlaceholderText({ text });
    const kids = tree.props.children;
    assert.equal(kids.length, 3);
    assert.equal(kids[0].props.children, 'before "');
    assert.equal(kids[1].type, 'LogImageChip');
    assert.equal(kids[1].props.placeholder.mime, 'image/gif');
    assert.equal(kids[2].props.children, '" after');
});

test('三种语言的文案都齐', () => {
    const { readFileSync } = require('node:fs');
    const path = require('node:path');
    const keys = Object.keys(JSON.parse(readFileSync(path.join(__dirname, '../public/locale/zh_hans.json'), 'utf8')).log.card.logImage);
    assert.ok(keys.length >= 10);
    for (const loc of ['en', 'zh_hant']) {
        const dict = JSON.parse(readFileSync(path.join(__dirname, `../public/locale/${loc}.json`), 'utf8')).log.card.logImage;
        assert.deepEqual(Object.keys(dict).sort(), [...keys].sort(), `${loc} 缺文案`);
    }
});
