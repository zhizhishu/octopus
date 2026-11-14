// Run from web/: node --test tests/mobile-touch-targets.test.cjs
//
// Mobile (max-sm) touch-target acceptance: every REAL clickable target must be
// >=44x44 on mobile while desktop paint/variants stay unchanged. Components are
// executed as actual TSX through tests/load-typescript.cjs with mocked React
// hooks/runtime, following tests/model-editor-dialog.test.cjs. The log page is
// locked with source assertions: loading the whole Log component would require
// the entire store/API/stream mock graph, which is out of scope here.
const assert = require('node:assert/strict');
const { test } = require('node:test');
const { readFileSync } = require('node:fs');
const path = require('node:path');
const { load } = require('./load-typescript.cjs');

// ---- tiny JSX/render helpers (same style as model-editor-dialog.test.cjs) ----
const jsx = (type, props) => (typeof type === 'function' ? type(props) : { type, props });
const jsxs = jsx;
const JSX_RUNTIME = { jsx, jsxs, Fragment: 'Fragment' };
const cnMock = (...args) => args.filter(Boolean).join(' ');

function find(node, pred, out = []) {
    if (Array.isArray(node)) { node.forEach((c) => find(c, pred, out)); return out; }
    if (!node || typeof node !== 'object') return out;
    if (pred(node)) out.push(node);
    find(node.props?.children, pred, out);
    return out;
}

const reactStubs = (extra = {}) => ({
    useState: extra.useState,
    useEffect: () => {},
    useRef: () => ({ current: null }),
    useMemo: extra.useMemo,
    useCallback: extra.useCallback,
    useId: () => ':r0:',
    ...extra.direct,
});

const src = (rel) => readFileSync(path.resolve(__dirname, '..', rel), 'utf8');

// ------------------------------ shared Button -------------------------------
test('Button base: mobile min 44x44, desktop sizes intact', () => {
    const { buttonVariants, Button } = load('src/components/ui/button.tsx', {
        react: reactStubs(),
        'react/jsx-runtime': JSX_RUNTIME,
        '@radix-ui/react-slot': { Slot: 'slot' },
        '@/lib/utils': { cn: cnMock },
    });
    const base = buttonVariants();
    assert.match(base, /max-sm:min-h-11/);
    assert.match(base, /max-sm:min-w-11/);
    // Desktop variants/sizes preserved.
    assert.match(base, /h-9 px-4 py-2/);
    assert.match(buttonVariants({ size: 'icon' }), /size-9/);
    assert.match(buttonVariants({ size: 'icon-sm' }), /size-8/);
    assert.match(buttonVariants({ size: 'lg' }), /h-10/);
    const el = Button({ variant: 'outline', size: 'icon-sm' });
    assert.equal(el.type, 'button');
    assert.match(el.props.className, /size-8/);
    assert.match(el.props.className, /max-sm:min-h-11/);
    assert.match(el.props.className, /max-sm:min-w-11/);
});

// --------------------------------- Input ------------------------------------
test('Input: mobile min-height 44, desktop h-9 kept', () => {
    const { Input } = load('src/components/ui/input.tsx', {
        react: reactStubs(),
        'react/jsx-runtime': JSX_RUNTIME,
        '@/lib/utils': { cn: cnMock },
    });
    const el = Input({});
    assert.equal(el.type, 'input');
    assert.match(el.props.className, /h-9 w-full/);
    assert.match(el.props.className, /max-sm:min-h-11/);
    assert.match(el.props.className, /md:text-sm/);
});

