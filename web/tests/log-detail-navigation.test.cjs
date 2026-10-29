const test = require('node:test');
const assert = require('node:assert/strict');

const { load } = require('./load-typescript.cjs');

// useLogDetailNavigation 的契约测试。
//
// 这个 hook 干两件事：翻到页边界时自动拉下一页、以及把左右键接上。最容易出错的
// 是第二件——**用户在搜索框里按左键只是移光标**，如果这里不区分事件源，打字时
// 日志会莫名其妙乱跳。下面把它钉死。

/**
 * 极简 React 模拟：hook 槽位跨渲染保留，cursor 每次渲染归零。
 *
 * 这不是要复刻 React，只是让 useState/useRef/useEffect/useCallback 在多次调用
 * 之间有正确的持久化语义——上一版没做这件事，测试才会误报。
 */
function createHarness() {
    const slots = [];
    const cleanups = [];
    const listeners = [];
    let cursor = 0;

    const react = {
        useState(initial) {
            const i = cursor++;
            if (!(i in slots)) slots[i] = typeof initial === 'function' ? initial() : initial;
            return [slots[i], (next) => { slots[i] = typeof next === 'function' ? next(slots[i]) : next; }];
        },
        useRef(initial) {
            const i = cursor++;
            if (!(i in slots)) slots[i] = { current: initial };
            return slots[i];
        },
        useCallback(fn) { cursor++; return fn; },
        useEffect(fn) {
            const i = cursor++;
            // 每次渲染都重跑，等价于依赖变化；对这些用例足够。
            cleanups[i]?.();
            cleanups[i] = fn();
        },
    };

    // hook 里只用 window.addEventListener('keydown')；把最后注册的处理器捞出来。
    const fakeWindow = {
        addEventListener(type, fn) { if (type === 'keydown') listeners.push(fn); },
        removeEventListener() {},
    };

    return {
        react,
        fakeWindow,
        listeners,
        /** 渲染一次（或重渲染）。 */
        render(hookFn, props) {
            cursor = 0;
            return hookFn(props);
        },
        /** 最近一次注册的 keydown 处理器。 */
        lastHandler() { return listeners[listeners.length - 1]; },
    };
}

const LOGS = [{ id: 10 }, { id: 9 }, { id: 8 }];

function setup(props = {}) {
    const harness = createHarness();
    const originalWindow = globalThis.window;
    const originalHTMLElement = globalThis.HTMLElement;

    class FakeHTMLElement {
        constructor(tag) {
            this.tagName = String(tag).toUpperCase();
            this.isContentEditable = false;
        }
    }
    globalThis.window = harness.fakeWindow;
    globalThis.HTMLElement = FakeHTMLElement;

    const { useLogDetailNavigation } = load('src/components/modules/log/use-log-detail-navigation.ts', {
        react: harness.react,
    });

    const base = { logs: LOGS, hasMore: false, loadMore: () => {}, isLoadingMore: false, ...props };
    const render = (overrides = {}) => harness.render(useLogDetailNavigation, { ...base, ...overrides });

    return {
        harness,
        FakeHTMLElement,
        render,
        restore() {
            globalThis.window = originalWindow;
            globalThis.HTMLElement = originalHTMLElement;
        },
    };
}

test('打开一条后在页内能向前翻', async () => {
    const s = setup();
    try {
        let r = s.render();
        r.open(9);
        r = s.render();          // 重渲染：activeId 已持久化
        assert.equal(r.activeId, 9);
        assert.equal(r.canPrev, true, '9 前面还有 10');

        await r.navigate(-1);
        r = s.render();
        assert.equal(r.activeId, 10, '应翻到上一条（更新的那条）');
    } finally {
        s.restore();
    }
});

test('走到本页末尾且还有下一页时，先触发加载把分页缝掉', async () => {
    let loads = 0;
    const s = setup({ hasMore: true, loadMore: () => { loads += 1; } });
    try {
        let r = s.render();
        r.open(8);               // 最后一条
        r = s.render();
        assert.equal(r.canNext, true, '还有下一页，所以还能往后');

        await r.navigate(1);
        assert.equal(loads, 1, '边界处应自动拉下一页');
    } finally {
        s.restore();
    }
});

