import { Navigate, Route, Routes } from 'react-router';
import { AuthGate } from '@/auth';
import { Layout } from '@/components/Layout';
import { History, Queue } from '@/pages/Activity';
import { Destinations } from '@/pages/Destinations';
import { Library } from '@/pages/Library';
import { NotFound } from '@/pages/NotFound';
import { General } from '@/pages/settings/General';
import { Status } from '@/pages/system/Status';

export function App() {
  return (
    <AuthGate>
      <Routes>
        <Route element={<Layout />}>
          <Route index element={<Navigate to="/activity/queue" replace />} />
          <Route path="activity" element={<Navigate to="/activity/queue" replace />} />
          <Route path="activity/queue" element={<Queue />} />
          <Route path="activity/history" element={<History />} />
          <Route path="library" element={<Library />} />
          <Route path="destinations" element={<Destinations />} />
          <Route path="settings" element={<Navigate to="/settings/general" replace />} />
          <Route path="settings/general" element={<General />} />
          <Route path="system" element={<Navigate to="/system/status" replace />} />
          <Route path="system/status" element={<Status />} />
          <Route path="*" element={<NotFound />} />
        </Route>
      </Routes>
    </AuthGate>
  );
}
