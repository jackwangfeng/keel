-- +goose Up
-- +goose StatementBegin

-- categories.parent_id 补成复合外键。
--
-- 00001 里写的是 `parent_id BIGINT REFERENCES categories(id)`，单列引用。
-- 设计文档（§3）里它一直是 `FOREIGN KEY (parent_id, merchant_id)
-- REFERENCES categories(id, merchant_id)`——脱节的是迁移，不是设计。
--
-- 实测过这不是理论问题：改之前，把 B 商家的分类挂到 A 商家的分类下面，
-- 数据库照单全收。RLS 拦不住它——两行分别在各自租户的可见范围内，
-- 插入时每一行都合法，错的是它们之间的那条边。
--
-- 落点是现成的：00001 建表时就带了 UNIQUE (id, merchant_id)。
ALTER TABLE categories DROP CONSTRAINT categories_parent_id_fkey;

ALTER TABLE categories
    ADD CONSTRAINT categories_parent_id_merchant_id_fkey
    FOREIGN KEY (parent_id, merchant_id)
    REFERENCES categories(id, merchant_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE categories DROP CONSTRAINT categories_parent_id_merchant_id_fkey;
ALTER TABLE categories
    ADD CONSTRAINT categories_parent_id_fkey
    FOREIGN KEY (parent_id) REFERENCES categories(id);
-- +goose StatementEnd
