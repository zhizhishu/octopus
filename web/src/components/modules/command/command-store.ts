import { create } from 'zustand';

type CommandStore = {
    open: boolean;
    setOpen: (open: boolean) => void;
    toggle: () => void;
};

/** Controls the global command palette so the toolbar button and the
 *  Ctrl/Cmd+K shortcut can both drive the same overlay. */
export const useCommandStore = create<CommandStore>((set) => ({
    open: false,
    setOpen: (open) => set({ open }),
    toggle: () => set((state) => ({ open: !state.open })),
}));
