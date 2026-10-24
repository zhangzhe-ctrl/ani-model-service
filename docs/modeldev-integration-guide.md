# ModelDev 训练产物注册对接指南

**目标读者**：ModelDev 服务开发者  
**前置条件**：Model Service 训练产物注册功能已部署  
**最后更新**：2026-10-20

## 概述

本文档指导 ModelDev 服务如何对接 Model Service 的训练产物注册功能，实现从训练完成到模型版本可部署的完整链路。

### 当前状态

- ✅ Model Service 端：三个注册 RPC 已实现并测试通过
- ⏸️ ModelDev 端：需要实现注册调用适配器和对象复制逻辑
- ⏸️ 端到端集成：等待 ModelDev 适配器完成后联调

### 架构概览

```
┌─────────────────┐
│  Training Pod   │  生成 model.tar，发布到 ModelDev Storage
└────────┬────────┘
         │
         ▼
┌─────────────────────────────────────────────────────────────┐
│  ModelDev 控制面 Register Worker                             │
│  1. 读取已验证的训练产物（ModelDev Storage）                  │
│  2. 调用 Model Service PrepareTrainedArtifact               │
│  3. 流式复制到 Model Storage                                 │
│  4. 调用 Model Service CompleteTrainedArtifact              │
│  5. 持久化 Model 回执到执行记录                               │
└─────────────────────────────────────────────────────────────┘
         │
         ▼
┌─────────────────┐
│  Model Service  │  校验、登记、创建 ready 的 ModelVersion
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│ Inference Svc   │  下载并部署模型
└─────────────────┘
```

---

## 一、对接范围

### ModelDev 需要实现的功能

1. **注册调用适配器**
   - 使用 Governance mTLS 身份调用 Model Service 的三个注册 RPC
   - 传递租户、执行 ID、幂等键等上下文
   
2. **对象复制逻辑**
   - 从 ModelDev Storage 读取已验证的 `model.tar`
   - 使用 Model Service 签发的上传地址流式复制
   - 处理复制失败和重试
   
3. **回执持久化**
   - 将 Model Service 返回的 `model_id`、`model_version_id` 写入执行记录
   - 供后续查询和推理服务引用
   
4. **错误处理**
   - 对象不存在、校验失败、冲突等场景的降级逻辑
   - 失败时调用 `AbortTrainedArtifact`

### ModelDev 不需要做的事情

- ❌ 不需要直接写 Model 数据库
- ❌ 不需要持有 Model Storage 的长期凭据
- ❌ 不需要推断推理启动命令（由训练快照或请求显式提供）
- ❌ 不需要在训练 Pod 中直接调用注册 RPC

---

## 二、调用协议

### 身份和权限

**当前阶段**：使用 Governance mTLS 身份（DNS SAN: `ani-governance`）

**必需的元数据**：
```go
metadata := map[string]string{
    "x-ani-tenant-id":  tenantID,
    "x-ani-actor":      "governance:modeldev-registration",
    "x-ani-request-id": requestID,
}
```

**依赖添加**：
```bash
# ModelDev 项目的 go.mod 添加 API 依赖
go get github.com/liangzai006/ani-model-service/api@v0.1.0
```

**gRPC 连接示例**：
```go
import (
    "crypto/tls"
    "crypto/x509"
    "google.golang.org/grpc"
    "google.golang.org/grpc/credentials"
    modelv1 "github.com/liangzai006/ani-model-service/api/model/v1"
)

func dialModelService() (modelv1.ModelServiceClient, error) {
    cert, err := tls.LoadX509KeyPair("/certs/governance.crt", "/certs/governance.key")
    if err != nil {
        return nil, err
    }
    
    caCert, err := os.ReadFile("/certs/ca.crt")
    if err != nil {
        return nil, err
    }
    caCertPool := x509.NewCertPool()
    caCertPool.AppendCertsFromPEM(caCert)
    
    tlsConfig := &tls.Config{
        Certificates: []tls.Certificate{cert},
        RootCAs:      caCertPool,
        ServerName:   "ani-model-service.ani.svc.cluster.local",
    }
    
    conn, err := grpc.Dial(
        "ani-model-service.ani.svc.cluster.local:9090",
        grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
    )
    if err != nil {
        return nil, err
    }
    
    return modelv1.NewModelServiceClient(conn), nil
}
```

