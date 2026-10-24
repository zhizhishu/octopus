package model

// 图片批处理（/v1/images/batches*）的实体与状态机。
//
// 【这是什么】上游（Gemini 开发者 API / Vertex AI）提供的**离线批量生图**通道：
// 我们把一批 prompt 打成 JSONL 交给上游，上游排队慢慢画（可长达 24 小时），
// 画完把结果写成一个结果对象，我们再去取。它与 /v1/images/generations 的区别不是
// "参数多几个"，而是**结算模型不同**——离线批处理按折扣价计费、且不保证时延。
//
// 【为什么不复用 ImageTask】见 model/image_task.go：那张表的状态机刻意只有三值，
// 一行一次上游调用，语义是"一次生成"。批处理是一行 N 条子请求、上游有独立批次号、
// 结果要按 custom_id 索引回来、失败要按条结算。硬塞进 ImageTask 会让那张表的
// "一次生成"语义当场失效，也会让客户端已经依赖的 /v1/images/tasks 契约漂移。
// 所以独立成两张表。
//
// 【铁律】这两张表**绝不存渠道密钥**，只存 ChannelID / ChannelKeyID 引用
// （理由同 ImageTask / VideoTask：key 会轮换、会禁用，只有存 id 才能在执行时
// 重新取到当前有效的那把）。

// 批处理任务状态取值。
//
// 【刻意只有七个值】判据与本仓 model/image_task.go 顶部那段注释同源：
// 状态机每多一个值，客户端就多一条必须处理而多数人不会处理的分支，而这张表的状态
// 靠后台推进器向前走——多出来的值极容易变成"永远停在那儿"。
//
// 上游自己的状态（JOB_STATE_PENDING / RUNNING / SUCCEEDED / …）**不进这里**，
// 原样存到 ProviderRawState 列供展示与排查。我们的状态只表达"接下来该谁动"。
const (
	// BatchImageJobStatusQueued 已受理、尚未交给上游。
	// 唯一的初始状态：这一行落库时我们还没调上游（理由同 ImageTask——提交路径里
	// 不许等上游，否则异步接口又把同步的等待搬回来了）。
	BatchImageJobStatusQueued = "queued"
	// BatchImageJobStatusSubmitted 上游已受理，等它画完。
	// 上游的 queued/running 都映射到这里，差别只在 ProviderRawState。
	BatchImageJobStatusSubmitted = "submitted"
	// BatchImageJobStatusIndexing 上游画完了，我们正在拉结果、建索引。
	// 单独一个状态的唯一理由：这一步会做网络 IO（可能几分钟），
	// 与"等上游"分开才能让推进器知道该做哪件事、也才能卡住取消。
	BatchImageJobStatusIndexing = "indexing"
	// BatchImageJobStatusCompleted 结果已索引完，可查可下。
	BatchImageJobStatusCompleted = "completed"
	// BatchImageJobStatusFailed 终态：上游失败、或我们这边不可恢复地失败了。
	BatchImageJobStatusFailed = "failed"
	// BatchImageJobStatusCancelled 终态：用户取消（上游能取消就一并取消）。
	BatchImageJobStatusCancelled = "cancelled"
	// BatchImageJobStatusOutputDeleted 终态：结果被删（用户主动删或过期清理）。
	// 与 cancelled 分开：这一单**画成了也结算了**，只是产物没了。
	BatchImageJobStatusOutputDeleted = "output_deleted"
)

// 批处理子项状态。只有三值，理由同上。
const (
	BatchImageItemStatusPending   = "pending"
	BatchImageItemStatusSuccess   = "success"
	BatchImageItemStatusFailed    = "failed"
	BatchImageItemStatusCancelled = "cancelled"
)

// 批处理上游通道。
//
// 【两个值就是全部】不是"暂时只做两个"——这两家是 Google 官方唯二的离线批处理
// 入口，形状还不同：Gemini 开发者 API 用 API key + File API 上传，Vertex 用
// 服务账号 + GCS 桶。别的"批处理"是各家私有协议，不在这个枚举里假装支持。
const (
	BatchImageProviderGeminiAPI = "gemini_api"
	BatchImageProviderVertex    = "vertex"
)

// BatchImageProviders 是全部合法上游，顺序即 /models 端点的返回顺序。
var BatchImageProviders = []string{
	BatchImageProviderGeminiAPI,
	BatchImageProviderVertex,
}

// IsSupportedBatchImageProvider 报告某个 provider 名是否合法。
func IsSupportedBatchImageProvider(provider string) bool {
	for _, p := range BatchImageProviders {
		if p == provider {
			return true
		}
	}
	return false
}

