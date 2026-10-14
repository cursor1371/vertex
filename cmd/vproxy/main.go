package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata"
	"unicode"

	"github.com/bsfdsagfadg/vertex/internal/api"
	"github.com/bsfdsagfadg/vertex/internal/engine/recaptcha"
	"github.com/bsfdsagfadg/vertex/internal/engine/vertex"
	"github.com/bsfdsagfadg/vertex/internal/infra/cli"
	"github.com/bsfdsagfadg/vertex/internal/infra/config"
	"github.com/bsfdsagfadg/vertex/internal/infra/db"
	"github.com/bsfdsagfadg/vertex/internal/infra/fingerprint"
	"github.com/bsfdsagfadg/vertex/internal/infra/logger"
	"github.com/bsfdsagfadg/vertex/internal/infra/spool"
	"github.com/bsfdsagfadg/vertex/internal/infra/stats"
	"github.com/bsfdsagfadg/vertex/internal/infra/transport"
	"github.com/bsfdsagfadg/vertex/internal/node/entrypool"
	"github.com/bsfdsagfadg/vertex/internal/node/exitpool"
	"github.com/bsfdsagfadg/vertex/internal/node/importer"
	"github.com/bsfdsagfadg/vertex/internal/node/nodestore"
)

var (
	//nolint:gochecknoglobals // Injected by build script
	version = "dev"
	//nolint:gochecknoglobals // Injected by build script
	buildCommit = "unknown"
	//nolint:gochecknoglobals // Injected by build script
	buildTime = "unknown"
)

//go:embed rules.txt
//nolint:gochecknoglobals // Embedded file
var rulesText string

const (
	shutdownGrace         = 25 * time.Second
	rulesAgreedFile       = "config/state/.rules_agreed"
	rulesAgreedFileDocker = "config/state/agreed-rules-docker.txt"
)

func rulesHash() string {
	cleanText := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, rulesText)
	sum := sha256.Sum256([]byte(cleanText))
	return hex.EncodeToString(sum[:])[:16]
}

func inDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	if data, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		s := string(data)
		if strings.Contains(s, "docker") || strings.Contains(s, "containerd") || strings.Contains(s, "kubepods") {
			return true
		}
	}
	return false
}

// 提取原有的终端普通打印，仅在需要同意规则阶段展示
func printLegacyBanner() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Printf("║  Vertex AI Proxy  %-42s ║\n", version)
	fmt.Println("║  PolyForm Noncommercial License 1.0.0   Deconstructed_Cube   ║")
	fmt.Printf("║  Build: %s / %s                                  ║\n", buildCommit, buildTime)
	fmt.Printf("║  Platform: %s/%s                                       ║\n", runtime.GOOS, runtime.GOARCH)
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")

	fmt.Println()
	fmt.Println("  ╔══════════════════════════════════════════════════════════╗")
	fmt.Println("  ║                                                          ║")
	fmt.Println("  ║   ⚠️  本软件完全免费，如果你花钱购买了这个软件，         ║")
	fmt.Println("  ║       你被骗了。请立即联系卖家退款。                     ║")
	fmt.Println("  ║                                                          ║")
	fmt.Println("  ║   获取正版：https://discord.gg/odysseia                  ║")
	fmt.Println("  ║                                                          ║")
	fmt.Println("  ╚══════════════════════════════════════════════════════════╝")
	fmt.Println()
}

