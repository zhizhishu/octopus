const { readFileSync } = require('node:fs');
const { createRequire } = require('node:module');
const path = require('node:path');
const vm = require('node:vm');
const ts = require('typescript');

// Execute production TypeScript without a build or a second test framework.
function load(relativePath, mocks = {}) {
    const filename = path.resolve(__dirname, '..', relativePath);
    const source = readFileSync(filename, 'utf8');
    const { outputText } = ts.transpileModule(source, {
        fileName: filename,
        compilerOptions: {
            module: ts.ModuleKind.CommonJS,
            target: ts.ScriptTarget.ES2022,
            jsx: ts.JsxEmit.ReactJSX,
            esModuleInterop: true,
        },
    });
    const realRequire = createRequire(filename);
    const module = { exports: {} };
    const mockedRequire = (id) => Object.hasOwn(mocks, id) ? mocks[id] : realRequire(id);
    const wrapper = vm.runInThisContext(
        `(function(require, module, exports, __filename, __dirname) {${outputText}\n})`,
        { filename },
    );
    wrapper(mockedRequire, module, module.exports, filename, path.dirname(filename));
    return module.exports;
}

module.exports = { load };
