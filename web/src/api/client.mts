// Keel 的 TypeScript SDK。
//
// ## 一句话
//
// 这里**没有一个手写的请求/响应 interface**。所有形状都从 schema.d.ts 的 `paths`
// 推导出来，而 schema.d.ts 是 `make generate` 从 docs/电商系统-OpenAPI.yaml 生成并
// 入库的产物。于是「契约是唯一真相源」在 TS 这一侧有了和 Go 侧一样的落点：
// 契约里把 `min_price_cents` 改个名字、把 `status` 从 required 里挪走，
// 重新 `make generate` 之后，任何读这些字段的调用点都会在 `tsc --strict` 下变红。
//
// 手写一份 `interface Product { ... }` 也能编译、也能跑通，但它和契约之间只剩
// 人的注意力在维系 —— 那正是 Go 侧 handler 拒绝手写响应结构体的同一个理由。
//
// ## 为什么是「一个原语」而不是 49 个方法
//
// 契约里有 49 条路径。给每条写一个方法，等于把契约抄第二遍：抄错了没人发现，
// 契约改了要手工同步 49 处。这里只有一个泛型原语 `request`（以及它的 `get` 等
// 薄壳），路径、方法、query、path 参数、请求体、响应体全部由 `paths` 索引出来。
// M1 只有 `GET /products` 真的实现了，所以只有它有一个具名便捷方法。
//
// ## 运行时零依赖
//
// 只用全局 fetch。没有 package.json、没有 node_modules —— 见 Makefile 里
// schema-check 的那段注释，以及 web/tsconfig.json。

import type { components, paths } from "./schema.js";

// ---------------------------------------------------------------------------
// 契约类型的对外别名
// ---------------------------------------------------------------------------

/** RFC 9457 Problem Details。服务端 4xx/5xx 的响应体（见 internal/problem）。 */
export type Problem = components["schemas"]["Problem"];

/** 分页元信息。注意它是**服务端钳制之后**的值 —— 见 internal/service/product.go。 */
export type PageMeta = components["schemas"]["PageMeta"];

/** 商品列表项。 */
export type ProductSummary = components["schemas"]["ProductSummary"];

/**
 * 「本次响应是按哪家门店算的」。
 *
 * 多门店之后，可见性、价格、库存三件都随门店变（数据模型 §4），
 * 所以每一条会受门店影响的读接口都回显它。**没有它，客户端拿到的
 * `in_stock` 与价格是不知道属于谁的。**
 *
 * `match_type = "none"` 是一个正常结果，不是错误：它表示这家商家还没配
 * 默认门店，而买家又不在任何围栏内 —— 要渲染的是「不在服务范围」，
 * 不是空列表。
 */
export type StoreContext = components["schemas"]["StoreContext"];

// ---------------------------------------------------------------------------
// 从 paths 推导：类型层
// ---------------------------------------------------------------------------

/** openapi-typescript 为每条路径都写出这 8 个键，没定义的那个写成 `never`。 */
export type HttpMethod =
    | "get"
    | "put"
    | "post"
    | "delete"
    | "options"
    | "head"
    | "patch"
    | "trace";

/** 去掉可选属性带来的 undefined。 */
type Defined<T> = Exclude<T, undefined>;

/**
 * `[T] extends [never]` 而不是 `T extends never`。
 *
 * 裸的 `T extends never` 是**分配式**条件类型：T 是 never 时它对空联合分配，
 * 结果是 never 而不是 true，于是判断永远不成立。用元组包一层关掉分配。
 */
type IsNever<T> = [T] extends [never] ? true : false;

/**
 * 路径 P 上方法 M 的 operation 对象；契约里没定义这个方法时是 never。
 *
 * 生成器把没定义的方法写成 `get?: never`，索引出来是 `undefined`，
 * 所以要先 Defined 再判断。
 */
export type Operation<P extends keyof paths, M extends HttpMethod> = M extends keyof paths[P]
    ? Defined<paths[P][M]>
    : never;

