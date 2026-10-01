package inference_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/keel/keel/internal/inference"
)

// 请求体照 TypeSafe 的线上格式（infero docs/keel-integration.md 第 5 节）；响应缺题、缺版本、题型不对、
// 概率越界都要报 ErrProtocol —— 缺了的答案当 0 写进标签，会被校准当成「不相关」。
func TestSystemOneDecideWireFormatAndValidation(t *testing.T) {
	var got map[string]any
	reply := `{"model":"kev-0.8b","model_version":"abc","answers":{"p1":{"type":"noul","noul":0.9}}}`
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != inference.SystemOnePath || r.Method != http.MethodPost {
			t.Errorf("打到了 %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	defer srv.Close()

	c, err := inference.NewSystemOne(srv.URL, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := inference.SystemOneRequest{State: "买家搜「复古地毯」", Model: "kev-latest",
		Questions: map[string]inference.SystemOneQuestion{
			"p1": {Type: inference.QuestionNoul, Instructions: "相关吗", Criteria: map[string]string{"true": "是", "false": "否"}},
		}}
	resp, err := c.Decide(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if *resp.Answers["p1"].Noul != 0.9 || resp.ModelVersion != "abc" {
		t.Errorf("响应解析不对：%+v", resp)
	}
	q := got["questions"].(map[string]any)["p1"].(map[string]any)
	if got["state"] != "买家搜「复古地毯」" || got["model"] != "kev-latest" || q["type"] != "noul" ||
		q["instructions"] != "相关吗" || q["criteria"].(map[string]any)["true"] != "是" {
		t.Errorf("请求体不是线上格式：%v", got)
	}

	for name, bad := range map[string]string{
		"缺题":   `{"model":"k","model_version":"v","answers":{}}`,
		"缺版本":  `{"model":"k","answers":{"p1":{"type":"noul","noul":0.9}}}`,
		"题型不对": `{"model":"k","model_version":"v","answers":{"p1":{"type":"score","score":0.5}}}`,
		"概率越界": `{"model":"k","model_version":"v","answers":{"p1":{"type":"noul","noul":1.5}}}`,
	} {
		reply = bad
		if _, err := c.Decide(context.Background(), req); !errors.Is(err, inference.ErrProtocol) {
			t.Errorf("%s：应当 ErrProtocol，得到 %v", name, err)
		}
	}

	reply, status = `boom`, http.StatusServiceUnavailable
	if _, err := c.Decide(context.Background(), req); !errors.Is(err, inference.ErrUnavailable) {
		t.Errorf("5xx 应当 ErrUnavailable，得到 %v", err)
	}
	if _, err := inference.NewSystemOne("", 0, nil); err == nil {
		t.Error("地址为空应当拒绝构造")
	}
}
