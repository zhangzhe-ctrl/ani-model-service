# 对接文档生成提示词 - 快速版

## 一句话提示

```
为 [对接方服务] 生成对接 [我方服务] 的 [功能名] 的完整文档（详细指南、快速开始、任务清单），包含架构图、完整代码示例、错误处理、测试用例、监控配置。
```

## 结构化提示（推荐）

```
为 [对接方服务] 生成对接文档：

**服务信息**
- 提供方：[我方服务名]
- 功能：[功能描述]
- 协议：[gRPC/REST/MQ]
- 端点：[服务地址]
- 认证：[mTLS/JWT/Key]

**核心方法**
1. [方法1]：[作用]
2. [方法2]：[作用]
3. [方法3]：[作用]

**关键技术点**
- 数据流：[A → B → C]
- 幂等性：[实现方式]
- 错误处理：[关键场景]

**文档要求**
生成三份文档：
1. [服务名]-integration-guide.md（详细版，15-25KB）
2. [服务名]-quick-start.md（快速版，3-5KB）
3. [服务名]-implementation-tasks.md（任务版，12-15KB）

**代码要求**
- 语言：[Go/Python/Java]
- 完整可运行示例
- 包含错误处理和重试

**工作量估算**
预计 [1-3] 周

**参考文件**
- [协议定义文件路径]
- [设计文档路径]
- [实现代码路径]
```

---

## 真实示例（直接复制修改）

```
为 ModelDev 服务生成对接文档：

**服务信息**
- 提供方：ani-model-service
- 功能：训练产物注册
- 协议：gRPC
- 端点：ani-model-service.ani.svc.cluster.local:9090
- 认证：mTLS (Governance 身份)

**核心方法**
1. PrepareTrainedArtifact：声明产物，获取上传地址
2. CompleteTrainedArtifact：完成注册，创建 ModelVersion
3. AbortTrainedArtifact：失败时终止

**关键技术点**
- 数据流：ModelDev Storage → Model Storage（流式复制）
- 幂等性：idempotency_key = training:{execution_id}:model-version
- 错误处理：上传失败 Abort，校验失败重试

**文档要求**
生成三份文档：
1. modeldev-integration-guide.md（详细版）
2. modeldev-quick-start.md（快速版）
3. modeldev-implementation-tasks.md（任务版）

**代码要求**
- 语言：Go
- 完整 gRPC 客户端 + 对象复制 + 流程编排
- 包含错误处理和重试

**工作量估算**
预计 2 周

**参考文件**
- api/model/v1/model.proto
- docs/superpowers/specs/2026-10-20-trained-artifact-registration-design.md
- internal/service/trained_artifact.go
```

---

## 填空式模板（最简单）

```
为 ___________ 生成对接 ___________ 的文档

提供方：___________
功能：___________
协议：___________ (gRPC/REST/MQ)
端点：___________
认证：___________ (mTLS/JWT/Key)

核心方法：
1. ___________
2. ___________
3. ___________

数据流：___________ → ___________ → ___________
幂等性：___________
工作量：___________ 周

生成：详细指南、快速开始、任务清单
语言：___________ (Go/Python/Java)
```

---

## 使用建议

### 场景 1：新功能对接（推荐结构化提示）
用于完整的跨服务对接，生成全套文档。

### 场景 2：补充现有文档（用一句话提示）
在已有文档基础上补充特定章节。

### 场景 3：快速原型（用填空式模板）
快速生成草稿，后续人工优化。

---

## 快速检查清单

生成后检查：
- [ ] 架构图是否清晰？
- [ ] 代码能直接运行？
- [ ] 错误场景覆盖全？
- [ ] 工作量估算合理？
- [ ] 有联系方式？

---

保存此文件，下次直接修改使用！
