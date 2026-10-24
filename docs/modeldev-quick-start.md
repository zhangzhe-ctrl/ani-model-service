# ModelDev 训练产物注册 - 快速开始

**5 分钟快速上手指南**

## 前置条件

✅ Model Service 已部署  
✅ 拥有 Governance mTLS 证书  
✅ 训练产物 `model.tar` 已发布到 ModelDev Storage

---

## 快速示例

### 1. 添加依赖并初始化客户端

```bash
# ModelDev 项目的 go.mod 添加依赖
go get github.com/liangzai006/ani-model-service/api@v0.1.0
```

```go
import (
    modelv1 "github.com/liangzai006/ani-model-service/api/model/v1"
    "google.golang.org/grpc"
    "google.golang.org/grpc/credentials"
)

// 使用 Governance mTLS 身份连接
conn, err := grpc.Dial(
    "ani-model-service.ani.svc.cluster.local:9090",
    grpc.WithTransportCredentials(
        credentials.NewTLS(governanceTLSConfig), // 使用 Governance 证书
    ),
)
client := modelv1.NewModelServiceClient(conn)
```

### 2. 准备元数据

```go
import "google.golang.org/grpc/metadata"

ctx := metadata.AppendToOutgoingContext(context.Background(),
    "x-ani-tenant-id",  "your-tenant-uuid",
    "x-ani-actor",      "governance:modeldev-registration",
    "x-ani-request-id", "request-uuid",
)
```

### 3. 调用注册流程

```go
// Step 1: Prepare
prepareResp, err := client.PrepareTrainedArtifact(ctx, &modelv1.PrepareTrainedArtifactRequest{
    TenantId:             tenantID,
    IdempotencyKey:       "training:exec-123:model-version",
    ExecutionId:          "exec-123",
    TargetModelId:        "existing-model-uuid",        // 已有 Model
    SourceModelVersionId: "base-version-uuid",          // 继承运行时
    Version:              "fine-tuned-epoch-3",
    FileName:             "model.tar",
    Format:               "safetensors",
    SizeBytes:            1234567890,
    ChecksumSha256:       "abcd1234...", // 64 位十六进制
})

registrationID := prepareResp.GetRegistrationId()
uploadURL := prepareResp.GetUploadUrl()

// Step 2: Upload（单次 PUT）
reader := getModelTarFromModelDevStorage() // 从 ModelDev Storage 读取
req, _ := http.NewRequestWithContext(ctx, "PUT", uploadURL, reader)
req.ContentLength = sizeBytes
resp, err := http.DefaultClient.Do(req)
// 检查 resp.StatusCode == 200

// Step 3: Complete
completeResp, err := client.CompleteTrainedArtifact(ctx, &modelv1.CompleteTrainedArtifactRequest{
    TenantId:       tenantID,
    RegistrationId: registrationID,
})

// Step 4: 保存回执
modelID := completeResp.GetModelId()
modelVersionID := completeResp.GetModelVersionId()
saveToExecutionRecord(executionID, modelID, modelVersionID)
```

---

## 两种注册模式

### 模式 A：已有 Model（推荐）

```go
prepareReq := &modelv1.PrepareTrainedArtifactRequest{
    // ... 基础字段
    TargetModelId:        "model-uuid",        // 指定已有 Model
    SourceModelVersionId: "base-version-uuid", // 继承运行时配置
}
```

### 模式 B：创建新 Model

```go
prepareReq := &modelv1.PrepareTrainedArtifactRequest{
    // ... 基础字段
    ModelName:    "new-model-name",
    DisplayName:  "New Model Display Name",
    Description:  "Model description",
    Capabilities: []string{"text-generation"},
    // 运行时配置（二选一）
    SourceModelVersionId: "base-version-uuid", // 继承
    // 或显式提供
    EngineType:     "vllm",
    EngineImage:    "registry/vllm:v0.5.0",
    StartupCommand: "vllm serve /models --served-model-name new-model",
}
```

---

## 错误处理

```go
// 上传失败时终止注册
if uploadErr != nil {
    client.AbortTrainedArtifact(ctx, &modelv1.AbortTrainedArtifactRequest{
        TenantId:       tenantID,
        RegistrationId: registrationID,
        Reason:         uploadErr.Error(),
    })
}
```

---

## 关键要点

| 项目 | 说明 |
|------|------|
| **幂等键** | 格式：`training:{execution_id}:model-version` |
| **文件名** | 固定为 `model.tar` |
| **格式** | 固定为 `safetensors`（表示归档内权重格式） |
| **运行时** | 推荐从 `source_model_version_id` 继承 |
| **上传** | 单次 PUT，有效期 15 分钟 |
| **回执** | 必须保存 `model_id` + `model_version_id` |

---

## 完整示例代码

参见：`docs/modeldev-integration-guide.md` 第五节

---

## 常见错误

| 错误码 | 原因 | 解决方法 |
|--------|------|----------|
| `400 INVALID_ARGUMENT` | 参数缺失或格式错误 | 检查必填字段和格式 |
| `409 REGISTRATION_CONFLICT` | 幂等键冲突 | 检查请求内容是否一致 |
| `400 OBJECT_INTEGRITY_MISMATCH` | SHA256 或大小不匹配 | 重新计算摘要并重试 |
| `404 REGISTRATION_NOT_FOUND` | 注册会话不存在 | 检查 registration_id |

---

## 下一步

1. 阅读完整文档：`docs/modeldev-integration-guide.md`
2. 查看 Protobuf 定义：`api/model/v1/model.proto`
3. 联系 Model Service 团队进行联调

**有问题？** Slack: #model-service-dev
