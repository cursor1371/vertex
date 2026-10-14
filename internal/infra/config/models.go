// 本文件实现模型清单与别名映射的加载。
//
// models.json 形如 {"models": [...], "alias_map": {"别名": "真名"},
// "family_map": {"模型名": "text|image|audio|vedio"}}。
// family_map 是家族分类的显式覆盖表（优先于名字启发式），可避免新模型被误判。
// 与 config.json 同目录解析（VPROXY_MODELS 环境变量可覆盖路径），带 60 秒内存缓存。
//
// 模型级开关（抗截断/非流前缀/尾部兼容）不在本文件：见 model_settings.go。
// models.json 的三字段结构刻意保持不变，scripts/更新模型.bat 会按此整体重写该文件。
package config

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// fakePrefixes 是假非流模型前缀。
// 模型名以此开头表示"先完整非流式生成、再以单包 SSE 返回"。
//
//nolint:gochecknoglobals // Read-only prefix list
var fakePrefixes = []string{"假非流-"}

// antiTruncatePrefixes 是抗截断模型前缀。
// 模型名以此开头表示"非流式入口改走真流式竞速引擎、再聚合为单个响应返回"，
// 以取得 StreamParallel「首个有效 chunk 即锁定胜出」的语义，对上游半路截断更耐受。
//
//nolint:gochecknoglobals // Read-only prefix list
var antiTruncatePrefixes = []string{"抗截断-"}

// FakePrefixes 返回假非流前缀列表（供 api 层剥离前缀复用，避免常量散落）。
func FakePrefixes() []string { return fakePrefixes }

// AntiTruncatePrefixes 返回抗截断前缀列表（同上）。
func AntiTruncatePrefixes() []string { return antiTruncatePrefixes }

// defaultModels 是 models.json 缺失/损坏时的回退清单。
//
// 语义边界（重要）：这是**首次初始化与损坏回退**的来源，不会被合并进已有配置——
// 用户主动移除过的模型不会因重启被重新加回，升级也不会补齐缺失模型。
//
//nolint:gochecknoglobals // Read-only default list
var defaultModels = []string{
	"gemini-2.5-flash", "gemini-2.5-flash-lite",
	"gemini-2.5-pro", "gemini-3-flash-preview",
	"gemini-3.1-flash-lite", "gemini-3.1-pro-preview",
	"gemini-3-pro-preview",
	"gemini-3.5-flash", "gemini-3.5-flash-lite",
	"gemini-3.6-flash", "gemini-3.7-flash", "gemini-3.8-flash",
	"gemini-4-argon",
	"gemini-omni-flash-preview", "gemini-omni-1.1-flash-preview",
	"gemini-2.5-flash-image", "gemini-3-pro-image",
	"gemini-3.1-flash-image", "gemini-3.1-flash-image-preview",
	"gemini-3.1-flash-lite-image", "gemini-nano-banana-2.1",
	"gemini-2.5-flash-tts", "gemini-2.5-pro-tts",
	"gemini-3.1-flash-tts-preview",
}

// modelsFile 是 models.json 内容结构。
type modelsFile struct { //nolint:govet
	Models    []string          `json:"models"`
	AliasMap  map[string]string `json:"alias_map"`
	FamilyMap map[string]string `json:"family_map"`
}

// 家族名常量：family_map 的值域，也是 ModelFamilyOf 的返回值。
const (
	FamilyNameText  = "text"
	FamilyNameImage = "image"
	FamilyNameAudio = "audio"
	// FamilyNameVideo 是视频家族（内部标识 vedio，界面显示「视频」）。
	// 上游目录中的 Veo 系模型归入此家族；后端视频生成能力尚未实现，
	// 该家族模型不对外暴露、不允许启用，请求在分发层被明确拒绝。
	FamilyNameVideo = "vedio"
)

// ValidFamilyNames 返回 family_map 允许的家族名（供 admin 侧校验）。
func ValidFamilyNames() []string {
	return []string{FamilyNameText, FamilyNameImage, FamilyNameAudio, FamilyNameVideo}
}

// familyOfName 是纯名字启发式（不读配置），family_map 未命中时回落至此。
//
// 判定优先级：TTS（含 "tts"）→ 音频；生图（含 "image"/"imagen"/"nano-banana"）→ 图像；
// 视频（含 "veo"，如 veo-3.0-generate-preview）→ 视频；其余文本。
// nano-banana 是 Google 生图模型的对外代号，名称里没有 image 字样，
// 必须显式识别，否则会被判成文本模型走错策略分支。
func familyOfName(model string) string {
	lower := strings.ToLower(strings.TrimSpace(model))
	if strings.Contains(lower, "tts") {
		return FamilyNameAudio
	}
	if strings.Contains(lower, "image") || strings.Contains(lower, "imagen") || strings.Contains(lower, "nano-banana") {
		return FamilyNameImage
	}
	if strings.Contains(lower, "veo") {
		return FamilyNameVideo
	}
	return FamilyNameText
}

