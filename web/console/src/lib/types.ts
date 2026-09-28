// Shapes returned by the platform API.

export type Money = { amount: string; currency?: string; micros: number };

export type Tier = "t1" | "t2" | "t3";

export type User = { id: string; email: string; name: string; email_verified: boolean };

export type Organization = {
  id: string;
  name: string;
  default_region: string;
  billing_country?: string;
  currency: string;
  is_business: boolean;
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
  price_in_per_1m: Money;
  price_out_per_1m: Money;
  price_per_hour_inr?: Money;
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
  cost: Money;
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
  earnings_total: Money;
  earnings_today: Money;
};

export type GpuSku = {
  id: string;
  gpu_model: string;
  vram_gb: number;
  tdp_watts: number;
  tier: Tier;
  is_spot: boolean;
  price_per_hour_inr: Money;
  price_per_hour_usd: Money;
  online_gpus: number;
  free_gpus: number;
  availability: number;
};

export type Pricing = {
  price_currency: string;
  fx_from_usd: Record<string, string>;
  host_share_percent: number;
  spot_price_percent: number;
  gpu_skus: GpuSku[];
};

export type NetworkStats = {
  hosts_online: number;
  gpus_online: number;
  vram_gb_online: number;
  gpus_by_tier: Record<Tier, number>;
  gpus_by_region: { region: string; gpus: number }[];
  deployments_serving: number;
  requests_24h: number;
  tokens_24h: number;
};

export type Region = { code: string; name: string; country: string };

export type Wallet = {
  balance: Money;
  currency: string;
  credit_limit: Money;
  low_balance_threshold: Money;
  low_balance: boolean;
  spend_24h: Money;
  spend_30d: Money;
  fx_from_usd: string;
  topup: { razorpay: boolean; test_credit: boolean; key_id: string };
};

export type LedgerEntry = {
  entry_id: string;
  delta: Money;
  balance_after: Money;
  kind: string;
  description?: string;
  created_at: string;
};
