package service

import (
	"errors"
	"strings"
	"testing"

	"github.com/keel/keel/internal/repository"
)

// validateFreightTemplate 是「省不重叠、恰好一条默认规则、不配送与规则不重叠」的唯一执行者
// （数据库只兜得住取值，00041 文件头第二节）。逐条敲一遍。

func okFreightInput() FreightTemplateInput {
	return FreightTemplateInput{
		Name: "默认运费", ChargeMode: freightChargeByPiece, IsDefault: true,
		Rules: []repository.FreightRule{
			{RegionCodes: []string{}, FirstUnit: 1, FirstFeeCents: 800, AdditionalUnit: 1, AdditionalFeeCents: 200},
			{RegionCodes: []string{"650000"}, FirstUnit: 1, FirstFeeCents: 2000, AdditionalUnit: 1, AdditionalFeeCents: 1000},
		},
		UndeliverableRegionCodes: []string{"810000"},
	}
}

func TestValidateFreightTemplateAcceptsAndPutsDefaultRuleLast(t *testing.T) {
	w, err := validateFreightTemplate(okFreightInput())
	if err != nil {
		t.Fatalf("合法模板被拒：%v", err)
	}
	if len(w.Rules) != 2 || !w.Rules[1].IsDefault() || w.Rules[0].RegionCodes[0] != "650000" {
		t.Fatalf("默认规则应排在最后存，实得 %+v", w.Rules)
	}
}

func TestValidateFreightTemplateRejects(t *testing.T) {
	storeID := int64(5)
	cases := []struct {
		name   string
		mutate func(*FreightTemplateInput)
		want   string
	}{
		{"没有默认规则", func(in *FreightTemplateInput) { in.Rules = in.Rules[1:] }, "恰好一条默认规则"},
		{"两条默认规则", func(in *FreightTemplateInput) {
			in.Rules = append(in.Rules, repository.FreightRule{RegionCodes: []string{}, FirstUnit: 1, AdditionalUnit: 1})
		}, "实得 2 条"},
		{"同一个省出现在两条规则里", func(in *FreightTemplateInput) {
			in.Rules = append(in.Rules, repository.FreightRule{RegionCodes: []string{"650000"}, FirstUnit: 1, AdditionalUnit: 1})
		}, "同时出现在"},
		{"不配送与规则重叠", func(in *FreightTemplateInput) {
			in.UndeliverableRegionCodes = []string{"650000"}
		}, "同时出现在"},
		{"不认识的区划码", func(in *FreightTemplateInput) {
			in.Rules[1].RegionCodes = []string{"440300"}
		}, "不是省级行政区划码"},
		{"门店模板设成全店默认", func(in *FreightTemplateInput) { in.StoreID = &storeID }, "不能设成全店默认"},
		{"计费方式越界", func(in *FreightTemplateInput) { in.ChargeMode = 3 }, "charge_mode"},
		{"续件单位为 0", func(in *FreightTemplateInput) { in.Rules[0].AdditionalUnit = 0 }, "单位"},
		{"运费为负", func(in *FreightTemplateInput) { in.Rules[0].FirstFeeCents = -1 }, "首费与续费"},
		{"名字为空", func(in *FreightTemplateInput) { in.Name = "  " }, "name"},
	}
	for _, tc := range cases {
		in := okFreightInput()
		in.Rules = append([]repository.FreightRule(nil), in.Rules...)
		tc.mutate(&in)
		_, err := validateFreightTemplate(in)
		if !errors.Is(err, ErrFreightBadRequest) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s：期望 ErrFreightBadRequest 且提到 %q，实得 %v", tc.name, tc.want, err)
		}
	}
}
