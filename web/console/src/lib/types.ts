// Shapes returned by the platform API.

export type Tier = "t1" | "t2" | "t3";

export type User = { id: string; email: string; name: string; email_verified: boolean };

export type Organization = {
  id: string;
  name: string;
  default_region: string;
  country?: string;
};

export type Me = { user: User; organization: Organization; role: string; is_platform_admin: boolean };

export type Model = {
  id: string;
  name: string;
  family: string;
  params_b: number;
  license: string;
  min_vram_gb: number;
  tiers_allowed: Tier[];
  is_byo: boolean;
  quantization_presets: { name: string; quantization: string; min_vram_gb: number; description: string }[];
  runtime_refs: Record<string, { model: string; quantization?: string; min_vram_gb?: number }>;
  description?: string;
  context_length?: number;
};

export type UsageSummary = {
  requests: number;
  errors: number;
  input_tokens: number;
  output_tokens: number;
};

export type DeploymentState =
  | "pending"
  | "scheduling"
  | "pulling"
  | "loading"
  | "warming"
  | "serving"
  | "degraded"
  | "paused"
  | "stopping"
  | "stopped"
  | "failed";

export type Deployment = {
  id: string;
  name: string;
  model_id: string;
  model_name: string;
  state: DeploymentState;
  desired_state: "running" | "paused" | "stopped";
  tier: Tier;
  region: string;
  min_replicas: number;
  max_replicas: number;
  resident_in: boolean;
  endpoint?: string;
  last_error?: string;
  state_changed_at: string;
  created_at: string;
  replicas_serving: number;
  replicas_active: number;
  usage_24h?: UsageSummary;
};

export type Replica = {
  id: string;
  state: string;
  detail?: string;
  last_error?: string;
  tier: Tier;
  region: string;
  gpu_model?: string;
  started_at?: string;
  total_requests: number;
  failed_requests: number;
  created_at: string;
};

export type DeploymentEvent = {
  id: number;
  replica_id?: string;
  kind: "state" | "replica" | "info" | "error";
  state?: string;
  message: string;
  created_at: string;
};

export type ApiKey = {
  id: string;
  name: string;
  prefix: string;
  scope: "org" | "deployment";
  deployment_id?: string;
  last_used_at?: string;
  created_at: string;
};

export type GPU = { id: string; model: string; vram_gb: number; driver_version?: string; uuid: string; status: string };

export type Host = {
  id: string;
  name: string;
  hostname?: string;
  tier: Tier;
  region: string;
  reputation: number;
  status: string;
  kyc_status: string;
  agent_version?: string;
  last_heartbeat_at?: string;
  os?: string;
  runtime?: string;
  runtime_healthy: boolean;
  cached_models: string[];
  paused: boolean;
  probation_until?: string;
  created_at: string;
};

export type HostSummary = Host & {
  online: boolean;
  gpus: GPU[];
  active_jobs: number;
  requests_total: number;
  requests_today: number;
};

export type WorkCount = { requests: number; tokens: number };

export type HostActivity = {
  summary: { today: WorkCount; month_to_date: WorkCount; lifetime: WorkCount };
  daily: { date: string; requests: number; tokens: number }[];
};

export type NetworkStats = {
  hosts_online: number;
  gpus_online: number;
  vram_gb_online: number;
  gpus_by_tier: Record<Tier, number>;
  gpus_free_by_tier: Record<Tier, number>;
  gpus_by_region: { region: string; gpus: number }[];
  deployments_serving: number;
  requests_24h: number;
  tokens_24h: number;
};

export type Region = { code: string; name: string; country: string };
