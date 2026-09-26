import { fileURLToPath, URL } from "node:url";
import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";

// 产物是一堆静态文件，由 nginx 托管（docker/Dockerfile.admin）。
// nginx 把 /api 反代到 app 服务，于是浏览器看到的是**同源**，不需要 CORS ——
// 后台 token 是个能改价、能退款的凭据，让它跨源飞来飞去要多配一层
// Access-Control-Allow-Credentials，而那一层配错的后果比不配严重。
export default defineConfig({
    plugins: [vue()],
    // 默认 "/"：nginx 把后台挂在自己的域名根上。挂到子路径下要改这里并重新构建。
    base: "/",
    resolve: {
        alias: {
            // 契约产物与那份零依赖 SDK 都在 web/src 下，**不复制一份进来**。
            // 复制等于制造第二个真相源，而这个仓库正在为同类问题付第四次账。
            "@contract": fileURLToPath(new URL("../src/api", import.meta.url)),
        },
    },
    server: {
        fs: {
            // dev server 要读得到 web/src（上面那个 alias 指向仓库 root 之外）。
            allow: [fileURLToPath(new URL("..", import.meta.url))],
        },
        proxy: {
            // `npm run dev` 时把 /api 转给本机的 app，形状与 nginx 那边一致，
            // 于是「开发时能跑、装进容器就 404」这类差异不会出现。
            "/api": {
                target: process.env["KEEL_API_ORIGIN"] ?? "http://127.0.0.1:8080",
                changeOrigin: false,
            },
        },
    },
    build: {
        outDir: "dist",
        // 产物体积报告的阈值。element-plus 全量引入本来就大，
        // 500 KB 的默认阈值只会每次构建打一屏警告。
        chunkSizeWarningLimit: 1500,
    },
});