// HeuristicFamilyName 导出纯名字启发式，供上游目录分类使用。
//
// 目录条目没有本地 family_map 覆盖，只应按名字推断；本地模型的家族判定
// 仍必须走 ModelFamilyOf（显式覆盖优先），不要用本函数绕过覆盖表。
func HeuristicFamilyName(model string) string {
	return familyOfName(model)
}

// FamilyNameWithOverrides 在给定的 family 覆盖表上做家族判定（不读磁盘）。
//
// 供管理端在写盘前校验提交内容使用（如"视频模型不得启用"），
// 语义与 ModelFamilyOf 一致：显式覆盖优先，未命中回落名字启发式。
func FamilyNameWithOverrides(model string, familyMap map[string]string) string {
	if f, ok := familyMap[model]; ok {
		return f
	}
	return familyOfName(model)
}

// modelFamilyOf 在已加载的 modelsFile 上做家族判定，避免重复取锁。
func modelFamilyOf(mf *modelsFile, model string) string {
	if f, ok := mf.FamilyMap[model]; ok {
		return f
	}
	return familyOfName(model)
}

// ModelFamilyOf 返回模型家族名（text/image/audio）：family_map 显式声明优先，
// 未命中回落名字启发式。
//
// 这是全仓库唯一的家族判定入口：transform 层（FamilyFor / IsImageModel）与
// 本包的文本变体展开（ModelsWithVariants）都委托它，避免「改一处、其余几处
// 仍按名字猜」造成的判定漂移。
func ModelFamilyOf(model string) string {
	return modelFamilyOf(loadModelsFile(), model)
}

// ModelFamilyOverride 返回 family_map 对某模型的显式家族声明。
// ok 为 false 表示无显式声明（调用方应回落自己的启发式）。供需要区分
// 「显式声明」与「启发式推断」的调用点使用（如 transform.IsImageModel）。
func ModelFamilyOverride(model string) (string, bool) {
	f, ok := loadModelsFile().FamilyMap[model]
	return f, ok
}

// FamilyMap 返回模型 → 家族 的显式覆盖表副本（供 admin 后台展示/编辑）。
func FamilyMap() map[string]string {
	mf := loadModelsFile()
	out := make(map[string]string, len(mf.FamilyMap))
	for k, v := range mf.FamilyMap {
		out[k] = v
	}
	return out
}

var (
	//nolint:gochecknoglobals // Global model cache
	modelsMu sync.Mutex
	//nolint:gochecknoglobals // Global model cache
	cachedModels *modelsFile
	//nolint:gochecknoglobals // Global model cache
	modelsCacheTime time.Time
)

// InvalidateModelsCache 强制清除 models.json 缓存（SIGHUP 立即热重载用）。
func InvalidateModelsCache() {
	modelsMu.Lock()
	defer modelsMu.Unlock()
	cachedModels = nil
	modelsCacheTime = time.Time{}
}

// modelsPath 解析 models.json 路径（环境变量 > exe 同级 config/ > 工作目录 config/）。
func modelsPath() string {
	if p := os.Getenv("VPROXY_MODELS"); p != "" {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "config", "models.json")
		if _, errStat := os.Stat(p); errStat == nil { //nolint:govet
			return p
		}
	}
	return filepath.Join("config", "models.json")
}

// loadModelsFile 读 models.json（带 60 秒缓存）；文件缺失/损坏退回默认清单 + 空别名表。
func loadModelsFile() *modelsFile {
	modelsMu.Lock()
	defer modelsMu.Unlock()

	if cachedModels != nil && time.Since(modelsCacheTime) < cacheTTL {
		return cachedModels
	}

	mf := &modelsFile{Models: defaultModels, AliasMap: map[string]string{}, FamilyMap: map[string]string{}}
	if data, err := os.ReadFile(modelsPath()); err == nil {
		var parsed modelsFile
		if errUnm := json.Unmarshal(data, &parsed); errUnm != nil { //nolint:govet
			log.Printf("[Config] 解析 models.json 失败: %v", err)
		} else if len(parsed.Models) > 0 {
			mf.Models = parsed.Models
			if parsed.AliasMap != nil {
				mf.AliasMap = parsed.AliasMap
			}
			if parsed.FamilyMap != nil {
				mf.FamilyMap = parsed.FamilyMap
			}
			log.Printf("[Config] 成功加载模型配置文件 models.json (模型数: %d)", len(mf.Models))
		}
	} else if !os.IsNotExist(err) {
		log.Printf("[Config] 读取 models.json 失败: %v", err)
	}
	cachedModels = mf
	modelsCacheTime = time.Now()
	return mf
}

