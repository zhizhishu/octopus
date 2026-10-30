import { create } from 'zustand';

// 别的页面跳到日志页时预先带上的筛选（用户页「看他的日志」、仪表盘点一行）。
// 只有这一个入口：写进来 → 切到日志页 → 日志页挂载时读一次就清掉，所以只影响这一次跳转。
// 不放进 URL：这个应用是单页切换（nav-store），没有路由参数。
export interface LogPreset {
    userId?: number;
    apiKeyId?: number;
    model?: string;
    search?: string;
    /** 日期（YYYY-MM-DD，含首尾）；不带就用日志页默认的近 7 天。 */
    startDate?: string;
    endDate?: string;
}

interface LogPresetState {
    preset: LogPreset | null;
    setPreset: (preset: LogPreset | null) => void;
    /** 只带用户（用户页「看他的日志」）。 */
    setUserID: (userID: number | undefined) => void;
    clear: () => void;
}

export const useLogPresetStore = create<LogPresetState>()((set) => ({
    preset: null,
    setPreset: (preset) => set({ preset }),
    setUserID: (userID) => set({ preset: userID === undefined ? null : { userId: userID } }),
    clear: () => set({ preset: null }),
}));
