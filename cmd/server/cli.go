package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"qoder2api/internal/auth"
	"qoder2api/internal/cosy"
	"qoder2api/internal/server"
	"qoder2api/internal/upstream"
)

func runCLI(cmd string, args []string) int {
	switch strings.ToLower(strings.TrimSpace(cmd)) {
	case "login":
		return cliLogin(args)
	case "signin", "checkin":
		return cliCheckin(args)
	case "acct", "accounts":
		return cliAcct(args)
	case "credit", "credits", "quota":
		return cliCredit(args)
	case "models":
		return cliModels(args)
	case "stats":
		return cliStats(args)
	case "version", "-v", "--version":
		fmt.Printf("%s %s\n", server.ServiceName, server.CurrentVersion)
		return 0
	case "help", "-h", "--help":
		printCLIHelp()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "Unknown subcommand: %s\n\n", cmd)
		printCLIHelp()
		return 2
	}
}

func printCLIHelp() {
	fmt.Printf(`qoder2api %s - Dual-Realm Qoder (CN & Global) API Reverse Proxy

Usage:
  qoder2api [flags]              Start gateway server (Desktop WebView2 GUI on Windows unless -headless)
  qoder2api serve [flags]        Start gateway server
  qoder2api login [flags]        Login via Qoder OAuth PKCE, import PAT (pt-), or scan local Desktop/CLI
  qoder2api signin [flags]       Run daily official Qoder campaigns check-in across accounts
  qoder2api acct <action>        Manage credentials (list | scan | remove <uid>)
  qoder2api credit [flags]       Query live credit balances & 7-day expiring quota across accounts
  qoder2api models [flags]       List CN & Global Qoder catalog models & pricing multipliers
  qoder2api stats [flags]        Query /v1/stats usage metrics from a running gateway

Server Flags:
  -config string                 Path to config.json (default "config.json")
  -headless                      Run headless without desktop WebView2 window or opening browser
`, server.CurrentVersion)
}

func loadConfigForCLI(cfgPath string) (*Config, error) {
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		return Load("")
	}
	return Load(cfgPath)
}

func saveAuthToDir(authDir string, a *auth.Auth) error {
	if a == nil {
		return fmt.Errorf("nil account")
	}
	if err := os.MkdirAll(authDir, 0755); err != nil {
		return err
	}
	if a.Owner == "" {
		a.Owner = "public"
	}
	if a.FilePath == "" {
		a.FilePath = filepath.Join(authDir, fmt.Sprintf("qoder-%s-%s.json", a.Realm(), auth.SanitizeFilename(a.UID)))
	}
	a.BackfillRealm()
	return a.SaveAtomic()
}

func cliLogin(args []string) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.json", "path to config.json")
	realm := fs.String("realm", "global", "target realm: global | cn")
	platform := fs.String("platform", "CLI", "OAuth platform: CLI | IDE")
	vpc := fs.String("vpc", "", "optional CN enterprise VPC endpoint")
	pat := fs.String("pat", "", "optional Qoder personal token (pt-...) for non-interactive import")
	scan := fs.Bool("scan", false, "scan and import local Qoder Desktop & CLI credentials")
	owner := fs.String("owner", "public", "owner scope")
	noBrowser := fs.Bool("no-browser", false, "print OAuth URL without opening browser")
	timeoutSec := fs.Int("timeout", 300, "OAuth polling timeout seconds")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := loadConfigForCLI(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config failed: %v\n", err)
		return 1
	}
	up := buildUpstream(cfg)

	if *scan {
		return cliScanAndImport(cfg.AuthDir, *owner)
	}

	if strings.TrimSpace(*pat) != "" {
		a, err := auth.Parse([]byte(strings.TrimSpace(*pat)))
		if err != nil {
			fmt.Fprintf(os.Stderr, "parse PAT failed: %v\n", err)
			return 1
		}
		a.SetRealm(upstream.NormalizeRealm(*realm))
		if a.Realm() == upstream.RealmCN {
			a.Domain = "qoder.com.cn"
		} else {
			a.Domain = "qoder.com"
		}
		if *vpc != "" {
			if _, _, _, _, perr := upstream.ParseVPCEndpoint(*vpc); perr != nil {
				fmt.Fprintf(os.Stderr, "invalid VPC endpoint: %v\n", perr)
				return 1
			}
			a.VPCEndpoint = strings.TrimSpace(*vpc)
		}
		a.SetProvider("pat")
		a.SetOwner(*owner)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := up.ExchangePersonalToken(ctx, a); err != nil {
			fmt.Fprintf(os.Stderr, "PAT exchange failed: %v\n", err)
			return 1
		}
		if err := saveAuthToDir(cfg.AuthDir, a); err != nil {
			fmt.Fprintf(os.Stderr, "save failed: %v\n", err)
			return 1
		}
		fmt.Printf("Imported PAT account: uid=%s realm=%s nick=%s file=%s\n", a.UID, a.Realm(), a.Nickname, a.FilePath)
		return 0
	}

	normRealm := upstream.NormalizeRealm(*realm)
	res, err := up.StartOAuth(normRealm, *platform, *vpc, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "start OAuth failed: %v\n", err)
		return 1
	}
	fmt.Printf("Qoder (%s) OAuth Login URL:\n  %s\n\n", res.Realm, res.AuthURL)
	if !*noBrowser {
		openBrowser(res.AuthURL)
	}
	fmt.Printf("Waiting for authorization (timeout %ds)...\n", *timeoutSec)
	deadline := time.Now().Add(time.Duration(*timeoutSec) * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		acct, pending, perr := up.PollOAuthOnce(ctx, res.Realm, res.VPCEndpoint, res.Nonce, res.Verifier)
		cancel()
		if perr != nil {
			fmt.Fprintf(os.Stderr, "OAuth poll error: %v\n", perr)
			return 1
		}
		if !pending && acct != nil {
			acct.SetProvider("oauth")
			acct.SetOwner(*owner)
			if err := saveAuthToDir(cfg.AuthDir, acct); err != nil {
				fmt.Fprintf(os.Stderr, "save credential failed: %v\n", err)
				return 1
			}
			fmt.Printf("Login succeeded! uid=%s nick=%s realm=%s file=%s\n", acct.UID, acct.Nickname, acct.Realm(), acct.FilePath)
			return 0
		}
		time.Sleep(2 * time.Second)
	}
	fmt.Fprintln(os.Stderr, "OAuth login timed out.")
	return 1
}