// batchImageJobTransitions 是状态机的**唯一**定义。
//
// 【为什么写成显式表而不是一连串 if】状态推进散在多个写者里（提交路径、推进器、
// 取消、过期清理），每个写者各写一遍"能不能从 A 到 B"，迟早有一处漏判。
// 一张表 + 一个查询函数，谁要推进都得先过它。
var batchImageJobTransitions = map[string][]string{
	BatchImageJobStatusQueued: {
		BatchImageJobStatusSubmitted,
		BatchImageJobStatusIndexing, // 上游一步到位给了内联结果（跳过 submitted）
		BatchImageJobStatusFailed,
		BatchImageJobStatusCancelled,
	},
	BatchImageJobStatusSubmitted: {
		BatchImageJobStatusIndexing,
		BatchImageJobStatusFailed,
		BatchImageJobStatusCancelled,
	},
	BatchImageJobStatusIndexing: {
		BatchImageJobStatusCompleted,
		BatchImageJobStatusFailed,
		// 索引途中被取消：允许，因为这一步耗时且用户可能已经不等了
		BatchImageJobStatusCancelled,
	},
	// 四个终态：出边为空 = 到这儿就结束。
	BatchImageJobStatusCompleted:     nil,
	BatchImageJobStatusFailed:        nil,
	BatchImageJobStatusCancelled:     nil,
	BatchImageJobStatusOutputDeleted: nil,
}

// IsBatchImageJobStatusAllowed 报告某个状态串是否合法（用于落库前的校验）。
func IsBatchImageJobStatusAllowed(status string) bool {
	_, ok := batchImageJobTransitions[status]
	return ok
}

// IsTerminalBatchImageJobStatus 报告某状态是否是终态。
//
// 终态 = 推进器不再动它、也不再扣任何钱。completed 是终态但产物仍可删——
// 删产物是 completed → output_deleted 这一次**原子改写**，不走 CanTransition
// （见 op.BatchImageMarkOutputDeleted 的注释）。
func IsTerminalBatchImageJobStatus(status string) bool {
	switch status {
	case BatchImageJobStatusCompleted,
		BatchImageJobStatusFailed,
		BatchImageJobStatusCancelled,
		BatchImageJobStatusOutputDeleted:
		return true
	}
	return false
}

// CanTransitionBatchImageJob 报告 from → to 是否是合法推进。
// 同状态"推进"一律拒绝：那不是推进，是重复写。
func CanTransitionBatchImageJob(from, to string) bool {
	allowed, ok := batchImageJobTransitions[from]
	if !ok {
		return false
	}
	for _, candidate := range allowed {
		if candidate == to {
			return true
		}
	}
	return false
}

// IsBatchImageItemStatusAllowed 报告子项状态串是否合法。
func IsBatchImageItemStatusAllowed(status string) bool {
	switch status {
	case BatchImageItemStatusPending,
		BatchImageItemStatusSuccess,
		BatchImageItemStatusFailed,
		BatchImageItemStatusCancelled:
		return true
	}
	return false
}

