-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION current_merchant() RETURNS BIGINT
LANGUAGE plpgsql STABLE AS $$
DECLARE v TEXT := nullif(current_setting('app.merchant_id', true), '');
BEGIN
    IF v IS NULL THEN
        RAISE EXCEPTION '租户上下文未设置：事务开始时必须 SET LOCAL app.merchant_id'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN v::BIGINT;
END $$;
-- +goose StatementEnd

ALTER TABLE categories ENABLE ROW LEVEL SECURITY;
ALTER TABLE categories FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON categories USING (merchant_id = current_merchant());

ALTER TABLE products ENABLE ROW LEVEL SECURITY;
ALTER TABLE products FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON products USING (merchant_id = current_merchant());

ALTER TABLE skus ENABLE ROW LEVEL SECURITY;
ALTER TABLE skus FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON skus USING (merchant_id = current_merchant());

-- +goose Down
DROP POLICY tenant ON skus;
DROP POLICY tenant ON products;
DROP POLICY tenant ON categories;
DROP FUNCTION current_merchant();
