import { Activity, Database, HardDrive, LogOut, Menu, Server, Settings } from 'lucide-react';
import { useState, type ComponentType } from 'react';
import { NavLink, Outlet, useLocation } from 'react-router';
import { useAuth } from '@/auth';
import { Logo } from './Logo';

interface NavItem {
  label: string;
  to: string;
  icon: ComponentType<{ className?: string }>;
  children?: { label: string; to: string }[];
}

// Left navigation in the *arr style: sections with sub-pages shown when the section is open.
export const NAV: NavItem[] = [
  {
    label: 'Activity',
    to: '/activity',
    icon: Activity,
    children: [
      { label: 'Queue', to: '/activity/queue' },
      { label: 'History', to: '/activity/history' },
    ],
  },
  { label: 'Library', to: '/library', icon: Database },
  { label: 'Destinations', to: '/destinations', icon: HardDrive },
  {
    label: 'Settings',
    to: '/settings',
    icon: Settings,
    children: [
      { label: 'General', to: '/settings/general' },
      { label: 'Plex', to: '/settings/plex' },
      { label: 'Connect', to: '/settings/connect' },
    ],
  },
  {
    label: 'System',
    to: '/system',
    icon: Server,
    children: [
      { label: 'Status', to: '/system/status' },
      { label: 'Tasks', to: '/system/tasks' },
    ],
  },
];

export function Layout() {
  const { status, logout } = useAuth();
  const { pathname } = useLocation();
  // On narrow screens the navigation is a drawer opened from the header; it closes on navigation.
  const [menuFor, setMenuFor] = useState<string | null>(null);
  const menuOpen = menuFor === pathname;
  return (
    <div className="flex h-full flex-col">
      <header className="flex h-14 shrink-0 items-center justify-between bg-[#1f2a1f] px-4">
        <div className="flex items-center gap-2">
          <button
            type="button"
            className="-ml-2 rounded p-2 text-ink-muted hover:bg-white/10 hover:text-ink md:hidden"
            aria-label="Menu"
            aria-expanded={menuOpen}
            aria-controls="main-nav"
            onClick={() => setMenuFor(menuOpen ? null : pathname)}
          >
            <Menu className="h-5 w-5" aria-hidden="true" />
          </button>
          <Logo className="h-8 w-8" />
          <span className="text-lg font-semibold tracking-wide">Bunkarr</span>
        </div>
        {status.via === 'session' && (
          <button
            onClick={() => void logout()}
            className="flex items-center gap-2 rounded px-3 py-1.5 text-sm text-ink-muted hover:bg-white/10 hover:text-ink"
            title={`Signed in as ${status.username ?? ''}`}
          >
            <LogOut className="h-4 w-4" /> Log out
          </button>
        )}
      </header>
      <div className="flex min-h-0 flex-1">
        <nav
          id="main-nav"
          aria-label="Main"
          onClick={(e) => {
            if (e.target instanceof Element && e.target.closest('a')) setMenuFor(null);
          }}
          className={`${menuOpen ? 'absolute bottom-0 left-0 top-14 z-40 block shadow-2xl' : 'hidden'} w-52 shrink-0 overflow-y-auto bg-sidebar py-2 md:static md:block md:shadow-none`}
        >
          {NAV.map((item) => {
            const open = pathname.startsWith(item.to);
            const Icon = item.icon;
            return (
              <div key={item.to} className={open ? 'border-l-4 border-accent bg-panel' : 'border-l-4 border-transparent'}>
                <NavLink
                  to={item.children ? item.children[0].to : item.to}
                  className={`flex items-center gap-3 px-4 py-2.5 ${open ? 'text-accent' : 'text-ink hover:text-accent'}`}
                >
                  <Icon className="h-4 w-4" />
                  {item.label}
                </NavLink>
                {open &&
                  item.children?.map((c) => (
                    <NavLink
                      key={c.to}
                      to={c.to}
                      className={({ isActive }) =>
                        `block py-1.5 pl-11 text-sm ${isActive ? 'text-accent' : 'text-ink-muted hover:text-ink'}`
                      }
                    >
                      {c.label}
                    </NavLink>
                  ))}
              </div>
            );
          })}
        </nav>
        <main className="min-w-0 flex-1 overflow-y-auto">
          <Outlet />
        </main>
      </div>
    </div>
  );
}
