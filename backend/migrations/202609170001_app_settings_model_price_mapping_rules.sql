-- +goose Up
-- Configurable model price mapping rules: an operator can declare that a model the price
-- dictionary does not know (a reverse-proxied `devin/swe-2`) is priced like an equivalent model it
-- does know (`moonshot` / `moonshot/kimi-k3`), without a code change per model. The rules are an
-- ordered JSON list of {source_provider, source_model, target_provider, target_model} objects
-- stored on the single app_settings row; an empty value (the default) means no mapping at all, so
-- every existing deployment prices records exactly as before.
ALTER TABLE app_settings ADD COLUMN model_price_mapping_rules TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE app_settings DROP COLUMN model_price_mapping_rules;
