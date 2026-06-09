// tsnet-demo: tsnet を使って自前 Headscale に参加し、 vpnsv (100.64.0.2) へ
// Tailscale 内部 Ping + TCP Dial (SSH バナー読み取り) を行う最小サンプル。
//
// 実行: TS_AUTHKEY="hskey-auth-XXXX" go run .
//
// 終了後は state dir (./tsnet-state) を残しておけば次回は AuthKey 不要。
// 完全に削除するなら `rm -rf ./tsnet-state` してから再実行。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"time"

	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
)

const (
	defaultControlURL = "https://headscale.example.com" // 自前 Headscale の URL に書き換えて使う
	defaultTarget     = "100.64.0.2"                    // tailnet 内の peer IP
	defaultTargetPort = "22"                            // SSH バナーを読むため
)

func main() {
	var (
		hostname     = flag.String("hostname", "tsnet-demo", "tailnet 上で表示されるホスト名")
		controlURL   = flag.String("control-url", defaultControlURL, "Headscale の URL")
		target       = flag.String("target", defaultTarget, "Ping / Dial 先の Tailscale IP")
		targetPort   = flag.String("port", defaultTargetPort, "Dial 先の TCP ポート")
		stateDir     = flag.String("state-dir", "./tsnet-state", "tsnet の state 保存ディレクトリ")
		pingMode     = flag.String("ping-mode", "disco", "ping モード: disco | icmp | tsmp")
		acceptRoutes = flag.Bool("accept-routes", false, "subnet router の advertised routes を受信する (LAN ホスト到達用)")
		skipDial     = flag.Bool("skip-dial", false, "TCP Dial をスキップ (ping だけ試したいとき)")
		verbose      = flag.Bool("v", false, "tsnet の詳細ログを表示")
	)
	flag.Parse()

	// ping モード解釈
	var pingType tailcfg.PingType
	switch *pingMode {
	case "disco":
		pingType = tailcfg.PingDisco // Tailscale 独自の経路確認 (デフォルト)
	case "icmp":
		pingType = tailcfg.PingICMP // 本物の ICMP、 相手 OS に届く
	case "tsmp":
		pingType = tailcfg.PingTSMP // Tailscale Mesh Protocol
	default:
		log.Fatalf("invalid ping mode %q (disco | icmp | tsmp)", *pingMode)
	}

	authKey := os.Getenv("TS_AUTHKEY")

	logf := func(format string, args ...any) {} // デフォルトはサイレント
	if *verbose {
		logf = log.Printf
	}

	// -------- 1. tsnet ノードを起動して tailnet 参加 --------
	srv := &tsnet.Server{
		Hostname:   *hostname,
		Dir:        *stateDir,
		ControlURL: *controlURL,
		AuthKey:    authKey,
		Logf:       logf,
	}
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	fmt.Printf("--- Starting tsnet node ---\n")
	fmt.Printf("Hostname:    %s\n", *hostname)
	fmt.Printf("ControlURL:  %s\n", *controlURL)
	fmt.Printf("StateDir:    %s\n", *stateDir)
	if authKey == "" {
		fmt.Printf("AuthKey:     (none — assuming existing state)\n")
	} else {
		fmt.Printf("AuthKey:     %s...\n", authKey[:min(20, len(authKey))])
	}
	fmt.Println()

	status, err := srv.Up(ctx)
	if err != nil {
		log.Fatalf("tsnet up failed: %v\n  Hint: pre-auth key を発行して TS_AUTHKEY 環境変数に設定する:\n  ssh <HEADSCALE_HOST> \"sudo headscale preauthkey create -u <USER_ID> -e 1h --reusable\"", err)
	}
	fmt.Printf("OK joined tailnet as %s (%v)\n\n", *hostname, status.TailscaleIPs)

	// -------- 2. LocalClient で自分と peer 一覧を表示 --------
	lc, err := srv.LocalClient()
	if err != nil {
		log.Fatalf("LocalClient: %v", err)
	}

	// -------- 2.5 (optional) RouteAll=true で subnet route を受信 --------
	if *acceptRoutes {
		fmt.Println("--- Setting RouteAll=true (accept subnet routes) ---")
		_, err := lc.EditPrefs(ctx, &ipn.MaskedPrefs{
			Prefs: ipn.Prefs{
				RouteAll: true,
			},
			RouteAllSet: true,
		})
		if err != nil {
			log.Printf("EditPrefs failed: %v", err)
		} else {
			fmt.Println("OK  RouteAll=true. LAN hosts via subnet router should be reachable.")
			// route 反映に少し時間がかかる
			time.Sleep(2 * time.Second)
		}
		fmt.Println()
	}

	st, err := lc.Status(ctx)
	if err != nil {
		log.Fatalf("Status: %v", err)
	}

	fmt.Println("--- Tailnet peers ---")
	fmt.Printf("Self:  %-20s  %v\n", st.Self.HostName, st.Self.TailscaleIPs)
	for _, peer := range st.Peer {
		online := "  offline"
		if peer.Online {
			online = "  online "
		}
		fmt.Printf("Peer: %s%-20s  %v\n", online, peer.HostName, peer.TailscaleIPs)
	}
	fmt.Println()

	targetAddr, err := netip.ParseAddr(*target)
	if err != nil {
		log.Fatalf("invalid target IP %q: %v", *target, err)
	}

	// -------- 3. TCP Dial して SSH バナーを読む (skip-dial で省略可) --------
	if !*skipDial {
		dialAddr := net.JoinHostPort(targetAddr.String(), *targetPort)
		fmt.Printf("--- Dialing %s over tailnet ---\n", dialAddr)

		dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
		defer dialCancel()

		conn, err := srv.Dial(dialCtx, "tcp", dialAddr)
		if err != nil {
			log.Printf("Dial: %v", err)
		} else {
			defer conn.Close()
			fmt.Printf("OK  connected to %s\n", conn.RemoteAddr())

			// バナー読み取り (port 22 なら SSH バナーが来る)
			conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			buf := make([]byte, 256)
			n, err := conn.Read(buf)
			if err != nil && n == 0 {
				log.Printf("Read failed (ポート 22 でない場合は banner が来ない): %v", err)
			} else {
				fmt.Printf("    banner: %q\n", string(buf[:n]))
			}
		}
		fmt.Println()
	}

	// -------- 4. Tailscale 内部 Ping (Dial 後なので magicsock 準備済) --------
	fmt.Printf("--- Pinging %s (mode=%s, retry up to 3 times) ---\n", targetAddr, *pingMode)
	var pingRes *ipnstate.PingResult
	for attempt := 1; attempt <= 3; attempt++ {
		pingCtx, pingCancel := context.WithTimeout(ctx, 20*time.Second)
		pingRes, err = lc.Ping(pingCtx, targetAddr, pingType)
		pingCancel()
		if err == nil {
			break
		}
		fmt.Printf("  attempt %d failed: %v\n", attempt, err)
		if attempt < 3 {
			time.Sleep(2 * time.Second)
		}
	}
	if err != nil {
		log.Printf("Ping failed after retries: %v", err)
	} else {
		fmt.Printf("OK  latency=%.1fms\n", pingRes.LatencySeconds*1000)
		fmt.Printf("    endpoint=%s\n", pingRes.Endpoint)
		fmt.Printf("    derp=%s\n", pingRes.DERPRegionCode)
		if pingRes.Endpoint != "" {
			fmt.Printf("    → P2P 直結 (UDP hole-punching 成功)\n")
		} else if pingRes.DERPRegionCode != "" {
			fmt.Printf("    → DERP 経由 (region=%s, 自前 nogulab なら 900)\n", pingRes.DERPRegionCode)
		}
	}
	fmt.Println()

	fmt.Println("=== Done ===")
	fmt.Println("State は ./tsnet-state に保存。 次回は AuthKey 不要で再起動可能。")
	fmt.Println("クリーンアップ: rm -rf ./tsnet-state + headscale でノード削除")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
