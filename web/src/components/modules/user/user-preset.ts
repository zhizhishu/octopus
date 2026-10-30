import { create } from 'zustand';

// 别的页面跳到用户页时要先打开的那个用户（仪表盘点用户名）。
// 用户页挂载时读一次、打开他的抽屉，然后清掉。
interface UserPresetState {
    focusUserID: number | null;
    setFocusUserID: (userID: number | null) => void;
    clear: () => void;
}

export const useUserPresetStore = create<UserPresetState>()((set) => ({
    focusUserID: null,
    setFocusUserID: (focusUserID) => set({ focusUserID }),
    clear: () => set({ focusUserID: null }),
}));
