const test = require('node:test');
const assert = require('node:assert/strict');

const { load } = require('./load-typescript.cjs');

// 这些用例锁的是 ExpandableValue 的对外契约，不是它的样式。
//
// 背景：卡片上大量字段靠 `title=` 承载完整内容，鼠标能悬停看到，触屏上打不开。
// 这个组件负责在移动端把 title 换成可点开的抽屉。它一旦退化成"只有 title"，
// 手机上那些信息就又没了，而且没有任何报错——所以必须有测试盯着。

const reactStub = {
    useState: (initial) => [typeof initial === 'function' ? initial() : initial, () => {}],
    Fragment: 'Fragment',
    createElement: (type, props, ...children) => ({ type, props, children }),
};

function loadComponent() {
    return load('src/components/modules/log/ExpandableValue.tsx', {
        'react': reactStub,
        '@/lib/utils': { cn: (...parts) => parts.filter(Boolean).join(' ') },
        '@/components/ui/dialog': {
            Dialog: 'Dialog',
            DialogContent: 'DialogContent',
            DialogHeader: 'DialogHeader',
            DialogTitle: 'DialogTitle',
        },
    });
}

/** 从 React.createElement 造出的树里把某类节点都捞出来。 */
function collect(node, predicate, out = []) {
    if (node === null || node === undefined || typeof node !== 'object') return out;
    if (Array.isArray(node)) {
        for (const child of node) collect(child, predicate, out);
        return out;
    }
    if (predicate(node)) out.push(node);
    for (const child of node.children ?? []) collect(child, predicate, out);
    // Fragment 的 children 在 props 里也可能被塞过。
    if (node.props?.children) collect(node.props.children, predicate, out);
    return out;
}

/** 摊平 Fragment，找到真正被渲染的那棵子树。 */
function render(props) {
    const { ExpandableValue } = loadComponent();
    const tree = ExpandableValue(props);
    return collect(tree, (n) => n.type === 'button' || n.type === 'span' || n.type === 'Dialog');
}

test('有完整值时同时给出桌面 title 与移动端可点按钮', () => {
    const nodes = render({ children: 'trunc…', label: '请求模型', value: 'claude-sonnet-4-5-20250929' });

    const span = nodes.find((n) => n.type === 'span');
    const button = nodes.find((n) => n.type === 'button');

    assert.ok(span, '桌面端应有承载 title 的 span');
    assert.equal(span.props.title, 'claude-sonnet-4-5-20250929');

    assert.ok(button, '移动端应有可点按钮——否则触屏上永远看不到全文');
    // 触控尺寸下限，别改成 py-0 那种贴边样式。
    assert.match(button.props.className, /min-h-11/);
    assert.match(button.props.className, /md:hidden/);
});

test('没有完整值时只渲染原样内容，不凭空造一个空抽屉', () => {
    const nodes = render({ children: 'abc', label: '请求模型', value: undefined });

    assert.equal(nodes.find((n) => n.type === 'button'), undefined);
    assert.equal(nodes.find((n) => n.type === 'Dialog'), undefined);
});

test('空字符串与 null 同样不渲染抽屉（空值没什么可展开的）', () => {
    for (const empty of ['', null]) {
        const nodes = render({ children: 'abc', label: 'x', value: empty });
        assert.equal(nodes.find((n) => n.type === 'button'), undefined, `value=${String(empty)} 不该出按钮`);
    }
});

test('点击移动端按钮会拦住事件冒泡——卡片整体可点，不能顺手把详情也打开', () => {
    const nodes = render({ children: 'trunc…', label: '请求模型', value: 'real-name' });
    const button = nodes.find((n) => n.type === 'button');

    let stopped = false;
    button.props.onClick({ stopPropagation: () => { stopped = true; } });
    assert.equal(stopped, true, '必须 stopPropagation，否则点全文会同时弹出详情弹窗');
});
