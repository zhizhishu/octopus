// Pure helpers for the price page and the "从上游更新价格" preview. No React here, so
// node --test can run them (tests/price-sync.test.cjs).

export type TokenPrice = { input: number; output: number; cache_read: number; cache_write: number };

export type SyncKind = 'new' | 'changed' | 'manual' | 'same';

export interface SyncItem {
    name: string;
    kind: SyncKind;
    source: string;
    exists: boolean;
    current: TokenPrice;
    next: TokenPrice;
    big: boolean;
    has_per_call: boolean;
}

export const PRICE_FIELDS = ['input', 'output', 'cache_read', 'cache_write'] as const;
export type PriceField = (typeof PRICE_FIELDS)[number];

/**
 * Ticked by default: 新增, and 变了 within ±50%. Not ticked: 手动改过 and big changes.
 * Same rule the auto-sync uses (minus manual rows, which it never touches).
 */
export function defaultSelected(items: SyncItem[]): Set<string> {
    return new Set(
        items
            .filter((it) => it.kind === 'new' || (it.kind === 'changed' && !it.big))
            .map((it) => it.name),
    );
}

export type SyncTab = 'pending' | SyncKind;

export function itemsForTab(items: SyncItem[], tab: SyncTab, search: string): SyncItem[] {
    const term = search.trim().toLowerCase();
    const rank = (it: SyncItem) => (it.kind === 'new' ? 0 : it.kind === 'changed' ? (it.big ? 2 : 1) : it.kind === 'manual' ? 3 : 4);
    // Ticked-by-default rows first, then the ones that wait for a look (big changes, manual rows).
    return items
        .filter((it) => {
            if (term && !it.name.toLowerCase().includes(term)) return false;
            if (tab === 'pending') return it.kind !== 'same';
            return it.kind === tab;
        })
        .map((it, i) => ({ it, i }))
        .sort((a, b) => rank(a.it) - rank(b.it) || a.i - b.i)
        .map(({ it }) => it);
}

export function tabCounts(items: SyncItem[]): Record<SyncTab, number> {
    const counts: Record<SyncTab, number> = { pending: 0, new: 0, changed: 0, manual: 0, same: 0 };
    for (const it of items) {
        counts[it.kind] += 1;
        if (it.kind !== 'same') counts.pending += 1;
    }
    return counts;
}

/** Pending items left unticked, split the way the footer explains them. */
export function unselectedSummary(items: SyncItem[], selected: Set<string>): { manual: number; big: number } {
    let manual = 0;
    let big = 0;
    for (const it of items) {
        if (it.kind === 'same' || selected.has(it.name)) continue;
        if (it.kind === 'manual') manual += 1;
        else if (it.big) big += 1;
    }
    return { manual, big };
}

/** Rounded signed percent change, or null when the old value was 0 ("原来没填"). */
export function percentChange(from: number, to: number): number | null {
    if (!from) return null;
    return Math.round(((to - from) / from) * 100);
}

/** Largest relative move of any field that was already set, e.g. 1.5 for 10 → 25. */
export function biggestRatio(from: TokenPrice, to: TokenPrice): number {
    let best = 0;
    for (const f of PRICE_FIELDS) {
        if (from[f] > 0) {
            const r = (to[f] - from[f]) / from[f];
            if (Math.abs(r) > Math.abs(best)) best = r;
        }
    }
    return best;
}

export function formatPrice(value: number): string {
    if (!Number.isFinite(value)) return '0';
    return String(Number(value.toFixed(6)));
}

export function isZeroPrice(p: TokenPrice): boolean {
    return !p.input && !p.output && !p.cache_read && !p.cache_write;
}

export type DisplaySource = 'library' | 'manual' | 'none';

export interface PriceRowLike extends TokenPrice {
    source?: string;
    per_call?: number;
    library?: TokenPrice | null;
}

/**
 * Where the page says a row's price comes from. Matches billing:
 * - manual: an admin typed the token price; the auto-sync never touches it.
 * - none (没价): no token price, no per-call price, and the library has none
 *   either, so it bills at 0.
 * - library: everything else, including a placeholder row that bills at the
 *   library price until a sync writes it in (see libraryPlaceholder).
 */
export function displaySource(row: PriceRowLike): DisplaySource {
    if (row.source === 'manual') return 'manual';
    if (isZeroPrice(row) && !(row.per_call ?? 0) && !row.library) return 'none';
    return 'library';
}

/**
 * A row with no price of its own bills at the library price (the backend falls
 * through to the catalog for an all-zero row). Return that price so the card shows
 * what is actually billed.
 */
export function libraryPlaceholder(row: PriceRowLike): TokenPrice | null {
    if (row.source === 'manual') return null;
    if (!isZeroPrice(row) || (row.per_call ?? 0) || !row.library) return null;
    return row.library;
}

export function sourceCounts(rows: PriceRowLike[]): Record<DisplaySource, number> {
    const counts: Record<DisplaySource, number> = { library: 0, manual: 0, none: 0 };
    for (const r of rows) counts[displaySource(r)] += 1;
    return counts;
}

export function toApplyChanges(items: SyncItem[], selected: Set<string>) {
    return items
        .filter((it) => it.kind !== 'same' && selected.has(it.name))
        .map((it) => ({ name: it.name, source: it.source, exists: it.exists, current: it.current, next: it.next }));
}

/** 'today' | 'yesterday' | 'tomorrow' | null (another day), by local calendar day. */
export function relativeDay(date: Date, now: Date): 'today' | 'yesterday' | 'tomorrow' | null {
    const day = (d: Date) => new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
    const diff = Math.round((day(date) - day(now)) / 86_400_000);
    if (diff === 0) return 'today';
    if (diff === -1) return 'yesterday';
    if (diff === 1) return 'tomorrow';
    return null;
}

export function formatClock(date: Date): string {
    return `${String(date.getHours()).padStart(2, '0')}:${String(date.getMinutes()).padStart(2, '0')}`;
}

export function formatMonthDay(date: Date): string {
    return `${date.getMonth() + 1}/${date.getDate()}`;
}

/** A Go zero time ("0001-01-01T00:00:00Z") means never. */
export function parseServerTime(value: string | null | undefined): Date | null {
    if (!value) return null;
    const d = new Date(value);
    if (Number.isNaN(d.getTime()) || d.getFullYear() <= 1) return null;
    return d;
}
