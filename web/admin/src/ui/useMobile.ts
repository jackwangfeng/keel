// 手机宽度判定（≤ 768px）。整个后台共用一个 matchMedia 监听，组件里 `const mobile = useMobile()` 拿到响应式的布尔值。
// 断点与 styles.css 里 `@media (max-width: 768px)` 那一段是同一个数，改一处要改两处。
import { ref, type Ref } from "vue";

export const MOBILE_QUERY = "(max-width: 768px)";

let state: Ref<boolean> | null = null;

export function useMobile(): Ref<boolean> {
    if (state !== null) return state;
    const mq = typeof window !== "undefined" && typeof window.matchMedia === "function" ? window.matchMedia(MOBILE_QUERY) : null;
    const s = ref(mq?.matches ?? false);
    mq?.addEventListener("change", (e) => {
        s.value = e.matches;
    });
    state = s;
    return s;
}