// BatchImageJob 是一条批处理任务。
//
// 一条任务对应上游的**一个批次**。客户端提交 N 条 prompt 得到 1 条 job + N 条 item。
type BatchImageJob struct {
	ID      int64  `json:"id" gorm:"primaryKey;autoIncrement"`
	BatchID string `json:"batch_id" gorm:"size:64;uniqueIndex;default:''"`

	CreatedAt   int64 `json:"created_at" gorm:"index"`
	UpdatedAt   int64 `json:"updated_at"`
	SubmittedAt int64 `json:"submitted_at" gorm:"default:0"`
	CompletedAt int64 `json:"completed_at" gorm:"default:0"`
	// SettledAt 非零 = 这一单的钱已经结清（幂等裁判，见 op.BatchImageSettle）。
	SettledAt int64 `json:"settled_at" gorm:"default:0"`

	// ExpiresAt 之后任务一律不可见（查询 404），推进器也不再动它。
	ExpiresAt int64 `json:"expires_at" gorm:"index"`

	// Status 见 BatchImageJobStatus* 常量。
	Status string `json:"status" gorm:"size:24;index;default:'queued'"`

	// ProviderRawState 是上游自己的状态串（JOB_STATE_RUNNING 之类），
	// 仅用于展示与排查。**不参与任何判断**：判断一律用 Status。
	ProviderRawState string `json:"provider_raw_state" gorm:"size:64;default:''"`

	// Model 是**上游模型名**（过了渠道 model_mapping 之后的值），真正发给上游的那个。
	Model string `json:"model" gorm:"size:255;default:''"`
	// RequestModel 是客户端请求的模型名。计费与日志按它算，理由同全部中继路径。
	RequestModel string `json:"request_model" gorm:"size:255;default:''"`

	// Provider 见 BatchImageProvider* 常量。
	Provider string `json:"provider" gorm:"size:32;index;default:''"`

	// OwnerTokenID / OwnerUserID 归属。查询侧必须按这两个字段过滤，
	// 不能只按 BatchID —— 否则知道批号就能看/下别人的图。
	OwnerTokenID int `json:"owner_token_id" gorm:"index;default:0"`
	OwnerUserID  int `json:"owner_user_id" gorm:"index;default:0"`

	// 渠道引用（不是密钥）。重启后靠它恢复"该去哪个上游问"。
	ChannelID    int `json:"channel_id" gorm:"index;default:0"`
	ChannelKeyID int `json:"channel_key_id" gorm:"default:0"`

	// TaskName 是用户给的批次名（可空），用于列表筛选与可读性。
	TaskName string `json:"task_name" gorm:"size:255;default:''"`

	// 幂等：同一 (OwnerUserID, OwnerTokenID, IdempotencyKey) 只建一条。
	// 空 key 不参与去重（存量/未传时按"每次都是新批次"处理）。
	// 🔴 唯一性由数据库裁决，不能只靠"先查再插"：两个相同 key 并发时都查不到
	// 旧记录，于是各建一单、各扣一次钱。真正的唯一约束是**部分索引**
	// （只约束非空 key，空 key 表示"每次都是新批次"），在 migrate/020 里用原生
	// SQL 建，因为 GORM 的 uniqueIndex tag 表达不了 "WHERE key <> ''"。
	IdempotencyKey string `json:"idempotency_key" gorm:"size:255;index:idx_batch_image_idem,priority:3;default:''"`
	// RequestHash 是归一化后请求的指纹，用于判定"同一个幂等键但请求变了"。
	RequestHash string `json:"request_hash" gorm:"size:64;default:''"`

	// 计数：提交时 ItemCount 定下来，其余三个随索引推进。
	ItemCount      int `json:"item_count" gorm:"default:0"`
	SuccessCount   int `json:"success_count" gorm:"default:0"`
	FailCount      int `json:"fail_count" gorm:"default:0"`
	CancelledCount int `json:"cancelled_count" gorm:"default:0"`

	// 生成参数（批次级，逐条 item 共用）。
	ResponseMimeType string `json:"response_mime_type" gorm:"size:64;default:''"`
	AspectRatio      string `json:"aspect_ratio" gorm:"size:32;default:''"`
	ImageSize        string `json:"image_size" gorm:"size:32;default:''"`

	// RequestJSON 是归一化后的完整请求（**含用户 prompt**，属敏感数据）。
	// 推进器靠它重建 JSONL 重放，json tag 是 "-" 保证它绝不出现在任何对外 JSON 里，
	// 也绝不许出现在任何日志/错误串里。
	RequestJSON string `json:"-" gorm:"type:text"`

	// 上游批次句柄与结果引用。三列都是上游给的字符串，我们只透传不解析。
	ProviderJobName   string `json:"provider_job_name" gorm:"size:512;default:''"`
	ProviderInputRef  string `json:"provider_input_ref" gorm:"size:512;default:''"`
	ProviderOutputRef string `json:"provider_output_ref" gorm:"size:512;default:''"`

	// 计费。HoldAmount 是提交时预扣的，ChargedAmount 是结算后实际收的。
	// 两者都由 op.BatchImageSettle 在 SettledAt 那一跳里定死。
	HoldAmount float64 `json:"hold_amount" gorm:"default:0"`
	// UnitPriceSnapshot 是**提交那一刻**的单价。
	//
	// 🔴 结算必须用这个快照，不能用当时的配置值：
	// 用户下单到任务结束可能隔几小时，中间管理员调价，用新价结算等于
	// 事后改价——涨了就少退（甚至不退）失败条目的钱，跌了就凭空多退。
	UnitPriceSnapshot float64 `json:"-" gorm:"default:0"`
	ChargedAmount     float64 `json:"charged_amount" gorm:"default:0"`

	// 产物生命周期。OutputExpiresAt 过了之后上游可能已经删了结果对象，
	// 我们据此把任务收口成 output_deleted（不再让客户端点进去拿到空）。
	OutputExpiresAt int64 `json:"output_expires_at" gorm:"default:0;index"`
	OutputDeletedAt int64 `json:"output_deleted_at" gorm:"default:0"`
	DownloadedAt    int64 `json:"downloaded_at" gorm:"default:0"`

	// 失败原因（已脱敏，见 relay/batch_image 的 sanitize）。
	ErrorCode    string `json:"error_code" gorm:"size:64;default:''"`
	ErrorMessage string `json:"error_message" gorm:"type:text"`

	Metadata map[string]string `json:"metadata,omitempty" gorm:"serializer:json"`
}

