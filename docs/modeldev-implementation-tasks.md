# ModelDev 训练产物注册 - 实现任务清单

## 总览

| 模块 | 优先级 | 预估工作量 | 负责人 | 状态 |
|------|--------|-----------|--------|------|
| gRPC 客户端封装 | P0 | 1 天 | | ⏸️ |
| 对象复制逻辑 | P0 | 2 天 | | ⏸️ |
| 注册流程编排 | P0 | 2 天 | | ⏸️ |
| 回执持久化 | P0 | 1 天 | | ⏸️ |
| 错误处理和重试 | P1 | 2 天 | | ⏸️ |
| 单元测试 | P1 | 2 天 | | ⏸️ |
| 集成测试 | P1 | 3 天 | | ⏸️ |
| 监控和告警 | P2 | 1 天 | | ⏸️ |

**总计**：约 14 天（2 周）

---

## 任务 1：gRPC 客户端封装 【P0 - 1 天】

### 目标
封装 Model Service 的 gRPC 客户端，处理 mTLS 连接和元数据注入。

### 输入
- Model Service 地址：`ani-model-service.ani.svc.cluster.local:9090`
- Governance mTLS 证书路径（ConfigMap 或 Secret）
- API 依赖：`github.com/liangzai006/ani-model-service/api@v0.1.0`

### 准备工作
```bash
# ModelDev 项目添加依赖
go get github.com/liangzai006/ani-model-service/api@v0.1.0
```

### 输出
```go
package modelclient

import (
    modelv1 "github.com/liangzai006/ani-model-service/api/model/v1"
)

type Client struct {
    grpcClient modelv1.ModelServiceClient
    tenantID   string
}

func NewClient(serviceAddress string, tlsConfig *tls.Config, tenantID string) (*Client, error)

func (c *Client) PrepareTrainedArtifact(ctx context.Context, req *PrepareRequest) (*PrepareResponse, error)
func (c *Client) CompleteTrainedArtifact(ctx context.Context, registrationID string) (*CompleteResponse, error)
func (c *Client) AbortTrainedArtifact(ctx context.Context, registrationID, reason string) error
```

### 验收标准
- [ ] 成功连接到 Model Service（测试环境）
- [ ] 元数据 `x-ani-tenant-id`、`x-ani-actor`、`x-ani-request-id` 正确注入
- [ ] 单元测试覆盖连接建立和元数据注入

### 依赖
- Governance mTLS 证书配置到 ModelDev 控制面

---

## 任务 2：对象复制逻辑 【P0 - 2 天】

### 目标
实现从 ModelDev Storage 流式复制 `model.tar` 到 Model Storage。

### 输入
- ModelDev Storage 对象路径
- Model Service 签发的上传 URL（单次 PUT）
- 对象大小

### 输出
```go
package uploader

type ArtifactUploader struct {
    modelDevStorage ObjectStorage
    httpClient      *http.Client
}

func (u *ArtifactUploader) Upload(
    ctx context.Context,
    modelDevPath string,
    uploadURL string,
    sizeBytes int64,
) error
```

### 验收标准
- [ ] 能够从 ModelDev Storage 流式读取对象
- [ ] 使用 HTTP PUT 上传到 Model Storage
- [ ] 设置正确的 `Content-Length` 和超时时间
- [ ] 单元测试覆盖正常上传和失败场景
- [ ] 集成测试验证端到端复制（使用测试对象）

### 性能要求
- 支持至少 10GB 文件上传
- 上传速率 > 10 MB/s

### 依赖
- ModelDev Storage 提供流式读取接口

---

## 任务 3：注册流程编排 【P0 - 2 天】

### 目标
编排完整的注册流程：Prepare → Upload → Complete。

### 输入
```go
type RegistrationInput struct {
    TenantID             string
    ExecutionID          string
    TargetModelID        string   // 可选
    ModelName            string   // 新建 Model 时必填
    DisplayName          string
    Description          string
    Capabilities         []string
    SourceModelVersionID string
    Version              string
    ModelDevStoragePath  string
    SizeBytes            int64
    ChecksumSHA256       string
}
```

