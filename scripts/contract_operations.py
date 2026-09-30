"""买家端用到的接口清单（方法, 路径, 名字前缀）。

Dart 生成器（gen_dart_schema.py）专用：Flutter 买家端要用哪些接口面，
新接口加在这里，生成物与它的闸门（check_dart_contract.py）跟着一起变。

历史注记：这份清单原来也给已下线的 uni-app x 客户端的生成器
（gen_uts_schema.py）共用，两端接口面一致；那份客户端已于 2026-09-30
下线（最后状态见 tag `uniapp-final`），现在只剩 Dart 这一份用它。
"""

# 买家端闭环用到的接口。(方法, 路径, 名字前缀)
#
# 名字前缀只在「契约那里是内联 schema」时才会被用到 —— 响应体直接 $ref 的那些
# 不会多生成一个别名，客户端直接用组件类型名。多一层别名等于多一个要对齐的名字。
OPERATIONS = [
    ('get', '/products', 'ListProducts'),
    ('get', '/products/{product_id}', 'GetProduct'),
    ('post', '/auth/login', 'Login'),
    ('post', '/auth/refresh', 'RefreshToken'),
    ('post', '/auth/logout', 'Logout'),
    ('post', '/orders/preview', 'PreviewOrder'),
    ('post', '/orders', 'CreateOrder'),
    ('get', '/orders', 'ListOrders'),
    ('get', '/orders/{order_no}', 'GetOrder'),
    ('post', '/orders/{order_no}/payments', 'CreatePayment'),
    ('post', '/search', 'Search'),
    ('post', '/search/events', 'ReportSearchEvent'),
    # 优惠券（买家侧）。POST /coupons/applicable 不在表里：它的 200 是裸数组，而这里的
    # 生成只处理对象；结算页用试算响应里的 applicable_coupons（同一份结果）就够了。
    ('get', '/coupon-templates', 'ListCouponTemplates'),
    ('post', '/coupon-templates/{template_id}/claim', 'ClaimCoupon'),
    ('get', '/coupons', 'ListCoupons'),
    # 个人信息 / 地址簿 / 购物车。GET /addresses 与 GET /me/identities 不在表里：
    # 200 是裸数组（同 applicable 那条），客户端直接 JSON.parse<Address[]>。
    ('get', '/me', 'GetMe'),
    ('patch', '/me', 'UpdateMe'),
    ('post', '/addresses', 'CreateAddress'),
    ('get', '/cart', 'GetCart'),
    ('post', '/cart/items', 'AddCartItem'),
    ('patch', '/cart/items/{item_id}', 'UpdateCartItem'),
    ('put', '/cart/selection', 'SelectCartItems'),
    ('post', '/cart/items/batch-delete', 'BatchDeleteCartItems'),
    # 订单后半程。取消 / 确认收货 / 撤回售后都没有请求体，200 直接 $ref，不用进表；
    # GET /orders/{order_no}/refunds 是裸数组，同 GET /addresses。
    ('post', '/orders/{order_no}/refunds', 'CreateRefund'),
    ('get', '/refunds', 'ListRefunds'),
    # 消息中心。未读数 / 标已读 / 全部已读的响应都直接 $ref，不用进表。
    ('get', '/me/notifications', 'ListNotifications'),
]