// BatchImageItem 是一条批处理任务的**一条子请求**。
//
// 【为什么不存图本体】结果图可能存在上游的结果对象里，本地只存"去哪一行的哪一段取"。
// 理由有三条，按重要性排：
//  1. 容器里的本地盘本身不持久（部署换镜像即丢），落一份等于给自己造一个假的
//     "已保存"——用户以为还在，其实重启就没了，比坦率地按上游保留期过期更糟。
//  2. 一批 200 条、每条可能几张 MB，落盘就要一整套配额/GC/备份/路径安全，
//     那是另一个子系统，不是这次要交付的协议面。
//  3. 上游结果对象是**唯一真源**，存两份就要处理"两份不一致"。
//
// 代价写在明处：上游删除结果（Gemini File API 默认 48 小时）之后，我们的索引还在、
// 图取不到了。所以有 OutputExpiresAt，过期后任务直接收口成 output_deleted，
// 而不是让客户端点进去拿到一个空文件。
type BatchImageItem struct {
	ID int64 `json:"id" gorm:"primaryKey;autoIncrement"`
	// BatchID 是**字符串批号**而不是 job 的自增 id：推进器按批号批量取子项，
	// 字符串批号也是客户端唯一见过的那个标识。
	BatchID string `json:"batch_id" gorm:"size:64;index:idx_batch_image_items_batch,priority:1;default:''"`

	CustomID string `json:"custom_id" gorm:"size:128;index:idx_batch_image_items_batch,priority:2;default:''"`

	Status string `json:"status" gorm:"size:24;index;default:'pending'"`

	// PromptPreview 是截断后的 prompt，供列表页展示。
	// Prompt 是原文，**敏感**（同 RequestJSON 的规矩：不进日志、不出对外 JSON）。
	//
	// 🔴 Prompt 与 ReferenceImagesJSON 是**提交时唯一的可重放输入**。
	// 推进器不重新问客户端要 prompt——它必须能从库里重建出一模一样的 JSONL。
	// 这两个字段任何一个没落库，整批都会生成"空 prompt 的图"，且用户已经被扣钱。
	PromptPreview string `json:"prompt_preview" gorm:"size:256;default:''"`
	Prompt        string `json:"-" gorm:"type:text"`
	// ReferenceImagesJSON 是参考图快照（[]ReferenceImage 的 JSON）。
	// 同样敏感：含 base64 图字节，绝不进对外 JSON、不进日志。
	ReferenceImagesJSON string `json:"-" gorm:"type:text"`

	RequestHash string `json:"request_hash" gorm:"size:64;default:''"`

	// 索引定位：结果对象里的第几行（1-based），以及这一行在**摊平后的结果流**里的
	// 字节偏移与长度。存偏移是为了取单张图时能直接 seek，不必把整批结果读一遍。
	SourceLineNumber  int    `json:"source_line_number" gorm:"default:0"`
	ProviderSourceObj string `json:"provider_source_object" gorm:"size:512;default:''"`
	SourceByteOffset  int64  `json:"source_byte_offset" gorm:"default:0"`
	SourceByteLength  int64  `json:"source_byte_length" gorm:"default:0"`

	// 结果形态：这一条回来几张图、什么类型、什么扩展名。
	MimeType      string `json:"mime_type" gorm:"size:128;default:''"`
	FileExtension string `json:"file_extension" gorm:"size:16;default:''"`
	ImageCount    int    `json:"image_count" gorm:"default:0"`

	IndexedAt int64 `json:"indexed_at" gorm:"default:0"`

	ErrorCode    string `json:"error_code" gorm:"size:64;default:''"`
	ErrorMessage string `json:"error_message" gorm:"type:text"`
}

// TableName 固定表名，不靠 gorm 的复数化猜测。
// 理由：迁移里要按名字建索引，猜错一次就是线上少一个索引。
func (BatchImageJob) TableName() string { return "batch_image_jobs" }

// TableName 同上。
func (BatchImageItem) TableName() string { return "batch_image_items" }
