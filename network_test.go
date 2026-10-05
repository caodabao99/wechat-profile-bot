package main

// 高阶洞察 · Phase A 社交网络智能回归。
// 覆盖：确定性图算法（Tarjan 割点 / Brandes 介数 / 圈名规则 / 结构洞撮合）、
//       脆弱度公式、整图重建幂等（同输入同输出）、空图优雅降级、缓存读写。

import (
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// seedConn 直接在 contact_connections 里插一条无向边（测试专用，绕开画像抽取）。
func seedConn(t *testing.T, db *sql.DB, a, b int64) {
	t.Helper()
	low, high := a, b
	if low > high {
		low, high = high, low
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO contact_connections
		(contact_a, contact_b, connection_type, detail, confidence, created_at)
		VALUES (?,?,?,?,?,?)`, low, high, "shared_interest", "t", 0.9, "2026-10-04 10:00:00"); err != nil {
		t.Fatal(err)
	}
}

func TestArticulationPointsPathAndCycle(t *testing.T) {
	// 路径图 0-1-2：中间点 1 是割点，两端不是。
	path := [][]int{{1}, {0, 2}, {1}}
	ap := articulationPoints(path)
	if len(ap) != 3 || ap[0] || !ap[1] || ap[2] {
		t.Fatalf("路径图中点应为唯一割点, got %v", ap)
	}
	// 三角形 0-1-2 全连：无割点。
	tri := [][]int{{1, 2}, {0, 2}, {0, 1}}
	ap2 := articulationPoints(tri)
	for i, v := range ap2 {
		if v {
			t.Fatalf("三角形不该有割点, node %d flagged", i)
		}
	}
	// 星形：中心 0 连 1,2,3 → 0 是割点，叶子不是。
	star := [][]int{{1, 2, 3}, {0}, {0}, {0}}
	ap3 := articulationPoints(star)
	if !ap3[0] || ap3[1] || ap3[2] || ap3[3] {
		t.Fatalf("星形中心应为割点, got %v", ap3)
	}
}

func TestBrandesBetweennessMiddleDominates(t *testing.T) {
	// 路径图 0-1-2-3：内部点介数应高于端点。
	adj := [][]int{{1}, {0, 2}, {1, 3}, {2}}
	bc := brandesBetweenness(adj)
	if len(bc) != 4 {
		t.Fatalf("应返回 4 个介数值, got %d", len(bc))
	}
	if bc[0] != 0 || bc[3] != 0 {
		t.Errorf("端点介数应为 0, got [%v %v]", bc[0], bc[3])
	}
	if !(bc[1] > bc[0] && bc[2] > bc[3]) {
		t.Errorf("内部点介数应高于端点, got %v", bc)
	}
	// 对称：路径中间两点对称，介数应相等。
	if absF(bc[1]-bc[2]) > 1e-9 {
		t.Errorf("对称路径中间两点介数应相等, got %v vs %v", bc[1], bc[2])
	}
}

func TestClusterLabelRules(t *testing.T) {
	mems := []netMem{{name: "老王", bal: 100, id: 1}, {name: "小李", bal: 50, id: 2}}
	// 主导类别 >= 50% 且非"其他" → "<类别>圈"
	if got := clusterLabel(map[string]int{"同事": 3, "朋友": 1}, mems); got != "同事圈" {
		t.Errorf("应命名为同事圈, got %s", got)
	}
	// "其他" 不作圈名 → 退回核心成员
	if got := clusterLabel(map[string]int{"其他": 4}, mems); got != "老王 的圈子" {
		t.Errorf("其他类应退回成员名, got %s", got)
	}
	// 无类别数据 → 核心成员名
	if got := clusterLabel(map[string]int{}, mems); got != "老王 的圈子" {
		t.Errorf("空类别应退回成员名, got %s", got)
	}
	// 无成员 → 兜底
	if got := clusterLabel(map[string]int{}, nil); got != "一个圈子" {
		t.Errorf("无成员应兜底, got %s", got)
	}
}

func TestFindIntroductionsStructureHole(t *testing.T) {
	// 图：0(簇A)—1(簇A)—2(簇B)。0 与 2 不同簇、无直接连线、共享邻居 1 → 一条撮合。
	g := &netGraph{
		ids:   []int64{100, 200, 300},
		index: map[int64]int{100: 0, 200: 1, 300: 2},
		adj:   [][]int{{1}, {0, 2}, {1}},
	}
	clusters := []NetworkCluster{{ID: 0, Label: "A圈"}, {ID: 1, Label: "B圈"}}
	nodeCluster := []int{0, 0, 1}
	names := map[int64]string{100: "甲", 200: "桥梁", 300: "乙"}
	intros := findIntroductions(g, nodeCluster, clusters, names, map[int64]LifeAsset{})
	if len(intros) != 1 {
		t.Fatalf("应发现 1 条结构洞撮合, got %d: %+v", len(intros), intros)
	}
	it := intros[0]
	if it.AID != 100 || it.BID != 300 {
		t.Errorf("配对应为 100/300, got %d/%d", it.AID, it.BID)
	}
	if it.Shared != 1 || it.BridgeName != "桥梁" {
		t.Errorf("应经桥梁(共享1), got shared=%d bridge=%s", it.Shared, it.BridgeName)
	}
}

// TestFindIntroductionsDeterministicBridge 锁定确定性修复：两个共享邻居时，
// 桥接人必稳定选下标最小者（升序邻接表），且不随多次调用飘移。
func TestFindIntroductionsDeterministicBridge(t *testing.T) {
	g := &netGraph{
		ids:   []int64{100, 200, 300, 400},
		index: map[int64]int{100: 0, 200: 1, 300: 2, 400: 3},
		adj:   [][]int{{1, 2}, {0, 3}, {0, 3}, {1, 2}},
	}
	nodeCluster := []int{0, -1, -1, 1}
	clusters := []NetworkCluster{{ID: 0, Label: "A"}, {ID: 1, Label: "B"}}
	names := map[int64]string{100: "甲", 200: "桥低", 300: "桥高", 400: "乙"}
	var firstName, firstReason string
	for i := 0; i < 50; i++ {
		intros := findIntroductions(g, nodeCluster, clusters, names, map[int64]LifeAsset{})
		if len(intros) != 1 {
			t.Fatalf("应发现 1 条撮合, got %d: %+v", len(intros), intros)
		}
		if intros[0].Shared != 2 {
			t.Fatalf("应有 2 个共享邻居, got %d", intros[0].Shared)
		}
		if intros[0].BridgeName != "桥低" {
			t.Fatalf("桥接人应稳定为最小下标邻居「桥低」, got %s", intros[0].BridgeName)
		}
		if i == 0 {
			firstName, firstReason = intros[0].BridgeName, intros[0].Reason
		} else if intros[0].BridgeName != firstName || intros[0].Reason != firstReason {
			t.Fatalf("第 %d 次结果飘移（非确定性）: %s/%s vs %s/%s", i, intros[0].BridgeName, intros[0].Reason, firstName, firstReason)
		}
	}
}

func TestFindIntroductionsEmptyWhenSingleCluster(t *testing.T) {
	g := &netGraph{
		ids:   []int64{1, 2},
		index: map[int64]int{1: 0, 2: 1},
		adj:   [][]int{{1}, {0}},
	}
	nodeCluster := []int{0, 0}
	clusters := []NetworkCluster{{ID: 0, Label: "唯一圈"}}
	intros := findIntroductions(g, nodeCluster, clusters, map[int64]string{1: "a", 2: "b"}, map[int64]LifeAsset{})
	if len(intros) != 0 {
		t.Fatalf("单簇不该硬凑撮合, got %+v", intros)
	}
}

func TestFragilityNoteBranches(t *testing.T) {
	if got := fragilityNote(0, 0, 0, 0); got == "" {
		t.Error("空图应有说明")
	}
	if got := fragilityNote(5, 0, 0, 0); got == "" {
		t.Error("无割点应有抗风险说明")
	}
	got := fragilityNote(5, 2, 1, 3)
	if got == "" {
		t.Error("有脆弱桥梁应有风险说明")
	}
}

func TestComputeNetworkInsightsEmptyGraph(t *testing.T) {
	db := regressionAssistantDB(t)
	now := time.Now()
	if err := ComputeNetworkInsights(db, now); err != nil {
		t.Fatal(err)
	}
	ins, genAt, err := GetCachedNetwork(db)
	if err != nil || ins == nil {
		t.Fatalf("应能读回缓存: %v", err)
	}
	if ins.NodeCount != 0 || ins.ClusterCount != 0 {
		t.Errorf("空库应得空网络, got nodes=%d clusters=%d", ins.NodeCount, ins.ClusterCount)
	}
	if len(ins.Bridges) != 0 || len(ins.Introductions) != 0 {
		t.Errorf("空库不应有桥梁/撮合: %+v", ins)
	}
	if IsNetworkStale(genAt, now) {
		t.Error("刚生成的缓存不该过期")
	}
	if !IsNetworkStale(genAt, now.Add(8*24*time.Hour)) {
		t.Error("8 天后应判过期")
	}
}

func TestComputeNetworkInsightsDeterministic(t *testing.T) {
	db := regressionAssistantDB(t)
	// 哑铃图：簇A(1-2-3)、簇B(4-5-6)、桥 7 连接两侧。
	ids := make([]int64, 0, 7)
	for i := 0; i < 7; i++ {
		ids = append(ids, regressionContact(t, db, string(rune('A'+i))))
	}
	a1, a2, a3, b1, b2, b3, bridge := ids[0], ids[1], ids[2], ids[3], ids[4], ids[5], ids[6]
	seedConn(t, db, a1, a2)
	seedConn(t, db, a2, a3)
	seedConn(t, db, a1, a3)
	seedConn(t, db, b1, b2)
	seedConn(t, db, b2, b3)
	seedConn(t, db, b1, b3)
	seedConn(t, db, a1, bridge)
	seedConn(t, db, bridge, b1)

	now := time.Now()
	if err := ComputeNetworkInsights(db, now); err != nil {
		t.Fatal(err)
	}
	first, _, err := GetCachedNetwork(db)
	if err != nil || first == nil {
		t.Fatal(err)
	}
	if first.NodeCount != 7 || first.EdgeCount != 8 {
		t.Errorf("节点/边数不符: nodes=%d edges=%d", first.NodeCount, first.EdgeCount)
	}
	if first.ClusterCount < 2 {
		t.Errorf("应识别出至少 2 个圈子, got %d", first.ClusterCount)
	}
	// 桥应是割点
	var bridgeFound bool
	for _, b := range first.Bridges {
		if b.ContactID == bridge {
			bridgeFound = true
			if !b.Articulation {
				t.Error("桥接点应被标为割点")
			}
		}
	}
	if !bridgeFound {
		t.Errorf("桥梁清单里应包含桥节点 %d: %+v", bridge, first.Bridges)
	}

	// 幂等：再算一次，关键结构指标必须完全一致（确定性）。
	if err := ComputeNetworkInsights(db, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	second, _, err := GetCachedNetwork(db)
	if err != nil || second == nil {
		t.Fatal(err)
	}
	if first.NodeCount != second.NodeCount || first.EdgeCount != second.EdgeCount ||
		first.ClusterCount != second.ClusterCount || len(first.Bridges) != len(second.Bridges) ||
		first.FragilityScore != second.FragilityScore {
		t.Fatalf("同输入两次结果不一致（非确定性）:\n first=%+v\n second=%+v", first, second)
	}
	for i := range first.Clusters {
		if first.Clusters[i].Label != second.Clusters[i].Label || first.Clusters[i].Size != second.Clusters[i].Size {
			t.Fatalf("第 %d 个簇两次不一致: %+v vs %+v", i, first.Clusters[i], second.Clusters[i])
		}
	}
}

// v4.5.0 B：图计算规模护栏。喂 >netMaxNodes 个节点（链式相连），应触发 top-K 截断、
// 只保留最亲密核心圈（无 life_state 缓存时余额全 0，按 id 升序稳定取 top-250），且确定性成立。
func TestBuildNetworkScaleGuardrailTruncates(t *testing.T) {
	db := regressionAssistantDB(t)
	const n = netMaxNodes + 10
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		ids = append(ids, regressionContact(t, db, fmt.Sprintf("node-%03d", i)))
	}
	// 链式：i—i+1，共 n-1 条边、n 个节点（>netMaxNodes）。
	for i := 0; i+1 < len(ids); i++ {
		seedConn(t, db, ids[i], ids[i+1])
	}
	now := time.Now()

	if err := ComputeNetworkInsights(db, now); err != nil {
		t.Fatal(err)
	}
	first, _, err := GetCachedNetwork(db)
	if err != nil || first == nil {
		t.Fatal(err)
	}
	if !first.Truncated {
		t.Fatalf("超上限应触发截断, got Truncated=%v nodes=%d", first.Truncated, first.NodeCount)
	}
	if first.NodeCap != netMaxNodes {
		t.Errorf("NodeCap 应为 %d, got %d", netMaxNodes, first.NodeCap)
	}
	if first.NodeCount > netMaxNodes {
		t.Errorf("入图节点数应被封顶 %d, got %d", netMaxNodes, first.NodeCount)
	}
	// 截断说明应被追加进 Insights。
	var noted bool
	for _, s := range first.Insights {
		if strings.Contains(s, "核心圈") {
			noted = true
		}
	}
	if !noted {
		t.Error("截断时应在 Insights 追加聚焦核心圈说明")
	}

	// 确定性：同输入再算一次，结构指标必须完全一致（tie-break id 升序稳定）。
	if err := ComputeNetworkInsights(db, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	second, _, err := GetCachedNetwork(db)
	if err != nil || second == nil {
		t.Fatal(err)
	}
	if first.NodeCount != second.NodeCount || first.EdgeCount != second.EdgeCount ||
		first.Truncated != second.Truncated || first.NodeCap != second.NodeCap {
		t.Fatalf("截断两次不一致: first(nodes=%d edges=%d) second(nodes=%d edges=%d)",
			first.NodeCount, first.EdgeCount, second.NodeCount, second.EdgeCount)
	}
}

// 小图（<netMaxNodes）不应触发截断。
func TestBuildNetworkUnderCapNotTruncated(t *testing.T) {
	db := regressionAssistantDB(t)
	var prev int64
	for i := 0; i < 5; i++ {
		id := regressionContact(t, db, "small-"+string(rune('a'+i)))
		if prev != 0 {
			seedConn(t, db, prev, id)
		}
		prev = id
	}
	if err := ComputeNetworkInsights(db, time.Now()); err != nil {
		t.Fatal(err)
	}
	ins, _, err := GetCachedNetwork(db)
	if err != nil || ins == nil {
		t.Fatal(err)
	}
	if ins.Truncated || ins.NodeCap != 0 {
		t.Errorf("小图不该截断, got Truncated=%v NodeCap=%d", ins.Truncated, ins.NodeCap)
	}
}

// v4.6.0：导出可渲染拓扑 Nodes/Edges 的正确性与确定性。
func TestNetworkTopologyExport(t *testing.T) {
	db := regressionAssistantDB(t)
	// 哑铃图：簇A(1-2-3)、簇B(4-5-6)、桥 7 连接 a1—bridge—b1，共 8 边 7 节点。
	ids := make([]int64, 0, 7)
	for i := 0; i < 7; i++ {
		ids = append(ids, regressionContact(t, db, string(rune('A'+i))))
	}
	a1, a2, a3, b1, b2, b3, bridge := ids[0], ids[1], ids[2], ids[3], ids[4], ids[5], ids[6]
	for _, e := range [][2]int64{
		{a1, a2}, {a2, a3}, {a1, a3},
		{b1, b2}, {b2, b3}, {b1, b3},
		{a1, bridge}, {bridge, b1},
	} {
		seedConn(t, db, e[0], e[1])
	}

	now := time.Now()
	ins, err := buildNetwork(db, now)
	if err != nil {
		t.Fatal(err)
	}
	// 数量一致
	if len(ins.Nodes) != ins.NodeCount || len(ins.Edges) != ins.EdgeCount {
		t.Fatalf("Nodes/Edges 数量不符: nodes=%d/%d edges=%d/%d", len(ins.Nodes), ins.NodeCount, len(ins.Edges), ins.EdgeCount)
	}
	// 节点按联系人 id 升序、边按 (A,B) 升序
	for i := 1; i < len(ins.Nodes); i++ {
		if ins.Nodes[i-1].ContactID >= ins.Nodes[i].ContactID {
			t.Fatalf("Nodes 未按 contactID 升序: %d >= %d", ins.Nodes[i-1].ContactID, ins.Nodes[i].ContactID)
		}
	}
	for i := 1; i < len(ins.Edges); i++ {
		p, c := ins.Edges[i-1], ins.Edges[i]
		if p.A > c.A || (p.A == c.A && p.B >= c.B) {
			t.Fatalf("Edges 未按 (A,B) 升序: %+v vs %+v", p, c)
		}
	}
	// 每条边端点都在节点集；每边 A<B
	nodeSet := map[int64]bool{}
	deg := map[int64]int{}
	for _, n := range ins.Nodes {
		nodeSet[n.ContactID] = true
	}
	for _, e := range ins.Edges {
		if !nodeSet[e.A] || !nodeSet[e.B] {
			t.Fatalf("边端点不在节点集: %+v", e)
		}
		if e.A >= e.B {
			t.Fatalf("边应 A<B: %+v", e)
		}
		if e.Weight <= 0 {
			t.Errorf("边权应为 confidence(>0): %+v", e)
		}
		deg[e.A]++
		deg[e.B]++
	}
	// degree 与邻接一致；sum degree = 2*edges
	var sumDeg int
	for _, n := range ins.Nodes {
		if n.Degree != deg[n.ContactID] {
			t.Errorf("节点 %d degree=%d，实际边度=%d", n.ContactID, n.Degree, deg[n.ContactID])
		}
		sumDeg += n.Degree
		if n.Betweenness < 0 || n.Betweenness > 100 {
			t.Errorf("betweenness 越界: %v", n.Betweenness)
		}
		if n.Cluster < -1 || (n.Cluster >= 0 && n.Cluster >= ins.ClusterCount) {
			t.Errorf("cluster 索引非法: %d (clusters=%d)", n.Cluster, ins.ClusterCount)
		}
	}
	if sumDeg != 2*ins.EdgeCount {
		t.Errorf("sum(degree)=%d，应为 2*edges=%d", sumDeg, 2*ins.EdgeCount)
	}
	// 桥应为割点且高介数；叶子端点不为割点
	for _, n := range ins.Nodes {
		if n.ContactID == bridge && !n.Articulation {
			t.Error("桥接节点应为割点")
		}
		if n.Fragile && !n.Articulation {
			t.Error("Fragile 蕴含 Articulation")
		}
	}
	// 确定性：同输入两次 Nodes/Edges 逐字段全等
	ins2, err := buildNetwork(db, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ins.Nodes, ins2.Nodes) || !reflect.DeepEqual(ins.Edges, ins2.Edges) {
		t.Fatalf("拓扑导出两次不一致（非确定性）:\n nodes1=%d edges1=%d\n nodes2=%d edges2=%d",
			len(ins.Nodes), len(ins.Edges), len(ins2.Nodes), len(ins2.Edges))
	}

	// 缓存往返：经 Compute+GetCachedNetwork（JSON 序列化）仍带拓扑
	if err := ComputeNetworkInsights(db, now); err != nil {
		t.Fatal(err)
	}
	cached, _, err := GetCachedNetwork(db)
	if err != nil || cached == nil {
		t.Fatal(err)
	}
	if len(cached.Nodes) != 7 || len(cached.Edges) != 8 {
		t.Errorf("缓存往返后拓扑丢失: nodes=%d edges=%d", len(cached.Nodes), len(cached.Edges))
	}
}

// 空图 Nodes/Edges 应为非 nil 空数组（供前端 .length 直读不崩）。
func TestNetworkTopologyExportEmpty(t *testing.T) {
	db := regressionAssistantDB(t)
	ins, err := buildNetwork(db, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if ins.Nodes == nil || ins.Edges == nil {
		t.Fatalf("空图 Nodes/Edges 应为空数组非 nil: nodes=%v edges=%v", ins.Nodes, ins.Edges)
	}
	if len(ins.Nodes) != 0 || len(ins.Edges) != 0 {
		t.Errorf("空图应为零节点零边: %d/%d", len(ins.Nodes), len(ins.Edges))
	}
}
