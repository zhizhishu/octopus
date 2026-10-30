const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

// 价格编辑里的「按次价格」要说清真实效果：生图按张（同步异步一样），没价的模型异步生图会被拒绝。
const root = path.join(__dirname, '..');
const locales = ['zh_hans', 'zh_hant', 'en'];

test('per-call price tip and no-price note exist in every locale', () => {
    for (const loc of locales) {
        const json = JSON.parse(fs.readFileSync(path.join(root, 'public/locale', `${loc}.json`), 'utf8'));
        const tip = json.model.overlay.perCallTip;
        assert.ok(typeof tip === 'string' && tip.length > 0, `${loc} perCallTip`);
        assert.ok(json.model.card.noPriceSub.length > 0, `${loc} noPriceSub`);
    }
    const zh = JSON.parse(fs.readFileSync(path.join(root, 'public/locale/zh_hans.json'), 'utf8'));
    assert.match(zh.model.overlay.perCallTip, /按张/);
    assert.match(zh.model.overlay.perCallTip, /异步/);
    assert.match(zh.model.card.noPriceSub, /异步生图不能用/);
});

test('edit overlay shows the per-call tip under the field', () => {
    const src = fs.readFileSync(path.join(root, 'src/components/modules/model/ItemOverlays.tsx'), 'utf8');
    assert.match(src, /\{t\('perCallTip'\)\}<\/p>/);
});