func main() {
	// 维护子命令必须在规则同意/日志/TUI 初始化之前处理：它们是一次性非交互操作，
	// 不应被交互式规则确认或看板输出干扰。
	// 注意：无参数启动仍是默认行为（各启动脚本均不带参数）。
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "rules":
			fmt.Print(rulesText)
			return
		case "rules-hash":
			fmt.Println(rulesHash())
			return
		case "bootstrap-paas":
			if err := runPAASBootstrap(); err != nil {
				log.Fatalf("[Bootstrap] %v", err)
			}
			return
		case "vacuum":
			os.Exit(runDBVacuum())
		case "version", "--version":
			fmt.Printf("vertex-proxy %s (commit %s, built %s, %s/%s)\n",
				version, buildCommit, buildTime, runtime.GOOS, runtime.GOARCH)
			os.Exit(0)
		}
	}

	setupTermuxCerts() // 优先初始化 Termux 证书

	logDir := filepath.Join(filepath.Dir(config.ConfigDir()), "logs")
	dailyLogger := logger.NewDailyLogger(logDir)

	// ---- 规则同意检查 ----
	curHash := rulesHash()
	accepted, present := os.LookupEnv("VPROXY_ACCEPT_RULES_HASH")
	envAgreed, rulesErr := validateRulesEnvironment(accepted, present, curHash)
	if rulesErr != nil {
		log.Fatalf("[Rules] %v", rulesErr)
	}
	if envAgreed {
		log.Printf("[Rules] 已通过环境变量确认规则版本 %s", curHash)
	} else if inDocker() {
		if !checkRulesAgreedDocker(curHash) {
			printLegacyBanner()
			fmt.Println()
			fmt.Println("  ╔══════════════════════════════════════════════════════════╗")
			fmt.Println("  ║  📦 检测到 Docker 环境                                   ║")
			fmt.Println("  ╚══════════════════════════════════════════════════════════╝")
			fmt.Println()
			fmt.Println("  Docker 容器中无法交互同意规则。请按以下步骤同意：")
			fmt.Println()
			fmt.Println("  1) 在挂载到容器 /app/config 的本机目录中创建文件：")
			fmt.Println("       config/state/agreed-rules-docker.txt")
			fmt.Println()
			fmt.Println("  2) 文件内容写入当前规则版本哈希（必须完全匹配）：")
			fmt.Printf("       %s\n", curHash)
			fmt.Println()
			fmt.Println("     一行命令：")
			fmt.Printf("       echo %s > ./config/state/agreed-rules-docker.txt\n", curHash)
			fmt.Println()
			fmt.Println("  3) 重启容器即可。")
			fmt.Println()
			os.Exit(0)
		}
	} else {
		if !checkRulesAgreed(curHash) {
			printLegacyBanner()
			fmt.Println(rulesText)
			fmt.Println()
			if hasOldAgreement() {
				fmt.Println("  ⚠️  规则已更新，需要您重新确认。")
				fmt.Println()
			}
			fmt.Print("  请输入 yes 同意以上规则（输入其他内容退出）：")
			reader := bufio.NewReader(os.Stdin)
			input, _ := reader.ReadString('\n')
			input = strings.TrimSpace(strings.ToLower(input))
			if input != "yes" {
				fmt.Println("  你未同意规则，程序退出。")
				os.Exit(0)
			}
			saveRulesAgreed(curHash)
			fmt.Println()
			fmt.Println("  ✓ 已同意规则，正在启动...")
			fmt.Println()
		}
	}

	// 启动节点只做本地解析，不拉取订阅、不探测网络；错误必须在监听前暴露。
	nodesText, nodesPresent := os.LookupEnv("VPROXY_BOOTSTRAP_NODES")
	bootstrapNodes, bootstrapErr := parseBootstrapNodes(nodesText, nodesPresent)
	if bootstrapErr != nil {
		log.Fatalf("[Bootstrap] %v", bootstrapErr)
	}

	// ---- 同意通过之后，干净地启动 TUI 看板，坚决不影响前面的交互输入 ───
	cli.InitTracker(dailyLogger)
	cli.SetAppInfo(version, buildCommit, buildTime, runtime.GOOS, runtime.GOARCH)

	cfg := config.GetProvider()
	dbPath := filepath.Join(config.ConfigDir(), "data.db")
	database, err := db.Open(dbPath)
	if err != nil {
		log.Fatalf("[vproxy] failed to init database: %v", err)
	}
	defer func() { _ = database.Close() }()

	usageStore := db.NewUsageStore(database)
	stats.Init(usageStore)
	stats.SetRetentionDays(cfg.HistoryRetentionDays())
	stats.SetRaceLogRetentionDays(cfg.RaceLogRetentionDays())
	// 维护器与各仓储共享同一连接池；注入给 stats 后，每轮保留清理会顺带归还空闲页。
	dbMaint := db.NewDBMaintenance(database, dbPath)
	stats.SetDBMaintenance(dbMaint)
	defer stats.Close()

	httpLogStore := db.NewHTTPLogStore(database)
	stats.InitHTTPLog(httpLogStore)
	stats.SetHTTPLogEnabled(cfg.HTTPLogEnabled())
	stats.SetHTTPLogRetentionDays(cfg.HTTPLogRetentionDays())
	stats.SetHTTPLogBodyCapBytes(cfg.HTTPLogBodyCapKB() * 1024)
	defer stats.CloseHTTPLog()

	// 浏览器指纹包：启动时按配置发布一次；关闭/手动包非法时发布 nil（既有行为）。
	// admin 保存与 SIGHUP 经 api.ApplyRuntimeHotSettings 走同一实现热更新。
	if _, fpErr := fingerprint.Apply(api.FingerprintSelection(cfg)); fpErr != nil {
		log.Printf("[vproxy] 浏览器指纹未生效，保持既有行为: %v", fpErr)
	}

	// ── 领域装配链（§2.3.1）：显式构造成品实例，逐层注入，零注册顺序契约 ──
	irCache := transport.NewIRCache()
	resolver := transport.NewIdentityResolver(irCache)

	// 健康禁用连败阈值：每次判定时从动态配置读取（面板修改后立即生效，无需重启）。
	healthThreshold := func() int { return config.GetProvider().HealthProbeFailThreshold() }
	// 全局故障保护参数：同样每次判定时读取，面板改完下一轮即生效。
	// 窗口内 ≥N 个不同节点同因失败 → 暂停该类别的自动禁用，避免整池被公共依赖故障误禁。
	globalFaultCfg := func() (bool, int, int) {
		p := config.GetProvider()
		return p.HealthGlobalFaultGuardEnabled(), p.HealthGlobalFaultWindowSeconds(), p.HealthGlobalFaultMinNodes()
	}

	exitMgr := exitpool.NewManager(database, resolver, exitpool.Hooks{
		InvalidateParsed: irCache.InvalidateBatch,
		InvalidateAll:    irCache.Clear,
	}, healthThreshold)
	if err := mergeBootstrapNodes(exitMgr, bootstrapNodes); err != nil {
		log.Fatalf("[Bootstrap] %v", err)
	}
	entryMgr := entrypool.NewEntryManager(database, resolver, entrypool.EntryHooks{
		InvalidateParsed: irCache.InvalidateOne,
	}, healthThreshold)
	// 两个池各持一台保护器：故障按池独立统计（出口与前置的探测目标与网络路径不同，
	// 共用一台会让出口池的抖动影响前置池的判定）。
	exitMgr.SetGlobalFaultGuard(nodestore.NewGlobalFaultGuard(globalFaultCfg))
	entryMgr.SetGlobalFaultGuard(nodestore.NewGlobalFaultGuard(globalFaultCfg))

	// 故障处理策略：动作与探测失败阈值按类别可配置（fault_policies 表 + 内置默认）。
	// 全局默认阈值从动态配置热读；自定义行启动时载入一次，之后由管理接口写入并即时刷新。
	// 两个池共享同一实例——"策略改了但某个池还在用旧值"这种漂移必须在结构上不可能发生。
	policySet := nodestore.NewPolicySet(func() int { return config.GetProvider().HealthProbeFailThreshold() })
	if policyRows, perr := nodestore.LoadPolicies(database); perr != nil {
		log.Printf("[Nodes] 读取故障处理策略失败（本次按内置默认运行，可在面板重新保存）: %v", perr)
	} else {
		policySet.Replace(nodestore.PolicyRowsToSet(policyRows))
	}
	exitMgr.SetPolicySet(policySet)
	entryMgr.SetPolicySet(policySet)
	exitMgr.StartHealthWorker()

	dialer := transport.NewSingDialer(cfg, transport.DialerDeps{
		Cache:   irCache,
		Namer:   exitMgr,
		Entries: entryMgr,
	})
	netClient := transport.NewNetworkClient(dialer)
	netClient.SetHTTPLogRecorder(stats.HTTPLogRecorder())
	stats.SetHTTPLogNodeNamer(exitMgr.NodeName)

	// 出口节点池与 IR 解析缓存异步预热：首屏 /api/admin/nodes 免冷加载。
	go func() {
		exitMgr.EnsureLoaded()
		irCache.Prewarm(exitMgr.AllRawURIs())
	}()

	spool.SetMaxSpillProvider(func() int64 { return int64(config.Load().MaxSpillMB) << 20 })

	if err := dialer.SyncEntryPool(); err != nil {
		log.Printf("[vproxy] 警告: 前置代理池预热失败 (%v)，已降级为直连模式", err)
	}

	keys := api.NewAPIKeyManager()
	keys.LoadKeys()

	api.EnsureAdminPassword()
	stopAdminCleanup := api.StartAdminSessionCleanup(time.Hour)

	tokenPool := recaptcha.NewTokenPool(netClient, cfg, exitMgr)
	// TUI 追踪器展示 RT 预取池快照与 rT 抓取记录流：拉取式，避免预取池为展示而写日志刷屏。
	// 始终返回快照（不再按预取开关整体隐藏）：抓取记录流在预取关闭时同样有内容（请求路径
	// 照样抓 token），池汇总改由 PrefetchEnabled 单独控制是否展示。
	cli.SetPoolStatsProvider(func() (cli.PoolStats, bool) {
		st := tokenPool.WarmStats()
		return cli.PoolStats{
			Size:            st.Size,
			Depth:           cfg.RecaptchaPrefetchDepth(),
			Hits:            st.Hits,
			Misses:          st.Misses,
			Expired:         st.Expired,
			Failures:        st.Failures,
			Paused:          st.Paused,
			Pauses:          st.Pauses,
			PrefetchEnabled: cfg.RecaptchaTokenPrefetch(),
			Recent:          recentTokenEvents(tokenPool),
		}, true
	})
	vc := vertex.NewVertexAIClient(cfg, netClient, tokenPool, exitMgr)

	serverDeps := api.ServerDeps{
		VC:      vc,
		Keys:    keys,
		Cfg:     cfg,
		Exit:    exitMgr,
		Entry:   entryMgr,
		Dialer:  dialer,
		Imports: importer.NewService(),
		Tokens:  tokenPool,
		RTTok:   tokenPool,
		IR:      irCache,
		Stats:   usageStore,
		HTTPLog: httpLogStore,
		DB:      dbMaint,
		Log:     dailyLogger,
	}
	srv := api.NewServer(serverDeps)
	// 健康禁用自愈：健康判定禁用的节点退出调度后，请求路径触达不了，无人点「批量测速」
	// 就是事实永久禁用。后台低频探测给出不依赖人工的恢复通道。
	stopHealthSelfHeal := api.StartHealthSelfHeal(serverDeps)
	//nolint:exhaustruct
	// srvBaseCtx 是所有连接 request ctx 的根（BaseContext）：Shutdown 只关 listener 与
	// 空闲连接、不取消活跃连接，不设它时在途流式请求会一直跑到 requestTimeout（180s），
	// 而 shutdownGrace=25s 恒小于它——Shutdown 必然超时，随后 StopAll/Close 会在 handler
	// 仍在跑时拆掉依赖（新会话还会按"透明降级直连"绕过前置代理）。先取消根 ctx，
	// 让取消信号沿 r.Context() → requestCtx → 竞速 ctx 级联打通，在途请求秒级退出。
	srvBaseCtx, cancelSrvBase := context.WithCancel(context.Background())
	httpServer := &http.Server{
		Addr:              "0.0.0.0:" + strconv.Itoa(cfg.PortAPI()),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext: func(net.Listener) context.Context {
			return srvBaseCtx
		},
	}

	shutdownCh := make(chan struct{})
	var shutdownOnce sync.Once

	shutdownFn := func(signalName string) {
		if signalName != "" {
			log.Printf("[vproxy] 收到 %s：关闭 TUI + 排干在途请求（最长 %s）…", signalName, shutdownGrace)
		} else {
			log.Printf("[vproxy] TUI 交互关闭，排干在途请求（最长 %s）…", shutdownGrace)
		}
		cancelSrvBase()
		cli.StopTUI()
		ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		if err := httpServer.Shutdown(ctx); err != nil {
			log.Printf("[vproxy] 优雅关闭超时/出错：%v（强制结束）", err)
		}
		cancel()
		stopHealthSelfHeal()
		exitMgr.StopHealthWorker() // 排空健康写队列，必须先于 defer 的 database.Close
		dialer.StopAll()
		tokenPool.Close()
		stopAdminCleanup()
		_ = dailyLogger.Close()
	}

	go func() {
		<-cli.TUIDone()
		shutdownOnce.Do(func() { shutdownFn(""); close(shutdownCh) })
	}()

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		for s := range sig {
			if s == syscall.SIGHUP {
				config.InvalidateCache()
				config.InvalidateModelsCache()
				// 模型级设置（model_settings.json）与目录快照同属模型域：手工改模型配置后
				// 发信号，必须一起清掉才能整体重载。
				config.InvalidateModelSettingsCache()
				// 运行时热生效项（历史保留期 / HTTP 日志三项）与面板保存共用同一实现：
				// 此前 SIGHUP 只清缓存，这些 setter 无人调用，手工改配置后发信号不生效。
				api.ApplyRuntimeHotSettings(cfg)
				log.Printf("[vproxy] 收到 SIGHUP：已清配置/模型/模型设置缓存，执行热重载")
				if err := dialer.SyncEntryPool(); err != nil {
					log.Printf("[vproxy] SIGHUP 前置代理池同步失败: %v", err)
				}
				continue
			}
			shutdownOnce.Do(func() { shutdownFn(s.String()); close(shutdownCh) })
			return
		}
	}()

	shutdownDone := make(chan struct{})
	go func() {
		<-shutdownCh
		close(shutdownDone)
	}()

	poolStr := "关闭"
	if cfg.ParallelPoolEnabled() {
		poolStr = strconv.Itoa(cfg.ParallelPoolSize()) + "（开启）"
	}
	log.Printf("[vproxy] 监听 %s（API 密钥 %d 个，max_retries=%d，并发池=%s）",
		httpServer.Addr, keys.Count(), cfg.MaxRetries(), poolStr)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("[vproxy] server error: %v", err)
	}
	<-shutdownDone
	log.Printf("[vproxy] 优雅关闭完成，vproxy 退出")
}