---

## 三、注册流程详解

### 步骤 1：准备注册请求

从 ModelDev 训练快照和发布记录中收集必要信息：

```go
type TrainedArtifactRegistrationRequest struct {
    // 租户和幂等
    TenantID       string
    IdempotencyKey string // 建议格式: "training:{execution_id}:model-version"
    ExecutionID    string
    
    // 目标归属（二选一）
    TargetModelID  string   // 已有 Model：填写 Model ID
    // 或
    ModelName      string   // 新建 Model：填写以下四个字段
    DisplayName    string
    Description    string
    Capabilities   []string // ["text-generation", "chat"]
    
    // 产物信息
    Version        string // 版本号，例如 "run-1"、"epoch-3"
    FileName       string // 固定为 "model.tar"
    Format         string // 固定为 "safetensors"
    SizeBytes      int64  // 整个 tar 的字节数
    ChecksumSHA256 string // 整个 tar 的 SHA256（64 位十六进制）
    
    // 推理运行时（二选一）
    // 方案 A：显式提供完整三元组
    EngineType     string // "vllm"、"sglang"
    EngineImage    string // "registry/vllm:v0.5.0"
    StartupCommand string // "vllm serve /models --served-model-name model"
    // 方案 B：从基础版本继承
    SourceModelVersionID string // ready 的基础 ModelVersion ID
}
```

**关键决策点**：

1. **目标归属**：
   - 如果训练快照有 `target_model_id`，使用已有 Model
   - 如果训练快照有 `new_model_name`，创建新 Model（必须提供 `display_name`、`description`、`capabilities`）

2. **推理运行时**：
   - **推荐**：从 ready 的 `source_model_version_id` 继承，确保运行时配置与基础模型一致
   - 或者：显式提供完整三元组（适用于自定义推理配置的场景）

**示例**：

```go
// 场景 1：在已有 Model 下创建新版本，继承运行时
req := &modelv1.PrepareTrainedArtifactRequest{
    TenantId:             "tenant-uuid",
    IdempotencyKey:       "training:exec-123:model-version",
    ExecutionId:          "exec-123",
    TargetModelId:        "model-uuid",
    SourceModelVersionId: "base-version-uuid", // 继承运行时
    Version:              "fine-tuned-epoch-3",
    FileName:             "model.tar",
    Format:               "safetensors",
    SizeBytes:            1234567890,
    ChecksumSha256:       "abcd1234...", // 64 位十六进制
}

// 场景 2：创建新 Model，显式提供运行时
req := &modelv1.PrepareTrainedArtifactRequest{
    TenantId:       "tenant-uuid",
    IdempotencyKey: "training:exec-456:model-version",
    ExecutionId:    "exec-456",
    ModelName:      "custom-qwen-7b",
    DisplayName:    "Custom Qwen 7B",
    Description:    "Fine-tuned Qwen 7B for customer service",
    Capabilities:   []string{"text-generation", "chat"},
    Version:        "v1.0",
    FileName:       "model.tar",
    Format:         "safetensors",
    SizeBytes:      7654321098,
    ChecksumSha256: "1234abcd...",
    EngineType:     "vllm",
    EngineImage:    "registry.example.com/vllm:v0.5.0",
    StartupCommand: "vllm serve /models --served-model-name custom-qwen-7b --dtype float16",
}
```

### 步骤 2：调用 PrepareTrainedArtifact

```go
ctx := metadata.AppendToOutgoingContext(context.Background(),
    "x-ani-tenant-id", tenantID,
    "x-ani-actor", "governance:modeldev-registration",
    "x-ani-request-id", requestID,
)

resp, err := client.PrepareTrainedArtifact(ctx, req)
if err != nil {
    // 处理错误：参数校验失败、跨租户访问、冲突等
    return err
}

// 响应包含：
// - registration_id: 注册会话 UUID
// - upload_url: 短期上传地址（单次 PUT）
// - storage_path: Model Storage 中的对象路径
// - expires_at: 上传地址过期时间（通常 15 分钟）
```

**重要**：
- 幂等重放会返回同一个 `registration_id` 和 `storage_path`
- 相同幂等键但请求内容不同（SHA、大小、运行时等）会返回 `409 REGISTRATION_CONFLICT`

