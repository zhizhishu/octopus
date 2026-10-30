import { create } from 'zustand';

// 别的页面跳到日志页时预先带上的筛选（例如用户页「看他的日志」）。
// 日志页挂载时读一次就清掉，所以只影响这一次跳转。
interface LogPresetState {
    userID?: number;
    setUserID: (userID: number | undefined) => void;
    clear: () => void;
}

export const useLogPresetStore = create<LogPresetState>()((set) => ({
    userID: undefined,
    setUserID: (userID) => set({ userID }),
    clear: () => set({ userID: undefined }),
}));