// --------------------------------- Select -----------------------------------
test('Select trigger + items: mobile min-height 44, viewport semantics kept', () => {
    const { SelectTrigger, SelectItem, SelectContent } = load('src/components/ui/select.tsx', {
        'react/jsx-runtime': JSX_RUNTIME,
        'lucide-react': { CheckIcon: 'CheckIcon', ChevronDownIcon: 'ChevronDownIcon', ChevronUpIcon: 'ChevronUpIcon' },
        '@/lib/utils': { cn: cnMock },
        // Radix internals call React hooks; stub every primitive select.tsx touches
        // so the real class composition is executed without a React runtime.
        '@radix-ui/react-select': Object.fromEntries(
            ['Root', 'Group', 'Trigger', 'Value', 'Icon', 'Portal', 'Content', 'Viewport',
             'Item', 'ItemText', 'ItemIndicator', 'Label', 'Separator', 'ScrollUpButton', 'ScrollDownButton']
                .map((k) => [k, (props) => ({ type: `select.${k}`, props })])
        ),
    });
    const trigger = SelectTrigger({});
    assert.match(trigger.props.className, /max-sm:min-h-11/);
    assert.match(trigger.props.className, /data-\[size=default\]:h-9 data-\[size=sm\]:h-8/);
    assert.match(trigger.props.className, /max-w-full/);
    const popup = SelectContent({});
    const content = find(popup, (node) => node.type === 'select.Content')[0];
    assert.match(content.props.className, /max-w-\[calc\(100vw-1rem\)\]/);
    const item = SelectItem({});
    assert.equal(item.type, 'select.Item');
    assert.match(item.props.className, /max-sm:min-h-11/);
    assert.match(item.props.className, /py-1\.5/);
});

// --------------------------------- Switch -----------------------------------
// Radix stub: Root/Thumb render plain elements so the real switch.tsx layout can
// be inspected (Root is the interactive element; rail must be noninteractive).
test('Switch: Root is a real 44x44 mobile target; painted rail stays 32x18; no nested interactive', () => {
    const { Switch } = load('src/components/ui/switch.tsx', {
        react: reactStubs(),
        'react/jsx-runtime': JSX_RUNTIME,
        '@radix-ui/react-switch': {
            Root: (props) => ({ type: 'button', props }),
            Thumb: (props) => ({ type: 'span', props }),
        },
        '@/lib/utils': { cn: cnMock },
    });
    const root = Switch({ checked: true, 'aria-label': 'live' });
    assert.equal(root.type, 'button');
    // Root itself is the target: mobile 44x44, focus/disabled behavior intact.
    assert.match(root.props.className, /max-sm:h-11 max-sm:w-11/);
    assert.match(root.props.className, /focus-visible:ring-\[3px\]/);
    assert.match(root.props.className, /disabled:opacity-50/);
    // Desktop Root keeps its original compact footprint.
    assert.match(root.props.className, /h-\[1\.15rem\] w-8/);

    const rail = find(root, (n) => n.props?.className?.includes('rounded-full border border-transparent'))[0];
    assert.ok(rail, 'painted rail span must exist');
    assert.equal(rail.type, 'span');
    assert.match(rail.props.className, /pointer-events-none/);
    assert.match(rail.props.className, /group-data-\[state=checked\]\/switch:bg-primary/);
    assert.match(rail.props.className, /group-data-\[state=unchecked\]\/switch:bg-input/);
    assert.match(rail.props.className, /max-sm:h-\[1\.15rem\] max-sm:w-8/);

    const thumb = find(root, (n) => n.props?.['data-slot'] === 'switch-thumb')[0];
    assert.ok(thumb, 'thumb must exist');
    assert.equal(thumb.type, 'span');
    assert.match(thumb.props.className, /size-4/);
    assert.match(thumb.props.className, /data-\[state=checked\]:translate-x-\[calc\(100%-2px\)\]/);
    assert.match(thumb.props.className, /max-sm:absolute max-sm:left-1\.5 max-sm:top-1\/2/);

    // No nested interactive element inside the Radix Root.
    assert.deepEqual(find(root.props.children, (n) => n.type === 'button'), []);
});

// -------------------------- ui-bits: Checkbox / Seg / HelpTip ----------------
test('Checkbox: real mobile button 44, 16px painted box, mixed + stopPropagation + onChange', () => {
    const { Checkbox } = load('src/components/modules/user/components/ui-bits.tsx', {
        'react/jsx-runtime': JSX_RUNTIME,
        'lucide-react': { Check: 'Check', CircleHelp: 'CircleHelp', Minus: 'Minus' },
        '@/lib/utils': { cn: cnMock },
    });
    const calls = [];
    const el = Checkbox({ checked: false, indeterminate: true, onChange: (v) => calls.push(v), label: 'select row' });
    assert.equal(el.type, 'button');
    assert.equal(el.props.role, 'checkbox');
    assert.equal(el.props['aria-checked'], 'mixed');
    assert.equal(el.props['aria-label'], 'select row');
    assert.match(el.props.className, /max-sm:size-11/);

    // The painted 16px box is a noninteractive decoration inside the button.
    const box = find(el, (n) => n.props?.className?.includes('rounded-[4px] border'))[0];
    assert.ok(box, 'painted box must exist');
    assert.equal(box.type, 'span');
    assert.match(box.props.className, /size-4/);
    assert.match(box.props.className, /group-hover\/chk:border-primary\/60/);
    assert.match(box.props.className, /border-primary bg-primary text-primary-foreground/);

    let stopped = false;
    el.props.onClick({ stopPropagation: () => { stopped = true; } });
    assert.ok(stopped, 'stopPropagation must be preserved');
    assert.deepEqual(calls, [true]);

    const off = Checkbox({ checked: false, onChange: (v) => calls.push(v), label: 'x' });
    assert.equal(off.props['aria-checked'], false);
});

