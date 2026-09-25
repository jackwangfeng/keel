-- +goose Up
CREATE TABLE merchants (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code         TEXT        NOT NULL UNIQUE,
    name         TEXT        NOT NULL,
    status       SMALLINT    NOT NULL DEFAULT 1,
    deleted_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE shop_settings (
    merchant_id       BIGINT   PRIMARY KEY REFERENCES merchants(id),
    domain            TEXT     UNIQUE,
    logo_url          TEXT,
    currency          TEXT     NOT NULL DEFAULT 'CNY',
    timezone          TEXT     NOT NULL DEFAULT 'Asia/Shanghai',
    auto_confirm_days SMALLINT NOT NULL DEFAULT 7,
    extra             JSONB    NOT NULL DEFAULT '{}',
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE categories (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id BIGINT      NOT NULL REFERENCES merchants(id),
    parent_id   BIGINT      REFERENCES categories(id),
    name        TEXT        NOT NULL,
    path        TEXT        NOT NULL,
    level       SMALLINT    NOT NULL DEFAULT 1,
    sort_order  INT         NOT NULL DEFAULT 0,
    status      SMALLINT    NOT NULL DEFAULT 1,
    deleted_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (id, merchant_id)
);

CREATE TABLE products (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id     BIGINT      NOT NULL REFERENCES merchants(id),
    category_id     BIGINT      NOT NULL,
    brand_id        BIGINT,
    title           TEXT        NOT NULL,
    subtitle        TEXT,
    description     TEXT,
    min_price_cents BIGINT      NOT NULL DEFAULT 0,
    max_price_cents BIGINT      NOT NULL DEFAULT 0,
    total_stock     INT         NOT NULL DEFAULT 0,
    sales_count     INT         NOT NULL DEFAULT 0,
    status          SMALLINT    NOT NULL DEFAULT 0,
    published_at    TIMESTAMPTZ,
    deleted_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (id, merchant_id),
    FOREIGN KEY (category_id, merchant_id) REFERENCES categories(id, merchant_id)
);
CREATE INDEX idx_products_listing
    ON products(merchant_id, category_id, status, published_at DESC)
    WHERE deleted_at IS NULL;

CREATE TABLE skus (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id  BIGINT      NOT NULL REFERENCES merchants(id),
    product_id   BIGINT      NOT NULL,
    sku_code     TEXT        NOT NULL,
    spec_values  JSONB       NOT NULL DEFAULT '{}',
    price_cents  BIGINT      NOT NULL,
    cost_cents   BIGINT      NOT NULL DEFAULT 0,
    weight_gram  INT         NOT NULL DEFAULT 0,
    image_url    TEXT,
    status       SMALLINT    NOT NULL DEFAULT 1,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_price_nonneg CHECK (price_cents >= 0),
    UNIQUE (id, merchant_id),
    FOREIGN KEY (product_id, merchant_id) REFERENCES products(id, merchant_id)
);
CREATE UNIQUE INDEX uk_skus_code ON skus(merchant_id, sku_code);

-- +goose Down
DROP TABLE skus;
DROP TABLE products;
DROP TABLE categories;
DROP TABLE shop_settings;
DROP TABLE merchants;
