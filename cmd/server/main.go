// main.go is the Qoder2Api entry point supporting Windows GUI (WebView2),
// headless gateway server mode, and WorkBuddy-aligned CLI subcommands.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"qoder2api/internal/auth"
	"qoder2api/internal/dshmgr"
	"qoder2api/internal/logfmt"
	"qoder2api/internal/pool"
	"qoder2api/internal/redisstore"
	"qoder2api/internal/scheduler"
	"qoder2api/internal/server"
	"qoder2api/internal/session"
	"qoder2api/internal/sysproc"
	"qoder2api/internal/upstream"
	"qoder2api/internal/usermgr"
)

func pauseOnExit() {
	if runtime.GOOS == "windows" {
		fmt.Println("\n[Tip] Press Enter to exit...")
		reader := bufio.NewReader(os.Stdin)
		_, _ = reader.ReadBytes('\n')
	}
}

func fatalExit(format string, v ...any) {
	msg := fmt.Sprintf(format, v...)
	log.Printf("[FATAL ERROR] %s", msg)
	showNativeError("Qoder 2API Startup Failed", msg)
	pauseOnExit()
	os.Exit(1)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	sysproc.HideWindow(cmd)
	_ = cmd.Start()
}

func anchorWorkingDir() {
	if exePath, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exePath)
		low := strings.ToLower(exeDir)
		if !strings.Contains(low, "go-build") && !strings.Contains(low, "temp") {
			_ = os.Chdir(exeDir)
		}
	}
}

func ensureDataDirs(cfg *Config) {
	if cfg.AuthDir != "" {
		if err := os.MkdirAll(cfg.AuthDir, 0777); err != nil {
			log.Printf("WARN: mkdir auth_dir %s: %v", cfg.AuthDir, err)
		}
		_ = os.Chmod(cfg.AuthDir, 0777)
	}
	if cfg.StateFile != "" {
		stateDir := filepath.Dir(cfg.StateFile)
		if err := os.MkdirAll(stateDir, 0777); err != nil {
			log.Printf("WARN: mkdir state dir %s: %v", stateDir, err)
		}
		_ = os.Chmod(stateDir, 0777)
	}
}

func buildPool(cfg *Config, store redisstore.Store, auths []*auth.Auth) *pool.Pool {
	p := pool.New(cfg.StateFile)
	p.SetStore(store)
	p.RestoreFromSnapshot()
	p.SyncToDir(auths)
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetDegrade(cfg.Pool.DegradeThreshold, cfg.DegradeCooldownDur, cfg.DegradeCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(cfg.Pool.MaxInFlightGlobal)
	p.SetSoftRateMax(cfg.SoftRateMaxDur)
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)
	p.SetCostExploreInterval(cfg.CostExploreIntervalDur)
	return p
}

func buildUpstream(cfg *Config) *upstream.Client {
	up := upstream.NewWithProxy(cfg.Upstream.Proxy)
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	if tr, ok := up.HTTP.Transport.(*http.Transport); ok && cfg.Upstream.HeaderTimeoutSeconds > 0 {
		tr.ResponseHeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	}
	if cfg.StateFile != "" {
		up.DataDir = filepath.Dir(cfg.StateFile)
	}
	up.PassthroughIP = cfg.Upstream.PassthroughIP
	up.GlobalVPC = cfg.Global.VPC
	up.APIBaseCN = cfg.Global.APIBaseCN
	up.APIBaseGlobal = cfg.Global.APIBaseGlobal
	up.OpenAPICN = cfg.Global.OpenAPICN
	up.OpenAPIGlobal = cfg.Global.OpenAPIGlobal
	up.WebsiteCN = cfg.Global.WebsiteCN
	up.WebsiteGlobal = cfg.Global.WebsiteGlobal

	upstream.ApplyQueueSettings(
		cfg.Upstream.WaitMode,
		cfg.Upstream.MaxQueueRetries,
		cfg.Upstream.WaitCapSeconds,
		cfg.Upstream.WaitDefaultSeconds,
		cfg.Upstream.WaitFirstToken,
		cfg.Upstream.QueueBudgetSeconds,
	)
	return up
}

