import { QueryClientProvider } from '@tanstack/react-query';
import { useState } from 'react';
import { Navigate, Route, Routes } from 'react-router';
import { AuthGate } from '@/auth';
import { Layout } from '@/components/Layout';
import { createQueryClient } from '@/lib/queryClient';
import { History } from '@/pages/activity/History';
import { JobDetail } from '@/pages/activity/JobDetail';
import { Queue } from '@/pages/activity/Queue';
import { Destinations } from '@/pages/destinations/Destinations';
import { FileDetail } from '@/pages/library/FileDetail';
import { Library } from '@/pages/library/Library';
import { SourceFiles } from '@/pages/library/SourceFiles';
import { NotFound } from '@/pages/NotFound';
import { Connect } from '@/pages/settings/Connect';
import { General } from '@/pages/settings/General';
import { Plex } from '@/pages/settings/Plex';
import { Tiers } from '@/pages/settings/Tiers';
import { Status } from '@/pages/system/Status';
import { Tasks } from '@/pages/system/Tasks';

export function App() {
  // One query cache per app instance (tests render a fresh App each time).
  const [queryClient] = useState(createQueryClient);
  return (
    <QueryClientProvider client={queryClient}>
      <AuthGate>
        <Routes>
          <Route element={<Layout />}>
            <Route index element={<Navigate to="/activity/queue" replace />} />
            <Route path="activity" element={<Navigate to="/activity/queue" replace />} />
            <Route path="activity/queue" element={<Queue />} />
            <Route path="activity/history" element={<History />} />
            <Route path="activity/jobs/:id" element={<JobDetail />} />
            <Route path="library" element={<Library />} />
            <Route path="library/sources/:id" element={<SourceFiles />} />
            <Route path="library/files/:id" element={<FileDetail />} />
            <Route path="destinations" element={<Destinations />} />
            <Route path="settings" element={<Navigate to="/settings/general" replace />} />
            <Route path="settings/general" element={<General />} />
            <Route path="settings/plex" element={<Plex />} />
            <Route path="settings/connect" element={<Connect />} />
            <Route path="settings/tiers" element={<Tiers />} />
            <Route path="system" element={<Navigate to="/system/status" replace />} />
            <Route path="system/status" element={<Status />} />
            <Route path="system/tasks" element={<Tasks />} />
            <Route path="*" element={<NotFound />} />
          </Route>
        </Routes>
      </AuthGate>
    </QueryClientProvider>
  );
}