### 步骤 3：流式复制对象

从 ModelDev Storage 读取 `model.tar`，通过 Model Service 签发的上传地址流式上传：

```go
import (
    "io"
    "net/http"
)

func copyArtifact(modelDevStoragePath string, uploadURL string, sizeBytes int64) error {
    // 1. 从 ModelDev Storage 读取
    reader, err := modelDevStorage.GetObject(ctx, modelDevStoragePath)
    if err != nil {
        return fmt.Errorf("read from modeldev storage: %w", err)
    }
    defer reader.Close()
    
    // 2. 构造 PUT 请求
    req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, reader)
    if err != nil {
        return err
    }
    req.ContentLength = sizeBytes
    
    // 3. 执行上传
    client := &http.Client{Timeout: 30 * time.Minute}
    resp, err := client.Do(req)
    if err != nil {
        return fmt.Errorf("upload to model storage: %w", err)
    }
    defer resp.Body.Close()
    
    if resp.StatusCode < 200 || resp.StatusCode >= 300 {
        body, _ := io.ReadAll(resp.Body)
        return fmt.Errorf("upload failed: status=%d body=%s", resp.StatusCode, body)
    }
    
    return nil
}
```

**注意事项**：
- 当前协议只支持单次 PUT，大文件（10GB+）可能不稳定
- 上传失败可以重试，但需要重新调用 `PrepareTrainedArtifact` 获取新的上传地址
- 上传地址有效期约 15 分钟，超时需要重新获取

### 步骤 4：完成注册

```go
completeReq := &modelv1.CompleteTrainedArtifactRequest{
    TenantId:       tenantID,
    RegistrationId: resp.GetRegistrationId(),
    IdempotencyKey: req.GetIdempotencyKey(), // 可选，填写时必须与 Prepare 一致
}

completeResp, err := client.CompleteTrainedArtifact(ctx, completeReq)
if err != nil {
    // 错误码：
    // - 404 REGISTRATION_NOT_FOUND: 注册会话不存在
    // - 409 REGISTRATION_TERMINAL: 已终止，不能完成
    // - 400 OBJECT_INTEGRITY_MISMATCH: 对象大小或 SHA 不匹配
    // - 502 STORAGE_PROVIDER_ERROR: Storage 依赖失败
    return err
}

// 成功响应包含：
receipt := Receipt{
    RegistrationID:     completeResp.GetRegistrationId(),
    ModelID:            completeResp.GetModelId(),
    ModelVersionID:     completeResp.GetModelVersionId(),
    ModelVersionStatus: completeResp.GetModelVersionStatus(), // "ready"
    StoragePath:        completeResp.GetStoragePath(),
    SizeBytes:          completeResp.GetSizeBytes(),
    ChecksumSHA256:     completeResp.GetChecksumSha256(),
}
```

**Complete 阶段的校验**：
1. 检查对象是否存在
2. 重新读取对象并校验实际大小和 SHA256
3. 在 PostgreSQL 事务中创建或锁定 Model、创建 ModelVersion 和 Artifact
4. 标记 ModelVersion 和父 Model 为 `ready`

**重要**：重复 Complete 会返回同一个回执，不会创建第二个版本。

### 步骤 5：持久化回执

将 Model Service 返回的回执写入 ModelDev 执行记录：

```go
type ModelRegistrationReceipt struct {
    RegistrationID string `json:"registration_id"`
    ModelID        string `json:"model_id"`
    ModelVersionID string `json:"model_version_id"`
    Status         string `json:"status"`
    StoragePath    string `json:"storage_path"`
    SizeBytes      int64  `json:"size_bytes"`
    ChecksumSHA256 string `json:"checksum_sha256"`
}

// 写入执行记录
err = modelDevDB.UpdateExecutionRecord(ctx, executionID, ModelRegistrationReceipt{
    RegistrationID: receipt.RegistrationID,
    ModelID:        receipt.ModelID,
    ModelVersionID: receipt.ModelVersionID,
    Status:         receipt.ModelVersionStatus,
    StoragePath:    receipt.StoragePath,
    SizeBytes:      receipt.SizeBytes,
    ChecksumSHA256: receipt.ChecksumSHA256,
})
```