func logPoolSummary(auths []*auth.Auth) {
	cnCount, globalCount := 0, 0
	for _, a := range auths {
		if a.Realm() == "global" {
			globalCount++
		} else {
			cnCount++
		}
		log.Printf("INFO: [startup] account: uid=%s nick=%s realm=%s owner=%s domain=%s",
			logfmt.Label(a.UID, a.Nickname), a.Nickname, a.Realm(), a.OwnerValue(), a.Domain)
	}
	log.Printf("INFO: [startup] account pool summary: total=%d (cn=%d, global=%d)", len(auths), cnCount, globalCount)
}

func main() {
	anchorWorkingDir()

	rawArgs := os.Args[1:]
	if len(rawArgs) > 0 && !strings.HasPrefix(rawArgs[0], "-") {
		if strings.EqualFold(rawArgs[0], "serve") {
			rawArgs = rawArgs[1:]
		} else {
			os.Exit(runCLI(rawArgs[0], rawArgs[1:]))
		}
	}

	fs := flag.NewFlagSet("qoder2api", flag.ExitOnError)
	cfgPath := fs.String("config", "config.json", "path to config json")
	headless := fs.Bool("headless", false, "run without GUI window")
	_ = fs.Parse(rawArgs)

	if *cfgPath == "config.json" {
		if _, err := os.Stat("config.json"); os.IsNotExist(err) {
			defaultCfg := Default()
			defaultCfg.Admin.Enabled = true
			defaultCfg.APIKey = "sk-qoder2api"
			if data, err := json.MarshalIndent(defaultCfg, "", "  "); err == nil {
				_ = os.WriteFile("config.json", data, 0644)
				log.Printf("Generated default config.json (listen=%s)", defaultCfg.Listen)
			}
		}
	}

	cfg, err := Load(*cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("config %s not found, using defaults+env", *cfgPath)
			cfg, err = Load("")
		}
		if err != nil {
			fatalExit("Failed to load config: %v\nPlease check config.json syntax", err)
		}
	}

	ensureDataDirs(cfg)
	auth.SetGlobalEnabled(cfg.Global.Enabled)

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Printf("WARN: load auths: %v (no accounts yet; add via Web UI or CLI login)", err)
		auths = nil
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	if cfg.StateFile != "" {
		metricsPath := filepath.Join(filepath.Dir(cfg.StateFile), "metrics.json")
		server.InitMetricsPersistence(metricsPath)
	}

	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)
	p := buildPool(cfg, store, auths)
	defer p.Close()

	logPoolSummary(auths)

	stopWatch := p.StartAuthDirWatch(cfg.AuthDir)
	defer stopWatch()

	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:               cfg.SessionTTL,
			GCInterval:        cfg.SessionGCInterval,
			Store:             store,
			Available:         p.AvailableUIDs,
			AvailableForModel: realmAwareAvailableForModel(p),
		})
		sessRouter.LoadFromStore()
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	up := buildUpstream(cfg)

	sch, err := scheduler.New(scheduler.Config{
		Pool:               p,
		Upstream:           up,
		CheckinHours:       cfg.Schedule.CheckinHours,
		KeepaliveHours:     cfg.Schedule.KeepaliveHours,
		ProBenefitHours:    cfg.Schedule.ProBenefitHours,
		CheckinDisabled:    !cfg.Schedule.CheckinEnabled,
		KeepaliveDisabled:  !cfg.Schedule.KeepaliveEnabled,
		ProBenefitDisabled: !cfg.Schedule.ProBenefitEnabled,
		ExpiringSoonWindow: cfg.ExpiringSoonDur,
	})
	if err != nil {
		fatalExit("Invalid scheduler config: %v", err)
	}
	if cfg.Schedule.CheckinEnabled {
		log.Printf("Checkin enabled (CST hours=%v)", cfg.Schedule.CheckinHours)
	} else {
		log.Printf("Checkin disabled (schedule.checkin_enabled=false)")
	}
	if cfg.Schedule.KeepaliveEnabled {
		log.Printf("Token keepalive enabled (CST hours=%v)", cfg.Schedule.KeepaliveHours)
	}
	if cfg.Schedule.ProBenefitEnabled {
		log.Printf("Pro benefit check enabled (CST hours=%v)", cfg.Schedule.ProBenefitHours)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	usersFile := filepath.Join("./data", "users.json")
	dshDir := filepath.Join("./data", "dsh")
	if cfg.StateFile != "" {
		usersFile = filepath.Join(filepath.Dir(cfg.StateFile), "users.json")
		dshDir = filepath.Join(filepath.Dir(cfg.StateFile), "dsh")
	}
	_ = os.MkdirAll(filepath.Dir(usersFile), 0777)
	userMgr, err := usermgr.New(usersFile, cfg.WebPassword, cfg.APIKey)
	if err != nil || userMgr == nil {
		log.Printf("WARN: [usermgr] init persistent store failed (%v), using in-memory store", err)
		userMgr, _ = usermgr.New("", cfg.WebPassword, cfg.APIKey)
	}

	dshManager := dshmgr.New(dshDir)
	alertMgr := server.NewAlertManager(server.AlertConfig{
		Enabled:   cfg.Alert.Enabled,
		Webhooks:  cfg.Alert.Webhooks,
		Events:    cfg.Alert.Events,
		MinCredit: cfg.Alert.MinCredit,
	})

	h, err := server.New(server.Config{
		Pool:               p,
		Upstream:           up,
		Scheduler:          sch,
		Sessions:           sessRouter,
		UserMgr:            userMgr,
		DshMgr:             dshManager,
		APIKey:             cfg.APIKey,
		WebPassword:        cfg.WebPassword,
		Version:            server.CurrentVersion,
		SoftCooldown:       cfg.SoftRateDur,
		MaxRotate:          cfg.Pool.MaxRotate,
		PromptMode:         cfg.Prompt.Mode,
		SystemPrompt:       cfg.PromptText,
		GlobalEnabled:      cfg.Global.Enabled,
		AdminEnabled:       cfg.Admin.Enabled,
		RedisMode:          redisMode,
		ModelRoutes:        cfg.ModelRoutes,
		ModelAliases:       cfg.ModelAliases,
		FreeModels:         cfg.FreeModels,
		ListenAddr:         cfg.Listen,
		StateFile:          cfg.StateFile,
		AuthDir:            cfg.AuthDir,
		ConfigPath:         *cfgPath,
		IPWhitelistEnabled: cfg.IPWhitelistEnabled,
		AllowedIPs:         cfg.AllowedIPs,
		AllowLocalhost:     cfg.AllowLocalhost,
		AllowRemoteAdmin:   cfg.AllowRemoteAdmin,
		AlertMgr:           alertMgr,
		StickyCount:        sessCount,
		StopFunc:           stop,
	})
	if err != nil {
		fatalExit("Failed to build HTTP handler: %v", err)
	}

	go sch.Run(ctx)

	go func() {
		select {
		case <-time.After(1 * time.Second):
			h.WarmupCreditsAsync()
		case <-ctx.Done():
		}
	}()

	go func() {
		select {
		case <-time.After(3 * time.Second):
		case <-ctx.Done():
			return
		}
		if cfg.Schedule.CheckinEnabled && len(p.AvailableUIDs()) > 0 {
			log.Printf("[scheduler] Startup check: triggering initial Qoder campaigns check-in...")
			sch.RunCheckinNow()
		}
		if cfg.Schedule.ProBenefitEnabled && len(p.AvailableUIDs()) > 0 && ctx.Err() == nil {
			sch.RunProBenefitNow(ctx)
		}
	}()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		server.SaveMetricsSnapshot()
		p.Flush()
		if cErr := store.Close(); cErr != nil {
			log.Printf("WARN: [server] redisstore close: %v", cErr)
		}
		_ = dshManager.Stop()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	host := "127.0.0.1"
	port := "7865"
	if strings.Contains(cfg.Listen, ":") {
		parts := strings.Split(cfg.Listen, ":")
		if parts[0] != "" && parts[0] != "0.0.0.0" {
			host = parts[0]
		}
		if len(parts) > 1 && parts[1] != "" {
			port = parts[1]
		}
	}
	uiURL := fmt.Sprintf("http://%s:%s", host, port)

	go func() {
		log.Printf("%s %s listening on %s (global_enabled=%v, api_key=%v, admin=%v)",
			server.ServiceName, server.CurrentVersion, cfg.Listen, cfg.Global.Enabled, cfg.APIKey != "", cfg.Admin.Enabled)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fatalExit("HTTP listen failed on %s: %v", cfg.Listen, err)
		}
	}()

	if !*headless {
		if runDesktopGUI(uiURL, stop) {
			return
		}
		go func() {
			time.Sleep(500 * time.Millisecond)
			log.Printf("[WebUI] Opening console in browser: %s", uiURL)
			openBrowser(uiURL)
		}()
	}

	<-ctx.Done()
	log.Printf("bye")
}
