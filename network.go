package main

// 高阶洞察 · Phase A：社交网络智能（结构层）。
//
// 设计原则：系统主动做，人只看结果 / 零操作减负 / 纯确定性可解释 / 不调大模型。
//   - 把 contact_connections 的两两连线升级成一张「社交网络图」，跑四个经典图算法：
//       · 连通分量(BFS) + 标签传播(LPA) → 圈簇识别（家人圈/同事圈/旧友圈…）
//       · Tarjan 关节点(割点)          → 桥梁人物：删掉即让网络碎片化的关键人
//       · Brandes 介数中心性(无权)      → 谁处在最多最短路径上 = 结构性枢纽
//       · 结构洞配对(共享邻居、不同簇) → 撮合引荐：你可以把 A 引荐给 B
//   - 全部确定性：邻接按 id 升序、LPA 固定轮次 + 最小标签 tie-break，同输入同输出
//   - 风险配色复用人生状态缓存(GetCachedLifeState)的 RiskLevel，不重算、不嵌套取锁
//   - 缓存 network_insight_cache，沿用 life_*/weekly_plan 的单行派生缓存模式
//
// 单连接池铁律：GetCachedLifeState 先在外层取锁（自带释放），再单独取锁把
// contact_connections 一次读尽并 Close，聚合全在 Go 层做，最后 saveXxxCache 再取锁写。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// NetworkCluster 一个识别出的圈子。
type NetworkCluster struct {
	ID        int      `json:"id"`
	Label     string   `json:"label"` // 圈名：主导类别或核心成员
	Size      int      `json:"size"`
	Members   []string `json:"members"` // 展示用成员名（截断）
	MemberIDs []int64  `json:"-"`
}

// NetworkBridge 一个桥梁/枢纽人物。
type NetworkBridge struct {
	ContactID        int64    `json:"contactId"`
	Name             string   `json:"name"`
	Betweenness      float64  `json:"betweenness"`      // 归一化介数重要度 0-100
	Articulation     bool     `json:"articulation"`     // 是否割点（删掉即碎片化）
	RiskLevel        string   `json:"riskLevel"`        // 复用人生状态风险
	Fragile          bool     `json:"fragile"`          // 割点且高风险 = 最脆弱
	ConnectsClusters []string `json:"connectsClusters"` // 它连通的圈名
}

// Introduction 一条撮合引荐建议（结构洞配对）。
type Introduction struct {
	AID        int64  `json:"aId"`
	AName      string `json:"aName"`
	BID        int64  `json:"bId"`
	BName      string `json:"bName"`
	BridgeName string `json:"bridgeName"`
	Shared     int    `json:"shared"` // 共同邻居数
	Reason     string `json:"reason"`
}

// NetworkNode 图上单个联系人的可渲染节点（供前端力导向图消费）。
type NetworkNode struct {
	ContactID    int64   `json:"contactId"`
	Name         string  `json:"name"`
	Cluster      int     `json:"cluster"`      // 所属簇下标，-1 表示未成圈
	RiskLevel    string  `json:"riskLevel"`    // 复用人生状态风险
	Betweenness  float64 `json:"betweenness"`  // 归一化介数重要度 0-100
	Articulation bool    `json:"articulation"` // 是否割点（前端标红环）
	Fragile      bool    `json:"fragile"`      // 割点且高风险
	Degree       int     `json:"degree"`       // 连接数（前端节点半径依据）
}

// NetworkEdge 图上的一条无向连线（已按端点联系人 id 归一化、去重）。
type NetworkEdge struct {
	A      int64   `json:"a"`
	B      int64   `json:"b"`
	Weight float64 `json:"weight"` // 互动强度代理：contact_connections.confidence
}