**为什么需要持久化完整回执**：
- `model_id` + `model_version_id`：推理服务部署时引用
- `storage_path` + `checksum_sha256`：审计和完整性验证
- `registration_id`：失败重试和问题排查

---

## 四、失败处理

### 场景 1：上传失败

```go
err := copyArtifact(modelDevPath, uploadURL, sizeBytes)
if err != nil {
    // 终止注册会话
    _, abortErr := client.AbortTrainedArtifact(ctx, &modelv1.AbortTrainedArtifactRequest{
        TenantId:       tenantID,
        RegistrationId: registrationID,
        Reason:         fmt.Sprintf("upload failed: %v", err),
    })
    // 更新 ModelDev 执行状态为失败
    return err
}
```

### 场景 2：对象校验失败

Complete 时 Model Service 重新校验对象，如果大小或 SHA256 不匹配：

```go
_, err := client.CompleteTrainedArtifact(ctx, completeReq)
if err != nil && grpc.Code(err) == codes.InvalidArgument {
    // Model Service 已自动将注册会话标记为 failed
    // ModelDev 只需记录失败原因
    return fmt.Errorf("model service rejected artifact: %w", err)
}
```

### 场景 3：幂等冲突

```go
_, err := client.PrepareTrainedArtifact(ctx, req)
if err != nil && grpc.Code(err) == codes.AlreadyExists {
    // 相同幂等键但请求内容不同
    // 可能是重试时参数被意外修改，或者幂等键生成逻辑有误
    return fmt.Errorf("idempotency conflict: %w", err)
}
```

### 场景 4：训练任务取消

```go
// 训练任务被用户取消或超时
_, err := client.AbortTrainedArtifact(ctx, &modelv1.AbortTrainedArtifactRequest{
    TenantId:       tenantID,
    RegistrationId: registrationID,
    Reason:         "training job cancelled by user",
})
```

---

## 五、完整代码示例

