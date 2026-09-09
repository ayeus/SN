-- AyeusANN Seed Data: Catalogue models
-- Run after migrations 000001 and 000002.
--
-- This file seeds catalogue metadata only. It deliberately does NOT seed a
-- model_artifacts row: an artifact record asserts "these exact bytes, with this
-- digest, live at this URL", and there is nothing truthful to put in the
-- checksum column until the weights have actually been fetched and hashed.
-- The previous version wrote
--   'sha256:placeholder_will_be_updated_with_real_checksum'
-- which every downstream verification step would have had to either trust or
-- special-case. Artifacts are registered by the artifact ingest path, which
-- computes the digest from the bytes it downloaded.

INSERT INTO models (
    id, name, family, params_b, license, min_vram_gb,
    tiers_allowed, price_in_per_1m, price_out_per_1m,
    price_per_hour_inr, is_byo, quantization_presets
) VALUES (
    '550e8400-e29b-41d4-a716-446655440001',
    'llama-3.1-8b-instruct',
    'llama',
    8.0,
    'Llama 3.1 Community License',
    16,
    '{t1,t2,t3}',
    0.0800,   -- $0.08 per 1M input tokens
    0.2200,   -- $0.22 per 1M output tokens
    29.00,    -- ₹29/hr (T2 4090 rate)
    FALSE,
    '[
        {"name": "best", "quantization": "none", "min_vram_gb": 20, "description": "Full precision FP16"},
        {"name": "balanced", "quantization": "awq-4bit", "min_vram_gb": 8, "description": "AWQ 4-bit quantization"},
        {"name": "budget", "quantization": "gptq-4bit", "min_vram_gb": 6, "description": "GPTQ 4-bit for budget GPUs"}
    ]'::jsonb
) ON CONFLICT DO NOTHING;

INSERT INTO models (
    id, name, family, params_b, license, min_vram_gb,
    tiers_allowed, price_in_per_1m, price_out_per_1m,
    price_per_hour_inr, is_byo, quantization_presets
) VALUES (
    '550e8400-e29b-41d4-a716-446655440011',
    'qwen2.5-7b-instruct',
    'qwen',
    7.6,
    'Apache-2.0',
    16,
    '{t1,t2,t3}',
    0.0700,
    0.2000,
    29.00,
    FALSE,
    '[
        {"name": "best", "quantization": "none", "min_vram_gb": 18, "description": "Full precision FP16"},
        {"name": "balanced", "quantization": "awq-4bit", "min_vram_gb": 8, "description": "AWQ 4-bit quantization"}
    ]'::jsonb
) ON CONFLICT DO NOTHING;

INSERT INTO models (
    id, name, family, params_b, license, min_vram_gb,
    tiers_allowed, price_in_per_1m, price_out_per_1m,
    price_per_hour_inr, is_byo, quantization_presets
) VALUES (
    '550e8400-e29b-41d4-a716-446655440012',
    'mistral-7b-instruct-v0.3',
    'mistral',
    7.2,
    'Apache-2.0',
    16,
    '{t1,t2,t3}',
    0.0700,
    0.1900,
    29.00,
    FALSE,
    '[
        {"name": "best", "quantization": "none", "min_vram_gb": 18, "description": "Full precision FP16"},
        {"name": "balanced", "quantization": "awq-4bit", "min_vram_gb": 8, "description": "AWQ 4-bit quantization"}
    ]'::jsonb
) ON CONFLICT DO NOTHING;

-- Larger model, tier 1 and 2 only: a laptop cannot hold it.
INSERT INTO models (
    id, name, family, params_b, license, min_vram_gb,
    tiers_allowed, price_in_per_1m, price_out_per_1m,
    price_per_hour_inr, is_byo, quantization_presets
) VALUES (
    '550e8400-e29b-41d4-a716-446655440013',
    'llama-3.3-70b-instruct',
    'llama',
    70.6,
    'Llama 3.3 Community License',
    140,
    '{t1,t2}',
    0.3500,
    0.9500,
    180.00,
    FALSE,
    '[
        {"name": "best", "quantization": "none", "min_vram_gb": 160, "description": "Full precision FP16, 2x80GB"},
        {"name": "balanced", "quantization": "awq-4bit", "min_vram_gb": 48, "description": "AWQ 4-bit, single 48GB card"}
    ]'::jsonb
) ON CONFLICT DO NOTHING;

-- Seed a default price book for partner rate
INSERT INTO price_books (id, name, description) VALUES (
    '550e8400-e29b-41d4-a716-446655440003',
    'partner-rate',
    'Incubator portfolio partner rate: cost + thin margin'
) ON CONFLICT DO NOTHING;

INSERT INTO price_book_entries (price_book_id, gpu_model, tier, price_per_hour, price_in_per_1m, price_out_per_1m) VALUES
    ('550e8400-e29b-41d4-a716-446655440003', 'RTX 4090', 't1', 18.00, 0.0500, 0.1400),
    ('550e8400-e29b-41d4-a716-446655440003', 'RTX 5090', 't1', 26.00, 0.0600, 0.1600)
ON CONFLICT DO NOTHING;