/** 契约里定义了方法 M 的全部路径。`PathsWith<"get">` 就是所有可 GET 的路径。 */
export type PathsWith<M extends HttpMethod> = {
    [P in keyof paths]: IsNever<Operation<P, M>> extends true ? never : P;
}[keyof paths];

/** 路径 P 上契约定义了的全部方法。 */
export type MethodsOf<P extends keyof paths> = {
    [M in HttpMethod]: IsNever<Operation<P, M>> extends true ? never : M;
}[HttpMethod];

/** operation 的 parameters 里某一组（query / path / header / cookie）的原始类型。 */
type ParamGroup<O, K extends string> = O extends { parameters: infer Ps }
    ? K extends keyof Ps
        ? Ps[K]
        : never
    : never;

/** operation 的 query 参数（已去掉 undefined；契约没有 query 时是 never）。 */
export type QueryOf<O> = Defined<ParamGroup<O, "query">>;

/** operation 的路径参数。 */
export type PathParamsOf<O> = Defined<ParamGroup<O, "path">>;

/** 从一个「有 content 的东西」里取 application/json 那一份。 */
type JsonOf<T> = T extends { content: infer C }
    ? C extends { "application/json": infer J }
        ? J
        : never
    : never;

/**
 * operation 的请求体。保留 undefined —— 下面 `Slot` 要靠它判断这个槽是不是可选的。
 *
 * `requestBody?: never` 的那些 operation 走的是 `Defined<B>` = never 这条：
 * never 对任何 extends 都成立，infer 出来的 J 也是 never，最终结果是 undefined，
 * 于是 body 槽被封成 `{ body?: never }`。
 */
type BodyOf<O> = O extends { requestBody?: infer B }
    ? Defined<B> extends { content: { "application/json": infer J } }
        ? undefined extends B
            ? J | undefined
            : J
        : never
    : never;

/**
 * 算成功响应的响应体。
 *
 * 成功状态码只认这四个。契约里 4xx/5xx 一律是 Problem，它们不走返回值走异常。
 * 204 的生成结果是 `content?: never`，`JsonOf` 在那里落到 never —— 于是
 * 「没有响应体」这件事在类型上就是 never，下面被翻译成 undefined。
 */
type SuccessStatus = 200 | 201 | 202 | 204;

type SuccessResponse<O> = O extends { responses: infer R }
    ? R[Extract<keyof R, SuccessStatus>]
    : never;

/** 路径 P、方法 M 的成功响应体类型。没有响应体（204）时是 undefined。 */
export type ResponseBodyOf<P extends keyof paths, M extends HttpMethod> = IsNever<
    JsonOf<SuccessResponse<Operation<P, M>>>
> extends true
    ? undefined
    : JsonOf<SuccessResponse<Operation<P, M>>>;

/**
 * 把一个「原始类型」翻译成请求参数对象上的一个槽位，三态：
 *   - 契约里没有这组参数        → `{ k?: never }`（传了就报错）
 *   - 契约里有，且整组是可选的  → `{ k?: T }`
 *   - 契约里有，且是必填的      → `{ k: T }`（漏传就报错）
 *
 * 第一态要配 tsconfig 的 exactOptionalPropertyTypes 才真的拦得住：
 * 没有它，`{ k?: never }` 允许显式写 `k: undefined`。
 */
type Slot<K extends string, Raw> = IsNever<Defined<Raw>> extends true
    ? { [_ in K]?: never }
    : undefined extends Raw
      ? { [_ in K]?: Defined<Raw> }
      : { [_ in K]: Defined<Raw> };

/** 一次调用的参数对象。query / path / body 三个槽全部由契约决定。 */
export type RequestOptions<P extends keyof paths, M extends HttpMethod> = Slot<
    "query",
    ParamGroup<Operation<P, M>, "query">
