package service

import (
	"strings"
)

// 省级行政区划（运费模板按省分组设价，数据模型 §7「运费模板」）。
//
// 34 个省级行政区（含港澳台），码是 GB/T 2260 的 6 位码（后四位为 0）。
// **这份清单与 00055 里两条 CHECK 的数组是同一份**：freight_region_test.go
// 读迁移文件逐个核对，两边任何一边多一个或少一个都会红 —— 库里认、代码不认
// （或者反过来）的区划码，表现是后台存得进去、计价时却永远匹配不上。

// province 是一个省级行政区：区划码、全称、简称。
//
// 简称用来匹配地址里的 province 文字（「内蒙古」与「内蒙古自治区」都认）：
// 地址的 region_code 是可空的（数据模型 §9），老地址与手填的地址常常没有它。
type province struct {
	Code  string
	Name  string
	Short string
}

var provinces = []province{
	{"110000", "北京市", "北京"},
	{"120000", "天津市", "天津"},
	{"130000", "河北省", "河北"},
	{"140000", "山西省", "山西"},
	{"150000", "内蒙古自治区", "内蒙古"},
	{"210000", "辽宁省", "辽宁"},
	{"220000", "吉林省", "吉林"},
	{"230000", "黑龙江省", "黑龙江"},
	{"310000", "上海市", "上海"},
	{"320000", "江苏省", "江苏"},
	{"330000", "浙江省", "浙江"},
	{"340000", "安徽省", "安徽"},
	{"350000", "福建省", "福建"},
	{"360000", "江西省", "江西"},
	{"370000", "山东省", "山东"},
	{"410000", "河南省", "河南"},
	{"420000", "湖北省", "湖北"},
	{"430000", "湖南省", "湖南"},
	{"440000", "广东省", "广东"},
	{"450000", "广西壮族自治区", "广西"},
	{"460000", "海南省", "海南"},
	{"500000", "重庆市", "重庆"},
	{"510000", "四川省", "四川"},
	{"520000", "贵州省", "贵州"},
	{"530000", "云南省", "云南"},
	{"540000", "西藏自治区", "西藏"},
	{"610000", "陕西省", "陕西"},
	{"620000", "甘肃省", "甘肃"},
	{"630000", "青海省", "青海"},
	{"640000", "宁夏回族自治区", "宁夏"},
	{"650000", "新疆维吾尔自治区", "新疆"},
	{"710000", "台湾省", "台湾"},
	{"810000", "香港特别行政区", "香港"},
	{"820000", "澳门特别行政区", "澳门"},
}

var provinceByCode = func() map[string]province {
	m := make(map[string]province, len(provinces))
	for _, p := range provinces {
		m[p.Code] = p
	}
	return m
}()

// validProvinceCode：是不是 34 个省级区划码之一。
func validProvinceCode(code string) bool {
	_, ok := provinceByCode[code]
	return ok
}

// provinceName 是区划码的全称；不认识的码原样返回。
func provinceName(code string) string {
	if p, ok := provinceByCode[code]; ok {
		return p.Name
	}
	return code
}

// provinceOf 把一个收货地址归到省。先看 region_code 的前两位（行政区划码的前两位
// 就是省），再看 province 文字的简称前缀。都归不到返回 ""。
//
// region_code 优先：文字会有写法差异（「内蒙古」「内蒙古自治区」「内蒙」），
// 区划码没有。但 region_code 前两位对不上任何一个省时**不报错、接着看文字** ——
// 一个写错的区划码不该让一个写对了省名的地址算不出运费。
func provinceOf(regionCode *string, provinceText string) string {
	if regionCode != nil {
		rc := strings.TrimSpace(*regionCode)
		if len(rc) >= 2 {
			code := rc[:2] + "0000"
			if validProvinceCode(code) {
				return code
			}
		}
	}
	t := strings.TrimSpace(provinceText)
	if t == "" {
		return ""
	}
	for _, p := range provinces {
		if strings.HasPrefix(t, p.Short) {
			return p.Code
		}
	}
	return ""
}
