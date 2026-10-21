const test = require('node:test');
const assert = require('node:assert/strict');
const { writeFileSync, mkdtempSync, rmSync } = require('node:fs');
const { tmpdir } = require('node:os');
const path = require('node:path');

const { load } = require('./load-typescript.cjs');

// 这些用例锁的是加载器本身的行为，不是产品代码。
//
// 背景：产品源码里 `import { x } from './sibling'` 是不带扩展名的（tsc 和打包器
// 都认），而 Node 的 CommonJS 解析不认。加载器曾因此只对"恰好写了真实路径"的
// 导入有效，一旦组件拆成多文件就整片测试失败。下面三条把这件事钉住。

const PACKAGE_ROOT = path.resolve(__dirname, '..');

function fixture(files) {
    const dir = mkdtempSync(path.join(tmpdir(), 'loader-fixture-'));
    for (const [name, source] of Object.entries(files)) {
        writeFileSync(path.join(dir, name), source);
    }
    return {
        dir,
        loadEntry(name, mocks) {
            return load(path.relative(PACKAGE_ROOT, path.join(dir, name)), mocks);
        },
        cleanup() {
            rmSync(dir, { recursive: true, force: true });
        },
    };
}

test('能加载不带扩展名的相对 TS 导入', () => {
    const mod = load('src/components/modules/log/probe-evidence.ts');
    assert.equal(typeof mod.describeProbeEvidence, 'function');
});

test('嵌套的 TS 子模块会一路沿用调用方的 mock，而不是自己去解析别名', () => {
    // 复现真实结构：parent.ts -> child.ts -> '@/lib/utils'。
    // 如果导航到子模块时把 mocks 丢了，child 就会真去解析 '@/lib/utils' 并失败。
    const f = fixture({
        'child.ts': "import { cn } from '@/lib/utils';\nexport const joined = cn('a', 'b');\n",
        'parent.ts': "import { joined } from './child';\nexport const viaParent = joined;\n",
    });
    try {
        const mod = f.loadEntry('parent.ts', {
            '@/lib/utils': { cn: (...parts) => parts.filter(Boolean).join('-') },
        });
        assert.equal(mod.viaParent, 'a-b');
    } finally {
        f.cleanup();
    }
});

test('同一个子模块在一次加载里只实例化一次（共享缓存）', () => {
    const f = fixture({
        // child 每次被实例化就把自己的 hits 加一；共享缓存下只会发生在 1。
        'child.ts': 'export const marker = { hits: 0 };\nmarker.hits++;\n',
        'a.ts': "import { marker } from './child';\nexport const fromA = marker;\n",
        'b.ts': "import { marker } from './child';\nexport const fromB = marker;\n",
        'entry.ts': "export { fromA } from './a';\nexport { fromB } from './b';\n",
    });
    try {
        const mod = f.loadEntry('entry.ts');
        assert.equal(mod.fromA.hits, 1);
        assert.equal(mod.fromA, mod.fromB);
    } finally {
        f.cleanup();
    }
});

test('相对路径指向不存在的文件时，错误来自真实解析器而不是被吞掉', () => {
    const f = fixture({ 'entry.ts': "import { x } from './missing';\nexport const y = x;\n" });
    try {
        assert.throws(() => f.loadEntry('entry.ts'), /Cannot find module/);
    } finally {
        f.cleanup();
    }
});
