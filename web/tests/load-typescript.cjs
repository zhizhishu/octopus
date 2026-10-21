const { readFileSync, existsSync } = require('node:fs');
const { createRequire } = require('node:module');
const path = require('node:path');
const vm = require('node:vm');
const ts = require('typescript');

// Execute production TypeScript without a build or a second test framework.
//
// TypeScript source imports siblings without an extension ('./probe-evidence'),
// because that is what the compiler and bundler expect. Node's CommonJS loader
// does not. Such imports are therefore routed back through `load` itself, so the
// child is transpiled the same way and — critically — keeps the caller's mocks:
// a nested component importing '@/lib/utils' must see the same stub the test
// installed, or it would try to resolve the alias for real and fail.
const EXTENSIONS = ['.ts', '.tsx', '.js', '.jsx', '.mjs', '.cjs'];
const TS_EXTENSIONS = new Set(['.ts', '.tsx']);

/**
 * Map an import request to a file on disk.
 *
 * Returns `{ file }` when the request points at an existing file, `{ error }`
 * when it clearly does not, and `null` when the request is not a relative path
 * (bare specifier, alias) and therefore has to go to the real resolver.
 */
function resolveRelativeFile(id, fromFile) {
    if (!id.startsWith('.')) return null;
    const base = path.resolve(path.dirname(fromFile), id);
    if (path.extname(id) !== '' && existsSync(base)) return { file: base };
    for (const ext of EXTENSIONS) {
        const candidate = base + ext;
        if (existsSync(candidate)) return { file: candidate };
    }
    // Directory import with an index file, same rule Node applies.
    for (const ext of EXTENSIONS) {
        const candidate = path.join(base, 'index' + ext);
        if (existsSync(candidate)) return { file: candidate };
    }
    return null;
}

function transpile(filename, source) {
    const { outputText } = ts.transpileModule(source, {
        fileName: filename,
        compilerOptions: {
            module: ts.ModuleKind.CommonJS,
            target: ts.ScriptTarget.ES2022,
            jsx: ts.JsxEmit.ReactJSX,
            esModuleInterop: true,
        },
    });
    return outputText;
}

/**
 * @param relativePath path relative to the package root
 * @param mocks        module id -> stub, applied to this module *and* everything
 *                     it pulls in through a relative TypeScript import
 * @param cache        per-top-level-load module cache, so a shared child is
 *                     instantiated once per test rather than once per importer
 */
function load(relativePath, mocks = {}, cache = new Map()) {
    const filename = path.resolve(__dirname, '..', relativePath);
    const cached = cache.get(filename);
    if (cached) return cached.exports;

    const source = readFileSync(filename, 'utf8');
    const outputText = transpile(filename, source);

    const realRequire = createRequire(filename);
    const module = { exports: {} };
    cache.set(filename, module);

    const mockedRequire = (id) => {
        if (Object.hasOwn(mocks, id)) return mocks[id];

        const resolved = resolveRelativeFile(id, filename);
        if (resolved === null) return realRequire(id);

        // Let Node handle plain JS so its own cache and interop stay in charge.
        if (!TS_EXTENSIONS.has(path.extname(resolved.file))) return realRequire(resolved.file);

        return load(path.relative(path.resolve(__dirname, '..'), resolved.file), mocks, cache);
    };

    const wrapper = vm.runInThisContext(
        `(function(require, module, exports, __filename, __dirname) {${outputText}\n})`,
        { filename },
    );
    wrapper(mockedRequire, module, module.exports, filename, path.dirname(filename));
    return module.exports;
}

module.exports = { load };
