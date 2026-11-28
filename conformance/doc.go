// Package conformance 是 MCP 工具面的共享契约测试套件：把 mcpkit/iface
// 协议中立面（ErrorKind 五分类 + 工具能力接口）与工具结果元数据信封契约
// 写成可复用的场景集，供第三方工具面实现直接引入做合规验收。
//
// 方法论对标 oekit/pkg/cache/cachetest：Contract 抽象 + Run(t, c) 命名子测试
// 矩阵，每条子测试钉一条必须跨实现一致的语义；实现方提供 Factory（把被测
// 工具面装配到套件给定的脚本化 Backend 上），套件只断言出口事实。
//
// # 契约轴（评审 2.3-问题五：生态兼容性需要形成稳定契约）
//
//  1. **分类锚在协议层事实，不随文案漂移**：五类 ErrorKind 各自钉典型错误
//     形态（传输层 stdlib 事实 / JSON-RPC 错误码 / isError:true 业务失败）。
//     同一事实换任何服务端文案，分类必须停在同一类上——这是「下游不再做
//     字符串匹配」的机器化形态。isError:true 无协议码信号的业务失败保守
//     归 KindUnknown（mcpkit#144）；mcpkit 单源（mcp.ClassifyError）允许对
//     payload 里的明确信号（tool not found 等窄短语）上浮精化，那是增强不是
//     契约下浮——本套件钉的是下限与稳定性。
//  2. **不重试口径**：KindToolMissing / KindInvalidArgs 是确定性失败——
//     重试一万次工具也不会出现、非法参数也不会变合法（Retryable() 恒 false）；
//     KindUnknown 保守不重试（混着确定性业务失败，盲重试只会放大失败）。
//  3. **元数据信封契约**（跨仓集成硬契约）：工具结果出口必带完整
//     CallToolResult JSON 信封（content / structuredContent / _meta），
//     「信封包装是特性不是缺陷」——消费方视角断言信封不被剥掉。
//  4. **能力接口零值/缺省**：iface.RawNamer / iface.OutputSchemer 的零值与
//     缺省行为（缺能力/回零值时消费方的回落口径）。
//
// # fake transport（负对照必须能判红）
//
// 套件自带两个 fake transport 实现（被测工具面的内存替身）：
//
//   - [CompliantTransport] 正对照：事实锚定分类 + 信封保真——Run 对它全绿；
//   - [ViolatingTransport] 负对照：按文案猜分类（历史反模式）+ 剥信封——
//     套件的分类漂移/信封子测试必须对它判红（contract_test.go 钉住）。
//
// 判红能力与正对照同一组检查（同一单源），不是另写一套断言。
//
// # 与 guardx/toolfail 分类轴对照（只对照，不互相改判据）
//
// iface.ErrorKind 判**协议层事实**（这次调用在传输层出了什么事）；
// toolfail.Class 判**处置层语义**（这个失败值不值得原样重试）。两轴互补
// 不互换，重试判定映射如下（改判据去各自仓的契约测试，别在这张表上改）：
//
//	| iface.ErrorKind | Retryable() | 对照 toolfail.Class | Policy.Retryable | 口径 |
//	|-----------------|-------------|---------------------|------------------|------|
//	| KindUnreachable | true        | ClassTransient      | true             | 一致：退避重试 |
//	| KindProtocol    | true        | ClassTransient      | true             | 重试面对齐 transient（server 短暂异常可恢复）；处置面（修 server/升级）偏 ClassPermanent——两轴张力如实记，判据不动 |
//	| KindToolMissing | false       | ClassNotFound       | false            | 一致：重试不创造对象 |
//	| KindInvalidArgs | false       | ClassInvalidArgument| false            | 一致：改参数才有用 |
//	| KindUnknown     | false       | ClassUnknown        | false            | 一致：不可分类即不可重试（保守缺省） |
//
// 表内 toolfail 列只读自 guardx/toolfail 的 policyTable（ClassTransient/
// ClassNotFound/ClassInvalidArgument/ClassUnknown 的 Retryable 位）；
// KindProtocol ↔ ClassTransient 的对齐只覆盖重试位，处置位的差异（transient
// 要退避 vs permanent 要修 server）由宿主编排层自行取舍，两仓判据均不因此
// 改写（guardx 侧同款叙述见 toolfail 包注释「与 mcpkit/mcp.ErrorKind 的分工」）。
//
// # 边界（诚实声明）
//
// 判定行为单源仍在 mcp/mcp.ClassifyError（要 inspect mcp-go transport 类型与
// 哨兵）；本包只消费 iface 的协议中立出口（ClassifiedError / KindOf），自身
// 只依赖标准库 + testing + iface，不引 mcp-go——第三方实现方零重依赖可测。
// CompliantTransport 的分类是**参考实现**（wire 事实 → ErrorKind 的对照样板），
// 不是第二判定源。
//
// 生命周期: Experimental——本包仍会改、删、拆，不承诺任何兼容，升级风险自负；定级理由见 api-lifecycle.yaml，分级规则见 docs/api-compat.md。
package conformance