### 输出
```go
package registration

type Orchestrator struct {
    modelClient *modelclient.Client
    uploader    *uploader.ArtifactUploader
}

func (o *Orchestrator) RegisterArtifact(
    ctx context.Context,
    input RegistrationInput,
) (*RegistrationReceipt, error)

type RegistrationReceipt struct {
    RegistrationID string
    ModelID        string
    ModelVersionID string
    Status         string
    StoragePath    string
    SizeBytes      int64
    ChecksumSHA256 string
}
```

### 验收标准
- [ ] Prepare 成功后获取上传地址
- [ ] 上传完成后调用 Complete
- [ ] 返回完整的注册回执
- [ ] 幂等重试返回相同结果
- [ ] 单元测试覆盖核心流程

### 依赖
- 任务 1 和任务 2 完成

---

## 任务 4：回执持久化 【P0 - 1 天】

### 目标
将 Model Service 返回的回执写入 ModelDev 执行记录。

### 输入
- 执行 ID
- 注册回执（来自任务 3）

### 输出
```go
package persistence

type ExecutionRecordStore interface {
    SaveModelRegistration(ctx context.Context, executionID string, receipt RegistrationReceipt) error
    GetModelRegistration(ctx context.Context, executionID string) (*RegistrationReceipt, error)
}
```

### 数据库 Schema 变更
```sql
-- 在 execution_records 表中增加字段
ALTER TABLE execution_records ADD COLUMN model_registration JSONB;

-- 索引
CREATE INDEX idx_execution_records_model_id 
ON execution_records ((model_registration->>'model_id'));
```

### 验收标准
- [ ] 回执成功写入 execution_records
- [ ] 可通过执行 ID 查询回执
- [ ] 单元测试覆盖写入和查询

### 依赖
- 数据库 schema 迁移

---

## 任务 5：错误处理和重试 【P1 - 2 天】

### 目标
处理各类失败场景并实现重试逻辑。

### 场景

#### 5.1 上传失败
```go
// 上传失败时终止注册
if uploadErr != nil {
    _ = orchestrator.modelClient.AbortTrainedArtifact(ctx, registrationID, uploadErr.Error())
    return fmt.Errorf("upload failed: %w", uploadErr)
}
```

#### 5.2 对象校验失败
```go
// Complete 返回 OBJECT_INTEGRITY_MISMATCH
// 检查 ModelDev Storage 中的原始对象
// 重新计算 SHA256
// 决定是重试还是标记为失败
```

#### 5.3 幂等冲突
```go
// Prepare 返回 REGISTRATION_CONFLICT
// 说明幂等键相同但请求内容不同
// 检查幂等键生成逻辑
```

#### 5.4 上传地址过期
```go
// 上传地址有效期 15 分钟
// 超时后重新调用 Prepare 获取新地址
```

### 重试策略
```go
type RetryConfig struct {
    MaxAttempts int           // 最大重试次数：3
    InitialDelay time.Duration // 初始延迟：1s
    MaxDelay    time.Duration // 最大延迟：30s
    Multiplier  float64       // 退避倍数：2.0
}
```

### 验收标准
- [ ] 上传失败时调用 Abort
- [ ] 幂等冲突时记录并告警
- [ ] 上传地址过期时自动重新获取
- [ ] 单元测试覆盖所有错误场景
- [ ] 集成测试验证重试逻辑

---

## 任务 6：单元测试 【P1 - 2 天】

### 目标
编写单元测试覆盖核心逻辑。

### 测试用例

#### 6.1 gRPC 客户端
- [ ] 测试连接建立
- [ ] 测试元数据注入
- [ ] 测试超时处理

#### 6.2 对象复制
- [ ] 测试正常上传
- [ ] 测试上传失败
- [ ] 测试读取失败

#### 6.3 注册流程
- [ ] 测试完整流程（已有 Model）
- [ ] 测试完整流程（新建 Model）
- [ ] 测试幂等重试
- [ ] 测试 Prepare 失败
- [ ] 测试 Upload 失败
- [ ] 测试 Complete 失败