test('CheckMark stays purely decorative (16px, not inflated)', () => {
    const s = src('src/components/modules/user/components/ui-bits.tsx');
    assert.match(s, /export function CheckMark[\s\S]*?size-4[\s\S]*?rounded-\[4px\] border/);
    assert.doesNotMatch(s, /export function CheckMark[\s\S]*?max-sm:size-11/);
});

test('HelpTip: focusable target 44 on mobile, 14px graphic kept', () => {
    const { HelpTip } = load('src/components/modules/user/components/ui-bits.tsx', {
        'react/jsx-runtime': JSX_RUNTIME,
        'lucide-react': { Check: 'Check', CircleHelp: 'CircleHelp', Minus: 'Minus' },
        '@/lib/utils': { cn: cnMock },
    });
    const tip = HelpTip({ content: 'why' });
    const target = find(tip, (n) => n.props?.tabIndex === 0)[0];
    assert.ok(target, 'focusable target must exist');
    assert.equal(target.props['aria-label'], 'why');
    assert.match(target.props.className, /size-3\.5/);
    assert.match(target.props.className, /max-sm:size-11/);
});

test('Seg: tab targets 44 on mobile (container 52 - p-1), desktop h-8/h-9 + p-[3px] kept', () => {
    const { Seg } = load('src/components/modules/user/components/ui-bits.tsx', {
        'react/jsx-runtime': JSX_RUNTIME,
        'lucide-react': { Check: 'Check', CircleHelp: 'CircleHelp', Minus: 'Minus' },
        '@/lib/utils': { cn: cnMock },
    });
    const picked = [];
    const list = Seg({
        value: 'a',
        options: [{ value: 'a', label: 'A' }, { value: 'b', label: 'B' }],
        onChange: (v) => picked.push(v),
        size: 'md',
    });
    assert.equal(list.type, 'div');
    assert.equal(list.props.role, 'tablist');
    assert.match(list.props.className, /max-sm:h-13/);
    assert.match(list.props.className, /max-sm:p-1/);
    assert.match(list.props.className, /h-9/);
    const tabs = find(list, (n) => n.props?.role === 'tab');
    assert.equal(tabs.length, 2);
    assert.equal(tabs[0].props['aria-selected'], true);
    for (const tab of tabs) assert.match(tab.props.className, /h-full/);
    tabs[1].props.onClick();
    assert.deepEqual(picked, ['b']);
});

