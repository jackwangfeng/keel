/// <reference types="vite/client" />

// Vite 注入的构建期变量。默认值在 src/api/client.ts 里给。
interface ImportMetaEnv {
    /** 服务地址前缀，含契约 servers 的 `/api/v1`。默认与 nginx 反代一致。 */
    readonly VITE_KEEL_API_BASE?: string;
}

interface ImportMeta {
    readonly env: ImportMetaEnv;
}
