import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Activity, AlertTriangle, Boxes, Layers, ListChecks, LogOut, Settings as Cog } from "lucide-react";
import { useState } from "react";
import { NavLink, Navigate, Route, Routes } from "react-router-dom";
import { api } from "./api";
import AttentionPage from "./pages/Attention";
import Dashboard from "./pages/Dashboard";
import Events from "./pages/Events";
import Jobs from "./pages/Jobs";
import SeasonsPage from "./pages/Seasons";
import SettingsPage from "./pages/Settings";

function AuthForm({ setup }: { setup: boolean }) {
  const qc = useQueryClient();
  const [u, setU] = useState("");
  const [p, setP] = useState("");
  const [err, setErr] = useState("");
  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    try {
      await (setup ? api.setup(u, p) : api.login(u, p));
      qc.invalidateQueries({ queryKey: ["me"] });
    } catch (x) {
      setErr((x as Error).message);
    }
  };
  return (
    <div className="flex min-h-screen items-center justify-center p-4">
      <form onSubmit={submit} className="card w-full max-w-sm space-y-4 p-6">
        <div className="flex items-center gap-2 text-lg font-semibold"><Boxes className="text-accent" /> consolidarr</div>
        <p className="text-sm text-muted">{setup ? "Create the admin account to get started." : "Sign in to continue."}</p>
        <div><label className="label">Username</label><input className="input" value={u} onChange={(e) => setU(e.target.value)} autoFocus /></div>
        <div><label className="label">Password</label><input className="input" type="password" value={p} onChange={(e) => setP(e.target.value)} /></div>
        {err && <div className="text-sm text-bad">{err}</div>}
        <button className="btn btn-primary w-full justify-center">{setup ? "Create account" : "Sign in"}</button>
      </form>
    </div>
  );
}

const nav = [
  { to: "/", label: "Dashboard", icon: Activity },
  { to: "/seasons", label: "Seasons", icon: Layers },
  { to: "/jobs", label: "Replacements", icon: ListChecks },
  { to: "/attention", label: "Needs attention", icon: AlertTriangle },
  { to: "/events", label: "Activity", icon: Activity },
  { to: "/settings", label: "Settings", icon: Cog },
];

export default function App() {
  const qc = useQueryClient();
  const me = useQuery({ queryKey: ["me"], queryFn: api.me });
  if (me.isLoading) return <div className="p-8 text-muted">Loading…</div>;
  if (me.data?.setupRequired) return <AuthForm setup />;
  if (!me.data?.authenticated) return <AuthForm setup={false} />;
  return (
    <div className="flex min-h-screen">
      <aside className="hidden w-56 shrink-0 flex-col border-r border-line bg-panel p-3 md:flex">
        <div className="mb-4 flex items-center gap-2 px-2 py-2 text-lg font-semibold"><Boxes className="text-accent" /> consolidarr</div>
        <nav className="space-y-1">
          {nav.map(({ to, label, icon: Icon }) => (
            <NavLink key={to} to={to} end={to === "/"}
              className={({ isActive }) => `flex items-center gap-2 rounded-md px-3 py-2 text-sm ${isActive ? "bg-accent/15 text-white" : "text-muted hover:text-white"}`}>
              <Icon size={16} /> {label}
            </NavLink>
          ))}
        </nav>
        <div className="mt-auto space-y-2 px-2 text-xs text-muted">
          <button className="flex items-center gap-2 hover:text-white" onClick={async () => { await api.logout(); qc.invalidateQueries({ queryKey: ["me"] }); }}>
            <LogOut size={14} /> Sign out
          </button>
          <div>v{me.data?.version}</div>
        </div>
      </aside>
      <main className="min-w-0 flex-1 p-4 md:p-6">
        <Routes>
          <Route path="/" element={<Dashboard />} />
          <Route path="/seasons" element={<SeasonsPage />} />
          <Route path="/jobs" element={<Jobs />} />
          <Route path="/attention" element={<AttentionPage />} />
          <Route path="/events" element={<Events />} />
          <Route path="/settings" element={<SettingsPage />} />
          <Route path="*" element={<Navigate to="/" />} />
        </Routes>
      </main>
    </div>
  );
}
