export interface AssetsFetcher {
  fetch(request: Request | string): Promise<Response>;
}

export interface Env {
  ASSETS?: AssetsFetcher;
  GITHUB_REPO?: string;
  GITHUB_TOKEN?: string;
  MAGPIE_APP_ID?: string;
  MAGPIE_INSTALLATION_ID?: string;
  MAGPIE_APP_PRIVATE_KEY?: string;
  CAPY_APP_ID?: string;
  CAPY_INSTALLATION_ID?: string;
  CAPY_APP_PRIVATE_KEY?: string;
  WEBHOOK_SECRET?: string;
  LANDER_WAKE_URL?: string;
  LANDER_WAKE_SECRET?: string;
  CACHE_TTL_SECONDS?: string;
}

export const USER_AGENT = "azure-winged-magpie-worker";

// Everything magpie links to or reads lives in the user repository: it carries the workspaces and
// landing requests and a copy of refs/suda/{main,history} (kept current by the lander), so
// redirects never depend on read access to the trunk repository.
export const DEFAULT_REPO = "gosuda/1";