// ---------------------------------- Navbar ----------------------------------
test('NavBar: dock buttons are real 44x44 on mobile, md classes unchanged, select handler intact', async () => {
    let picked = null;
    const setActiveItem = (item) => { picked = item; };
    const { NavBar } = load('src/components/modules/navbar/navbar.tsx', {
        react: reactStubs(),
        'react/jsx-runtime': JSX_RUNTIME,
        '@/lib/utils': { cn: cnMock },
        'motion/react': { motion: new Proxy({}, { get: (_, k) => (props) => ({ type: `motion.${String(k)}`, props }) }) },
        './nav-store': { useNavStore: () => ({ activeItem: 'dashboard', setActiveItem: () => {} }) },
        '@/route/config': { routesForRole: () => [{ id: 'dashboard', icon: 'Icon' }, { id: 'logs', icon: 'Icon' }] },
        '@/route/use-preload': { usePreload: () => ({ preload: () => Promise.resolve() }) },
        '@/lib/animations/fluid-transitions': { ENTRANCE_VARIANTS: { navbar: {} } },
        '@/api/endpoints/user': { useAuthStore: (sel) => sel({ user: { role: 'admin' } }) },
        'next-intl': { useTranslations: () => (key) => key },
    });
    const nav = NavBar({ activeItem: 'dashboard', setActiveItem });
    const buttons = find(nav, (n) => n.type === 'motion.button');
    assert.equal(buttons.length, 2);
    for (const btn of buttons) {
        // Mobile dock item: the actual <button> footprint is 44x44 (was h-10 w-10).
        assert.match(btn.props.className, /h-11 w-11/);
        // Desktop sidebar item untouched.
        assert.match(btn.props.className, /md:h-auto md:w-32/);
    }
    const active = buttons.find((b) => b.props['aria-current'] === 'page');
    assert.ok(active, 'active item must be marked');
    assert.equal(active.props['aria-label'], 'dashboard');
    assert.equal(typeof active.props.onClick, 'function');
    active.props.onClick();
    await new Promise((r) => setImmediate(r));
    await new Promise((r) => setImmediate(r));
    assert.equal(picked, 'dashboard');
    // Dock still scrolls internally (no document overflow by construction).
    const s = src('src/components/modules/navbar/navbar.tsx');
    assert.match(s, /overflow-x-auto/);
    assert.match(s, /max-w-\[calc\(100vw-1rem\)\]/);
    assert.match(s, /aria-current="page"/);
});

// ------------------------ Log page (source-level locks) ----------------------
// Whole-component load would need the full log store/API/stream mock graph; the
// responsive primitives and their exact call sites are locked at source level.
test('Log page: listed controls >=44 on mobile, desktop classes preserved', () => {
    const s = src('src/components/modules/log/index.tsx');
    // Category tabs (desktop 32px preserved via sm:h-8).
    assert.ok(s.includes("'inline-flex h-11 items-center justify-center gap-1.5 rounded-lg px-2.5 text-xs font-medium whitespace-nowrap transition-colors sm:h-8'"));
    // Search input + clear button: 44 button inside a 44 input, text cleared via pr-11.
    assert.match(s, /className="h-11 w-full rounded-lg border border-input bg-background pl-8 pr-11[^"]*sm:h-9 sm:pr-7"/);
    assert.match(s, /className="absolute right-0 top-1\/2 grid size-11 -translate-y-1\/2[^"]*sm:right-1\.5 sm:size-5"/);
    // Mobile filter toggle.
    assert.ok(s.includes("'inline-flex h-11 shrink-0 items-center gap-1 rounded-lg border px-3 text-xs transition-colors sm:hidden'"));
    // Native selects (endpoint/provider/model/mismatch): 4 sites.
    assert.equal((s.match(/className="h-11 sm:h-9 min-w-0 w-full rounded-lg border border-input bg-background px-2 text-xs text-foreground outline-none sm:px-3"/g) || []).length, 4);
    // Date shortcuts.
    assert.ok(s.includes("'inline-flex h-11 flex-1 items-center justify-center rounded-lg px-2 text-xs font-medium whitespace-nowrap transition-colors sm:h-8 sm:flex-none sm:px-2.5'"));
    // Calendar trigger.
    assert.ok(s.includes("'h-11 w-11 shrink-0 rounded-lg sm:h-8 sm:w-8'"));
    // Severity filter pills.
    assert.ok(s.includes("'inline-flex h-11 min-w-0 flex-1 items-center justify-center gap-1 rounded-lg px-1.5 text-xs font-medium whitespace-nowrap transition-colors sm:h-8 sm:flex-none sm:gap-1.5 sm:px-2'"));
    // Eye + export buttons.
    assert.equal((s.match(/max-sm:h-11 max-sm:w-11/g) || []).length, 2);
    // Mobile pagination buttons.
    assert.equal((s.match(/className="h-11 flex-1 rounded-lg"/g) || []).length, 2);
});

test('AutoRefreshControl: live toggle + pending pill 44 on mobile', () => {
    const s = src('src/components/modules/log/AutoRefreshControl.tsx');
    assert.equal((s.match(/max-sm:h-11/g) || []).length, 2);
    // Stable status slot and pause labels untouched (freeze behavior lives here).
    assert.match(s, /stableSlot = 'inline-flex h-4 w-36/);
    assert.match(s, /max-sm:h-11 max-sm:px-3/);
});
