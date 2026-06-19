import {
  createRootRoute,
  createRoute,
  createRouter,
  Link,
  Outlet,
} from "@tanstack/react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { LogOut, MessageSquare, Moon, Settings, Sun } from "lucide-react";
import { api } from "./lib/api";
import { useUIPrefs } from "./lib/store";
import { Button } from "./components/ui";
import { ChatPage } from "./pages/Chat";
import { AdminPage } from "./pages/Admin";

const navLink =
  "inline-flex items-center gap-2 rounded-md px-3 py-2 text-sm font-medium text-zinc-700 hover:bg-zinc-100 dark:text-zinc-200 dark:hover:bg-zinc-800";
const navLinkActive = "bg-zinc-100 text-zinc-900 dark:bg-zinc-800 dark:text-white";

function Layout() {
  const me = useQuery({ queryKey: ["me"], queryFn: api.getMe });
  const cfg = useQuery({ queryKey: ["config"], queryFn: api.getConfig });
  const qc = useQueryClient();
  const { theme, toggleTheme } = useUIPrefs();

  const logout = async () => {
    await api.logout().catch(() => {});
    await qc.invalidateQueries({ queryKey: ["me"] });
  };

  return (
    <div className="flex h-full flex-col bg-zinc-50 text-zinc-900 dark:bg-zinc-950 dark:text-zinc-100">
      <header className="flex items-center gap-2 border-b border-zinc-200 px-4 py-2 dark:border-zinc-800">
        <span className="font-semibold">{cfg.data?.agent_name ?? "Hina"}</span>
        <nav className="ml-4 flex gap-1">
          <Link to="/" className={navLink} activeProps={{ className: navLinkActive }} activeOptions={{ exact: true }}>
            <MessageSquare size={16} /> Chat
          </Link>
          {me.data?.role === "admin" && (
            <Link to="/admin" className={navLink} activeProps={{ className: navLinkActive }}>
              <Settings size={16} /> Admin
            </Link>
          )}
        </nav>
        <div className="ml-auto flex items-center gap-2">
          <Button variant="ghost" onClick={toggleTheme} aria-label="Toggle theme">
            {theme === "dark" ? <Sun size={16} /> : <Moon size={16} />}
          </Button>
          <span className="text-sm text-zinc-500">{me.data?.username}</span>
          <Button variant="ghost" onClick={logout}>
            <LogOut size={16} /> Logout
          </Button>
        </div>
      </header>
      <main className="min-h-0 flex-1">
        <Outlet />
      </main>
    </div>
  );
}

const rootRoute = createRootRoute({ component: Layout });
const chatRoute = createRoute({ getParentRoute: () => rootRoute, path: "/", component: ChatPage });
const adminRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/admin",
  component: AdminPage,
});

const routeTree = rootRoute.addChildren([chatRoute, adminRoute]);

export const router = createRouter({ routeTree });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
