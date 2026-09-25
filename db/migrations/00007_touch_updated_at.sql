-- +goose Up
-- +goose StatementBegin

-- 通用 updated_at 触发器（数据模型 §1 定义了它，但此前没有任何迁移建过它）。
--
-- 在此之前，全部七张带 updated_at 的表都只有 DEFAULT now() —— 也就是说那一列
-- 记的是**创建时间**，改一行不会动它。这种错不会报警，只会让「这条记录最后
-- 什么时候变过」这个问题在半年后得到一个自信而错误的答案。
--
-- 一次给全部带这一列的表挂上，不给任何一张开先例：只挂一部分的话，
-- 「哪些表的 updated_at 是自动的」就变成一张要靠人记的表，而那正是这个仓库
-- 一直在消灭的东西。新表由 TestUpdatedAtIsMaintainedByTrigger 兜住 ——
-- 它从系统目录枚举带 updated_at 的表，少挂一张就红。
CREATE OR REPLACE FUNCTION touch_updated_at() RETURNS trigger AS $$
BEGIN NEW.updated_at = now(); RETURN NEW; END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- +goose StatementBegin
DO $$
DECLARE t text;
BEGIN
    FOR t IN
        SELECT c.relname
          FROM pg_class c
          JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE n.nspname = 'public' AND c.relkind = 'r'
           AND EXISTS (SELECT 1 FROM pg_attribute a
                        WHERE a.attrelid = c.oid
                          AND a.attname = 'updated_at' AND a.attnum > 0)
    LOOP
        EXECUTE format(
            'CREATE OR REPLACE TRIGGER touch_%1$s_updated_at '
            'BEFORE UPDATE ON %1$I FOR EACH ROW EXECUTE FUNCTION touch_updated_at()', t);
    END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
DECLARE t text;
BEGIN
    FOR t IN
        SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE n.nspname = 'public' AND c.relkind = 'r'
           AND EXISTS (SELECT 1 FROM pg_attribute a
                        WHERE a.attrelid = c.oid
                          AND a.attname = 'updated_at' AND a.attnum > 0)
    LOOP
        EXECUTE format('DROP TRIGGER IF EXISTS touch_%1$s_updated_at ON %1$I', t);
    END LOOP;
END $$;
DROP FUNCTION IF EXISTS touch_updated_at();
-- +goose StatementEnd
