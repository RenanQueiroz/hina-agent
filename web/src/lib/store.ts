import { create } from "zustand";
import { persist } from "zustand/middleware";

// Frontend-only UI preferences (NOT server state — that's TanStack Query's job).
interface UIPrefs {
  theme: "light" | "dark";
  toggleTheme: () => void;
}

export const useUIPrefs = create<UIPrefs>()(
  persist(
    (set) => ({
      theme: "light",
      toggleTheme: () =>
        set((s) => ({ theme: s.theme === "light" ? "dark" : "light" })),
    }),
    { name: "hina-ui-prefs" },
  ),
);
