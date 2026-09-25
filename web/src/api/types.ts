export type AuthRequired = 'enabled' | 'disabled_for_local_addresses';

export interface AuthStatus {
  setupRequired: boolean;
  authenticated: boolean;
  via?: 'apikey' | 'session' | 'local';
  username: string | null;
  authenticationRequired: AuthRequired;
}

export interface SystemStatus {
  appName: string;
  version: string;
  commit: string;
  buildDate: string;
  startTime: string;
  uptimeSeconds: number;
  databasePath: string;
  schemaVersion: number;
  configDir: string;
  goVersion: string;
  os: string;
  arch: string;
  isDocker: boolean;
  authenticationMethod: string;
  authenticationRequired: AuthRequired;
}

export interface GeneralSettings {
  apiKey: string;
  authenticationRequired: AuthRequired;
  authenticationMethod: string;
  bindAddress: string;
  port: number;
}