// NetworkInsight 一次完整的社交网络洞察快照。
type NetworkInsight struct {
	GeneratedAt    string           `json:"generatedAt"`
	NodeCount      int              `json:"nodeCount"`
	EdgeCount      int              `json:"edgeCount"`
	ClusterCount   int              `json:"clusterCount"`
	Clusters       []NetworkCluster `json:"clusters"`
	Bridges        []NetworkBridge  `json:"bridges"`
	FragilityScore float64          `json:"fragilityScore"` // 0-1，越高越依赖少数桥
	FragilityNote  string           `json:"fragilityNote"`
	Truncated      bool             `json:"truncated"` // 节点超上限时为真，图仅覆盖核心圈
	NodeCap        int              `json:"nodeCap"`   // 触发截断时的入图上限（0=未截断）
	Introductions  []Introduction   `json:"introductions"`
	Insights       []string         `json:"insights"`
	Nodes          []NetworkNode    `json:"nodes"` // 可渲染拓扑（v4.6.0，加性字段）
	Edges          []NetworkEdge    `json:"edges"`
}

const (
	netLPAIterations  = 12  // 标签传播固定轮次（确定性）
	netMaxBridges     = 8   // 桥梁人物展示上限
	netMaxIntros      = 5   // 撮合引荐展示上限
	netMinClusterSize = 2   // 少于两人的"簇"不成圈
	netMaxMembersShow = 8   // 每簇展示成员名上限
	netMaxNodes       = 250 // 入图规模上限：超出只保留最亲密的 top-K 核心圈，防 Brandes/Tarjan 拖慢
)

// ComputeNetworkInsights 重算社交网络洞察并写入缓存。
func ComputeNetworkInsights(db *sql.DB, now time.Time) error {
	ins, err := buildNetwork(db, now)
	if err != nil {
		return err
	}
	return saveNetworkCache(db, now, ins)
}

// netGraph 是内部图结构：节点用连续下标 0..n-1，ids[i] 回映联系人 id。
type netGraph struct {
	ids   []int64       // 下标 → 联系人 id
	index map[int64]int // 联系人 id → 下标
	adj   [][]int       // 邻接表（升序去重）
}

