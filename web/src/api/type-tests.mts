// 负面编译测试：证明这个 SDK 的类型约束**真的存在**。
//
// ## 为什么需要这个文件
//
// 「`tsc` 通过了」只证明代码合法，不证明类型有约束力。一个把所有东西都推成
// `any` 的 SDK 同样能让 tsc 通过 —— 而它一个错误也拦不住。
//
// 所以这里写的是一批**本该编译失败**的调用，每一条上面挂 `@ts-expect-error`。
// 这个指令是反着的：它要求下一行**必须**报错；如果那一行没报错，tsc 反而会以
// "Unused '@ts-expect-error' directive" 失败。于是每一条都是一个可执行的断言，
// 而不是一句注释。
//
// 这个文件在 tsconfig 的 include 范围里，`make schema-check` 会编译它。
// 它没有任何运行时代码（客户端和数据都是 `declare` 出来的），永远不会被执行。
//
// ## 验证过它有区分力
//
// 变异验证过两次（改坏 → 确认对应的断言变红 → 恢复）：
//   - 把 client.mts 里 `RequestOptions` 的 query 槽换成 `Record<string, unknown>`
//     → 「query 传错类型」那两条（底层原语上的）立刻变成 "Unused '@ts-expect-error'"
//   - 把 `OptionalArgs` 改成一律 `[options?: ...]`
//     → 「/products/{product_id} 的参数对象不能省」那条变成 "Unused"
// 也就是说这些断言确实咬在 SDK 的类型上，不是咬在别的什么错误上。

import type {
    KeelClient,
    MethodsOf,
    PageMeta,
    PathsWith,
    Problem,
    ProblemError,
    ProductListPage,
    ProductListQuery,
    ProductSummary,
    ResponseBodyOf,
} from "./client.mts";

// ---------------------------------------------------------------------------
// 编译期断言的工具
// ---------------------------------------------------------------------------

/**
 * 严格类型相等。用两个泛型函数签名的互相赋值来判断，而不是双向 extends ——
 * 后者会把 `any` 判成等于任何类型，于是「SDK 把一切推成 any」这个最该被抓到的
 * 退化方式恰好逃掉。
 */
type Equal<X, Y> = (<T>() => T extends X ? 1 : 2) extends <T>() => T extends Y ? 1 : 2 ? true : false;

type Expect<T extends true> = T;

/** 一个对象类型的必填键。 */
type RequiredKeys<T> = { [K in keyof T]-?: object extends Pick<T, K> ? never : K }[keyof T];

// ---------------------------------------------------------------------------
// 正面断言：推导出来的确实是契约里那个形状，不是 any、不是 never
// ---------------------------------------------------------------------------

// GET /products 的响应体 = PageMeta 的三个字段 + items。
// 这条如果退化成 any 或 unknown，Equal 立刻为 false。
type _ProductListPageShape = Expect<Equal<ProductListPage, PageMeta & { items: ProductSummary[] }>>;

// 分页元信息原样透出，SDK 没有在中间重新包装。
type _PageMetaShape = Expect<Equal<PageMeta, { page: number; page_size: number; total: number }>>;

// query 参数是契约里那六个，全部可选。
type _ProductListQueryShape = Expect<
    Equal<
        ProductListQuery,
        {
            page?: number;
            page_size?: number;
            category_id?: number;
            sort?: "default" | "price_asc" | "price_desc" | "sales_desc" | "newest";
            min_price_cents?: number;
            max_price_cents?: number;
        }
    >
>;

// isProblem 只查 type / title / status 三个字段，而这三个正是契约里 Problem 的
// 必填字段。契约给 Problem 加一个必填字段，这条断言红 —— 那时 isProblem 要跟着改，
// 否则它会把一个缺字段的响应判成合法 Problem。
type _ProblemRequired = Expect<Equal<RequiredKeys<Problem>, "type" | "title" | "status">>;

// 契约里 /products 只定义了 GET。
type _ProductsMethods = Expect<Equal<MethodsOf<"/products">, "get">>;

