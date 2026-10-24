# 跨服务对接文档生成提示词模板

## 使用场景

当一个服务（提供方）完成了新功能，需要另一个服务（对接方）调用时，使用本模板生成完整的对接文档。

---

## 基础提示词

```
我需要为 [对接方服务名] 生成一份完整的对接文档，帮助他们对接 [提供方服务名] 的 [功能名称]。

## 背景信息

### 提供方（我方）已完成的工作
- 服务名：[提供方服务名，如 ani-model-service]
- 功能：[功能描述，如训练产物注册]
- 协议：[gRPC / REST / 消息队列等]
- 已完成内容：
  - [ ] 协议定义（Protobuf / OpenAPI）
  - [ ] 服务端实现
  - [ ] 单元测试和集成测试
  - [ ] 数据库 schema
  - [ ] 权限配置

### 对接方需要做的工作
- 服务名：[对接方服务名，如 ani-modeldev-service]
- 对接目标：[对接的具体目标，如训练完成后自动注册模型版本]
- 当前状态：[对接方的现状，如尚未开始 / 有部分接口但不完整]

### 核心协议
- 协议类型：[gRPC / REST API / 消息队列]
- 端点：[服务地址，如 ani-model-service.ani.svc.cluster.local:9090]
- 认证方式：[mTLS / JWT / API Key]
- 关键方法：[列出核心方法，如 PrepareTrainedArtifact、CompleteTrainedArtifact]

### 关键技术点
- 数据流：[描述数据流向，如 ModelDev Storage → Model Storage]
- 幂等性：[如何保证幂等，如通过 idempotency_key]
- 错误处理：[关键错误场景，如上传失败、校验失败]
- 监控要求：[需要的监控指标]

## 文档要求

请生成以下三份文档：

### 1. 完整对接指南（详细版，15-25KB）
文件名：[对接方服务名]-integration-guide.md

章节结构：
1. 概述
   - 当前状态
   - 架构概览（包含 ASCII 架构图）
2. 对接范围
   - 需要实现的功能
   - 不需要做的事情
3. 调用协议
   - 身份和权限配置
   - gRPC/REST 连接示例
4. 核心流程详解（逐步说明，包含代码）
5. 完整代码示例（200+ 行可运行的代码）
6. 失败处理（各种错误场景和降级方案）
7. 测试验证（单元测试和集成测试指导）
8. 监控和告警（指标定义和告警规则）
9. 常见问题 FAQ
10. 参考资料和联系方式

### 2. 快速开始指南（简化版，3-5KB）
文件名：[对接方服务名]-quick-start.md

内容：
- 前置条件
- 5 分钟快速示例（最小可运行代码）
- 关键要点表格
- 常见错误速查
- 下一步引导

### 3. 实现任务清单（项目管理版，12-15KB）
文件名：[对接方服务名]-implementation-tasks.md

内容：
- 总览表格（任务、优先级、工作量、状态）
- 每个任务的详细说明：
  - 目标
  - 输入/输出
  - 代码骨架
  - 验收标准
  - 依赖关系
- 单元测试和集成测试用例
- 监控指标和告警配置
- 里程碑规划
- 风险和缓解措施
- 协作方式和联系方式

## 代码要求

- 提供完整可运行的代码示例（不是伪代码）
- 使用 [编程语言，如 Go]
- 包含错误处理和重试逻辑
- 代码注释清晰
- 可以直接复制粘贴使用

## 风格要求

- 技术准确，面向工程师
- 结构清晰，易于查找
- 提供多层次文档（快速上手 → 详细指南 → 任务清单）
- 包含图表和表格辅助理解
- 工作量估算要实际可行

## 参考资料

提供方的关键文件路径：
- 协议定义：[如 api/model/v1/model.proto]
- 设计文档：[如 docs/superpowers/specs/xxx-design.md]
- 服务端实现：[如 internal/service/xxx.go]
- 测试文件：[如 internal/service/xxx_test.go]

请基于这些信息生成完整的对接文档。
```

---

## 实际使用示例

### 示例 1：ModelDev 对接 Model Service 训练产物注册