func cliScanAndImport(authDir, owner string) int {
	cands := cosy.ScanLocalCredentials()
	fmt.Printf("Scanned local Qoder Desktop & CLI stores: %d entry(ies)\n", len(cands))
	imported := 0
	for _, c := range cands {
		status := "unreadable"
		if c.Valid {
			status = "valid"
		} else if c.Error != "" {
			status = "err: " + c.Error
		}
		fmt.Printf("  [%s] realm=%-6s uid=%-18s nick=%-16s status=%s (%s)\n", c.Kind, c.Realm, c.UID, c.Nickname, status, c.Path)
		if !c.Valid || c.AccessToken == "" {
			continue
		}
		a := &auth.Auth{
			UID:              c.UID,
			Nickname:         c.Nickname,
			AccessToken:      c.AccessToken,
			RefreshToken:     c.RefreshToken,
			ExpiresAt:        c.ExpiresAt,
			Domain:           c.Domain,
			UserType:         c.UserType,
			OrganizationID:   c.OrganizationID,
			OrganizationName: c.OrganizationName,
			Owner:            owner,
		}
		a.SetRealm(c.Realm)
		if c.Kind == "app" {
			a.SetProvider("desktop-app")
		} else {
			a.SetProvider("cli")
		}
		if err := saveAuthToDir(authDir, a); err != nil {
			fmt.Fprintf(os.Stderr, "    save failed for %s: %v\n", c.UID, err)
			continue
		}
		imported++
	}
	fmt.Printf("Imported %d valid credential(s) into %s\n", imported, authDir)
	return 0
}

