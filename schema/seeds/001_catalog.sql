-- SpazeNode Seed Data: Catalog model for MVP
-- Run after migration 000001

-- Seed the first catalog model: Llama 3.1 8B Instruct
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
);

-- Seed a model artifact
INSERT INTO model_artifacts (
    id, model_id, version, artifact_url, size_bytes, checksum, format
) VALUES (
    '550e8400-e29b-41d4-a716-446655440002',
    '550e8400-e29b-41d4-a716-446655440001',
    'v1.0',
    's3://spazenode-models/llama-3.1-8b-instruct/v1.0/',
    16106127360,  -- ~15GB
    'sha256:placeholder_will_be_updated_with_real_checksum',
    'safetensors'
);

-- Seed a default price book for partner rate
INSERT INTO price_books (id, name, description) VALUES (
    '550e8400-e29b-41d4-a716-446655440003',
    'partner-rate',
    'Incubator portfolio partner rate: cost + thin margin'
);

INSERT INTO price_book_entries (price_book_id, gpu_model, tier, price_per_hour, price_in_per_1m, price_out_per_1m) VALUES
    ('550e8400-e29b-41d4-a716-446655440003', 'RTX 4090', 't1', 18.00, 0.0500, 0.1400),
    ('550e8400-e29b-41d4-a716-446655440003', 'RTX 5090', 't1', 26.00, 0.0600, 0.1600);
