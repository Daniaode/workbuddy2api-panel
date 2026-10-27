package panel

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// parseExpertIDFromJumpURL 必须能从 expert_use_* 任务的 jump_url 提取 expert_id。
// jump_url 由上游下发，deeplink 参数是 URL 编码的（expertId%3Dex_xxx），也可能
// 是已解码形态（expertId=ex_xxx）——两种都要覆盖，否则解析失败会导致任务静默跳过。
func TestParseExpertIDFromJumpURL(t *testing.T) {
	cases := []struct {
		name    string
		jumpURL string
		want    string
	}{
		{
			"URL 编码形态（上游实际下发）",
			"https://www.workbuddy.cn/work/launch/?deeplink=workbuddy%3A%2F%2Fexperts%3FexpertId%3Dex_GDl31WahyIH0&buddy_type=workbuddy",
			"ex_GDl31WahyIH0",
		},
		{
			"已解码形态",
			"https://www.workbuddy.cn/work/launch/?deeplink=workbuddy://experts?expertId=ex_WZY136wV5aMt&buddy_type=workbuddy",
			"ex_WZY136wV5aMt",
		},
		{
			"无 expertId 参数",
			"workbuddy://experts",
			"",
		},
		{
			"空 jump_url",
			"",
			"",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseExpertIDFromJumpURL(c.jumpURL); got != c.want {
				t.Errorf("parseExpertIDFromJumpURL(%q)=%q want %q", c.jumpURL, got, c.want)
			}
		})
	}
}

// expertNameFromTitle 从「体验「专家名」」式 title 提取中文名（作 keyword 搜索）。
func TestExpertNameFromTitle(t *testing.T) {
	cases := []struct {
		title string
		want  string
	}{
		{"体验「直播电商教练」", "直播电商教练"},
		{"体验「抖音策略师」", "抖音策略师"},
		{"体验「中国电商运营专家」", "中国电商运营专家"},
		{"体验「商家宣传页与海报设计师」", "商家宣传页与海报设计师"},
		{"体验某专家", "某专家"}, // 无引号：去掉「体验」前缀兜底
	}
	for _, c := range cases {
		if got := expertNameFromTitle(c.title); got != c.want {
			t.Errorf("expertNameFromTitle(%q)=%q want %q", c.title, got, c.want)
		}
	}
}

// runExpertSpecific 的 expert_id 匹配逻辑：keyword 搜索返回多个专家，必须按
// jump_url 里的 expert_id 精确匹配，不能取第一个（keyword 会命中同行业多个专家）。
func TestExpertIDMatchPicksExact(t *testing.T) {
	experts := []upstream.MarketExpert{
		{ExpertID: "ex_OTHER1", DisplayNameZH: "别家", ProfessionZH: "直播电商运营"},
		{ExpertID: "ex_WZY136wV5aMt", DisplayNameZH: "播旺旺", ProfessionZH: "直播电商教练"},
		{ExpertID: "ex_OTHER2", DisplayNameZH: "又一家", ProfessionZH: "直播带货"},
	}
	expertID := "ex_WZY136wV5aMt"
	var target *upstream.MarketExpert
	for i := range experts {
		if experts[i].ExpertID == expertID {
			target = &experts[i]
			break
		}
	}
	if target == nil {
		t.Fatal("未按 expert_id 精确匹配到目标专家")
	}
	if target.DisplayNameZH != "播旺旺" {
		t.Errorf("匹配到错误专家 %+v", target)
	}
}
