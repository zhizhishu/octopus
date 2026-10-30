import { useSyncExternalStore } from 'react';

// 与 Tailwind 的 md 断点一致：< 768px 视为手机布局。
const QUERY = '(max-width: 767px)';

function subscribe(onChange: () => void) {
    if (typeof window === 'undefined' || !window.matchMedia) return () => undefined;
    const mql = window.matchMedia(QUERY);
    mql.addEventListener('change', onChange);
    return () => mql.removeEventListener('change', onChange);
}

function getSnapshot() {
    return typeof window !== 'undefined' && !!window.matchMedia && window.matchMedia(QUERY).matches;
}

/** 只用于手机端换布局（分组折叠、吸底按钮）；桌面端返回 false，渲染保持原样。 */
export function useIsMobile(): boolean {
    return useSyncExternalStore(subscribe, getSnapshot, () => false);
}