// BaseModels 返回基础模型清单（不含假流式变体）。
func BaseModels() []string {
	mf := loadModelsFile()
	out := make([]string, len(mf.Models))
	copy(out, mf.Models)
	return out
}

// AliasMap 返回别名 → 真名的映射副本（供 admin 后台展示/编辑）。
func AliasMap() map[string]string {
	mf := loadModelsFile()
	out := make(map[string]string, len(mf.AliasMap))
	for k, v := range mf.AliasMap {
		out[k] = v
	}
	return out
}

// ModelsWithVariants 返回每个基础模型 + 其变体的完整清单
// （对文本模型依次塞入 m、假非流-m、抗截断-m；非文本模型仅塞入 m）。
//
// 不产出组合变体（如 抗截断-假非流-m）：两者作用于相反端点，组合无意义且会让清单膨胀；
// 但 resolver 仍兼容组合形态的解析（见 stripVariantPrefixes）。
//
// /v1/models、/v1beta/models、单模型 404 校验都用它，以保证变体名自洽——这是
// 对外清单的唯一出口，各端点不得自行拼接变体名。
//
// 规则：
//   - 家族判定走 modelFamilyOf(mf, ...)：family_map 显式声明为 image/audio 的模型
//     即使名字里没有 image/tts，也不会被追加文本变体；
//   - 文本变体按模型级显式选项增删（option 缺省时默认追加，见 computeEffectiveOptions）；
//   - 视频家族（vedio）整体跳过：上游目录可见，但后端能力未实现，
//     仅因模型名写进 models.json 就向客户端宣告"可用"违背诚实原则。
//
// 注意（契约）：隐藏变体不是权限控制——变体不在清单里只代表不主动对外"广告"，
// 直接调用该变体 ID 仍按变体语义生效（与"models 数组是暴露面而非权限面"的既有语义一致）。
func ModelsWithVariants() []string {
	mf := loadModelsFile()
	ms := loadModelSettings()
	legacyEnabled, legacyList := legacyTrailingSnapshot()
	base := mf.Models
	result := make([]string, 0, len(base)*3)
	for _, m := range base {
		family := modelFamilyOf(mf, m)
		if family == FamilyNameVideo {
			log.Printf("[Config] 模型 %s 属视频家族(vedio)，后端尚未支持，不对外暴露", m)
			continue
		}
		result = append(result, m)
		if family != FamilyNameText {
			continue
		}
		eff := computeEffectiveOptions(ms, family, m, legacyEnabled, legacyList)
		if eff.NonStreamPrefix {
			result = append(result, fakePrefixes[0]+m)
		}
		if eff.AntiTruncate {
			result = append(result, antiTruncatePrefixes[0]+m)
		}
	}
	return result
}

// ResolveModelName 把模型名经 alias_map 重映射。
// 命中别名返回真名，否则原样透传。
func ResolveModelName(model string) string {
	if real, ok := loadModelsFile().AliasMap[model]; ok {
		return real
	}
	return model
}

// WriteModels 把模型清单、别名映射与家族覆盖表写回 models.json（原子写）并清空缓存，
// 使下次读取即生效。aliasMap / familyMap 为 nil 时写空表。
func WriteModels(models []string, aliasMap, familyMap map[string]string) error {
	if aliasMap == nil {
		aliasMap = map[string]string{}
	}
	if familyMap == nil {
		familyMap = map[string]string{}
	}
	if err := writeJSONFile(modelsPath(), modelsFile{Models: models, AliasMap: aliasMap, FamilyMap: familyMap}); err != nil {
		return err
	}
	InvalidateModelsCache()
	return nil
}

func (c AppConfig) BaseModels() []string                 { return BaseModels() }
func (c AppConfig) AliasMap() map[string]string          { return AliasMap() }
func (c AppConfig) FamilyMap() map[string]string         { return FamilyMap() }
func (c AppConfig) ModelFamilyOf(model string) string    { return ModelFamilyOf(model) }
func (c AppConfig) ModelsWithVariants() []string         { return ModelsWithVariants() }
func (c AppConfig) FakePrefixes() []string               { return FakePrefixes() }
func (c AppConfig) AntiTruncatePrefixes() []string       { return AntiTruncatePrefixes() }
func (c AppConfig) ResolveModelName(model string) string { return ResolveModelName(model) }
