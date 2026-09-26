<script setup lang="ts">
// 订单。**这一页刻意只有字，没有表格。**
//
// 对着 main 上的契约与路由表数过一遍，后台订单这一块今天的事实是：
//
//  1. **没有「后台订单列表」这条接口。** 契约里订单查询只有买家侧的
//     `GET /orders` / `GET /orders/{order_no}`，它们按买家自己的 user_id
//     过滤，后台拿不到「这家店的全部订单」。画一个订单表格出来，
//     数据只能是编的。
//  2. 契约里确实有两条后台订单侧的操作 —— `POST /admin/orders/{order_no}/shipments`
//     （发货）与 `POST /admin/refunds/{refund_no}/audit`（退款审核）——
//     但**服务端还没有 handler**：它们挂在
//     `internal/handler/contract_test.go` 的 `notYetRouted` 里，
//     那份清单两个方向都被测试锁着（契约里有而没落地的，必须在那里写明理由）。
//     给它们做表单，点下去只会打到一个没注册的路由。
//
// 所以这里把三件事原样写出来。这个仓库的习惯是把没做的事放在看得见的地方，
// 而不是让人点进去才发现。
</script>

<template>
    <div class="wrap">
        <el-alert type="info" :closable="false" show-icon class="mb12">
            <template #title>后台订单这一块还没有可用的接口</template>
            下面三条是对着 <code>docs/电商系统-OpenAPI.yaml</code> 与 <code>internal/app/app.go</code>
            的路由表数出来的事实，不是占位文案。
        </el-alert>

        <el-card>
            <el-descriptions :column="1" border>
                <el-descriptions-item label="后台订单列表">
                    <el-tag type="danger" size="small">契约里就没有</el-tag>
                    <p class="hint">
                        契约里的订单查询是买家侧的 <code>GET /orders</code> /
                        <code>GET /orders/{order_no}</code>，按买家自己的 user_id 过滤。
                        没有「按商家看全部订单」的那条。缺一条接口就自己发明一条，
                        是这个仓库最不想要的东西——界面会先长出一个契约里不存在的形状，
                        然后反过来要求后端照着它实现。
                    </p>
                </el-descriptions-item>
                <el-descriptions-item label="发货">
                    <el-tag type="warning" size="small">契约里有，服务端还没落地</el-tag>
                    <p class="hint">
                        <code>POST /admin/orders/{order_no}/shipments</code>。shipments 表已落地
                        （数据模型 §5），后台鉴权也已落地，缺的是 handler 与 §5 那三条发货规则。
                        账挂在 <code>internal/handler/contract_test.go</code> 的
                        <code>notYetRouted</code> 里。
                    </p>
                </el-descriptions-item>
                <el-descriptions-item label="退款审核">
                    <el-tag type="warning" size="small">契约里有，服务端还没落地</el-tag>
                    <p class="hint">
                        <code>POST /admin/refunds/{refund_no}/audit</code>。退款域的表已落地（§11），
                        后台鉴权也已落地，缺的是 handler 与退款状态机那几条边。同样挂在
                        <code>notYetRouted</code> 里。
                    </p>
                </el-descriptions-item>
            </el-descriptions>

            <p class="hint foot">
                这两条一旦有了 handler，这一页要做的事是加两个表单
                （请求体 <code>ShipmentCreateRequest</code> 与审核那个内联对象都已经在
                <code>web/src/api/schema.d.ts</code> 里），路由与菜单不用动。
            </p>
        </el-card>
    </div>
</template>

<style scoped>
.wrap {
    max-width: 900px;
}
.mb12 {
    margin-bottom: 12px;
}
.foot {
    margin-top: 12px;
}
code {
    background: var(--el-fill-color-light);
    padding: 1px 4px;
    border-radius: 3px;
}
</style>