func cliCheckin(args []string) int {
	fs := flag.NewFlagSet("signin", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.json", "path to config.json")
	uidFilter := fs.String("uid", "", "optional UID filter")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadConfigForCLI(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config failed: %v\n", err)
		return 1
	}
	auth.SetGlobalEnabled(cfg.Global.Enabled)
	auths, _ := auth.LoadDir(cfg.AuthDir)
	if len(auths) == 0 {
		fmt.Println("No accounts found in", cfg.AuthDir)
		return 0
	}
	up := buildUpstream(cfg)
	failed := 0
	for _, a := range auths {
		if *uidFilter != "" && a.UID != *uidFilter {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		if a.NeedsRefresh(24 * time.Hour) {
			_ = up.RefreshToken(a)
		}
		sum, cerr := up.DailyCheckinDetailed(ctx, a)
		proClaimed, _ := up.CheckAndClaimProUpgrade(ctx, a)
		cancel()
		if cerr != nil {
			failed++
			fmt.Printf("%-18s %-6s FAIL    %v\n", a.UID, a.Realm(), cerr)
			continue
		}
		proTag := ""
		if proClaimed {
			proTag = " [pro-upgraded]"
		}
		st := "OK"
		if sum.Already {
			st = "ALREADY"
		}
		fmt.Printf("%-18s %-6s %-7s earned=%d%s %s\n", a.UID, a.Realm(), st, sum.Earned, proTag, sum.Message)
	}
	if failed == len(auths) && len(auths) > 0 {
		return 1
	}
	return 0
}

func cliAcct(args []string) int {
	fs := flag.NewFlagSet("acct", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.json", "path to config.json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	action := "list"
	if len(rest) > 0 {
		action = strings.ToLower(rest[0])
	}
	cfg, err := loadConfigForCLI(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config failed: %v\n", err)
		return 1
	}
	auth.SetGlobalEnabled(cfg.Global.Enabled)

	switch action {
	case "scan":
		return cliScanAndImport(cfg.AuthDir, "public")
	case "list":
		auths, _ := auth.LoadDir(cfg.AuthDir)
		fmt.Printf("Accounts in %s (%d total):\n", cfg.AuthDir, len(auths))
		for _, a := range auths {
			expStr := time.Unix(a.ExpiresAt, 0).Format("2006-01-02 15:04")
			if a.ExpiresAt <= 0 {
				expStr = "-"
			}
			fmt.Printf("  %-20s realm=%-6s prov=%-11s owner=%-8s exp=%s file=%s\n",
				a.UID, a.Realm(), a.ProviderValue(), a.OwnerValue(), expStr, filepath.Base(a.FilePath))
		}
		return 0
	case "remove", "rm", "delete":
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: qoder2api acct remove <uid>")
			return 2
		}
		target := rest[1]
		auths, _ := auth.LoadDir(cfg.AuthDir)
		for _, a := range auths {
			if a.UID == target || a.RawUID() == target {
				if err := os.Remove(a.FilePath); err != nil {
					fmt.Fprintf(os.Stderr, "remove %s failed: %v\n", a.FilePath, err)
					return 1
				}
				fmt.Printf("Removed %s (%s)\n", a.UID, a.FilePath)
				return 0
			}
		}
		fmt.Fprintf(os.Stderr, "account %q not found in %s\n", target, cfg.AuthDir)
		return 1
	default:
		fmt.Fprintln(os.Stderr, "Usage: qoder2api acct [list|scan|remove <uid>]")
		return 2
	}
}

func cliCredit(args []string) int {
	fs := flag.NewFlagSet("credit", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.json", "path to config.json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadConfigForCLI(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config failed: %v\n", err)
		return 1
	}
	auth.SetGlobalEnabled(cfg.Global.Enabled)
	auths, _ := auth.LoadDir(cfg.AuthDir)
	up := buildUpstream(cfg)
	var sumTotal, sumExp float64
	fmt.Printf("%-20s %-6s %-16s %-10s %-10s %s\n", "UID", "REALM", "NICKNAME", "REMAIN", "EXP_7D", "SOURCE")
	for _, a := range auths {
		rem, det, cerr := up.UserResourceDetailed(a, cfg.ExpiringSoonDur)
		if cerr != nil {
			fmt.Printf("%-20s %-6s %-16s ERR: %v\n", a.UID, a.Realm(), a.Nickname, cerr)
			continue
		}
		var exp float64
		src := "-"
		if det != nil {
			exp = det.Expiring
			if det.SourcePath != "" {
				src = det.SourcePath
			}
		}
		sumTotal += rem
		sumExp += exp
		fmt.Printf("%-20s %-6s %-16s %-10.1f %-10.1f %s\n", a.UID, a.Realm(), a.Nickname, rem, exp, src)
	}
	fmt.Printf("Total remaining: %.1f (expiring soon: %.1f) across %d account(s)\n", sumTotal, sumExp, len(auths))
	return 0
}

func cliModels(args []string) int {
	fs := flag.NewFlagSet("models", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.json", "path to config.json")
	jsonOut := fs.Bool("json", false, "output JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, _ := loadConfigForCLI(*cfgPath)
	up := upstream.New()
	if cfg != nil {
		up = buildUpstream(cfg)
	}
	infos := up.MergedModelInfos(true, true)
	if *jsonOut {
		b, _ := json.MarshalIndent(infos, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	fmt.Printf("%-28s %-8s %-8s %-8s %s\n", "ID", "REALM", "COST", "CTX", "NAME")
	for _, m := range infos {
		cost := fmt.Sprintf("%.2fx", m.PriceFactor)
		if m.IsFree || m.PriceFactor <= 0 {
			cost = "free"
		}
		fmt.Printf("%-28s %-8s %-8s %-8d %s\n", m.ID, m.Realm, cost, m.ContextLength, m.DisplayName)
	}
	return 0
}

func cliStats(args []string) int {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.json", "path to config.json")
	urlFlag := fs.String("url", "", "gateway base URL override")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, _ := loadConfigForCLI(*cfgPath)
	base := *urlFlag
	if base == "" {
		listen := ":7865"
		if cfg != nil && cfg.Listen != "" {
			listen = cfg.Listen
		}
		if strings.HasPrefix(listen, ":") {
			listen = "127.0.0.1" + listen
		}
		base = "http://" + listen
	}
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(base, "/")+"/v1/stats", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid URL: %v\n", err)
		return 1
	}
	if cfg != nil && cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "request failed: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Println(string(body))
	if resp.StatusCode >= 400 {
		return 1
	}
	return 0
}