```go
package registration

import (
    "context"
    "fmt"
    "io"
    "net/http"
    "time"
    
    modelv1 "github.com/liangzai006/ani-model-service/api/model/v1"
    "google.golang.org/grpc/metadata"
)

type TrainingArtifact struct {
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

type RegistrationReceipt struct {
    RegistrationID string
    ModelID        string
    ModelVersionID string
    Status         string
    StoragePath    string
    SizeBytes      int64
    ChecksumSHA256 string
}

func RegisterTrainedArtifact(
    ctx context.Context,
    client modelv1.ModelServiceClient,
    artifact TrainingArtifact,
    modelDevStorage ObjectStorage,
) (*RegistrationReceipt, error) {
    
    // 1. 准备请求
    idempotencyKey := fmt.Sprintf("training:%s:model-version", artifact.ExecutionID)
    requestID := generateRequestID()
    
    ctx = metadata.AppendToOutgoingContext(ctx,
        "x-ani-tenant-id", artifact.TenantID,
        "x-ani-actor", "governance:modeldev-registration",
        "x-ani-request-id", requestID,
    )
    
    prepareReq := &modelv1.PrepareTrainedArtifactRequest{
        TenantId:             artifact.TenantID,
        IdempotencyKey:       idempotencyKey,
        ExecutionId:          artifact.ExecutionID,
        TargetModelId:        artifact.TargetModelID,
        ModelName:            artifact.ModelName,
        DisplayName:          artifact.DisplayName,
        Description:          artifact.Description,
        Capabilities:         artifact.Capabilities,
        SourceModelVersionId: artifact.SourceModelVersionID,
        Version:              artifact.Version,
        FileName:             "model.tar",
        Format:               "safetensors",
        SizeBytes:            artifact.SizeBytes,
        ChecksumSha256:       artifact.ChecksumSHA256,
    }
    
    // 2. 调用 Prepare
    prepareResp, err := client.PrepareTrainedArtifact(ctx, prepareReq)
    if err != nil {
        return nil, fmt.Errorf("prepare failed: %w", err)
    }
    
    registrationID := prepareResp.GetRegistrationId()
    uploadURL := prepareResp.GetUploadUrl()
    
    // 3. 流式复制对象
    reader, err := modelDevStorage.GetObject(ctx, artifact.ModelDevStoragePath)
    if err != nil {
        _ = abortRegistration(ctx, client, artifact.TenantID, registrationID, 
            fmt.Sprintf("failed to read from modeldev storage: %v", err))
        return nil, fmt.Errorf("read artifact: %w", err)
    }
    defer reader.Close()
    
    httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, reader)
    if err != nil {
        _ = abortRegistration(ctx, client, artifact.TenantID, registrationID, err.Error())
        return nil, err
    }
    httpReq.ContentLength = artifact.SizeBytes
    
    httpClient := &http.Client{Timeout: 30 * time.Minute}
    httpResp, err := httpClient.Do(httpReq)
    if err != nil {
        _ = abortRegistration(ctx, client, artifact.TenantID, registrationID, 
            fmt.Sprintf("upload failed: %v", err))
        return nil, fmt.Errorf("upload: %w", err)
    }
    defer httpResp.Body.Close()
    
    if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
        body, _ := io.ReadAll(httpResp.Body)
        reason := fmt.Sprintf("upload failed: status=%d body=%s", httpResp.StatusCode, body)
        _ = abortRegistration(ctx, client, artifact.TenantID, registrationID, reason)
        return nil, fmt.Errorf("upload: %s", reason)
    }
    
    // 4. 完成注册
    completeReq := &modelv1.CompleteTrainedArtifactRequest{
        TenantId:       artifact.TenantID,
        RegistrationId: registrationID,
        IdempotencyKey: idempotencyKey,
    }
    
    completeResp, err := client.CompleteTrainedArtifact(ctx, completeReq)
    if err != nil {
        return nil, fmt.Errorf("complete failed: %w", err)
    }
    
    // 5. 返回回执
    return &RegistrationReceipt{
        RegistrationID: completeResp.GetRegistrationId(),
        ModelID:        completeResp.GetModelId(),
        ModelVersionID: completeResp.GetModelVersionId(),
        Status:         completeResp.GetModelVersionStatus(),
        StoragePath:    completeResp.GetStoragePath(),
        SizeBytes:      completeResp.GetSizeBytes(),
        ChecksumSHA256: completeResp.GetChecksumSha256(),
    }, nil
}

func abortRegistration(
    ctx context.Context,
    client modelv1.ModelServiceClient,
    tenantID, registrationID, reason string,
) error {
    _, err := client.AbortTrainedArtifact(ctx, &modelv1.AbortTrainedArtifactRequest{
        TenantId:       tenantID,
        RegistrationId: registrationID,
        Reason:         reason,
    })
    return err
}

func generateRequestID() string {
    return fmt.Sprintf("modeldev-reg-%d", time.Now().UnixNano())
}

type ObjectStorage interface {
    GetObject(ctx context.Context, path string) (io.ReadCloser, error)
}
```

---

## 六、测试验证

### 单元测试

```go
func TestRegisterTrainedArtifact_Success(t *testing.T) {
    // Mock Model Service client
    // Mock ModelDev Storage
    // 验证正常流程
}

func TestRegisterTrainedArtifact_UploadFailed_Aborts(t *testing.T) {
    // 验证上传失败时调用 Abort
}

func TestRegisterTrainedArtifact_IdempotentReplay(t *testing.T) {
    // 验证幂等重试返回相同结果
}
```

### 集成测试

1. **端到端流程**：
   - 运行一个真实的训练任务
   - 等待产物发布到 ModelDev Storage
   - 调用注册流程
   - 验证 Model Service 中存在 ready 的 ModelVersion
   - 验证 Inference 能下载并部署

2. **失败恢复**：
   - 模拟上传中断，验证重试逻辑
   - 模拟对象校验失败，验证降级处理

3. **并发安全**：
   - 同时注册多个训练产物
   - 验证幂等键和事务隔离

---

## 七、监控和告警

### 关键指标

```go
// 注册成功率
modeldev_registration_success_total
modeldev_registration_failed_total

// 注册耗时（分阶段）
modeldev_registration_prepare_duration_seconds
modeldev_registration_upload_duration_seconds
modeldev_registration_complete_duration_seconds

// 对象复制速率
modeldev_artifact_upload_bytes_total
modeldev_artifact_upload_speed_mbps
```

### 告警规则

