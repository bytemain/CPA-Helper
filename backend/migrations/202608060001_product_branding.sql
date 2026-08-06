-- +goose Up
ALTER TABLE app_settings ADD COLUMN product_name VARCHAR(64) NOT NULL DEFAULT '';
ALTER TABLE app_settings ADD COLUMN product_logo TEXT NOT NULL DEFAULT '';

-- +goose Down
