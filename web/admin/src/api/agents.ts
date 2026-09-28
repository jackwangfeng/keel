// AI 员工（AI 经营 M9）：几个跨组件复用的小工具，避免在几个 .vue 里各写一份。

import type { AdminAgent } from "./client.ts";

/** 能分配给 AI 员工的角色。契约里 AdminAgent.role 本身就是 2/3/4——不存在「AI 管理员」。 */
export const AGENT_ROLES: AdminAgent["role"][] = [2, 3, 4];

/**
 * 发密钥成功后，给人看的 MCP 接入配置片段。
 *
 * `secret` 只在发密钥的那一次响应里出现（服务端只存哈希），所以这段 JSON
 * 也只在那一次弹窗里拼得出来——不缓存、不二次显示。
 */
export function mcpConfigSnippet(secret: string): string {
    const url = `${globalThis.location.origin}/api/v1/mcp`;
    return JSON.stringify(
        {
            mcpServers: {
                keel: {
                    type: "http",
                    url,
                    headers: { Authorization: `Bearer ${secret}` },
                },
            },
        },
        null,
        4,
    );
}