func checkRulesAgreed(curHash string) bool {
	data, err := os.ReadFile(rulesAgreedFile)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), curHash)
}

func hasOldAgreement() bool {
	data, err := os.ReadFile(rulesAgreedFile)
	if err != nil {
		return false
	}
	return len(strings.TrimSpace(string(data))) > 0
}

func saveRulesAgreed(curHash string) {
	_ = os.MkdirAll("config/state", 0o700)
	line := fmt.Sprintf("%s\t%s\n", time.Now().Format(time.RFC3339), curHash)
	_ = os.WriteFile(rulesAgreedFile, []byte(line), 0o600)
}

func checkRulesAgreedDocker(curHash string) bool {
	data, err := os.ReadFile(rulesAgreedFileDocker)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), curHash)
}

// tuiTokenEventRows 是 TUI 记录面板一次展示的条数（缓冲里保留更多，由引擎侧环形缓冲决定）。
const tuiTokenEventRows = 5

// recentTokenEvents 把引擎侧的 rT 抓取事件映射为 TUI 展示视图。
// cli 包不反向依赖引擎层，故映射放在装配层（与 SetPoolStatsProvider 同一处）。
func recentTokenEvents(pool *recaptcha.TokenPool) []cli.TokenEvent {
	events := pool.RecentTokenEvents(tuiTokenEventRows)
	out := make([]cli.TokenEvent, 0, len(events))
	for _, e := range events {
		out = append(out, cli.TokenEvent{
			At:        e.At,
			Source:    e.Source,
			Node:      e.NodeName,
			LatencyMs: e.LatencyMs,
			OK:        e.OK,
			Err:       e.Err,
			Canceled:  e.Canceled,
			RTRank:    e.RTRank,
		})
	}
	return out
}