```yaml
- alert: ModelDevRegistrationFailureRateHigh
  expr: |
    rate(modeldev_registration_failed_total[5m]) 
    / rate(modeldev_registration_success_total[5m]) > 0.1
  annotations:
    summary: "ModelDev 训练产物注册失败率超过 10%"

- alert: ModelDevArtifactUploadSlow
  expr: |
    histogram_quantile(0.95, 
      rate(modeldev_registration_upload_duration_seconds_bucket[5m])
    ) > 1800
  annotations:
    summary: "95% 的产物上传超过 30 分钟"
```

---

## 八、常见问题

### Q1: 幂等键应该如何生成？

**A**: 推荐格式 `training:{execution_id}:model-version`，确保：
- 同一个训练执行只生成一个 ModelVersion
- 不同执行的幂等键不冲突
- 重试时使用相同的幂等键

### Q2: 运行时配置应该显式传入还是继承？

**A**: **推荐继承**，理由：
- 基础模型的运行时配置已验证可用
- 避免因命令拼接错误导致推理失败
- 简化训练快照的字段

显式传入仅用于：
- 自定义推理参数（如 dtype、max_tokens）
- 使用不同推理引擎

### Q3: 上传失败如何重试？

**A**: 重新调用完整流程（Prepare → Upload → Complete）：
- Prepare 的幂等逻辑会返回同一个 `registration_id` 和新的上传地址
- 上传地址有效期 15 分钟，超时需要重新获取
- Complete 会重新校验对象

### Q4: 如何处理大文件上传？

**A**: 当前协议只支持单次 PUT，限制约 5-10GB：
- 对于更大的模型，需要等待 Model Service 支持分片上传
- 或者考虑压缩、模型剪枝等优化手段

### Q5: ModelDev 专用 mTLS 身份何时配置？

**A**: 当前阶段使用 Governance 身份：
- Governance 控制面代理 ModelDev 的注册请求
- 后续如果 ModelDev 需要直接调用，增加专用身份并更新 RPC 策略

### Q6: 训练产物已经在 ModelDev Storage，为什么还要复制？

**A**: 安全和隔离边界：
- ModelDev Storage 和 Model Storage 是不同的服务边界
- Model Service 不持有 ModelDev Storage 的凭据
- 复制到 Model Storage 后，产物由 Model Service 管理生命周期

### Q7: 如果 Complete 返回 `OBJECT_INTEGRITY_MISMATCH` 怎么办？

**A**: 说明上传的对象大小或 SHA256 与 Prepare 时声明的不一致：
1. 检查 ModelDev Storage 中的原始对象是否损坏
2. 检查 SHA256 计算逻辑是否正确
3. 检查对象复制过程是否完整（是否提前终止）
4. 重新计算 SHA256 并重试注册

---

## 九、参考资料

- **Model Service 设计文档**：`docs/superpowers/specs/2026-10-20-trained-artifact-registration-design.md`
- **Model Service 对接文档**：`docs/modeldev-trained-artifact-integration.md`
- **Protobuf 定义**：`api/model/v1/model.proto`
- **RPC 策略配置**：`cmd/ani-model-service/rpc_policy.go`

---

## 十、对接 Checklist

在开始实现前，确认以下准备工作：

- [ ] Model Service 训练产物注册功能已部署到测试环境
- [ ] Governance mTLS 证书和 CA 证书已配置到 ModelDev 控制面
- [ ] ModelDev Storage 能够提供流式读取接口
- [ ] 训练快照包含所有必需字段（目标归属、运行时配置、产物大小和摘要）
- [ ] ModelDev 执行记录数据库有字段存储 Model 回执

实现过程中：

- [ ] 实现 gRPC 客户端连接和元数据注入
- [ ] 实现 Prepare → Upload → Complete 完整流程
- [ ] 实现失败时的 Abort 调用
- [ ] 实现回执持久化到执行记录
- [ ] 编写单元测试覆盖核心场景
- [ ] 编写集成测试验证端到端流程
- [ ] 配置监控指标和告警规则

联调验证：

- [ ] 在测试环境运行端到端训练任务
- [ ] 验证 Model Service 中能查询到 ready 的 ModelVersion
- [ ] 验证 Inference 能成功部署并提供服务
- [ ] 验证失败场景的降级和重试逻辑
- [ ] 压测验证并发注册的稳定性

---

## 联系方式

如有疑问或需要协助，请联系 Model Service 团队：
- Slack: #model-service-dev
- 邮件: model-service-team@example.com
