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
    /**
     * 统计时区里算好的起止（unix 秒，两头都含）。日志页默认按浏览器时区切天，
     * 两边时区不同时日界会错开几小时；带了这两个就按它们查，保证点进去的条数对得上。
     */
    startTime?: number;
    endTime?: number;
}

/**
 * 日志页实际用的时间范围：日期还是带进来的那两天，就用带进来的精确起止；
 * 用户改过日期（或没带），交回日志页自己的按天换算。
 */
export function entryTimeRange(preset: LogPreset | null | undefined, startDate: string, endDate: string): { startTime: number; endTime: number } | null {
    if (!preset || preset.startTime === undefined || preset.endTime === undefined) return null;
    if (preset.startDate !== startDate || preset.endDate !== endDate) return null;
    if (!(preset.endTime >= preset.startTime)) return null;
    return { startTime: preset.startTime, endTime: preset.endTime };
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
