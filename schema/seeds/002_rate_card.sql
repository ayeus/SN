-- AyeusANN Seed Data: public GPU rate card (PRD §9 launch rate card, mid-2026)
-- Read by the deploy wizard (GPU cards with ₹/hr + $/hr + live availability,
-- PRD F-3) and the host earnings calculator (net of electricity, PRD F-13).
-- tdp_watts is the board power used to estimate a host's electricity cost.

INSERT INTO gpu_skus (id, gpu_model, match_pattern, vram_gb, tdp_watts, tier, price_per_hour_inr, price_per_hour_usd, is_spot, display_order) VALUES
    ('h100-sxm-t1',  'NVIDIA H100 SXM 80GB', '%H100%',          80, 700, 't1', 210.00, 2.4900, FALSE, 10),
    ('a100-80g-t1',  'NVIDIA A100 80GB',     '%A100%80%',       80, 400, 't1', 117.00, 1.3900, FALSE, 20),
    ('rtx-5090-t1',  'NVIDIA RTX 5090',      '%5090%',          32, 575, 't1',  42.00, 0.5000, FALSE, 30),
    ('rtx-5090-t2',  'NVIDIA RTX 5090',      '%5090%',          32, 575, 't2',  42.00, 0.5000, FALSE, 40),
    ('rtx-4090-t2',  'NVIDIA RTX 4090',      '%4090%',          24, 450, 't2',  29.00, 0.3400, FALSE, 50),
    ('rtx-4090-t3',  'NVIDIA RTX 4090 (spot)', '%4090%',        24, 450, 't3',  16.00, 0.1900, TRUE,  60),
    ('rtx-3090-t3',  'NVIDIA RTX 3090 (spot)', '%3090%',        24, 350, 't3',  11.00, 0.1300, TRUE,  70)
ON CONFLICT (id) DO UPDATE SET
    gpu_model = EXCLUDED.gpu_model,
    match_pattern = EXCLUDED.match_pattern,
    vram_gb = EXCLUDED.vram_gb,
    tdp_watts = EXCLUDED.tdp_watts,
    tier = EXCLUDED.tier,
    price_per_hour_inr = EXCLUDED.price_per_hour_inr,
    price_per_hour_usd = EXCLUDED.price_per_hour_usd,
    is_spot = EXCLUDED.is_spot,
    display_order = EXCLUDED.display_order;
