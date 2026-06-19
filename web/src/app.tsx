import { useEffect } from "react";
import { useQuery } from "@tanstack/react-query";
import { RouterProvider } from "@tanstack/react-router";
import { api } from "./lib/api";
import { useUIPrefs } from "./lib/store";
import { router } from "./router";
import { Login } from "./pages/Login";
import { ChangePassword } from "./pages/ChangePassword";
import { Spinner } from "./components/ui";

// App gates the whole UI on the auth/session state. Login and the forced
// first-run password change live outside the router; the authed app (chat +
// admin) is the TanStack Router tree.
export function App() {
  const theme = useUIPrefs((s) => s.theme);
  useEffect(() => {
    document.documentElement.classList.toggle("dark", theme === "dark");
  }, [theme]);

  const me = useQuery({ queryKey: ["me"], queryFn: api.getMe });

  if (me.isLoading) {
    return (
      <div className="grid h-full place-items-center bg-zinc-50 dark:bg-zinc-950">
        <Spinner />
      </div>
    );
  }
  if (me.isError || !me.data) {
    return <Login onSuccess={() => me.refetch()} />;
  }
  if (me.data.must_change_password) {
    return <ChangePassword onDone={() => me.refetch()} />;
  }
  return <RouterProvider router={router} />;
}