```
我需要为 ModelDev 服务生成一份完整的对接文档，帮助他们对接 Model Service 的训练产物注册功能。

## 背景信息

### 提供方（我方）已完成的工作
- 服务名：ani-model-service
- 功能：训练产物注册（支持训练完成后自动创建可部署的 ModelVersion）
- 协议：gRPC
- 已完成内容：
  - [x] Protobuf 定义（PrepareTrainedArtifact、CompleteTrainedArtifact、AbortTrainedArtifact）
  - [x] 服务端实现（525 行核心代码）
  - [x] 单元测试和集成测试（8 个测试全部通过）
  - [x] 数据库 schema（trained_artifact_registrations 表）
  - [x] 权限配置（RPC 策略允许 Governance 身份）

### 对接方需要做的工作
- 服务名：ani-modeldev-service
- 对接目标：训练完成后，自动将 model.tar 注册到 Model Service，生成 ready 的 ModelVersion
- 当前状态：尚未开始，训练产物当前只发布到 ModelDev Storage

### 核心协议
- 协议类型：gRPC
- 端点：ani-model-service.ani.svc.cluster.local:9090
- 认证方式：mTLS（使用 Governance 身份，DNS SAN: ani-governance）
- 关键方法：
  1. PrepareTrainedArtifact：声明产物信息，获取上传地址
  2. CompleteTrainedArtifact：完成注册，创建 ModelVersion
  3. AbortTrainedArtifact：失败时终止注册

### 关键技术点
- 数据流：ModelDev Storage 读取 model.tar → 流式复制到 Model Storage（单次 PUT）
- 幂等性：通过 idempotency_key（格式：training:{execution_id}:model-version）
- 错误处理：上传失败调用 Abort，校验失败重试，幂等冲突告警
- 监控要求：注册成功率、耗时分布、上传速率

## 文档要求

请生成以下三份文档：

1. 完整对接指南（modeldev-integration-guide.md）
2. 快速开始指南（modeldev-quick-start.md）
3. 实现任务清单（modeldev-implementation-tasks.md）

代码要求：
- 使用 Go 语言
- 包含完整的 gRPC 客户端连接、对象复制、注册流程编排
- 包含错误处理和重试逻辑

参考资料：
- 协议定义：api/model/v1/model.proto
- 设计文档：docs/superpowers/specs/2026-10-20-trained-artifact-registration-design.md
- 服务端实现：internal/service/trained_artifact.go
- 测试文件：internal/service/trained_artifact_test.go
```

---

## 高级选项

### 如果需要补充特定内容

```
在生成的文档基础上，请额外补充：

1. 性能优化建议
   - 大文件上传的最佳实践
   - 并发注册的限流策略
   
2. 故障排查手册
   - 常见错误码和解决方法
   - 日志查看和问题定位
   
3. 安全最佳实践
   - mTLS 证书轮换
   - 敏感数据处理
```

### 如果需要特定编程语言

```
代码示例请使用 [Python / Java / Rust / TypeScript] 语言，并遵循该语言的惯用写法。
```

### 如果需要特定工具链

```
对接方使用以下技术栈，请在示例中体现：
- HTTP 客户端：[如 requests / axios / reqwest]
- 测试框架：[如 pytest / Jest / testify]
- 监控工具：[如 Prometheus / Datadog]
```

---

## 验证清单

生成文档后，请确认以下内容：

- [ ] 架构图清晰展示了数据流和服务交互
- [ ] 代码示例可以直接运行（或仅需最小修改）
- [ ] 错误处理覆盖了所有关键场景
- [ ] 工作量估算合理（通常 1-3 周）
- [ ] 监控指标和告警规则完整
- [ ] 提供了联系方式和协作渠道
- [ ] 文档分层（快速 → 详细 → 任务）满足不同角色需求

---

## 输出文件命名规范

```
docs/
  ├── [对接方]-integration-guide.md      # 完整指南
  ├── [对接方]-quick-start.md            # 快速开始
  └── [对接方]-implementation-tasks.md   # 任务清单
```

例如：
```
docs/
  ├── modeldev-integration-guide.md
  ├── modeldev-quick-start.md
  └── modeldev-implementation-tasks.md
```

---

## 后续维护

生成文档后，记得：

1. 将提示词和生成的文档提交到版本控制
2. 在 README 或主文档索引中增加链接
3. 通知对接方团队并组织 kickoff 会议
4. 根据对接过程中的反馈更新文档
5. 完成对接后编写实施总结

---

## 许可和归属

本提示词模板可用于任何内部跨服务对接场景，无需额外授权。

生成日期：2026-10-20  
维护者：Model Service 团队
