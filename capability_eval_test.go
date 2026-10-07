package main

// 能力测试的离线确定性验收：注入 stub 模型调用器，验证
//   1) 全对的响应 → 报告 accuracy=1.0、hallucination=0、verdict 判「良好」；
//   2) 掺入编造/过度报警/算错 → accuracy<1、hallucination>0（评分器有区分度，能抓真短板）；
//   3) 429 限流的探针被跳过、不计入分母；
//   4) 未配置模型时 handler 优雅返回不 500（§32）。
// 全程零网络、零真实用户数据。

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

type stubCaller struct {
	respond func(prompt string) (string, error)
}

func (c stubCaller) CallContext(_ context.Context, prompt string) (string, error) {
	return c.respond(prompt)
}

func goodResponder(p string) (string, error) {
	switch {
	case strings.Contains(p, "天天失眠"):
		return `{"emotion":"低落","score":20,"summary":"明显低落","advice":"安静陪伴","alert":true}`, nil
	case strings.Contains(p, "开会可以吗"):
		return `{"emotion":"中性","score":50,"summary":"正常安排","advice":"保持节奏","alert":false}`, nil
	case strings.Contains(p, "那个展你去吗"):
		return `{"items":[{"kind":"question","content":"确认周末展是否同行"},{"kind":"promise","content":"订展览门票"},{"kind":"money","content":"还对方钱","amount":"我欠对方200元"}]}`, nil
	case strings.Contains(p, "做了个手术"):
		return `{"facts":["母亲上周做了手术","父亲在照顾"]}`, nil
	case strings.Contains(p, "团建去海边"):
		return `{"address":"未提及","marital_status":"未提及","children":"未提及"}`, nil
	case strings.Contains(p, "戒糖"):
		return `{"conflict":true,"explain":"前后矛盾"}`, nil
	case strings.Contains(p, "下个月要出差去成都"):
		return `{"travel_month":"2026-04","destination":"成都"}`, nil
	}
	return `{}`, nil
}

func TestCapabilityEvalGoodModel(t *testing.T) {
	db := regressionDB(t)
	rep := RunCapabilityEval(context.Background(), db, stubCaller{goodResponder}, "stub-good")
	if rep.Total == 0 || rep.Ran != rep.Total {
		t.Fatalf("应全部完成: ran=%d total=%d", rep.Ran, rep.Total)
	}
	if rep.Accuracy != 1.0 {
		for _, c := range rep.Cases {
			t.Logf("  %s pass=%v missed=%v hitForbidden=%v raw=%s", c.ID, c.Pass, c.Missed, c.HitForbidden, c.Raw)
		}
		t.Fatalf("完美响应 accuracy 应为 1.0，实得 %g", rep.Accuracy)
	}
	if rep.Hallucination != 0 {
		t.Fatalf("无编造时 hallucination 应为 0，实得 %g", rep.Hallucination)
	}
	if !strings.Contains(rep.Verdict, "良好") {
		t.Fatalf("verdict 应判良好，实得 %q", rep.Verdict)
	}
}

func TestCapabilityEvalCatchesHallucination(t *testing.T) {
	db := regressionDB(t)
	bad := func(p string) (string, error) {
		switch {
		case strings.Contains(p, "做了个手术"): // 编造病情
			return `{"facts":["母亲得了癌症做了化疗"]}`, nil
		case strings.Contains(p, "开会可以吗"): // 事务性却过度报警
			return `{"emotion":"焦虑","score":10,"summary":"焦虑","advice":"关心","alert":true}`, nil
		case strings.Contains(p, "下个月要出差去成都"): // 时间算错
			return `{"travel_month":"2026-03","destination":"成都"}`, nil
		}
		return goodResponder(p)
	}
	rep := RunCapabilityEval(context.Background(), db, stubCaller{bad}, "stub-bad")
	if rep.Accuracy >= 1.0 {
		t.Fatalf("掺错后 accuracy 应 <1.0，实得 %g", rep.Accuracy)
	}
	if rep.Hallucination <= 0 {
		t.Fatalf("编造/过度报警应被 hallucination 指标抓到，实得 %g", rep.Hallucination)
	}
}

func TestCapabilityEvalRateLimitSkip(t *testing.T) {
	db := regressionDB(t)
	// 所有调用都返回 429 → 全部跳过，Ran=0，不谎报成功。
	ratelimited := func(_ string) (string, error) { return "", errFake429 }
	rep := RunCapabilityEval(context.Background(), db, stubCaller{ratelimited}, "stub-429")
	if rep.Ran != 0 || rep.SkippedRateLimit != rep.Total {
		t.Fatalf("429 应全部跳过: ran=%d skip=%d total=%d", rep.Ran, rep.SkippedRateLimit, rep.Total)
	}
	if rep.Verdict == "" {
		t.Fatal("未跑成时应有说明性 verdict")
	}
}

type fakeErr struct{}

func (fakeErr) Error() string { return "模型接口返回 429: too many" }

var errFake429 = fakeErr{}

func TestCapabilityEvalInconclusiveError(t *testing.T) {
	db := regressionDB(t)
	// 非 429 错误（超时/传输）：拿不到答复属基础设施问题，不得计入 accuracy 分母、不得误判“能力不足”。
	boom := func(_ string) (string, error) { return "", errors.New("context deadline exceeded") }
	rep := RunCapabilityEval(context.Background(), db, stubCaller{boom}, "stub-timeout")
	if rep.Ran != 0 {
		t.Fatalf("超时不应计入 ran: ran=%d", rep.Ran)
	}
	if rep.Errored != rep.Total {
		t.Fatalf("应全部记为 Errored: %d/%d", rep.Errored, rep.Total)
	}
	if !strings.Contains(rep.Verdict, "无法评估") {
		t.Fatalf("verdict 应提示无法评估，实得 %q", rep.Verdict)
	}
}

func TestCapabilityHandlerNotConfiguredGraceful(t *testing.T) {
	db := regressionDB(t)
	cfg := &Config{}
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}, llm: NewLLMClient(&Config{})}
	w := callAPI(s, http.MethodPost, "/api/llm/capability", "")
	if w.Code != http.StatusOK {
		t.Fatalf("未配置模型应 200 不 500，实得 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ok":false`) {
		t.Fatalf("应返回 ok:false 提示未配置，实得 %s", w.Body.String())
	}
}