#### 6.4 回执持久化
- [ ] 测试写入成功
- [ ] 测试查询成功
- [ ] 测试并发写入

### 覆盖率要求
- 核心业务逻辑：> 80%
- 错误处理分支：> 60%

---

## 任务 7：集成测试 【P1 - 3 天】

### 目标
验证端到端流程和跨服务集成。

### 测试环境
- Model Service（测试环境）
- ModelDev Storage（测试 bucket）
- PostgreSQL（测试数据库）

### 测试用例

#### 7.1 端到端正常流程
```
1. 准备测试产物（1GB model.tar）
2. 上传到 ModelDev Storage
3. 调用注册流程
4. 验证 Model Service 中存在 ready 的 ModelVersion
5. 验证回执已持久化到 execution_records
6. 验证 Inference 能下载对象
```

#### 7.2 失败恢复
```
1. 模拟上传中断（50% 处断开连接）
2. 验证调用 Abort
3. 重试注册
4. 验证最终成功
```

#### 7.3 幂等性
```
1. 完成一次注册
2. 使用相同幂等键重试
3. 验证返回相同的 model_id 和 model_version_id
4. 验证没有创建第二个版本
```

#### 7.4 并发注册
```
1. 同时注册 10 个不同训练产物
2. 验证全部成功
3. 验证没有资源争抢或死锁
```

#### 7.5 大文件处理
```
1. 测试 5GB 产物上传
2. 测试 10GB 产物上传（当前上限）
3. 记录上传耗时和速率
```

### 验收标准
- [ ] 所有测试用例通过
- [ ] 端到端耗时 < 5 分钟（1GB 文件）
- [ ] 并发注册成功率 100%

---

## 任务 8：监控和告警 【P2 - 1 天】

### 目标
配置监控指标和告警规则。

### 指标定义

```go
package metrics

var (
    // 注册总数
    RegistrationTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Name: "modeldev_registration_total",
            Help: "Total number of trained artifact registrations",
        },
        []string{"status"}, // "success", "failed"
    )
    
    // 注册耗时（分阶段）
    RegistrationDuration = prometheus.NewHistogramVec(
        prometheus.HistogramOpts{
            Name:    "modeldev_registration_duration_seconds",
            Help:    "Duration of registration phases",
            Buckets: []float64{1, 5, 10, 30, 60, 120, 300, 600},
        },
        []string{"phase"}, // "prepare", "upload", "complete"
    )
    
    // 上传速率
    UploadSpeedMbps = prometheus.NewHistogram(
        prometheus.HistogramOpts{
            Name:    "modeldev_artifact_upload_speed_mbps",
            Help:    "Upload speed in Mbps",
            Buckets: prometheus.LinearBuckets(1, 5, 20), // 1-100 Mbps
        },
    )
    
    // 错误计数
    RegistrationErrors = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Name: "modeldev_registration_errors_total",
            Help: "Total registration errors by type",
        },
        []string{"error_type"}, // "upload_failed", "integrity_mismatch", etc.
    )
)
```

### 告警规则

```yaml
groups:
  - name: modeldev-registration
    interval: 30s
    rules:
      - alert: ModelDevRegistrationFailureRateHigh
        expr: |
          sum(rate(modeldev_registration_total{status="failed"}[5m]))
          / sum(rate(modeldev_registration_total[5m])) > 0.1
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "ModelDev 训练产物注册失败率超过 10%"
          description: "过去 5 分钟失败率：{{ $value | humanizePercentage }}"
      
      - alert: ModelDevArtifactUploadSlow
        expr: |
          histogram_quantile(0.95, 
            rate(modeldev_registration_duration_seconds_bucket{phase="upload"}[10m])
          ) > 1800
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "产物上传速度慢"
          description: "95% 的上传超过 30 分钟"
      
      - alert: ModelDevRegistrationStuck
        expr: |
          time() - max(modeldev_registration_total) by (job) > 3600
        labels:
          severity: critical
        annotations:
          summary: "注册流程超过 1 小时无进展"
```