// 204 没有响应体，推导结果必须是 undefined 而不是 never 或 any
// （never 会让 `const x = await client.request("delete", "/cart")` 之后的任何
// 使用都莫名其妙地通过）。
type _NoContentBody = Expect<Equal<ResponseBodyOf<"/cart", "delete">, undefined>>;

// PathsWith<"get"> 是个真实的路径联合，包含 /products。
type _ProductsIsGettable = Expect<"/products" extends PathsWith<"get"> ? true : false>;

// ---------------------------------------------------------------------------
// 负面断言：下面每一行都**必须**编译失败
// ---------------------------------------------------------------------------

declare const client: KeelClient;
declare const page: ProductListPage;
declare const problemError: ProblemError;

// --- 响应体：字段名拿错 ---

// 正确的写法编译得过（对照组：没有它，下面的失败可能只是因为整个类型都坏了）。
const _okPage: number = page.page_size;
const _okTotal: number = page.total;

// @ts-expect-error 契约里叫 page_size，不叫 pageSize
page.pageSize;

// @ts-expect-error 契约里叫 total，没有 totalCount
page.totalCount;

// --- 响应体：把 PageMeta 当数组 ---

// @ts-expect-error 响应体是 PageMeta & { items }，不是数组；items 才是数组
page.map((x: ProductSummary) => x);

// @ts-expect-error 同上：整个响应体不能赋给 ProductSummary[]
const _notAnArray: ProductSummary[] = page;

// 对照组：items 确实是数组。
const _items: ProductSummary[] = page.items;

// --- 列表项：字段名与取值范围 ---

// @ts-expect-error 契约里是 min_price_cents（金额单位「分」，见契约设计约定 1）
page.items[0]?.minPriceCents;

// status 是 0 | 1 | 2 的字面量联合，不是 number。
declare const summary: ProductSummary;
const _status: 0 | 1 | 2 = summary.status;
// @ts-expect-error 2（已下架）也在契约的取值里，漏掉它就会漏掉「订单里引用的历史商品」
const _statusTooNarrow: 0 | 1 = summary.status;

// --- query 参数 ---

// 对照组：契约里有的参数、类型对的，编译得过。
void client.listProducts({ page: 2, page_size: 50, sort: "price_desc" });

// @ts-expect-error page 是 number，不是 string —— 契约说了算
void client.listProducts({ page: "2" });

// @ts-expect-error 契约里没有 limit 这个参数（分页参数叫 page_size）
void client.listProducts({ limit: 10 });

// @ts-expect-error sort 是字面量联合，"price" 不在里面
void client.listProducts({ sort: "price" });

// @ts-expect-error 同样的约束在底层原语上也在，不是只在便捷方法上
void client.get("/products", { query: { page: "2" } });

// --- 路径与方法 ---

// @ts-expect-error 契约里没有这条路径
void client.get("/prodcuts");

// @ts-expect-error 契约里 /products 只有 GET，没有 POST
void client.request("post", "/products");

// @ts-expect-error /categories 没有任何 query 参数，传了就是错
void client.get("/categories", { query: { page: 1 } });

// @ts-expect-error /products/{product_id} 的 path 参数是必填的，整个参数对象不能省
void client.get("/products/{product_id}");

// @ts-expect-error product_id 是 number，不是 string
void client.get("/products/{product_id}", { path: { product_id: "7" } });

// 对照组：路径参数给对了就编译得过，而且响应体是 ProductDetail 不是 ProductSummary。
void client.get("/products/{product_id}", { path: { product_id: 7 } });

// --- 错误 ---

// 对照组：Problem 的字段读得到，且类型正确。
const _problemType: string = problemError.problem.type;
const _problemStatus: number = problemError.problem.status;

// @ts-expect-error 契约里的 Problem 没有 code / message（那是被这份契约否掉的信封格式）
problemError.problem.code;

// @ts-expect-error trace_id 是可选的 string，不能直接当 string 用
const _traceId: string = problemError.problem.trace_id;
