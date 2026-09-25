// 独立 module：这个例子需要 cgo 与 Rust 工具链，不应该把这个要求
// 传染给将来的主 module。
module github.com/keel/examples/dtmrs-embedded

go 1.25.0

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/pgx/v5 v5.11.0 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/text v0.29.0 // indirect
)