### Grafana Dashboard

```json
{
  "dashboard": {
    "title": "ModelDev 训练产物注册",
    "panels": [
      {
        "title": "注册成功率",
        "targets": [
          {
            "expr": "sum(rate(modeldev_registration_total{status=\"success\"}[5m])) / sum(rate(modeldev_registration_total[5m]))"
          }
        ]
      },
      {
        "title": "注册耗时（P50/P95/P99）",
        "targets": [
          {
            "expr": "histogram_quantile(0.5, rate(modeldev_registration_duration_seconds_bucket[5m]))",
            "legendFormat": "P50"
          },
          {
            "expr": "histogram_quantile(0.95, rate(modeldev_registration_duration_seconds_bucket[5m]))",
            "legendFormat": "P95"
          }
        ]
      },
      {
        "title": "上传速率",
        "targets": [
          {
            "expr": "histogram_quantile(0.5, rate(modeldev_artifact_upload_speed_mbps_bucket[5m]))"
          }
        ]
      }
    ]
  }
}
```

### 验收标准
- [ ] 指标正确采集到 Prometheus
- [ ] Grafana dashboard 显示正常
- [ ] 告警规则已配置并测试触发

---

## 里程碑

### 里程碑 1：基础功能（第 1 周）
- [ ] 任务 1：gRPC 客户端封装
- [ ] 任务 2：对象复制逻辑
- [ ] 任务 3：注册流程编排
- [ ] 任务 4：回执持久化

**交付物**：能够完成一次端到端注册（手动测试）

### 里程碑 2：生产就绪（第 2 周）
- [ ] 任务 5：错误处理和重试
- [ ] 任务 6：单元测试
- [ ] 任务 7：集成测试
- [ ] 任务 8：监控和告警

**交付物**：通过所有测试，监控配置完成，可以上线

---

## 风险和缓解

| 风险 | 影响 | 概率 | 缓解措施 |
|------|------|------|---------|
| Governance 证书配置延迟 | 阻塞任务 1 | 中 | 提前与 Infra 团队沟通，准备测试证书 |
| ModelDev Storage 接口不稳定 | 影响任务 2 | 低 | 与 Storage 团队对齐接口 SLA |
| 大文件上传超时 | 影响用户体验 | 中 | 在测试中明确上限，后续增加分片支持 |
| 端到端测试环境不稳定 | 延迟任务 7 | 中 | 准备独立测试环境，不依赖共享资源 |

---

## 协作

### 依赖其他团队

- **Model Service 团队**
  - 提供测试环境访问
  - 协助排查集成问题
  
- **Infra 团队**
  - 配置 Governance mTLS 证书到 ModelDev 控制面
  - 配置监控和告警

- **Storage 团队**
  - 确认 ModelDev Storage 流式读取接口

### 协作方式
- Slack: #model-service-dev, #modeldev-dev
- 每日站会同步进度和阻塞
- 代码 Review 互相参与

---

## 文档

### 开发文档
- [ ] API 设计文档（gRPC 客户端）
- [ ] 错误处理和重试策略
- [ ] 性能测试报告

### 运维文档
- [ ] 部署指南
- [ ] 监控和告警配置
- [ ] 故障排查手册

### 用户文档
- [ ] 更新训练产物发布流程文档
- [ ] 增加常见问题 FAQ

---

## 验收标准（整体）

- [ ] 所有单元测试通过，覆盖率 > 80%
- [ ] 所有集成测试通过
- [ ] 端到端流程在测试环境运行成功
- [ ] 监控指标和告警配置完成
- [ ] 代码 Review 完成
- [ ] 文档更新完成
- [ ] 与 Model Service 团队联调通过

---

## 联系方式

- **项目负责人**：待定
- **Model Service 对接人**：待定
- **技术咨询**：#model-service-dev

**参考文档**：
- `docs/modeldev-integration-guide.md`：完整对接指南
- `docs/modeldev-quick-start.md`：快速开始示例
