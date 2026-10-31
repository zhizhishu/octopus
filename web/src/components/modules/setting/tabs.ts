// 设置页顶部分类标签：纯数据 + URL hash 读写，不依赖 React，便于单测。

export const SETTING_TAB_IDS = ['general', 'relay', 'identity', 'users', 'system', 'logs'] as const;

export type SettingTabId = (typeof SETTING_TAB_IDS)[number];

export const DEFAULT_SETTING_TAB: SettingTabId = 'general';

// 标签上的小圆标：这个分类里有几张卡片。改了 index.tsx 里某个标签的卡片数，这里要跟着改
// （tests/setting-tabs.test.cjs 会按渲染结果核对）。
export const SETTING_TAB_CARD_COUNTS: Record<SettingTabId, number> = {
    general: 4,
    relay: 5,
    identity: 5,
    users: 3,
    system: 3,
    logs: 2,
};

export function isSettingTabId(value: string): value is SettingTabId {
    return (SETTING_TAB_IDS as readonly string[]).includes(value);
}

/** '#relay' / 'relay' / '#/relay' → 'relay'；认不出的一律回到默认标签。 */
export function parseSettingTabHash(hash: string | null | undefined): SettingTabId {
    let raw = (hash ?? '').replace(/^#\/?/, '');
    // 手改地址栏可能带坏掉的 % 编码（如 #%E0），decodeURIComponent 会抛错把整页弄崩，这里兜住。
    try {
        raw = decodeURIComponent(raw);
    } catch {
        return DEFAULT_SETTING_TAB;
    }
    raw = raw.trim().toLowerCase();
    return isSettingTabId(raw) ? raw : DEFAULT_SETTING_TAB;
}

export function settingTabHash(id: SettingTabId): string {
    return `#${id}`;
}

type HistoryLike = { replaceState: (data: unknown, unused: string, url?: string | null) => void };
type LocationLike = { pathname: string; search: string; hash: string };

/**
 * 把当前标签写进 URL hash。用 replaceState 而不是直接改 location.hash：
 * 不往浏览器历史里堆记录（后退键不会在标签间来回跳），也不触发 hashchange。
 */
export function writeSettingTabHash(location: LocationLike, history: HistoryLike, id: SettingTabId): void {
    const next = settingTabHash(id);
    if (location.hash === next) return;
    history.replaceState(null, '', `${location.pathname}${location.search}${next}`);
}

/**
 * 离开设置页时清掉设置标签的 hash（应用其它页面不走 URL 路由，留着会变成误导的尾巴）。
 * 只清认得的设置标签 hash，别的 hash 原样保留。
 */
export function clearSettingTabHash(location: LocationLike, history: HistoryLike): void {
    const raw = location.hash.replace(/^#\/?/, '').toLowerCase();
    if (!isSettingTabId(raw)) return;
    history.replaceState(null, '', `${location.pathname}${location.search}`);
}