> &
    Slot<"path", ParamGroup<Operation<P, M>, "path">> &
    Slot<"body", BodyOf<Operation<P, M>>> & {
        /** 额外的请求头。契约声明的头（如 Idempotency-Key）也从这里给。 */
        headers?: Readonly<Record<string, string>>;
        /** 取消信号。 */
        signal?: AbortSignal;
    };

/**
 * 参数对象在「所有槽都可选」时可以整个省掉，否则必须传。
 *
 * `{} extends T` 正是「T 的每个属性都是可选的」。没有这一层，
 * `client.get("/products")` 要写成 `client.get("/products", {})`；
 * 而把参数无条件写成可选，则 `GET /products/{product_id}` 漏传 path 参数不会报错 ——
 * 那等于把这个 SDK 最该拦住的一类错误放了过去。
 */
type OptionalArgs<P extends keyof paths, M extends HttpMethod> = {} extends RequestOptions<P, M>
    ? [options?: RequestOptions<P, M>]
    : [options: RequestOptions<P, M>];

// ---------------------------------------------------------------------------
// 错误
// ---------------------------------------------------------------------------

/** 本 SDK 抛出的所有错误的基类。调用方可以 `catch (e) { if (e instanceof KeelError) ... }`。 */
export class KeelError extends Error {
    constructor(message: string) {
        super(message);
        this.name = new.target.name;
    }
}

/**
 * 服务端按 RFC 9457 回了一个 Problem。
 *
 * `problem` 是契约里的 Problem 类型，不是 any：客户端按 `problem.type` 分支
 * （「这家店不存在」和「服务挂了」得区分得开，见 internal/problem 的包注释），
 * 而 type 是不是还在、叫不叫这个名字，由契约说了算。
 */
export class ProblemError extends KeelError {
    /** HTTP 状态码。与 problem.status 分开存：真实世界里两者可能不一致。 */
    readonly status: number;
    readonly url: string;
    readonly problem: Problem;

    constructor(url: string, status: number, problem: Problem) {
        super(`${status} ${problem.title} (${problem.type})`);
        this.status = status;
        this.url = url;
        this.problem = problem;
    }
}

/**
 * 服务端回了个失败状态码，但响应体不是一个合法的 Problem。
 *
 * 单独一个类，不是把它塞进 ProblemError 里编一个假的 Problem：编出来的 Problem
 * 会让调用方以为自己拿到了服务端的判断，然后按一个 SDK 自己捏造的 `type` 去分支。
 * 这类响应真实存在（网关的 502 HTML、代理截断的空响应），它们的正确含义是
 * 「这一跳根本没到 Keel，或者 Keel 违约了」，和任何业务错误都不是一回事。
 */
export class UnexpectedResponseError extends KeelError {
    readonly status: number;
    readonly url: string;
    readonly contentType: string | null;
    /** 原始响应体，截断到 2KB —— 出错时贴日志够用，又不至于把一屏 HTML 灌进去。 */
    readonly bodyText: string;

    constructor(url: string, status: number, contentType: string | null, bodyText: string) {
        super(`${status} 的响应体不是契约里的 Problem（content-type: ${contentType ?? "无"}）`);
        this.status = status;
        this.url = url;
        this.contentType = contentType;
        this.bodyText = bodyText;
    }
}

/**
 * 运行时判断一个值是不是 Problem。
 *
 * 只查契约里 `required: [type, title, status]` 那三个字段 —— 不多不少。
 * 多查（比如要求 detail）会把合法的 Problem 判成非法；少查则会让
 * `{}` 这种空对象冒充 Problem，于是调用方读到 `problem.type` 是 undefined
 * 而类型上它是 string。web/src/api/type-tests.mts 里有一条编译期断言钉着
 * 「契约的必填字段就是这三个」，契约加一个必填字段，那条断言会红。
 */
export function isProblem(value: unknown): value is Problem {
    if (typeof value !== "object" || value === null) return false;
    const v = value as Record<string, unknown>;
    return typeof v["type"] === "string" && typeof v["title"] === "string" && typeof v["status"] === "number";
}

