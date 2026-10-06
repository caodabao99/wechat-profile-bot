package main

import (
	"math"
	"testing"
)

// §7.1/7.2/7.3/7.5：确定性证据分类器单测。
func TestClassifyEvidenceType(t *testing.T) {
	cases := []struct {
		name    string
		typ     string
		value   string
		content string
		want    string
	}{
		{"身份-第一人称=direct", "occupation", "律师", "我是律师", EvDirect},
		{"身份-他人话题=topic_related", "occupation", "律师", "你认识那个律师吗", EvTopicRelated},
		{"身份-计划语气=strong_context", "occupation", "律师", "我准备转做律师", EvStrongContext},
		{"身份-否定=conflict", "occupation", "律师", "我现在不做律师了", EvConflict},
		{"身份-城市第一人称=direct", "location", "深圳", "我住在深圳", EvDirect},
		{"集合-兴趣本人提及=direct", "interest", "跑步", "今天去跑步了", EvDirect},
		{"集合-兴趣他人话题=topic_related", "interest", "跑步", "他每天早上跑步", EvTopicRelated},
		{"字面未命中=weak_context", "occupation", "律师", "我最近挺忙", EvWeakContext},
	}
	for _, c := range cases {
		if got := classifyEvidenceType(c.typ, c.value, c.content); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}

// §7.4：topic_related/weak_context/conflict 证据不得提高数值置信度；仅 direct(+0.08)/strong_context(+0.03) 提高。
func TestEvidenceConfContrib(t *testing.T) {
	for _, et := range []string{EvTopicRelated, EvWeakContext, EvConflict, EvInsufficient} {
		if evidenceConfContrib(et) != 0 {
			t.Errorf("%s 不应提高置信度", et)
		}
	}
	if evidenceConfContrib(EvDirect) <= evidenceConfContrib(EvStrongContext) {
		t.Error("direct 增益应高于 strong_context")
	}
}

// 既有向后兼容：兴趣型「跑步」三条本人字面命中 → 全部 direct → 置信度≈0.84（旧公式一致），
// 且证据行现带 evidence_type=direct，视图可核对。
func TestInterestEvidenceStillDirect(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "跑步人")
	if err := SaveProfile(db, cid, `{"interests":["跑步"],"summary":"s"}`, "s", "i"); err != nil {
		t.Fatal(err)
	}
	regressionMessages(t, db, cid, "今天去跑步了", "周末也跑步", "一起跑步吗", "吃饭了吗")
	if _, _, err := RebuildFactsAndEvidence(db, cid); err != nil {
		t.Fatal(err)
	}
	fs, err := GetFacts(db, cid, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		if f.Type == "interest" && f.Value == "跑步" {
			if len(f.Evidence) != 3 {
				t.Fatalf("应挂 3 条证据, got %d", len(f.Evidence))
			}
			if f.Confidence < 0.8 || f.Confidence > 0.85 {
				t.Fatalf("兴趣直接证据置信度应≈0.84, got %f", f.Confidence)
			}
			for _, ev := range f.Evidence {
				if ev.EvidenceType != EvDirect {
					t.Errorf("兴趣本人命中应判 direct, got %s (%+v)", ev.EvidenceType, ev)
				}
			}
			return
		}
	}
	t.Fatal("未找到跑步事实")
}

// §7.4 关键：身份事实仅被他人话题命中 → topic_related，置信度保持基线 0.6、不抬高。
func TestTopicRelatedDoesNotBoostIdentityFact(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "被议论的人")
	if err := SaveProfile(db, cid, `{"basic_info":{"occupation":"律师"},"summary":"s"}`, "s", "i"); err != nil {
		t.Fatal(err)
	}
	regressionMessages(t, db, cid, "你认识那个律师吗", "那个律师收费贵不贵")
	if _, _, err := RebuildFactsAndEvidence(db, cid); err != nil {
		t.Fatal(err)
	}
	fs, err := GetFacts(db, cid, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		if f.Type == "occupation" && f.Value == "律师" {
			if len(f.Evidence) == 0 {
				t.Fatal("应命中关键词证据（但为 topic_related）")
			}
			for _, ev := range f.Evidence {
				if ev.EvidenceType != EvTopicRelated || ev.IsDirectSupport {
					t.Errorf("他人话题命中应为 topic_related 且非直接支撑, got %+v", ev)
				}
			}
			if math.Abs(f.Confidence-0.6) > 1e-6 {
				t.Fatalf("§7.4：纯关键词不得提高置信度, got %f want 0.6", f.Confidence)
			}
			return
		}
	}
	t.Fatal("未找到律师事实")
}

// 混合：一条第一人称 direct + 一条他人 topic → 仅 direct 计入置信度（0.68），强度 0.5。
func TestMixedEvidenceWeightsOnlyDirect(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "混合证据")
	if err := SaveProfile(db, cid, `{"basic_info":{"occupation":"律师"},"summary":"s"}`, "s", "i"); err != nil {
		t.Fatal(err)
	}
	regressionMessages(t, db, cid, "我是律师", "那个律师很忙")
	if _, _, err := RebuildFactsAndEvidence(db, cid); err != nil {
		t.Fatal(err)
	}
	fs, err := GetFacts(db, cid, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		if f.Type == "occupation" && f.Value == "律师" {
			if len(f.Evidence) != 2 {
				t.Fatalf("应挂 2 条证据, got %d", len(f.Evidence))
			}
			// 仅 1 条 direct 计入 → conf=0.6+0.08=0.68（若两条都按旧公式=0.76，证明 §7.4 生效）
			if math.Abs(f.Confidence-0.68) > 1e-6 {
				t.Fatalf("混合证据置信度应只计入 direct=0.68, got %f", f.Confidence)
			}
			if math.Abs(f.EvidenceStrength-0.5) > 1e-6 {
				t.Fatalf("证据强度应为 direct/total=0.5, got %f", f.EvidenceStrength)
			}
			if f.ConfidenceType != "direct" {
				t.Errorf("恰 1 条直接证据应为 direct, got %s", f.ConfidenceType)
			}
			return
		}
	}
	t.Fatal("未找到律师事实")
}