test('没有下一页时走到边界不发起多余请求', async () => {
    let loads = 0;
    const s = setup({ hasMore: false, loadMore: () => { loads += 1; } });
    try {
        let r = s.render();
        r.open(8);
        r = s.render();
        assert.equal(r.canNext, false);
        await r.navigate(1);
        assert.equal(loads, 0);
    } finally {
        s.restore();
    }
});

test('正在加载时不重复发起，避免连按方向键打出一串请求', async () => {
    let loads = 0;
    const s = setup({ hasMore: true, loadMore: () => { loads += 1; }, isLoadingMore: true });
    try {
        let r = s.render();
        r.open(8);
        r = s.render();
        await r.navigate(1);
        await r.navigate(1);
        assert.equal(loads, 0, 'isLoadingMore 为真时应直接返回');
    } finally {
        s.restore();
    }
});

test('第一条不能再往前翻', () => {
    const s = setup();
    try {
        let r = s.render();
        r.open(10);
        r = s.render();
        assert.equal(r.canPrev, false);
        assert.equal(r.canNext, true);
    } finally {
        s.restore();
    }
});

test('没有打开的日志时两个方向都不开放', () => {
    const s = setup();
    try {
        const r = s.render();
        assert.equal(r.canPrev, false);
        assert.equal(r.canNext, false);
        assert.equal(r.activeLog, null);
    } finally {
        s.restore();
    }
});

test('关键：焦点在输入框里时方向键放行给光标，不翻日志', () => {
    const s = setup();
    try {
        let r = s.render();
        r.open(9);
        r = s.render();

        const handler = s.harness.lastHandler();
        assert.ok(handler, '应注册 keydown 监听');

        let prevented = false;
        handler({
            key: 'ArrowRight',
            isComposing: false,
            target: new s.FakeHTMLElement('input'),
            preventDefault: () => { prevented = true; },
        });
        assert.equal(prevented, false, '输入框里的方向键必须放行');
    } finally {
        s.restore();
    }
});

test('textarea 与 contentEditable 同样放行', () => {
    const s = setup();
    try {
        let r = s.render();
        r.open(9);
        r = s.render();
        const handler = s.harness.lastHandler();

        const textarea = new s.FakeHTMLElement('textarea');
        const editable = new s.FakeHTMLElement('div');
        editable.isContentEditable = true;

        for (const target of [textarea, editable]) {
            let prevented = false;
            handler({ key: 'ArrowLeft', isComposing: false, target, preventDefault: () => { prevented = true; } });
            assert.equal(prevented, false, `${target.tagName} 里的方向键应放行`);
        }
    } finally {
        s.restore();
    }
});

test('输入法组合中的方向键不响应，别打断选词', () => {
    const s = setup();
    try {
        let r = s.render();
        r.open(9);
        r = s.render();
        const handler = s.harness.lastHandler();

        let prevented = false;
        handler({
            key: 'ArrowRight',
            isComposing: true,
            target: new s.FakeHTMLElement('div'),
            preventDefault: () => { prevented = true; },
        });
        assert.equal(prevented, false);
    } finally {
        s.restore();
    }
});

test('列表中途插入行后，打开时拿到的 navigate 仍翻到新的上一条', async () => {
    const s = setup();
    try {
        let r = s.render();
        r.open(9);
        r = s.render();
        const navigate = r.navigate;
        const inserted = [{ id: 10 }, { id: 12 }, { id: 9 }, { id: 8 }];
        r = s.render({ logs: inserted });
        await navigate(-1);
        r = s.render({ logs: inserted });
        assert.equal(r.activeId, 12, '应翻到插入后的上一条，而不是打开弹窗时的旧邻居');
    } finally {
        s.restore();
    }
});

test('弹窗关掉之后不再监听键盘', () => {
    const s = setup();
    try {
        let r = s.render();
        r.open(9);
        r = s.render();
        r.close(false);
        s.render();
        // 关闭后即使有残留处理器，也不该还能翻。
        assert.equal(s.render().activeLog, null);
    } finally {
        s.restore();
    }
});
