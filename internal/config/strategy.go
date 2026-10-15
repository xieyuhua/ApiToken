package config

// 支持的路由策略。
//
// 这四个字符串之前散落在 gateway / admin.settings / admin.diagnostics 四处，
// 靠手写 case 列表对齐；新增策略要同时改四个地方，漏一处就变成
// 「设置页能存、转发时不认识」的隐性降级。这里集中定义后，各处只引用常量。
const (
	// StrategyPriorityRoundRobin 按优先级分组，组内加权轮询（默认）。
	StrategyPriorityRoundRobin = "priority_round_robin"
	// StrategyRoundRobin 忽略优先级，全局加权轮询。
	StrategyRoundRobin = "round_robin"
	// StrategyFailover 严格按优先级排序的主备模式。
	StrategyFailover = "failover"
	// StrategyRandom 每次随机打散候选。
	StrategyRandom = "random"
)

// Strategies 全部合法策略，顺序即文档/提示里展示的顺序。
var Strategies = []string{
	StrategyPriorityRoundRobin,
	StrategyRoundRobin,
	StrategyFailover,
	StrategyRandom,
}

// IsValidStrategy 判断策略名是否受支持。
func IsValidStrategy(s string) bool {
	for _, v := range Strategies {
		if v == s {
			return true
		}
	}
	return false
}
