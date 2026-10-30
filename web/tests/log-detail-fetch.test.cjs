const test = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const path = require('node:path');

const itemPath = path.join(__dirname, '../src/components/modules/log/Item.tsx');
const indexPath = path.join(__dirname, '../src/components/modules/log/index.tsx');

test('detail dialog fetches a log once and keys the cache by id', () => {
    const src = readFileSync(itemPath, 'utf8');
    const calls = src.match(/fetchLogById\(/g) || [];
    assert.equal(calls.length, 1, 'LazyLogBodies and DetailAuditReport must share one fetch');
    assert.match(src, /queryKey:\s*\['relay-log-detail',\s*logId\]/);
    assert.doesNotMatch(src, /prevProps\s*,\s*nextProps/);
});

test('log page passes a stable detail-open callback', () => {
    const src = readFileSync(indexPath, 'utf8');
    assert.match(src, /useCallback\(\(logId: number, open: boolean\)/);
    assert.doesNotMatch(src, /onDetailOpenChange=\{\(open\)/);
});