// ---------------------------------------------------------------------------
// 客户端
// ---------------------------------------------------------------------------

/**
 * fetch 的最小签名。
 *
 * 不写成 `typeof fetch`：那会把 Request / RequestInfo 这些只有 DOM lib 才有的
 * 类型钉进公开 API。这里只用得到 (url, init) 这一种调用方式。
 */
export type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

export interface KeelClientOptions {
    /**
     * 服务地址，**含契约里 servers 的路径前缀**，例如 http://localhost:8080/api/v1。
     *
     * 契约的路径写的是 /products，实际挂在 /api/v1/products（见 internal/app.Router），
     * 那个前缀属于 server URL 而不属于路径 —— 所以它在这里给，不在每次调用时拼。
     */
    baseUrl: string;
    /** 每个请求都带的头。 */
    headers?: Readonly<Record<string, string>>;
    /** 换掉 fetch（测试、或者需要自带 agent 的环境）。默认用全局 fetch。 */
    fetch?: FetchLike;
}

/** 契约 servers 里的本地单商家地址。 */
export const DEFAULT_BASE_URL = "http://localhost:8080/api/v1";

export class KeelClient {
    readonly baseUrl: string;
    readonly #headers: Readonly<Record<string, string>>;
    readonly #fetch: FetchLike;

    constructor(options: KeelClientOptions) {
        // 去掉末尾斜杠：契约里的路径全部以 / 开头，不去掉就会拼出 //products。
        this.baseUrl = options.baseUrl.replace(/\/+$/, "");
        this.#headers = options.headers ?? {};
        this.#fetch = options.fetch ?? ((input, init) => globalThis.fetch(input, init));
    }

    /**
     * 类型安全的调用原语。路径、方法、参数、响应体全部由契约推导。
     *
     * 失败（非 2xx）抛 ProblemError / UnexpectedResponseError，不返回。
     * 让错误走返回值的话，每个调用点都要先解一层联合类型才能读 items，
     * 而漏解的那些会一直编译得过 —— 直到线上。
     */
    async request<P extends keyof paths, M extends MethodsOf<P>>(
        method: M,
        path: P,
        ...args: OptionalArgs<P, M>
    ): Promise<ResponseBodyOf<P, M>> {
        // 内部退化成不带泛型的实现。类型安全在**边界**上（上面那个签名），
        // 这里再套一层泛型只是把同样的断言写两遍。
        const options = (args[0] ?? {}) as LooseOptions;
        const body = await this.#send(String(method).toUpperCase(), String(path), options);
        return body as ResponseBodyOf<P, M>;
    }

    /** `request("get", ...)` 的薄壳，路径被收窄到契约里可 GET 的那些。 */
    async get<P extends PathsWith<"get">>(
        path: P,
        ...args: OptionalArgs<P, "get">
    ): Promise<ResponseBodyOf<P, "get">> {
        // 这里的 as 是为了让 P 的约束（PathsWith<"get">）和 request 的约束
        // （M extends MethodsOf<P>）对上 —— 两者等价，但 TS 推不出来。
        return this.request("get" as MethodsOf<P>, path, ...(args as OptionalArgs<P, MethodsOf<P>>)) as Promise<
            ResponseBodyOf<P, "get">
        >;
    }

    /**
     * GET /products。M1 唯一真正实现了的接口。
     *
     * 返回的是契约里 `allOf: [PageMeta, {items}]` 的那个形状，**原样透出**：
     * page / page_size / total 是服务端钳制之后的值（internal/service/product.go
     * 里的 clampPaging），SDK 不在客户端再算一遍。客户端重算的话，传
     * page_size=100000 的调用方会以为自己拿到了十万件。
     */
    async listProducts(query?: ProductListQuery): Promise<ProductListPage> {
        // exactOptionalPropertyTypes 下不能写 `{ query }` —— query 可能是
        // undefined，而 `query?: T` 的意思是「要么不出现，要么是 T」。
        return query === undefined ? this.get("/products") : this.get("/products", { query });
    }