func buildNetwork(db *sql.DB, now time.Time) (*NetworkInsight, error) {
	// 1) 先在外层取人生状态（GetCachedLifeState 内部自取锁、自带释放），
	//    拿到风险/类别/名字/余额配色；拿不到也不影响结构分析。
	assets := map[int64]LifeAsset{}
	names := map[int64]string{}
	if st, _, err := GetCachedLifeState(db); err == nil && st != nil {
		for _, a := range st.Assets {
			assets[a.ContactID] = a
			if a.Name != "" {
				names[a.ContactID] = a.Name
			}
		}
	}

	// 2) 单独取锁把 contact_connections 边一次读尽进内存并 Close。
	type edge struct {
		a, b int64
		w    float64 // confidence：互动强度代理
	}
	var edges []edge
	dbMu.Lock()
	rows, err := db.Query(`SELECT contact_a, contact_b, COALESCE(confidence, 0) FROM contact_connections`)
	if err != nil {
		dbMu.Unlock()
		return nil, fmt.Errorf("读取关系连线失败: %w", err)
	}
	for rows.Next() {
		var a, b int64
		var w float64
		if rows.Scan(&a, &b, &w) == nil && a != b {
			edges = append(edges, edge{a, b, w})
		}
	}
	rows.Close()

	// 3) 补齐图上出现但人生状态里没有的联系人名字（同一段持锁内读尽）。
	needNames := map[int64]bool{}
	for _, e := range edges {
		if _, ok := names[e.a]; !ok {
			needNames[e.a] = true
		}
		if _, ok := names[e.b]; !ok {
			needNames[e.b] = true
		}
	}
	if len(needNames) > 0 {
		ids := make([]int64, 0, len(needNames))
		for id := range needNames {
			ids = append(ids, id)
		}
		placeholders := make([]string, len(ids))
		args := make([]interface{}, len(ids))
		for i, id := range ids {
			placeholders[i] = "?"
			args[i] = id
		}
		q := `SELECT id, COALESCE(NULLIF(remark,''), name) FROM contacts WHERE id IN (` +
			join(placeholders, ",") + `)`
		if nrows, err := db.Query(q, args...); err == nil {
			for nrows.Next() {
				var id int64
				var nm string
				if nrows.Scan(&id, &nm) == nil {
					names[id] = nm
				}
			}
			nrows.Close()
		}
	}
	dbMu.Unlock()

	ins := &NetworkInsight{GeneratedAt: now.Format("2006-01-02 15:04:05")}

	// 4) 建图：节点取所有出现在边里的联系人，按 id 升序编号（确定性）。
	nodeSet := map[int64]bool{}
	for _, e := range edges {
		nodeSet[e.a] = true
		nodeSet[e.b] = true
	}
	ids := make([]int64, 0, len(nodeSet))
	for id := range nodeSet {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	// 4b) 规模护栏（B）：节点过多时只保留最亲密的 top-K 核心圈。
	//   确定性：按 LifeAsset.Balance 降序，tie-break 联系人 id 升序；再过滤掉两端不都在核心圈的边。
	if len(ids) > netMaxNodes {
		ranked := make([]int64, len(ids))
		copy(ranked, ids)
		sort.SliceStable(ranked, func(i, j int) bool {
			bi, bj := assets[ranked[i]].Balance, assets[ranked[j]].Balance
			if bi != bj {
				return bi > bj
			}
			return ranked[i] < ranked[j]
		})
		keepSet := map[int64]bool{}
		for _, id := range ranked[:netMaxNodes] {
			keepSet[id] = true
		}
		keptEdges := edges[:0]
		for _, e := range edges {
			if keepSet[e.a] && keepSet[e.b] {
				keptEdges = append(keptEdges, e)
			}
		}
		edges = keptEdges
		// 重建节点集（仅保留仍在边中的核心节点，按 id 升序）。
		nodeSet2 := map[int64]bool{}
		for _, e := range edges {
			nodeSet2[e.a] = true
			nodeSet2[e.b] = true
		}
		ids = ids[:0]
		for _, id := range ranked[:netMaxNodes] {
			if nodeSet2[id] {
				ids = append(ids, id)
			}
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		ins.Truncated = true
		ins.NodeCap = netMaxNodes
	}
	g := &netGraph{ids: ids, index: map[int64]int{}, adj: make([][]int, len(ids))}
	for i, id := range ids {
		g.index[id] = i
	}
	// 边去重（无向）；同一对多条取最大 confidence（与读取顺序无关，保确定性）。
	seenEdge := map[[2]int]bool{}
	weightByPair := map[[2]int]float64{}
	for _, e := range edges {
		u, v := g.index[e.a], g.index[e.b]
		key := [2]int{u, v}
		if u > v {
			key = [2]int{v, u}
		}
		if e.w > weightByPair[key] {
			weightByPair[key] = e.w
		}
		if seenEdge[key] {
			continue
		}
		seenEdge[key] = true
		g.adj[u] = append(g.adj[u], v)
		g.adj[v] = append(g.adj[v], u)
	}
	for i := range g.adj {
		sort.Ints(g.adj[i])
	}
	ins.NodeCount = len(ids)
	ins.EdgeCount = len(seenEdge)

	if len(ids) == 0 {
		ins.Clusters = []NetworkCluster{}
		ins.Bridges = []NetworkBridge{}
		ins.Introductions = []Introduction{}
		ins.Insights = []string{}
		ins.Nodes = []NetworkNode{}
		ins.Edges = []NetworkEdge{}
		ins.FragilityNote = "还没有识别出人际关联，等画像里出现共同城市/兴趣/职业后会自动成网。"
		return ins, nil
	}

	// 5) 连通分量(BFS)。
	comp := make([]int, len(ids))
	for i := range comp {
		comp[i] = -1
	}
	nComp := 0
	for s := range comp {
		if comp[s] != -1 {
			continue
		}
		queue := []int{s}
		comp[s] = nComp
		for len(queue) > 0 {
			u := queue[0]
			queue = queue[1:]
			for _, v := range g.adj[u] {
				if comp[v] == -1 {
					comp[v] = nComp
					queue = append(queue, v)
				}
			}
		}
		nComp++
	}

	// 6) 圈簇识别：每个连通分量内做确定性标签传播(LPA)。
	labels := make([]int, len(ids))
	for i := range labels {
		labels[i] = i // 初始标签=自身下标（确定性起点）
	}
	for it := 0; it < netLPAIterations; it++ {
		changed := false
		for u := 0; u < len(ids); u++ { // 固定升序遍历
			if len(g.adj[u]) == 0 {
				continue // 孤立点保持自身
			}
			count := map[int]int{}
			for _, v := range g.adj[u] {
				count[labels[v]]++
			}
			best, bestN := labels[u], -1
			// 票数最多；tie-break 取最小标签（确定性）
			keys := make([]int, 0, len(count))
			for k := range count {
				keys = append(keys, k)
			}
			sort.Ints(keys)
			for _, k := range keys {
				if count[k] > bestN {
					best, bestN = k, count[k]
				}
			}
			if best != labels[u] {
				labels[u] = best
				changed = true
			}
		}
		if !changed {
			break
		}
	}

	// 归集成簇：按 (label) 分组，仅 size>=netMinClusterSize 成圈。
	groupMembers := map[int][]int{}
	for u := range labels {
		groupMembers[labels[u]] = append(groupMembers[labels[u]], u)
	}
	var clusters []NetworkCluster
	cid := 0
	for label, members := range groupMembers {
		if len(members) < netMinClusterSize {
			continue
		}
		sort.Ints(members)
		cl := NetworkCluster{ID: cid, Size: len(members), MemberIDs: make([]int64, 0, len(members))}
		catCount := map[string]int{}
		var mems []netMem
		for _, u := range members {
			id := g.ids[u]
			cl.MemberIDs = append(cl.MemberIDs, id)
			nm := names[id]
			if nm == "" {
				nm = fmt.Sprintf("联系人%d", id)
			}
			bal := 0
			if a, ok := assets[id]; ok {
				bal = a.Balance
				if a.Category != "" {
					catCount[a.Category]++
				}
			}
			mems = append(mems, netMem{name: nm, bal: bal, id: id})
		}
		// 圈名：取主导类别；无明显主导则取余额最高的核心成员
		cl.Label = clusterLabel(catCount, mems)
		// 展示成员：按余额降序取前若干
		sort.Slice(mems, func(i, j int) bool {
			if mems[i].bal != mems[j].bal {
				return mems[i].bal > mems[j].bal
			}
			return mems[i].id < mems[j].id
		})
		for i, m := range mems {
			if i >= netMaxMembersShow {
				break
			}
			cl.Members = append(cl.Members, m.name)
		}
		_ = label
		clusters = append(clusters, cl)
		cid++
	}
	// 簇按规模降序、再按首成员 id 升序（确定性）
	sort.Slice(clusters, func(i, j int) bool {
		if clusters[i].Size != clusters[j].Size {
			return clusters[i].Size > clusters[j].Size
		}
		return firstID(clusters[i]) < firstID(clusters[j])
	})
	// 重排稳定 id
	for i := range clusters {
		clusters[i].ID = i
	}
	if clusters == nil {
		clusters = []NetworkCluster{}
	}
	ins.Clusters = clusters
	ins.ClusterCount = len(clusters)

	// 节点 → 所属簇下标（-1 表示未成圈），供桥梁与撮合使用。
	nodeCluster := make([]int, len(ids))
	for i := range nodeCluster {
		nodeCluster[i] = -1
	}
	for ci, cl := range clusters {
		for _, id := range cl.MemberIDs {
			nodeCluster[g.index[id]] = ci
		}
	}

	// 7) Tarjan 关节点(割点)——割点集合是图不变量，与遍历顺序无关。
	artic := articulationPoints(g.adj)

	// 8) Brandes 介数中心性(无权)——最短路集合也是图不变量。
	btw := brandesBetweenness(g.adj)
	maxBtw := 0.0
	for _, v := range btw {
		if v > maxBtw {
			maxBtw = v
		}
	}

	// 组装桥梁人物：割点 + 介数靠前者。
	bridges := []NetworkBridge{}
	for u := range ids {
		if !artic[u] && !(maxBtw > 0 && btw[u] >= 0.15*maxBtw) {
			continue
		}
		id := g.ids[u]
		nb := names[id]
		if nb == "" {
			nb = fmt.Sprintf("联系人%d", id)
		}
		risk := ""
		if a, ok := assets[id]; ok {
			risk = a.RiskLevel
		}
		normB := 0.0
		if maxBtw > 0 {
			normB = roundF(btw[u]/maxBtw*100, 2)
		}
		// 它连通了哪些不同的圈
		setConn := map[int]bool{}
		for _, v := range g.adj[u] {
			if c := nodeCluster[v]; c >= 0 {
				setConn[c] = true
			}
		}
		conns := make([]int, 0, len(setConn))
		for c := range setConn {
			conns = append(conns, c)
		}
		sort.Ints(conns)
		var connNames []string
		for _, c := range conns {
			connNames = append(connNames, clusters[c].Label)
		}
		if connNames == nil {
			connNames = []string{}
		}
		bridges = append(bridges, NetworkBridge{
			ContactID: id, Name: nb, Betweenness: normB,
			Articulation: artic[u], RiskLevel: risk,
			Fragile:          artic[u] && risk == "high",
			ConnectsClusters: connNames,
		})
	}
	sort.Slice(bridges, func(i, j int) bool {
		if bridges[i].Articulation != bridges[j].Articulation {
			return bridges[i].Articulation
		}
		if bridges[i].Betweenness != bridges[j].Betweenness {
			return bridges[i].Betweenness > bridges[j].Betweenness
		}
		return bridges[i].ContactID < bridges[j].ContactID
	})
	if len(bridges) > netMaxBridges {
		bridges = bridges[:netMaxBridges]
	}
	ins.Bridges = bridges

	// 9) 网络脆弱度：割点数 / 节点数 × 高风险割点占比加权。
	artCount := 0
	for u := range artic {
		if artic[u] {
			artCount++
		}
	}
	fragileCount := 0
	for _, b := range bridges {
		if b.Fragile {
			fragileCount++
		}
	}
	score := 0.0
	if len(ids) > 0 {
		base := float64(artCount) / float64(len(ids))
		// 高风险桥占比抬升脆弱度
		penalty := 0.0
		if artCount > 0 {
			penalty = float64(fragileCount) / float64(artCount)
		}
		score = minF(base*0.6+penalty*0.4, 1)
	}
	ins.FragilityScore = roundF(score*100, 0) / 100 // 0-1，两位
	ins.FragilityNote = fragilityNote(len(ids), artCount, fragileCount, len(clusters))

	// 10) 撮合引荐：不同簇、未直接相连、共享 >=1 邻居(结构洞)的配对。
	ins.Introductions = findIntroductions(g, nodeCluster, clusters, names, assets)

	// 11) 洞察句子。
	ins.Insights = networkInsights(len(ids), ins.EdgeCount, len(clusters), artCount, fragileCount, clusters, bridges)
	// 规模护栏触发时置顶一句诚实说明（其余人未纳入图分析）。
	if ins.Truncated {
		ins.Insights = append([]string{fmt.Sprintf("社交网络较大，图分析已聚焦最亲密的 %d 人核心圈（其余暂未纳入）。", netMaxNodes)}, ins.Insights...)
	}

	// 12) 导出可渲染拓扑（v4.6.0）：节点按联系人 id 升序（ids 已升序），
	//   边按 (A,B) 升序（u 升序、adj[u] 升序且 v>u），与图读取/遍历顺序无关，保确定性。
	nodes := make([]NetworkNode, 0, len(ids))
	for u := range ids {
		id := g.ids[u]
		nm := names[id]
		if nm == "" {
			nm = fmt.Sprintf("联系人%d", id)
		}
		risk := ""
		if a, ok := assets[id]; ok {
			risk = a.RiskLevel
		}
		normB := 0.0
		if maxBtw > 0 {
			normB = roundF(btw[u]/maxBtw*100, 2)
		}
		nodes = append(nodes, NetworkNode{
			ContactID: id, Name: nm, Cluster: nodeCluster[u], RiskLevel: risk,
			Betweenness: normB, Articulation: artic[u],
			Fragile: artic[u] && risk == "high", Degree: len(g.adj[u]),
		})
	}
	ins.Nodes = nodes
	edgesOut := []NetworkEdge{}
	for u := range ids {
		for _, v := range g.adj[u] {
			if v <= u {
				continue // 无向边只取 u<v 一次
			}
			edgesOut = append(edgesOut, NetworkEdge{A: g.ids[u], B: g.ids[v], Weight: roundF(weightByPair[[2]int{u, v}], 4)})
		}
	}
	ins.Edges = edgesOut

	return ins, nil
}

// firstID 取簇内最小联系人 id（用于确定性排序）。
func firstID(cl NetworkCluster) int64 {
	var lo int64 = -1
	for _, id := range cl.MemberIDs {
		if lo < 0 || id < lo {
			lo = id
		}
	}
	if lo < 0 {
		return 0
	}
	return lo
}

// netMem 圈成员中间态（名字 / 余额 / id）。
type netMem struct {
	name string
	bal  int
	id   int64
}

// clusterLabel 依据类别分布与核心成员给圈子起名。
func clusterLabel(catCount map[string]int, mems []netMem) string {
	// 找主导类别（票数最多；tie-break 固定顺序）
	bestCat, bestN := "", 0
	for _, c := range []string{"家人", "同事", "朋友", "其他"} {
		if n := catCount[c]; n > bestN {
			bestCat, bestN = c, n
		}
	}
	total := 0
	for _, n := range catCount {
		total += n
	}
	if bestCat != "" && bestCat != "其他" && total > 0 && float64(bestN)/float64(total) >= 0.5 {
		return bestCat + "圈"
	}
	if len(mems) > 0 {
		return mems[0].name + " 的圈子"
	}
	return "一个圈子"
}

// articulationPoints 返回每个下标是否为关节点（割点）。无向图，Tarjan DFS。
func articulationPoints(adj [][]int) []bool {
	n := len(adj)
	disc := make([]int, n)
	low := make([]int, n)
	parent := make([]int, n)
	ap := make([]bool, n)
	for i := range disc {
		disc[i] = -1
		parent[i] = -1
	}
	time1 := 0
	// 对每个连通分量迭代式 DFS（避免递归栈，且按升序起点保证确定性）。
	for s := 0; s < n; s++ {
		if disc[s] != -1 {
			continue
		}
		type frame struct {
			u, ni int
		}
		stack := []frame{{s, 0}}
		rootChildren := 0
		for len(stack) > 0 {
			top := &stack[len(stack)-1]
			u := top.u
			if top.ni == 0 {
				disc[u] = time1
				low[u] = time1
				time1++
			}
			if top.ni < len(adj[u]) {
				v := adj[u][top.ni]
				top.ni++
				if disc[v] == -1 {
					parent[v] = u
					if u == s {
						rootChildren++
					}
					stack = append(stack, frame{v, 0})
				} else if v != parent[u] {
					if disc[v] < low[u] {
						low[u] = disc[v]
					}
				}
			} else {
				// u 处理完毕，回溯更新父的低链并判定割点
				p := parent[u]
				if p != -1 {
					if low[u] < low[p] {
						low[p] = low[u]
					}
					if parent[p] != -1 && low[u] >= disc[p] {
						ap[p] = true
					}
				}
				stack = stack[:len(stack)-1]
			}
		}
		if rootChildren > 1 {
			ap[s] = true
		}
	}
	return ap
}

// brandesBetweenness 无权图介数中心性（未归一化）。n≤数百、m≤500，毫秒级。
func brandesBetweenness(adj [][]int) []float64 {
	n := len(adj)
	bc := make([]float64, n)
	for s := 0; s < n; s++ {
		var stack []int
		pred := make([][]int, n)
		sigma := make([]float64, n)
		dist := make([]int, n)
		delta := make([]float64, n)
		for i := range dist {
			dist[i] = -1
		}
		sigma[s] = 1
		dist[s] = 0
		queue := []int{s}
		for len(queue) > 0 {
			v := queue[0]
			queue = queue[1:]
			stack = append(stack, v)
			for _, w := range adj[v] {
				if dist[w] < 0 {
					dist[w] = dist[v] + 1
					queue = append(queue, w)
				}
				if dist[w] == dist[v]+1 {
					sigma[w] += sigma[v]
					pred[w] = append(pred[w], v)
				}
			}
		}
		for i := len(stack) - 1; i >= 0; i-- {
			w := stack[i]
			for _, v := range pred[w] {
				if sigma[w] > 0 {
					delta[v] += (sigma[v] / sigma[w]) * (1 + delta[w])
				}
			}
			if w != s {
				bc[w] += delta[w]
			}
		}
	}
	// 无向图每条边被两个源点各计一次 → 除以 2
	for i := range bc {
		bc[i] /= 2
	}
	return bc
}

// findIntroductions 找结构洞配对：分处不同簇、尚无直接连线、但共享邻居的两人。
func findIntroductions(g *netGraph, nodeCluster []int, clusters []NetworkCluster,
	names map[int64]string, assets map[int64]LifeAsset) []Introduction {

	out := []Introduction{}
	if len(clusters) < 2 {
		return out
	}
	direct := map[[2]int]bool{}
	for u := range g.adj {
		for _, v := range g.adj[u] {
			key := [2]int{u, v}
			if u > v {
				key = [2]int{v, u}
			}
			direct[key] = true
		}
	}
	n := len(g.ids)
	for u := 0; u < n; u++ {
		cu := nodeCluster[u]
		if cu < 0 {
			continue
		}
		for w := u + 1; w < n; w++ {
			cw := nodeCluster[w]
			if cw < 0 || cw == cu {
				continue
			}
			key := [2]int{u, w}
			if u > w {
				key = [2]int{w, u}
			}
			if direct[key] {
				continue // 已认识
			}
			// 统计共同邻居（作为桥）。遍历升序邻接表 g.adj[u] 而非 map，
			//   保证 bridgeIdx 的 tie-break 确定性（同输入同输出）。
			shared := 0
			bridgeIdx := -1
			for _, x := range g.adj[u] {
				if isNeighbor(g.adj[x], w) {
					shared++
					if bridgeIdx < 0 {
						bridgeIdx = x
					}
				}
			}
			if shared < 1 {
				continue
			}
			aID, bID := g.ids[u], g.ids[w]
			an, bn := names[aID], names[bID]
			if an == "" {
				an = fmt.Sprintf("联系人%d", aID)
			}
			if bn == "" {
				bn = fmt.Sprintf("联系人%d", bID)
			}
			bn2 := "你们的共同好友"
			if bridgeIdx >= 0 {
				if s := names[g.ids[bridgeIdx]]; s != "" {
					bn2 = s
				}
			}
			// 撮合理由：跨了两个圈子
			reason := fmt.Sprintf("通过 %s 可以把「%s」和「%s」牵上线——他们分属你的两个不同圈子，此前没有直接往来", bn2, an, bn)
			out = append(out, Introduction{
				AID: aID, AName: an, BID: bID, BName: bn,
				BridgeName: bn2, Shared: shared, Reason: reason,
			})
		}
	}
	// 排序：共同邻居多者在前，再按 (余额和) 高者、按 id 稳定
	sort.Slice(out, func(i, j int) bool {
		if out[i].Shared != out[j].Shared {
			return out[i].Shared > out[j].Shared
		}
		si := assetBal(assets, out[i].AID) + assetBal(assets, out[i].BID)
		sj := assetBal(assets, out[j].AID) + assetBal(assets, out[j].BID)
		if si != sj {
			return si > sj
		}
		if out[i].AID != out[j].AID {
			return out[i].AID < out[j].AID
		}
		return out[i].BID < out[j].BID
	})
	if len(out) > netMaxIntros {
		out = out[:netMaxIntros]
	}
	return out
}

// isNeighbor 判断 w 是否在 adj 的升序邻接表里。
func isNeighbor(adjSorted []int, w int) bool {
	lo, hi := 0, len(adjSorted)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		if adjSorted[mid] == w {
			return true
		} else if adjSorted[mid] < w {
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return false
}

func assetBal(assets map[int64]LifeAsset, id int64) int {
	if a, ok := assets[id]; ok {
		return a.Balance
	}
	return 0
}

// fragilityNote 生成脆弱度一句话说明。
func fragilityNote(nodes, artCount, fragileCount, clusters int) string {
	switch {
	case nodes == 0:
		return "还没有识别出人际关联。"
	case artCount == 0:
		return "你的社交网络比较分散，没有哪一个人是不可或缺的枢纽，很抗风险。"
	case fragileCount > 0:
		return fmt.Sprintf("有 %d 位关键人正处在高风险断联中——他们一旦淡出，你的几个圈子之间可能就此断开。", fragileCount)
	default:
		return fmt.Sprintf("你的网络靠 %d 位桥梁人物把 %d 个圈子连在一起，多留意他们的状态。", artCount, clusters)
	}
}

// networkInsights 组装网络层的自动洞察句。
func networkInsights(nodes, edges, clusters, artCount, fragileCount int,
	cs []NetworkCluster, bs []NetworkBridge) []string {
	out := []string{}
	if nodes == 0 {
		return out
	}
	if clusters > 0 {
		names := make([]string, 0, len(cs))
		for i, c := range cs {
			if i >= 3 {
				break
			}
			names = append(names, fmt.Sprintf("%s(%d人)", c.Label, c.Size))
		}
		out = append(out, fmt.Sprintf("识别出 %d 个圈子：%s", clusters, join(names, "、")))
	}
	for _, b := range bs {
		if b.Fragile && len(b.ConnectsClusters) >= 2 {
			out = append(out, fmt.Sprintf("%s 是关键桥梁（连接 %s），但正在高风险断联",
				b.Name, join(b.ConnectsClusters, " 与 ")))
		}
	}
	if fragileCount == 0 && artCount > 0 {
		out = out[:minInt(len(out), 1)]
	}
	if len(out) > 4 {
		out = out[:4]
	}
	return out
}

// ---------- 缓存读写（镜像 life_state.go） ----------

func saveNetworkCache(db *sql.DB, now time.Time, ins *NetworkInsight) error {
	data, err := json.Marshal(ins)
	if err != nil {
		return fmt.Errorf("序列化社交网络洞察失败: %w", err)
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	_, err = db.Exec(
		`INSERT INTO network_insight_cache (id, generated_at, net_json) VALUES (1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET generated_at=excluded.generated_at, net_json=excluded.net_json`,
		now.Format(time.RFC3339), string(data))
	return err
}

// GetCachedNetwork 读取缓存；无行返回 nil,nil。
func GetCachedNetwork(db *sql.DB) (*NetworkInsight, time.Time, error) {
	dbMu.Lock()
	var genAt, raw string
	err := db.QueryRow(`SELECT generated_at, net_json FROM network_insight_cache WHERE id = 1`).Scan(&genAt, &raw)
	dbMu.Unlock()
	if err == sql.ErrNoRows {
		return nil, time.Time{}, nil
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	var ins NetworkInsight
	if err := json.Unmarshal([]byte(raw), &ins); err != nil {
		return nil, time.Time{}, fmt.Errorf("社交网络缓存解析失败: %w", err)
	}
	t, _ := time.Parse(time.RFC3339, genAt)
	return &ins, t, nil
}

// IsNetworkStale 缓存是否超过 7 天。
func IsNetworkStale(generatedAt time.Time, now time.Time) bool {
	if generatedAt.IsZero() {
		return true
	}
	return now.Sub(generatedAt) > 7*24*time.Hour
}

// ---------- 小工具 ----------

func join(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	s := parts[0]
	for _, p := range parts[1:] {
		s += sep + p
	}
	return s
}

func roundF(f float64, places int) float64 {
	p := 1.0
	for i := 0; i < places; i++ {
		p *= 10
	}
	return float64(int(f*p+0.5)) / p
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