    async #send(method: string, path: string, options: LooseOptions): Promise<unknown> {
        const url = this.#url(path, options.path, options.query);

        const init: RequestInit = {
            method,
            headers: {
                Accept: "application/json, application/problem+json",
                ...this.#headers,
                ...(options.headers ?? {}),
            },
        };
        if (options.body !== undefined) {
            init.body = JSON.stringify(options.body);
            init.headers = { ...(init.headers as Record<string, string>), "Content-Type": "application/json" };
        }
        if (options.signal !== undefined) init.signal = options.signal;

        const response = await this.#fetch(url, init);
        const text = await response.text();

        if (!response.ok) throw toError(url, response, text);

        // 空响应体（204，或者 200 但没有 body）→ undefined。
        if (text === "") return undefined;
        try {
            return JSON.parse(text) as unknown;
        } catch {
            throw new UnexpectedResponseError(url, response.status, response.headers.get("content-type"), truncate(text));
        }
    }

    #url(path: string, pathParams: Record<string, unknown> | undefined, query: Record<string, unknown> | undefined): string {
        // 契约里的路径模板是 /products/{product_id}。
        const filled = path.replace(/\{([^}]+)\}/g, (_match, name: string) => {
            const value = pathParams?.[name];
            if (value === undefined || value === null) {
                // 类型层已经拦住了这种调用（Slot 把必填的 path 槽写成必填），
                // 但 SDK 也会被没开类型检查的 JS 调用方用到。
                throw new KeelError(`路径参数 ${name} 缺失：${path}`);
            }
            return encodeURIComponent(String(value));
        });
        const qs = buildQuery(query);
        return `${this.baseUrl}${filled}${qs === "" ? "" : `?${qs}`}`;
    }
}

// ---------------------------------------------------------------------------
// GET /products 的类型别名（从契约推导，供调用方标注变量用）
// ---------------------------------------------------------------------------

/** GET /products 的响应体：PageMeta 的三个字段 + items。 */
export type ProductListPage = ResponseBodyOf<"/products", "get">;

/** GET /products 的 query 参数。 */
export type ProductListQuery = QueryOf<Operation<"/products", "get">>;

// ---------------------------------------------------------------------------
// 内部
// ---------------------------------------------------------------------------

/** #send / #url 内部用的松散形状。公开 API 的类型安全由 RequestOptions 保证。 */
interface LooseOptions {
    query?: Record<string, unknown>;
    path?: Record<string, unknown>;
    body?: unknown;
    headers?: Readonly<Record<string, string>>;
    signal?: AbortSignal;
}

function buildQuery(query: Record<string, unknown> | undefined): string {
    if (query === undefined) return "";
    const params = new URLSearchParams();
    for (const [key, value] of Object.entries(query)) {
        // 不传和传空是两件事：漏掉 undefined，而不是把它变成 "undefined" 字符串。
        if (value === undefined || value === null) continue;
        if (Array.isArray(value)) {
            for (const item of value as unknown[]) {
                if (item === undefined || item === null) continue;
                params.append(key, String(item));
            }
            continue;
        }
        params.append(key, String(value));
    }
    return params.toString();
}

function toError(url: string, response: Response, text: string): KeelError {
    let parsed: unknown;
    try {
        parsed = JSON.parse(text) as unknown;
    } catch {
        return new UnexpectedResponseError(url, response.status, response.headers.get("content-type"), truncate(text));
    }
    if (!isProblem(parsed)) {
        return new UnexpectedResponseError(url, response.status, response.headers.get("content-type"), truncate(text));
    }
    return new ProblemError(url, response.status, parsed);
}

function truncate(text: string): string {
    return text.length <= 2048 ? text : `${text.slice(0, 2048)}…（已截断，共 ${text.length} 字节）`;
}
